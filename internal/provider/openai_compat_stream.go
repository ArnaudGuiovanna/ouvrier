package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// CompleteStream satisfies StreamingProvider for OpenAI-compatible endpoints.
// It issues a streaming chat-completions request and forwards text deltas via
// onDelta as they arrive, returning the fully assembled Response. onDelta may
// be nil.
func (p *openAICompatProvider) CompleteStream(ctx context.Context, req Request, onDelta func(Delta)) (Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	ref, _ := ParseModelID(req.Model)
	if ref.Provider != p.name {
		return Response{}, fmt.Errorf("%s provider cannot run model %q", p.name, req.Model)
	}
	started := time.Now()

	body, err := buildOpenAICompatRequest(ref.Name, req)
	if err != nil {
		return Response{}, err
	}
	body.Stream = true
	body.StreamOptions = &openAICompatStreamOpts{IncludeUsage: true}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("marshal %s request: %w", p.name, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.requestURL(ref.Name), bytes.NewReader(payload))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	p.setAuthHeader(httpReq.Header)

	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return Response{}, transportError(p.name, err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 16<<10))
		return Response{}, statusError(p.name, httpResp.Status, httpResp.StatusCode, string(raw))
	}

	resp, err := decodeOpenAICompatStream(httpResp.Body, onDelta)
	if err != nil {
		return Response{}, fmt.Errorf("decode %s response: %w", p.name, err)
	}
	return attachResponseMetadata(resp, p.name, req.Model, started, req, promptCacheUnsupported), nil
}

type openAICompatPendingToolCall struct {
	id   string
	name string
	args strings.Builder
}

func decodeOpenAICompatStream(r io.Reader, onDelta func(Delta)) (Response, error) {
	var text strings.Builder
	var finishReason string
	var usage openAICompatUsage
	var streamErr error
	toolCalls := map[int]*openAICompatPendingToolCall{}

	scanErr := scanSSE(r, func(ev sseEvent) bool {
		data := strings.TrimSpace(ev.Data)
		if data == "" || data == "[DONE]" {
			return data != "[DONE]"
		}
		var chunk openAICompatStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			streamErr = fmt.Errorf("decode provider stream event: %w", err)
			return false
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
			if choice.Delta.Content != "" {
				if err := providerTextOverflow(text.Len(), len(choice.Delta.Content)); err != nil {
					streamErr = err
					return false
				}
				text.WriteString(choice.Delta.Content)
				if onDelta != nil {
					onDelta(Delta{Text: choice.Delta.Content})
				}
			}
			for _, tc := range choice.Delta.ToolCalls {
				if tc.Index < 0 || tc.Index >= maxProviderToolCalls {
					streamErr = fmt.Errorf("provider stream tool index %d exceeds limit %d", tc.Index, maxProviderToolCalls)
					return false
				}
				pending := toolCalls[tc.Index]
				if pending == nil {
					if len(toolCalls) >= maxProviderToolCalls {
						streamErr = fmt.Errorf("provider stream exceeds %d tool calls", maxProviderToolCalls)
						return false
					}
					pending = &openAICompatPendingToolCall{}
					toolCalls[tc.Index] = pending
				}
				if tc.ID != "" {
					if err := validateProviderToolIdentity(tc.ID, pending.name); err != nil {
						streamErr = err
						return false
					}
					pending.id = tc.ID
				}
				if tc.Function.Name != "" {
					if err := validateProviderToolIdentity(pending.id, tc.Function.Name); err != nil {
						streamErr = err
						return false
					}
					pending.name = tc.Function.Name
				}
				if err := providerToolArgsOverflow(pending.args.Len(), len(tc.Function.Arguments)); err != nil {
					streamErr = err
					return false
				}
				pending.args.WriteString(tc.Function.Arguments)
			}
		}
		return true
	})
	if scanErr != nil {
		return Response{}, scanErr
	}
	if streamErr != nil {
		return Response{}, streamErr
	}

	calls, err := assembleOpenAICompatStreamToolCalls(toolCalls)
	if err != nil {
		return Response{}, err
	}
	stopReason, err := openAICompatStopReason(finishReason, len(calls) > 0)
	if err != nil {
		return Response{}, err
	}
	return Response{
		Text:       text.String(),
		ToolCalls:  calls,
		StopReason: stopReason,
		Usage: Usage{
			InputTokens:  usage.PromptTokens,
			OutputTokens: usage.CompletionTokens,
		},
	}, nil
}

func assembleOpenAICompatStreamToolCalls(pending map[int]*openAICompatPendingToolCall) ([]ToolCall, error) {
	if len(pending) == 0 {
		return nil, nil
	}
	indexes := make([]int, 0, len(pending))
	for idx := range pending {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	out := make([]ToolCall, 0, len(indexes))
	for _, idx := range indexes {
		p := pending[idx]
		args := strings.TrimSpace(p.args.String())
		if args == "" {
			args = "{}"
		}
		call := ToolCall{ID: p.id, Name: p.name, Arguments: json.RawMessage(args)}
		if err := call.Validate(); err != nil {
			return nil, err
		}
		out = append(out, call)
	}
	return out, nil
}
