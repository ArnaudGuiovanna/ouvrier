package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// CompleteStream satisfies StreamingProvider. It issues a streaming
// /v1/messages request and forwards text deltas via onDelta as they arrive,
// returning the fully assembled Response. onDelta may be nil.
func (a *Anthropic) CompleteStream(ctx context.Context, req Request, onDelta func(Delta)) (Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	ref, _ := ParseModelID(req.Model)
	if ref.Provider != a.Name() {
		return Response{}, fmt.Errorf("anthropic provider cannot run model %q", req.Model)
	}
	started := time.Now()

	body, err := buildAnthropicRequest(ref.Name, req)
	if err != nil {
		return Response{}, err
	}
	body.Stream = true
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("marshal anthropic request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	httpResp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return Response{}, transportError(a.Name(), err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 16<<10))
		return Response{}, statusError(a.Name(), httpResp.Status, httpResp.StatusCode, string(raw))
	}

	resp, err := decodeAnthropicStream(httpResp.Body, onDelta)
	if err != nil {
		return Response{}, err
	}
	resp.Metadata.PromptCache.Applied = resp.Metadata.PromptCache.Applied || anthropicPromptCacheRequestedPrefix(req)
	return attachResponseMetadata(resp, a.Name(), req.Model, started, req, promptCacheAnthropic), nil
}

type anthropicStreamEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	ContentBlock struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content_block"`
	Message struct {
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			CacheRead    int `json:"cache_read_input_tokens"`
			CacheCreate  int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type anthropicPendingToolUse struct {
	id   string
	name string
	args bytes.Buffer
}

func decodeAnthropicStream(r io.Reader, onDelta func(Delta)) (Response, error) {
	resp := Response{StopReason: StopEndTurn}
	var text bytes.Buffer
	var stopErr error
	blocks := map[int]*anthropicPendingToolUse{}

	scanErr := scanSSE(r, func(ev sseEvent) bool {
		if ev.Data == "" {
			return true
		}
		var msg anthropicStreamEvent
		if err := json.Unmarshal([]byte(ev.Data), &msg); err != nil {
			return true
		}
		switch msg.Type {
		case "message_start":
			resp.Usage.InputTokens = msg.Message.Usage.InputTokens
			resp.Metadata.PromptCache.ReadInputTokens = msg.Message.Usage.CacheRead
			if msg.Message.Usage.CacheRead > 0 || msg.Message.Usage.CacheCreate > 0 {
				resp.Metadata.PromptCache.Applied = true
				resp.Metadata.PromptCache.WriteInputTokens += msg.Message.Usage.CacheCreate
			}
		case "content_block_start":
			if msg.ContentBlock.Type == "tool_use" {
				blocks[msg.Index] = &anthropicPendingToolUse{id: msg.ContentBlock.ID, name: msg.ContentBlock.Name}
			}
		case "content_block_delta":
			switch msg.Delta.Type {
			case "text_delta":
				if msg.Delta.Text != "" {
					text.WriteString(msg.Delta.Text)
					if onDelta != nil {
						onDelta(Delta{Text: msg.Delta.Text})
					}
				}
			case "input_json_delta":
				if b := blocks[msg.Index]; b != nil {
					b.args.WriteString(msg.Delta.PartialJSON)
				}
			}
		case "message_delta":
			if msg.Delta.StopReason != "" {
				mapped, err := anthropicStopReason(msg.Delta.StopReason)
				if err != nil {
					stopErr = err
					return false
				}
				resp.StopReason = mapped
			}
			if msg.Usage.OutputTokens > 0 {
				resp.Usage.OutputTokens = msg.Usage.OutputTokens
			}
		}
		return true
	})
	if scanErr != nil {
		return Response{}, fmt.Errorf("decode anthropic stream: %w", scanErr)
	}
	if stopErr != nil {
		return Response{}, stopErr
	}

	resp.Text = text.String()
	for idx := 0; idx <= maxBlockIndex(blocks); idx++ {
		b, ok := blocks[idx]
		if !ok {
			continue
		}
		args := b.args.Bytes()
		if len(bytes.TrimSpace(args)) == 0 {
			args = []byte("{}")
		}
		resp.ToolCalls = append(resp.ToolCalls, ToolCall{
			ID:        b.id,
			Name:      b.name,
			Arguments: append(json.RawMessage(nil), args...),
		})
	}
	return resp, nil
}

func maxBlockIndex(blocks map[int]*anthropicPendingToolUse) int {
	max := -1
	for idx := range blocks {
		if idx > max {
			max = idx
		}
	}
	return max
}
