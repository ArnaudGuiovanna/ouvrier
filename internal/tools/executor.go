package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"

	"ouvrier/internal/policy"
	"ouvrier/internal/provider"
)

var (
	ErrInvalidTool  = errors.New("invalid tool")
	ErrToolNotFound = errors.New("tool not found")
)

type Executor struct {
	mu     sync.RWMutex
	tools  map[string]registeredTool
	policy policy.PermissionPolicy
}

type registeredTool struct {
	name     string
	fn       reflect.Value
	typ      reflect.Type
	handler  Handler
	metadata Metadata
}

func NewExecutor(options ...Option) *Executor {
	executor := &Executor{
		tools:  make(map[string]registeredTool),
		policy: policy.NewDefaultPolicy(),
	}
	for _, option := range options {
		if option != nil {
			option(executor)
		}
	}
	if executor.policy == nil {
		executor.policy = policy.NewDefaultPolicy()
	}
	return executor
}

func (e *Executor) NewScope() *Executor {
	if e == nil {
		return NewExecutor()
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return NewExecutor(WithPermissionPolicy(e.policy))
}

func (e *Executor) Register(name string, fn any, options ...RegisterOption) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidTool)
	}
	tool, err := newRegisteredTool(name, fn)
	if err != nil {
		return err
	}
	for _, option := range options {
		if option == nil {
			return fmt.Errorf("%w: nil register option", ErrInvalidTool)
		}
		option(&tool)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.tools[name] = tool
	return nil
}

func (e *Executor) RegisterHandler(name string, handler Handler, options ...RegisterOption) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidTool)
	}
	if handler == nil {
		return fmt.Errorf("%w: %q handler is required", ErrInvalidTool, name)
	}
	tool := registeredTool{
		name:     name,
		handler:  handler,
		metadata: Metadata{Effect: policy.EffectSideEffecting},
	}
	for _, option := range options {
		if option == nil {
			return fmt.Errorf("%w: nil register option", ErrInvalidTool)
		}
		option(&tool)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.tools[name] = tool
	return nil
}

func (e *Executor) Execute(ctx context.Context, call provider.ToolCall) (result provider.ToolResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return provider.ToolResult{}, err
	}
	if err := call.Validate(); err != nil {
		return provider.ToolResult{}, err
	}

	tool, ok := e.lookup(call.Name)
	if !ok {
		return provider.ToolResult{}, fmt.Errorf("%w: %s", ErrToolNotFound, call.Name)
	}
	if result, allowed, err := e.authorizeToolCall(ctx, tool, call); err != nil || !allowed {
		return result, err
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			result = errorResult(call, fmt.Errorf("tool panic: %v", recovered))
			err = nil
		}
	}()

	if tool.handler != nil {
		result, err := tool.handler.Execute(ctx, call)
		if err != nil {
			return errorResult(call, err), nil
		}
		return result, nil
	}

	if err := validateToolArguments(tool.metadata.InputSchema, call.Arguments); err != nil {
		return errorResult(call, err), nil
	}
	if err := reserveIdempotency(ctx, tool, call.Arguments); err != nil {
		return errorResult(call, err), nil
	}
	args, err := buildCallArgs(ctx, tool.typ, tool.metadata, call.Arguments)
	if err != nil {
		return errorResult(call, err), nil
	}
	return toolResultFromValues(call, tool.fn.Call(args))
}

func (e *Executor) lookup(name string) (registeredTool, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	tool, ok := e.tools[name]
	return tool, ok
}

func (e *Executor) CanRunParallelSubAgent(name string) bool {
	tool, ok := e.lookup(name)
	return ok && tool.metadata.Kind == ToolKindSubAgent
}

func newRegisteredTool(name string, fn any) (registeredTool, error) {
	value := reflect.ValueOf(fn)
	if !value.IsValid() || value.Kind() != reflect.Func {
		return registeredTool{}, fmt.Errorf("%w: %q must be a function", ErrInvalidTool, name)
	}
	if value.IsNil() {
		return registeredTool{}, fmt.Errorf("%w: %q function is nil", ErrInvalidTool, name)
	}
	if err := validateSignature(name, value.Type()); err != nil {
		return registeredTool{}, err
	}
	return registeredTool{
		name:     name,
		fn:       value,
		typ:      value.Type(),
		metadata: Metadata{Effect: policy.EffectSideEffecting},
	}, nil
}

func validateSignature(name string, typ reflect.Type) error {
	contextType := reflect.TypeFor[context.Context]()
	if typ.NumIn() == 0 || typ.In(0) != contextType {
		return fmt.Errorf("%w: %q first parameter must be context.Context", ErrInvalidTool, name)
	}
	if typ.NumIn() > 2 {
		return fmt.Errorf("%w: %q supports context plus at most one typed argument", ErrInvalidTool, name)
	}

	errorType := reflect.TypeFor[error]()
	switch typ.NumOut() {
	case 1:
		if typ.Out(0) != errorType {
			return fmt.Errorf("%w: %q must return error", ErrInvalidTool, name)
		}
	case 2:
		if typ.Out(1) != errorType {
			return fmt.Errorf("%w: %q second return value must be error", ErrInvalidTool, name)
		}
	default:
		return fmt.Errorf("%w: %q must return error or (value, error)", ErrInvalidTool, name)
	}
	return nil
}

func buildCallArgs(ctx context.Context, typ reflect.Type, metadata Metadata, raw json.RawMessage) ([]reflect.Value, error) {
	args := []reflect.Value{reflect.ValueOf(ctx)}
	if typ.NumIn() == 1 {
		return args, nil
	}

	value, err := decodeArgument(typ.In(1), metadata, raw)
	if err != nil {
		return nil, err
	}
	return append(args, value), nil
}

func decodeArgument(typ reflect.Type, metadata Metadata, raw json.RawMessage) (reflect.Value, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte(`{}`)
	}
	var err error
	raw, err = unwrapSingleValueArgument(typ, metadata.ArgumentName, raw)
	if err != nil {
		return reflect.Value{}, fmt.Errorf("decode tool arguments: %w", err)
	}
	if typ.Kind() == reflect.Pointer {
		value := reflect.New(typ.Elem())
		if err := decodeJSONArgument(raw, value.Interface(), shouldRejectUnknownFields(typ.Elem())); err != nil {
			return reflect.Value{}, fmt.Errorf("decode tool arguments: %w", err)
		}
		return value, nil
	}

	value := reflect.New(typ)
	if err := decodeJSONArgument(raw, value.Interface(), shouldRejectUnknownFields(typ)); err != nil {
		return reflect.Value{}, fmt.Errorf("decode tool arguments: %w", err)
	}
	return value.Elem(), nil
}

func shouldRejectUnknownFields(typ reflect.Type) bool {
	return typ.Kind() == reflect.Struct
}

func validateToolArguments(schemaJSON json.RawMessage, raw json.RawMessage) error {
	if len(bytes.TrimSpace(schemaJSON)) == 0 {
		return nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte(`{}`)
	}

	var parsed jsonschema.Schema
	if err := json.Unmarshal(schemaJSON, &parsed); err != nil {
		return fmt.Errorf("validate tool arguments: decode schema JSON: %w", err)
	}
	resolved, err := parsed.Resolve(nil)
	if err != nil {
		return fmt.Errorf("validate tool arguments: resolve schema: %w", err)
	}

	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("validate tool arguments: decode JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("validate tool arguments: arguments must contain a single JSON value")
	}
	if err := resolved.Validate(value); err != nil {
		return fmt.Errorf("validate tool arguments: %w", err)
	}
	return nil
}

func decodeJSONArgument(raw json.RawMessage, target any, rejectUnknown bool) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if rejectUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("arguments must contain a single JSON value")
	}
	return nil
}

func unwrapSingleValueArgument(typ reflect.Type, name string, raw json.RawMessage) (json.RawMessage, error) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() == reflect.Struct {
		return raw, nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return raw, nil
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return raw, nil
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil {
		return nil, err
	}
	value, ok := object[name]
	if !ok {
		return nil, fmt.Errorf("missing field %q", name)
	}
	if len(object) != 1 {
		unknown := make([]string, 0, len(object)-1)
		for key := range object {
			if key != name {
				unknown = append(unknown, key)
			}
		}
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown field %q", unknown[0])
	}
	return value, nil
}

func toolResultFromValues(call provider.ToolCall, values []reflect.Value) (provider.ToolResult, error) {
	errValue := values[len(values)-1]
	if !errValue.IsNil() {
		return errorResult(call, errValue.Interface().(error)), nil
	}
	if len(values) == 1 {
		return successResult(call, "ok")
	}
	return successResult(call, values[0].Interface())
}

func successResult(call provider.ToolCall, value any) (provider.ToolResult, error) {
	content, err := json.Marshal(value)
	if err != nil {
		return provider.ToolResult{}, fmt.Errorf("marshal tool result: %w", err)
	}
	return provider.ToolResult{
		ToolCallID: call.ID,
		Name:       call.Name,
		Content:    content,
	}, nil
}

func errorResult(call provider.ToolCall, err error) provider.ToolResult {
	content, _ := json.Marshal(err.Error())
	return provider.ToolResult{
		ToolCallID: call.ID,
		Name:       call.Name,
		Content:    content,
		IsError:    true,
	}
}
