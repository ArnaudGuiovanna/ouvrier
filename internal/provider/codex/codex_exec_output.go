package codex

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

const (
	maxCodexExecLineBytes   = 1 << 20
	maxCodexExecStdoutBytes = 8 << 20
	maxCodexExecStdoutLines = 100_000
	maxCodexExecTextBytes   = 1 << 20
	maxCodexExecStderrBytes = 64 << 10
	codexExecProcessWait    = 2 * time.Second
)

// codexExecOutput is a bounded Scanner replacement used as Cmd.Stdout. Giving
// os/exec ownership of the copy goroutine lets Cmd.WaitDelay close inherited
// pipes after the Codex leader exits, while Write errors cancel the whole
// process group immediately on malformed or excessive output.
type codexExecOutput struct {
	mu          sync.Mutex
	pending     []byte
	text        strings.Builder
	stdoutBytes int
	stdoutLines int
	onDelta     func(provider.Delta)
	cancel      context.CancelFunc
	err         error
}

func newCodexExecOutput(cancel context.CancelFunc, onDelta func(provider.Delta)) *codexExecOutput {
	return &codexExecOutput{cancel: cancel, onDelta: onDelta}
}

func (o *codexExecOutput) Write(data []byte) (int, error) {
	if o == nil {
		return 0, fmt.Errorf("codex provider stdout collector is nil")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return 0, o.err
	}
	if len(data) > maxCodexExecStdoutBytes-o.stdoutBytes {
		return 0, o.failLocked(fmt.Errorf("stdout exceeds %d bytes", maxCodexExecStdoutBytes))
	}
	o.stdoutBytes += len(data)

	originalLength := len(data)
	consumed := 0
	for len(data) > 0 {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			if len(data) > maxCodexExecLineBytes-len(o.pending) {
				return consumed, o.failLocked(fmt.Errorf("stdout line exceeds %d bytes", maxCodexExecLineBytes))
			}
			o.pending = append(o.pending, data...)
			return originalLength, nil
		}
		part := data[:newline]
		if len(part) > maxCodexExecLineBytes-len(o.pending) {
			return consumed, o.failLocked(fmt.Errorf("stdout line exceeds %d bytes", maxCodexExecLineBytes))
		}
		o.pending = append(o.pending, part...)
		consumed += newline + 1
		data = data[newline+1:]
		if err := o.acceptLineLocked(dropCodexExecCarriageReturn(string(o.pending))); err != nil {
			return consumed, err
		}
		o.pending = o.pending[:0]
	}
	return originalLength, nil
}

func (o *codexExecOutput) Flush() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return o.err
	}
	if len(o.pending) == 0 {
		return nil
	}
	line := dropCodexExecCarriageReturn(string(o.pending))
	o.pending = nil
	return o.acceptLineLocked(line)
}

func (o *codexExecOutput) acceptLineLocked(line string) error {
	if o.stdoutLines >= maxCodexExecStdoutLines {
		return o.failLocked(fmt.Errorf("stdout exceeds %d lines", maxCodexExecStdoutLines))
	}
	o.stdoutLines++
	chunk, err := agentTextFromJSONL(line)
	if err != nil {
		return o.failLocked(fmt.Errorf("stdout contains %w", err))
	}
	if chunk == "" {
		return nil
	}
	extra := len(chunk)
	if o.text.Len() > 0 {
		extra++
	}
	if extra > maxCodexExecTextBytes-o.text.Len() {
		return o.failLocked(fmt.Errorf("assistant text exceeds %d bytes", maxCodexExecTextBytes))
	}
	if o.text.Len() > 0 {
		o.text.WriteByte('\n')
	}
	o.text.WriteString(chunk)
	if o.onDelta != nil {
		o.onDelta(provider.Delta{Text: chunk})
	}
	return nil
}

func (o *codexExecOutput) failLocked(err error) error {
	if o.err == nil {
		o.err = err
		if o.cancel != nil {
			o.cancel()
		}
	}
	return o.err
}

func (o *codexExecOutput) Text() string {
	if o == nil {
		return ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.TrimSpace(o.text.String())
}

func (o *codexExecOutput) Err() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

func dropCodexExecCarriageReturn(line string) string {
	return strings.TrimSuffix(line, "\r")
}
