package ovr

import (
	"fmt"
	"strings"
	"time"
)

const (
	defaultBashToolName       = "bash"
	defaultBashTimeout        = 30 * time.Second
	defaultBashMaxOutputBytes = 64 * 1024
)

// BashOption configures the sandboxed Bash capability.
type BashOption interface {
	applyBash(*bashSpec)
}

type bashSpec struct {
	name                string
	sandbox             SandboxConfig
	timeout             time.Duration
	maxOutputBytes      int
	unsafeHostExecution bool
	err                 error
}

type bashPipeOption struct {
	spec bashSpec
}

// Bash declares a bash command tool named "bash" for a Pipe.
//
// By default Bash requires a platform sandbox that can enforce workspace,
// process, environment, and network boundaries. The runtime fails at startup
// when those guarantees are unavailable.
func Bash(sandbox SandboxConfig, options ...BashOption) PipeOption {
	spec := bashSpec{
		name:           defaultBashToolName,
		sandbox:        sandbox,
		timeout:        defaultBashTimeout,
		maxOutputBytes: defaultBashMaxOutputBytes,
	}
	for _, option := range options {
		if option == nil {
			spec.setErr(fmt.Errorf("%w: nil Bash option", ErrInvalidNode))
			continue
		}
		option.applyBash(&spec)
	}
	return bashPipeOption{spec: spec}
}

func (o bashPipeOption) applyPipe(config *pipeConfig) {
	if len(config.bash) > 0 {
		config.setErr(fmt.Errorf("%w: Bash declared more than once", ErrInvalidNode))
		return
	}
	config.bash = append(config.bash, o.spec)
}

type bashTimeoutOption struct {
	duration time.Duration
	err      error
}

// BashTimeout configures the maximum wall-clock duration for one bash command.
func BashTimeout(value string) BashOption {
	duration, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return bashTimeoutOption{err: fmt.Errorf("%w: Bash timeout must be a valid duration", ErrInvalidNode)}
	}
	if duration <= 0 {
		return bashTimeoutOption{err: fmt.Errorf("%w: Bash timeout must be greater than zero", ErrInvalidNode)}
	}
	return bashTimeoutOption{duration: duration}
}

func (o bashTimeoutOption) applyBash(spec *bashSpec) {
	if o.err != nil {
		spec.setErr(o.err)
		return
	}
	spec.timeout = o.duration
}

type bashMaxOutputBytesOption struct {
	max int
}

// BashMaxOutputBytes bounds captured stdout and stderr for one bash command.
func BashMaxOutputBytes(max int) BashOption {
	return bashMaxOutputBytesOption{max: max}
}

func (o bashMaxOutputBytesOption) applyBash(spec *bashSpec) {
	if o.max <= 0 {
		spec.setErr(fmt.Errorf("%w: Bash max output bytes must be greater than zero", ErrInvalidNode))
		return
	}
	spec.maxOutputBytes = o.max
}

type bashUnsafeHostExecutionOption struct{}

// UnsafeBashHostExecution allows the host-shell Bash fallback.
//
// Use this only for local/dev or trusted workloads. Ouvrier still enforces
// ToolExecutor permission decisions, environment allowlisting, working
// directory resolution, timeout, and bounded output, but this option does not
// provide a real OS sandbox.
func UnsafeBashHostExecution() BashOption {
	return bashUnsafeHostExecutionOption{}
}

func (bashUnsafeHostExecutionOption) applyBash(spec *bashSpec) {
	spec.unsafeHostExecution = true
}

func (s bashSpec) validateBash() error {
	if s.err != nil {
		return s.err
	}
	if strings.TrimSpace(s.name) == "" {
		return fmt.Errorf("%w: Bash tool name is required", ErrInvalidNode)
	}
	if strings.TrimSpace(s.sandbox.root) == "" {
		return fmt.Errorf("%w: Bash sandbox root is required", ErrInvalidNode)
	}
	if s.timeout <= 0 {
		return fmt.Errorf("%w: Bash timeout must be greater than zero", ErrInvalidNode)
	}
	if s.maxOutputBytes <= 0 {
		return fmt.Errorf("%w: Bash max output bytes must be greater than zero", ErrInvalidNode)
	}
	return nil
}

func (s *bashSpec) setErr(err error) {
	if s.err == nil {
		s.err = err
	}
}
