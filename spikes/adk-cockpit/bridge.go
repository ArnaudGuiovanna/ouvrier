package adkspike

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/adk/v2/agent"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// GovernedExecutor is the only authority allowed to execute a cockpit tool.
// A production implementation must include Ouvrier permission checks,
// sandboxing, redaction, transcript persistence and audit emission.
type GovernedExecutor interface {
	Execute(context.Context, ToolCall) (ToolResult, error)
}

// ToolCall is the ADK-neutral request handed to Ouvrier's governed boundary.
type ToolCall struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input,omitempty"`
}

// ToolResult is the ADK-neutral result returned by Ouvrier's governed boundary.
type ToolResult struct {
	Summary  string         `json:"summary"`
	Verified bool           `json:"verified,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
}

// ToolSpec is declarative metadata for one Ouvrier-native tool. It cannot
// carry an ADK tool implementation or an executable callback.
type ToolSpec struct {
	Name                  string
	Description           string
	CompletesWhenVerified bool
}

type toolArguments struct {
	Attempt int    `json:"attempt,omitempty"`
	Goal    string `json:"goal,omitempty"`
	Query   string `json:"query,omitempty"`
}

// newGovernedTool is deliberately unexported: callers cannot obtain a raw ADK
// tool or attach an operation that bypasses GovernedExecutor.
func newGovernedTool(spec ToolSpec, executor GovernedExecutor, proofs *proofTracker) (adktool.Tool, error) {
	spec.Name = strings.TrimSpace(spec.Name)
	if spec.Name == "" {
		return nil, errors.New("adk spike: tool name is required")
	}
	if executor == nil {
		return nil, fmt.Errorf("adk spike: governed executor is required for %q", spec.Name)
	}
	if proofs == nil {
		return nil, fmt.Errorf("adk spike: proof tracker is required for %q", spec.Name)
	}
	handler := func(ctx agent.Context, args toolArguments) (ToolResult, error) {
		input := map[string]any{}
		if args.Attempt != 0 {
			input["attempt"] = args.Attempt
		}
		if args.Goal != "" {
			input["goal"] = args.Goal
		}
		if args.Query != "" {
			input["query"] = args.Query
		}
		detachedInput, err := cloneJSONMap(input)
		if err != nil {
			return ToolResult{}, fmt.Errorf("clone governed tool input: %w", err)
		}
		result, err := executor.Execute(ctx, ToolCall{
			ID:    ctx.FunctionCallID(),
			Name:  spec.Name,
			Input: detachedInput,
		})
		if err != nil {
			return ToolResult{}, err
		}
		detachedData, err := cloneJSONMap(result.Data)
		if err != nil {
			return ToolResult{}, fmt.Errorf("clone governed tool result: %w", err)
		}
		if spec.CompletesWhenVerified && result.Verified {
			proofs.record(ctx.InvocationID(), ctx.FunctionCallID(), spec.Name)
			ctx.Actions().Escalate = true
			ctx.Actions().SkipSummarization = true
		}
		return ToolResult{
			Summary:  result.Summary,
			Verified: result.Verified,
			Data:     detachedData,
		}, nil
	}
	return functiontool.New(functiontool.Config{
		Name:        spec.Name,
		Description: spec.Description,
	}, handler)
}

// cloneJSONMap establishes ownership at adapter boundaries. JSON is the
// cockpit tool contract, so values that cannot round-trip through JSON fail
// closed instead of being shared by reference.
func cloneJSONMap(src map[string]any) (map[string]any, error) {
	if src == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(src)
	if err != nil {
		return nil, err
	}
	var dst map[string]any
	if err := json.Unmarshal(encoded, &dst); err != nil {
		return nil, err
	}
	return dst, nil
}
