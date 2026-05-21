package ovr

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"ouvrier/internal/policy"
)

// ToolOption configures a Go tool registered on a Pipe.
type ToolOption interface {
	applyTool(*toolSpec)
}

type toolSpec struct {
	name             string
	fn               any
	fnType           reflect.Type
	description      string
	params           map[string]string
	effect           policy.Effect
	idempotencyKey   string
	sideEffects      []string
	requiresApproval bool
	timeout          time.Duration
	err              error
}

type toolPipeOption struct {
	spec toolSpec
}

// Tool registers a Go function as an agent tool for a Pipe.
func Tool(name string, goFunc any, options ...ToolOption) PipeOption {
	spec := toolSpec{
		name:   strings.TrimSpace(name),
		fn:     goFunc,
		effect: policy.EffectSideEffecting,
	}
	if goFunc != nil {
		if typ := reflect.TypeOf(goFunc); typ.Kind() == reflect.Func {
			spec.fnType = typ
		}
	}

	for _, option := range options {
		if option == nil {
			spec.setErr(fmt.Errorf("%w: nil Tool option", ErrInvalidNode))
			continue
		}
		option.applyTool(&spec)
	}

	return toolPipeOption{spec: spec}
}

type readOnlyOption struct{}

// ReadOnly marks a Tool as side-effect free and eligible for safe retry/parallel policies.
func ReadOnly() ToolOption {
	return readOnlyOption{}
}

func (readOnlyOption) applyTool(spec *toolSpec) {
	spec.effect = policy.EffectReadOnly
	spec.idempotencyKey = ""
	spec.sideEffects = nil
}

type sideEffectingOption struct {
	labels []string
}

// SideEffecting marks a Tool as mutating external state.
func SideEffecting(labels ...string) ToolOption {
	return sideEffectingOption{labels: cleanToolLabels(labels)}
}

func (o sideEffectingOption) applyTool(spec *toolSpec) {
	spec.effect = policy.EffectSideEffecting
	spec.idempotencyKey = ""
	spec.sideEffects = append([]string(nil), o.labels...)
	if len(spec.sideEffects) == 0 {
		spec.setErr(fmt.Errorf("%w: Tool side effect label is required", ErrInvalidNode))
	}
}

type idempotentOption struct {
	keyExpression string
}

// Idempotent marks a side-effecting Tool as replay-safe for a stable key expression.
func Idempotent(keyExpression string) ToolOption {
	return idempotentOption{keyExpression: strings.TrimSpace(keyExpression)}
}

func (o idempotentOption) applyTool(spec *toolSpec) {
	if o.keyExpression == "" {
		spec.setErr(fmt.Errorf("%w: Tool idempotency key is required", ErrInvalidNode))
		return
	}
	spec.effect = policy.EffectIdempotent
	spec.idempotencyKey = o.keyExpression
}

type requiresApprovalOption struct{}

// RequiresApproval marks a Tool as blocked unless an explicit policy allows it.
func RequiresApproval() ToolOption {
	return requiresApprovalOption{}
}

func (requiresApprovalOption) applyTool(spec *toolSpec) {
	spec.requiresApproval = true
}

type toolTimeoutOption struct {
	duration time.Duration
	err      error
}

// ToolTimeout configures the maximum wall-clock duration for one Tool invocation.
func ToolTimeout(value string) ToolOption {
	duration, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return toolTimeoutOption{err: fmt.Errorf("%w: Tool timeout must be a valid duration", ErrInvalidNode)}
	}
	if duration <= 0 {
		return toolTimeoutOption{err: fmt.Errorf("%w: Tool timeout must be greater than zero", ErrInvalidNode)}
	}
	return toolTimeoutOption{duration: duration}
}

func (o toolTimeoutOption) applyTool(spec *toolSpec) {
	if o.err != nil {
		spec.setErr(o.err)
		return
	}
	spec.timeout = o.duration
}

func cleanToolLabels(labels []string) []string {
	cleaned := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label != "" {
			cleaned = append(cleaned, label)
		}
	}
	return cleaned
}

func (o toolPipeOption) applyPipe(config *pipeConfig) {
	config.tools = append(config.tools, o.spec)
}

func (s toolSpec) validateTool() error {
	if s.err != nil {
		return s.err
	}
	if s.name == "" {
		return fmt.Errorf("%w: Tool name is required", ErrInvalidNode)
	}

	value := reflect.ValueOf(s.fn)
	if !value.IsValid() || value.Kind() != reflect.Func {
		return fmt.Errorf("%w: Tool %q must be a function", ErrInvalidNode, s.name)
	}
	if value.IsNil() {
		return fmt.Errorf("%w: Tool %q function is nil", ErrInvalidNode, s.name)
	}

	typ := value.Type()
	contextType := reflect.TypeFor[context.Context]()
	if typ.NumIn() == 0 || typ.In(0) != contextType {
		return fmt.Errorf("%w: Tool %q first parameter must be context.Context", ErrInvalidNode, s.name)
	}

	errorType := reflect.TypeFor[error]()
	switch typ.NumOut() {
	case 1:
		if typ.Out(0) != errorType {
			return fmt.Errorf("%w: Tool %q must return error", ErrInvalidNode, s.name)
		}
	case 2:
		if typ.Out(1) != errorType {
			return fmt.Errorf("%w: Tool %q second return value must be error", ErrInvalidNode, s.name)
		}
	default:
		return fmt.Errorf("%w: Tool %q must return error or (value, error)", ErrInvalidNode, s.name)
	}

	return nil
}

func (s *toolSpec) setErr(err error) {
	if s.err == nil {
		s.err = err
	}
}

type describeOption struct {
	text string
}

// Describe sets the LLM-facing description for a Tool.
func Describe(text string) ToolOption {
	return describeOption{text: strings.TrimSpace(text)}
}

func (o describeOption) applyTool(spec *toolSpec) {
	if o.text == "" {
		spec.setErr(fmt.Errorf("%w: Tool description is required", ErrInvalidNode))
		return
	}
	spec.description = o.text
}

type paramOption struct {
	name        string
	description string
}

// Param sets the LLM-facing description for a Tool parameter.
func Param(name, description string) ToolOption {
	return paramOption{
		name:        strings.TrimSpace(name),
		description: strings.TrimSpace(description),
	}
}

func (o paramOption) applyTool(spec *toolSpec) {
	if o.name == "" {
		spec.setErr(fmt.Errorf("%w: Tool parameter name is required", ErrInvalidNode))
		return
	}
	if o.description == "" {
		spec.setErr(fmt.Errorf("%w: Tool parameter %q description is required", ErrInvalidNode, o.name))
		return
	}
	if spec.params == nil {
		spec.params = make(map[string]string)
	}
	if _, exists := spec.params[o.name]; exists {
		spec.setErr(fmt.Errorf("%w: Tool parameter %q described more than once", ErrInvalidNode, o.name))
		return
	}
	spec.params[o.name] = o.description
}
