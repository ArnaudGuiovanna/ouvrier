package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"

	"ouvrier/internal/provider"
	"ouvrier/internal/sandbox"
)

const DefaultBashMaxOutputBytes = 64 * 1024

type BashHandlerConfig struct {
	MaxOutputBytes     int
	AllowHostExecution bool
}

type BashHandler struct {
	sandbox        *sandbox.Sandbox
	shellPath      string
	maxOutputBytes int
}

type BashRequest struct {
	Command string `json:"command"`
	Workdir string `json:"workdir,omitempty"`
}

type BashResult struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr,omitempty"`
	ExitCode        int    `json:"exit_code"`
	TimedOut        bool   `json:"timed_out,omitempty"`
	Truncated       bool   `json:"truncated,omitempty"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
	Error           string `json:"error,omitempty"`
}

func NewBashHandler(sandbox *sandbox.Sandbox, cfg BashHandlerConfig) (*BashHandler, error) {
	if sandbox == nil {
		return nil, fmt.Errorf("%w: bash sandbox is required", ErrInvalidTool)
	}
	if !cfg.AllowHostExecution {
		return nil, fmt.Errorf("%w: bash host execution cannot enforce filesystem, process, and network isolation", ErrInvalidTool)
	}
	shellPath, err := exec.LookPath("bash")
	if err != nil {
		return nil, fmt.Errorf("%w: bash executable not found", ErrInvalidTool)
	}
	maxOutputBytes := cfg.MaxOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = DefaultBashMaxOutputBytes
	}
	return &BashHandler{
		sandbox:        sandbox,
		shellPath:      shellPath,
		maxOutputBytes: maxOutputBytes,
	}, nil
}

func (h *BashHandler) Execute(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := decodeBashRequest(call.Arguments)
	if err != nil {
		return bashToolResult(call, BashResult{ExitCode: -1, Error: err.Error()}, true)
	}
	workdir, err := h.resolveWorkdir(request.Workdir)
	if err != nil {
		return bashToolResult(call, BashResult{ExitCode: -1, Error: err.Error()}, true)
	}

	stdout := &boundedBuffer{limit: h.maxOutputBytes}
	stderr := &boundedBuffer{limit: h.maxOutputBytes}
	cmd := exec.CommandContext(ctx, h.shellPath, "-c", request.Command)
	cmd.Dir = workdir
	cmd.Env = environmentList(h.sandbox.Environment())
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	configureProcessGroupKill(cmd)

	runErr := cmd.Run()
	result := BashResult{
		Stdout:          stdout.String(),
		Stderr:          stderr.String(),
		ExitCode:        bashExitCode(runErr),
		TimedOut:        ctx.Err() != nil,
		Truncated:       stdout.Truncated() || stderr.Truncated(),
		StdoutTruncated: stdout.Truncated(),
		StderrTruncated: stderr.Truncated(),
	}
	if runErr != nil {
		result.Error = runErr.Error()
		if result.TimedOut {
			result.Error = ctx.Err().Error()
		}
		return bashToolResult(call, result, true)
	}
	return bashToolResult(call, result, false)
}

func decodeBashRequest(raw json.RawMessage) (BashRequest, error) {
	var request BashRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return BashRequest{}, fmt.Errorf("decode bash arguments: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err != nil {
			return BashRequest{}, fmt.Errorf("decode bash arguments: %w", err)
		}
		return BashRequest{}, errors.New("decode bash arguments: arguments must contain a single JSON value")
	}
	request.Command = strings.TrimSpace(request.Command)
	if request.Command == "" {
		return BashRequest{}, errors.New("bash command is required")
	}
	if strings.ContainsRune(request.Command, 0) {
		return BashRequest{}, errors.New("bash command is invalid")
	}
	request.Workdir = strings.TrimSpace(request.Workdir)
	if request.Workdir == "" {
		request.Workdir = "."
	}
	return request, nil
}

func (h *BashHandler) resolveWorkdir(path string) (string, error) {
	workdir, err := h.sandbox.Resolve(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(workdir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: bash workdir must be a directory", ErrInvalidTool)
	}
	return workdir, nil
}

func configureProcessGroupKill(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

func bashExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func bashToolResult(call provider.ToolCall, result BashResult, isError bool) (provider.ToolResult, error) {
	content, err := json.Marshal(result)
	if err != nil {
		return provider.ToolResult{}, err
	}
	return provider.ToolResult{
		ToolCallID: call.ID,
		Name:       call.Name,
		Content:    content,
		IsError:    isError,
	}, nil
}

func environmentList(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}

type boundedBuffer struct {
	limit     int
	buf       bytes.Buffer
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return len(p), nil
	}
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || len(p) > 0
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	_, _ = b.buf.Write(p)
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	return b.buf.String()
}

func (b *boundedBuffer) Truncated() bool {
	return b.truncated
}
