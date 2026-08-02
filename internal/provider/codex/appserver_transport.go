package codex

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	maxAppServerMessageBytes = 8 << 20
	maxAppServerStderrBytes  = 64 << 10
	appServerCloseTimeout    = 750 * time.Millisecond
	appServerMessageQueue    = 8
	appServerWriteQueue      = 4
)

// AppServerTransport starts one Codex app-server process. It is injectable so
// the bidirectional protocol can be tested without a Codex installation.
type AppServerTransport interface {
	LookPath(file string) (string, error)
	Start(name string, args ...string) (AppServerProcess, error)
}

// AppServerProcess is a JSONL message transport for one app-server process.
// Send and Receive exchange complete JSON values without trailing newlines.
// Close must be safe to call concurrently with either method and must unblock
// both: provider cancellation relies on that lifecycle contract.
type AppServerProcess interface {
	Send(context.Context, []byte) error
	Receive(context.Context) ([]byte, error)
	Stderr() string
	Close() error
}

type defaultAppServerTransport struct{}

func (defaultAppServerTransport) LookPath(file string) (string, error) {
	return exec.LookPath(file)
}

func (defaultAppServerTransport) Start(name string, args ...string) (AppServerProcess, error) {
	cmd := exec.Command(name, args...)
	// The cockpit may have loaded a worker .env into the parent process. Codex
	// app-server is a trusted model transport, but it must never inherit that
	// worker's credentials implicitly. Keep only the small process/auth/runtime
	// environment needed by a signed-in Codex CLI.
	cmd.Env = sanitizedCodexEnvironment(os.Environ())
	if err := configureAppServerProcess(cmd); err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	stderr := newBoundedBuffer(maxAppServerStderrBytes)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}

	process := &stdioAppServerProcess{
		cmd:      cmd,
		stdin:    stdin,
		stdout:   stdout,
		stderr:   stderr,
		messages: make(chan []byte, appServerMessageQueue),
		writes:   make(chan appServerWrite, appServerWriteQueue),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go process.writeLoop()
	go process.readAndWait()
	return process, nil
}

var codexEnvironmentAllowlist = map[string]struct{}{
	"PATH": {}, "HOME": {}, "USER": {}, "LOGNAME": {}, "SHELL": {},
	"TMPDIR": {}, "TMP": {}, "TEMP": {},
	"LANG": {}, "LANGUAGE": {}, "LC_ALL": {}, "TERM": {}, "COLORTERM": {},
	"XDG_CONFIG_HOME": {}, "XDG_CACHE_HOME": {}, "XDG_DATA_HOME": {}, "XDG_RUNTIME_DIR": {},
	"CODEX_HOME":    {},
	"SSL_CERT_FILE": {}, "SSL_CERT_DIR": {},
	"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "ALL_PROXY": {}, "NO_PROXY": {},
	"http_proxy": {}, "https_proxy": {}, "all_proxy": {}, "no_proxy": {},
}

func sanitizedCodexEnvironment(environ []string) []string {
	out := make([]string, 0, len(codexEnvironmentAllowlist))
	seen := make(map[string]struct{}, len(codexEnvironmentAllowlist))
	for _, item := range environ {
		name, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if _, allowed := codexEnvironmentAllowlist[name]; !allowed {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, item)
	}
	return out
}

type appServerWrite struct {
	message []byte
	done    chan error
}

type stdioAppServerProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *boundedBuffer

	messages chan []byte
	writes   chan appServerWrite
	stop     chan struct{}
	done     chan struct{}

	closeOnce sync.Once
	stopOnce  sync.Once
	errMu     sync.RWMutex
	finalErr  error
	closeErr  error
}

func (p *stdioAppServerProcess) Send(ctx context.Context, message []byte) error {
	if len(message) > maxAppServerMessageBytes {
		return fmt.Errorf("codex app-server message exceeds %d bytes", maxAppServerMessageBytes)
	}
	request := appServerWrite{
		message: append([]byte(nil), message...),
		done:    make(chan error, 1),
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.stop:
		return io.ErrClosedPipe
	case <-p.done:
		return p.processError()
	case p.writes <- request:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.stop:
		return io.ErrClosedPipe
	case <-p.done:
		return p.processError()
	case err := <-request.done:
		return err
	}
}

func (p *stdioAppServerProcess) Receive(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.stop:
		return nil, io.ErrClosedPipe
	case message, ok := <-p.messages:
		if !ok {
			return nil, p.processError()
		}
		return message, nil
	}
}

func (p *stdioAppServerProcess) Stderr() string { return p.stderr.String() }

func (p *stdioAppServerProcess) Close() error {
	p.closeOnce.Do(func() {
		p.signalStop()
		_ = p.stdin.Close()
		terminateErr := terminateAppServerProcess(p.cmd)

		timer := time.NewTimer(appServerCloseTimeout)
		defer timer.Stop()
		select {
		case <-p.done:
		case <-timer.C:
			// Closing stdout also releases Scanner if a broken process tree
			// retained the pipe. We still return a hard failure: callers must
			// not assume the requested containment guarantee was enforced.
			_ = p.stdout.Close()
			p.closeErr = fmt.Errorf("codex app-server process did not terminate within %s", appServerCloseTimeout)
		}
		if terminateErr != nil {
			p.closeErr = errors.Join(p.closeErr, fmt.Errorf("terminate codex app-server process group: %w", terminateErr))
		}
		if p.closeErr == nil {
			err := p.processError()
			if !errors.Is(err, io.EOF) && !isKilledProcessError(err) {
				p.closeErr = err
			}
		}
	})
	return p.closeErr
}

func (p *stdioAppServerProcess) writeLoop() {
	for {
		select {
		case <-p.stop:
			return
		case <-p.done:
			return
		case request := <-p.writes:
			select {
			case <-p.stop:
				request.done <- io.ErrClosedPipe
				return
			default:
			}
			message := append(request.message, '\n')
			_, err := p.stdin.Write(message)
			request.done <- err
			if err != nil {
				p.signalStop()
				_ = terminateAppServerProcess(p.cmd)
				return
			}
		}
	}
}

func (p *stdioAppServerProcess) readAndWait() {
	scanner := bufio.NewScanner(p.stdout)
	scanner.Buffer(make([]byte, 0, 64<<10), maxAppServerMessageBytes)

readLoop:
	for scanner.Scan() {
		message := append([]byte(nil), scanner.Bytes()...)
		select {
		case p.messages <- message:
		case <-p.stop:
			break readLoop
		}
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		p.signalStop()
		_ = terminateAppServerProcess(p.cmd)
	}
	waitErr := p.cmd.Wait()
	if scanErr != nil {
		p.setFinalError(fmt.Errorf("read Codex app-server stdout: %w", scanErr))
	} else if waitErr != nil {
		p.setFinalError(waitErr)
	} else {
		p.setFinalError(io.EOF)
	}
	close(p.messages)
	close(p.done)
}

func (p *stdioAppServerProcess) signalStop() {
	p.stopOnce.Do(func() { close(p.stop) })
}

func (p *stdioAppServerProcess) setFinalError(err error) {
	p.errMu.Lock()
	p.finalErr = err
	p.errMu.Unlock()
}

func (p *stdioAppServerProcess) processError() error {
	p.errMu.RLock()
	err := p.finalErr
	p.errMu.RUnlock()
	if err == nil {
		return io.ErrClosedPipe
	}
	return err
}

func isKilledProcessError(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ProcessState != nil && !exitErr.ProcessState.Success()
}

type boundedBuffer struct {
	mu        sync.RWMutex
	limit     int
	buf       []byte
	truncated bool
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit}
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	originalLength := len(data)
	remaining := b.limit - len(b.buf)
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
			b.truncated = true
		}
		b.buf = append(b.buf, data...)
	} else if len(data) > 0 {
		b.truncated = true
	}
	return originalLength, nil
}

func (b *boundedBuffer) String() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	text := string(b.buf)
	if b.truncated {
		text += "\n[stderr truncated]"
	}
	return text
}
