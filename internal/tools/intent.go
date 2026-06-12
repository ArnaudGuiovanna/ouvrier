package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/policy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

// ToolIntent is the metadata-only record of one non-read tool call written
// before the tool executes and completed after it returns, so durable-run
// recovery can detect indeterminate side effects. It never carries tool
// arguments or results: IdemKey is the idempotency reservation key for
// idempotent tools (matching idempotency.go's hashing exactly) and a
// tool-name + arguments hash otherwise.
type ToolIntent struct {
	ExecID     string
	ToolCallID string
	StepIndex  int
	ToolName   string
	Effect     string
	IdemKey    string
}

// ToolIntentRecorder is the seam between the tool executor and the durable
// run journal. BeginToolIntent must persist the intent before the tool body
// runs; CompleteToolIntent stamps the same (execID, toolCallID) row once the
// call has returned a definite outcome.
type ToolIntentRecorder interface {
	BeginToolIntent(context.Context, ToolIntent) error
	CompleteToolIntent(ctx context.Context, execID, toolCallID string) error
}

type toolIntentContextValue struct {
	recorder  ToolIntentRecorder
	execID    string
	stepIndex int
}

type toolIntentContextKey struct{}

// ContextWithToolIntentRecorder installs the durable-run tool intent recorder
// plus the executing pipeline's exec id and top-level step index. When absent
// (durable runs off), the executor writes nothing.
func ContextWithToolIntentRecorder(ctx context.Context, recorder ToolIntentRecorder, execID string, stepIndex int) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if recorder == nil || strings.TrimSpace(execID) == "" {
		return ctx
	}
	return context.WithValue(ctx, toolIntentContextKey{}, toolIntentContextValue{
		recorder:  recorder,
		execID:    strings.TrimSpace(execID),
		stepIndex: stepIndex,
	})
}

func toolIntentFromContext(ctx context.Context) (toolIntentContextValue, bool) {
	if ctx == nil {
		return toolIntentContextValue{}, false
	}
	value, ok := ctx.Value(toolIntentContextKey{}).(toolIntentContextValue)
	return value, ok && value.recorder != nil && value.execID != ""
}

// beginToolIntent writes the intent row for a non-read tool call and returns
// the completion func to invoke once the call has a definite outcome. It
// returns (nil, nil) when no recorder is installed or the tool is read-only.
func beginToolIntent(ctx context.Context, tool registeredTool, call provider.ToolCall) (func(context.Context) error, error) {
	value, ok := toolIntentFromContext(ctx)
	if !ok {
		return nil, nil
	}
	effect := normalizeEffect(tool.metadata.Effect)
	if effect == policy.EffectReadOnly {
		return nil, nil
	}

	intent := ToolIntent{
		ExecID:     value.execID,
		ToolCallID: call.ID,
		StepIndex:  value.stepIndex,
		ToolName:   tool.name,
		Effect:     string(effect),
		IdemKey:    toolIntentIdemKey(tool, call),
	}
	if err := value.recorder.BeginToolIntent(ctx, intent); err != nil {
		return nil, fmt.Errorf("record tool intent: %w", err)
	}
	return func(completeCtx context.Context) error {
		if err := value.recorder.CompleteToolIntent(completeCtx, value.execID, call.ID); err != nil {
			return fmt.Errorf("complete tool intent: %w", err)
		}
		return nil
	}, nil
}

// toolIntentIdemKey derives the key recovery uses to decide whether a call's
// side effect can be reconciled: for idempotent tools with a declared key
// expression it is the exact ReserveIdempotency key, otherwise a hash of the
// tool name and raw arguments.
func toolIntentIdemKey(tool registeredTool, call provider.ToolCall) string {
	if normalizeEffect(tool.metadata.Effect) == policy.EffectIdempotent {
		if expression := strings.TrimSpace(tool.metadata.IdempotencyKey); expression != "" {
			if key, err := idempotencyReservationKey(tool.name, expression, call.Arguments); err == nil {
				return key
			}
		}
	}
	sum := sha256.Sum256([]byte(tool.name + "\x00" + string(call.Arguments)))
	return "args:" + hex.EncodeToString(sum[:])
}
