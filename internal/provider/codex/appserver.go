package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

const (
	appServerClientVersion = "1"
	appServerGuard         = `Ouvrier is the sole capability governor for this session. Use only the dynamic function tools supplied by Ouvrier. Never invoke built-in shell, file-editing, MCP, app, plugin, skill, web-search, image, browser, permission, or sub-agent tools. If the supplied dynamic tools are insufficient, explain what is missing instead of using another capability.`
	maxAppServerInbox      = 32
	maxAppServerTextBytes  = 1 << 20
	maxAppServerTextItems  = 4096
)

var errAppServerClosed = errors.New("codex app-server provider is closed")

var (
	_ provider.StreamingProvider = (*AppServerProvider)(nil)
	_ interface{ Close() error } = (*AppServerProvider)(nil)
	_ interface {
		AbortTurn(context.Context) error
	} = (*AppServerProvider)(nil)
)

// AppServerProvider drives Codex's structured app-server protocol. Unlike the
// exec Provider, it surfaces dynamic tool requests to Ouvrier's provider loop;
// Ouvrier remains responsible for authorizing and executing every capability.
//
// App-server is stateful while a dynamic tool call is outstanding. Calls to
// Complete must therefore be serialized and the next request must contain the
// matching provider.ToolResult. AppServerProvider enforces that contract.
type AppServerProvider struct {
	Transport AppServerTransport
	Bin       string
	Model     string
	CWD       string

	// gate serializes the stateful app-server protocol while still allowing a
	// queued Complete call to honor its own context. It is deliberately not
	// used by Close: Close must tear down a process while Complete is blocked.
	gateOnce sync.Once
	gate     chan struct{}

	// lifecycleMu protects process and closed. Protocol fields below remain
	// owned by gate, so process I/O never happens while lifecycleMu is held.
	lifecycleMu sync.Mutex
	process     AppServerProcess
	closed      bool

	nextID      int64
	initialized bool
	turn        *appServerTurn
	inbox       []rpcEnvelope
}

type appServerTurn struct {
	threadID string
	turnID   string
	tools    map[string]struct{}
	pending  *pendingDynamicCall
	text     []string
	deltas   map[string]*strings.Builder
	deltaIDs []string
	textCost int

	usageTotal        appServerTokenCounts
	pendingUsage      provider.Usage
	pendingCacheRead  int
	pendingCacheWrite int
}

type appServerTokenCounts struct {
	input      int
	output     int
	cacheRead  int
	cacheWrite int
}

type pendingDynamicCall struct {
	requestID json.RawMessage
	call      provider.ToolCall
}

// NewAppServer constructs the structured Codex app-server provider. cwd may be
// empty, in which case Codex resolves its normal process working directory.
func NewAppServer(model, cwd string) *AppServerProvider {
	return &AppServerProvider{
		Transport: defaultAppServerTransport{},
		Bin:       "codex",
		Model:     model,
		CWD:       cwd,
	}
}

func (p *AppServerProvider) Name() string { return "codex" }

func (p *AppServerProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	return p.complete(ctx, req, nil)
}

func (p *AppServerProvider) CompleteStream(ctx context.Context, req provider.Request, onDelta func(provider.Delta)) (provider.Response, error) {
	return p.complete(ctx, req, onDelta)
}

func (p *AppServerProvider) complete(ctx context.Context, req provider.Request, onDelta func(provider.Delta)) (provider.Response, error) {
	if err := p.acquire(ctx); err != nil {
		return provider.Response{}, err
	}
	defer p.release()

	if p.isClosed() {
		return provider.Response{}, errAppServerClosed
	}
	if err := ctx.Err(); err != nil {
		return provider.Response{}, err
	}

	if p.turn != nil && p.turn.pending != nil {
		if err := p.answerPendingTool(ctx, req); err != nil {
			return provider.Response{}, err
		}
	} else if p.turn != nil {
		return provider.Response{}, p.failProtocol(errors.New("active Codex turn has no pending dynamic tool call"))
	} else {
		if err := validateDynamicTools(req.Tools); err != nil {
			return provider.Response{}, err
		}
		if err := p.ensureInitialized(ctx); err != nil {
			return provider.Response{}, err
		}
		if err := p.startTurn(ctx, req); err != nil {
			return provider.Response{}, err
		}
	}

	resp, err := p.readTurn(ctx, onDelta)
	if err != nil {
		return resp, err
	}
	return resp, nil
}

func (p *AppServerProvider) acquire(ctx context.Context) error {
	p.gateOnce.Do(func() {
		p.gate = make(chan struct{}, 1)
		p.gate <- struct{}{}
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.gate:
		return nil
	}
}

func (p *AppServerProvider) release() { p.gate <- struct{}{} }

func (p *AppServerProvider) ensureInitialized(ctx context.Context) error {
	if p.isClosed() {
		return errAppServerClosed
	}
	if p.initialized && p.currentProcess() != nil {
		return nil
	}
	transport := p.Transport
	if transport == nil {
		transport = defaultAppServerTransport{}
	}
	bin := strings.TrimSpace(p.Bin)
	if bin == "" {
		bin = "codex"
	}
	if _, err := transport.LookPath(bin); err != nil {
		return fmt.Errorf("codex app-server provider: %s not found on PATH", bin)
	}
	process, err := transport.Start(bin, "app-server", "--listen", "stdio://")
	if err != nil {
		return fmt.Errorf("start codex app-server: %w", err)
	}
	if err := p.installProcess(process); err != nil {
		_ = process.Close()
		return err
	}
	p.nextID = 0
	p.inbox = nil

	params := map[string]any{
		"clientInfo": map[string]any{
			"name":    "ouvrier",
			"title":   "Ouvrier",
			"version": appServerClientVersion,
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	}
	if _, err := p.request(ctx, "initialize", params); err != nil {
		return p.failProtocol(fmt.Errorf("initialize Codex app-server: %w", err))
	}
	if err := p.send(ctx, rpcNotification{Method: "initialized"}); err != nil {
		return p.failProtocol(fmt.Errorf("notify Codex app-server initialization: %w", err))
	}
	p.initialized = true
	return nil
}

func (p *AppServerProvider) startTurn(ctx context.Context, req provider.Request) error {
	dynamicTools := make([]dynamicToolSpec, 0, len(req.Tools))
	toolNames := make(map[string]struct{}, len(req.Tools))
	for _, tool := range req.Tools {
		dynamicTools = append(dynamicTools, dynamicToolSpec{
			Type:        "function",
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: append(json.RawMessage(nil), tool.InputSchema...),
		})
		toolNames[tool.Name] = struct{}{}
	}

	params := map[string]any{
		// The sandbox and disabled capability config are the enforcement
		// boundary. developerInstructions is defense in depth for model
		// behavior, not an authorization mechanism.
		"approvalPolicy":        "never",
		"sandbox":               "read-only",
		"ephemeral":             true,
		"dynamicTools":          dynamicTools,
		"developerInstructions": appServerGuard,
		"environments":          []any{},
		"serviceName":           "ouvrier",
		"config":                restrictedAppServerConfig(),
	}
	if system := strings.TrimSpace(req.System); system != "" {
		params["baseInstructions"] = system
	}
	if model := modelName(req.Model, p.Model); model != "" {
		params["model"] = model
	}
	if cwd := strings.TrimSpace(p.CWD); cwd != "" {
		params["cwd"] = cwd
	}

	raw, err := p.request(ctx, "thread/start", params)
	if err != nil {
		return p.failProtocol(fmt.Errorf("start Codex app-server thread: %w", err))
	}
	var threadResponse struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &threadResponse); err != nil {
		return p.failProtocol(fmt.Errorf("decode Codex thread/start response: %w", err))
	}
	if strings.TrimSpace(threadResponse.Thread.ID) == "" {
		return p.failProtocol(errors.New("codex thread/start response omitted thread.id"))
	}

	turnParams := map[string]any{
		"threadId":       threadResponse.Thread.ID,
		"input":          []map[string]string{{"type": "text", "text": renderAppServerInput(req.Messages)}},
		"approvalPolicy": "never",
		"sandboxPolicy": map[string]any{
			"type":          "readOnly",
			"networkAccess": false,
		},
		"environments": []any{},
	}
	if cwd := strings.TrimSpace(p.CWD); cwd != "" {
		turnParams["cwd"] = cwd
	}
	raw, err = p.request(ctx, "turn/start", turnParams)
	if err != nil {
		return p.failProtocol(fmt.Errorf("start Codex app-server turn: %w", err))
	}
	var turnResponse struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(raw, &turnResponse); err != nil {
		return p.failProtocol(fmt.Errorf("decode Codex turn/start response: %w", err))
	}
	if strings.TrimSpace(turnResponse.Turn.ID) == "" {
		return p.failProtocol(errors.New("codex turn/start response omitted turn.id"))
	}
	p.turn = &appServerTurn{
		threadID: threadResponse.Thread.ID,
		turnID:   turnResponse.Turn.ID,
		tools:    toolNames,
		deltas:   make(map[string]*strings.Builder),
	}
	return nil
}

func restrictedAppServerConfig() map[string]any {
	return map[string]any{
		"web_search":            "disabled",
		"project_doc_max_bytes": 0,
		"mcp_servers":           map[string]any{},
		"history": map[string]any{
			"persistence": "none",
		},
		"agents": map[string]any{
			"enabled": false,
		},
		"apps": map[string]any{
			"_default": map[string]any{"enabled": false},
		},
		"features": map[string]any{
			"apps":                         false,
			"multi_agent":                  false,
			"plugins":                      false,
			"shell_tool":                   false,
			"skill_mcp_dependency_install": false,
			"unified_exec":                 false,
		},
	}
}

func validateDynamicTools(tools []provider.ToolSpec) error {
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			return errors.New("codex app-server dynamic tool name is required")
		}
		if name != tool.Name {
			return fmt.Errorf("codex app-server dynamic tool name %q has surrounding whitespace", tool.Name)
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("codex app-server dynamic tool %q is duplicated", name)
		}
		seen[name] = struct{}{}
		if len(tool.InputSchema) == 0 || !json.Valid(tool.InputSchema) {
			return fmt.Errorf("codex app-server dynamic tool %q has an invalid input schema", name)
		}
	}
	return nil
}

func (p *AppServerProvider) answerPendingTool(ctx context.Context, req provider.Request) error {
	pending := p.turn.pending
	result, ok := findToolResult(req.Messages, pending.call.ID)
	if !ok {
		return fmt.Errorf("codex app-server: result for dynamic tool call %q is required", pending.call.ID)
	}
	if result.Name != pending.call.Name {
		return fmt.Errorf("codex app-server: result tool %q does not match pending tool %q", result.Name, pending.call.Name)
	}
	text := toolResultText(result.Content)
	response := rpcResult{
		ID: pending.requestID,
		Result: map[string]any{
			"contentItems": []map[string]string{{"type": "inputText", "text": text}},
			"success":      !result.IsError,
		},
	}
	if err := p.send(ctx, response); err != nil {
		return p.failProtocol(fmt.Errorf("answer Codex dynamic tool call: %w", err))
	}
	p.turn.pending = nil
	return nil
}

func findToolResult(messages []provider.Message, callID string) (provider.ToolResult, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		for j := len(messages[i].Blocks) - 1; j >= 0; j-- {
			result := messages[i].Blocks[j].ToolResult
			if messages[i].Blocks[j].Type == provider.BlockToolResult && result != nil && result.ToolCallID == callID {
				return *result, true
			}
		}
	}
	return provider.ToolResult{}, false
}

func toolResultText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	if len(content) == 0 {
		return ""
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, content); err == nil {
		return compact.String()
	}
	return string(content)
}

func (p *AppServerProvider) readTurn(ctx context.Context, onDelta func(provider.Delta)) (provider.Response, error) {
	for {
		envelope, err := p.nextEnvelope(ctx)
		if err != nil {
			response := p.takeResponse(p.takeText())
			if p.isClosed() {
				_ = p.shutdownProcess()
				return response, errAppServerClosed
			}
			return response, p.failProtocol(fmt.Errorf("read Codex app-server event: %w", err))
		}

		if envelope.Method != "" && envelope.hasID() {
			if envelope.Method != "item/tool/call" {
				err := p.rejectServerRequest(ctx, envelope)
				return p.takeResponse(p.takeText()), p.failProtocol(err)
			}
			call, err := p.parseDynamicCall(envelope)
			if err != nil {
				_ = p.sendRPCError(ctx, envelope.ID, -32602, err.Error())
				return p.takeResponse(p.takeText()), p.failProtocol(err)
			}
			p.turn.pending = &pendingDynamicCall{requestID: append(json.RawMessage(nil), envelope.ID...), call: call}
			response := p.takeResponse(p.takeText())
			response.ToolCalls = []provider.ToolCall{call}
			response.StopReason = provider.StopToolUse
			return response, nil
		}

		if envelope.Method == "" {
			return p.takeResponse(p.takeText()), p.failProtocol(errors.New("unexpected Codex app-server response while a turn is active"))
		}
		if isAppServerRequestMethod(envelope.Method) {
			return p.takeResponse(p.takeText()), p.failProtocol(fmt.Errorf("codex server request %q omitted its id", envelope.Method))
		}
		switch envelope.Method {
		case "item/agentMessage/delta":
			var params struct {
				ThreadID string `json:"threadId"`
				TurnID   string `json:"turnId"`
				ItemID   string `json:"itemId"`
				Delta    string `json:"delta"`
			}
			if err := json.Unmarshal(envelope.Params, &params); err != nil {
				return p.takeResponse(p.takeText()), p.failProtocol(fmt.Errorf("decode Codex agent delta: %w", err))
			}
			if err := p.validateTurnScope(params.ThreadID, params.TurnID); err != nil {
				return p.takeResponse(p.takeText()), p.failProtocol(err)
			}
			if params.ItemID == "" {
				return p.takeResponse(p.takeText()), p.failProtocol(errors.New("codex agent delta omitted itemId"))
			}
			if err := p.turn.appendAgentDelta(params.ItemID, params.Delta); err != nil {
				return p.takeResponse(p.takeText()), p.failProtocol(err)
			}
			if onDelta != nil && params.Delta != "" {
				onDelta(provider.Delta{Text: params.Delta})
			}

		case "item/completed":
			var params struct {
				ThreadID string `json:"threadId"`
				TurnID   string `json:"turnId"`
				Item     struct {
					ID   string `json:"id"`
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
			}
			if err := json.Unmarshal(envelope.Params, &params); err != nil {
				return p.takeResponse(p.takeText()), p.failProtocol(fmt.Errorf("decode Codex completed item: %w", err))
			}
			if err := p.validateTurnScope(params.ThreadID, params.TurnID); err != nil {
				return p.takeResponse(p.takeText()), p.failProtocol(err)
			}
			if err := p.turn.completeAgentItem(params.Item.ID, params.Item.Type, params.Item.Text); err != nil {
				return p.takeResponse(p.takeText()), p.failProtocol(err)
			}

		case "thread/tokenUsage/updated":
			if err := p.observeTokenUsage(envelope.Params); err != nil {
				return p.takeResponse(p.takeText()), p.failProtocol(err)
			}

		case "turn/completed":
			var params struct {
				ThreadID string `json:"threadId"`
				Turn     struct {
					ID     string `json:"id"`
					Status string `json:"status"`
					Error  *struct {
						Message string `json:"message"`
					} `json:"error"`
				} `json:"turn"`
			}
			if err := json.Unmarshal(envelope.Params, &params); err != nil {
				return p.takeResponse(p.takeText()), p.failProtocol(fmt.Errorf("decode Codex completed turn: %w", err))
			}
			if err := p.validateTurnScope(params.ThreadID, params.Turn.ID); err != nil {
				return p.takeResponse(p.takeText()), p.failProtocol(err)
			}
			response := p.takeResponse(p.takeText())
			p.turn = nil
			switch params.Turn.Status {
			case "completed":
				response.StopReason = provider.StopEndTurn
				return response, nil
			case "failed", "interrupted":
				message := params.Turn.Status
				if params.Turn.Error != nil && strings.TrimSpace(params.Turn.Error.Message) != "" {
					message += ": " + params.Turn.Error.Message
				}
				return response, fmt.Errorf("codex app-server turn %s", message)
			default:
				return response, p.failProtocol(fmt.Errorf("codex turn completed with unknown status %q", params.Turn.Status))
			}

		case "error":
			var params struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal(envelope.Params, &params)
			if strings.TrimSpace(params.Error.Message) == "" {
				params.Error.Message = "unspecified Codex app-server error"
			}
			return p.takeResponse(p.takeText()), p.failProtocol(errors.New(params.Error.Message))

		default:
			// Lifecycle, reasoning, and warning notifications do not authorize
			// capabilities and are intentionally ignored here.
		}
	}
}

// observeTokenUsage consumes the stable thread/tokenUsage/updated shape from
// the Codex app-server protocol. The notification contains cumulative totals;
// responses expose only the positive delta since the previous Ouvrier
// response so multi-tool turns are not double-counted by the harness.
func (p *AppServerProvider) observeTokenUsage(raw json.RawMessage) error {
	var params struct {
		ThreadID   string `json:"threadId"`
		TurnID     string `json:"turnId"`
		TokenUsage struct {
			Total *struct {
				InputTokens           *int64 `json:"inputTokens"`
				OutputTokens          *int64 `json:"outputTokens"`
				CachedInputTokens     *int64 `json:"cachedInputTokens"`
				CacheWriteInputTokens int64  `json:"cacheWriteInputTokens"`
			} `json:"total"`
		} `json:"tokenUsage"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return fmt.Errorf("decode Codex token usage: %w", err)
	}
	if err := p.validateTurnScope(params.ThreadID, params.TurnID); err != nil {
		return err
	}
	if params.TokenUsage.Total == nil {
		return errors.New("codex token usage omitted tokenUsage.total")
	}
	total := params.TokenUsage.Total
	if total.InputTokens == nil || total.OutputTokens == nil || total.CachedInputTokens == nil {
		return errors.New("codex token usage omitted a required token count")
	}
	input, err := appServerTokenCount("inputTokens", *total.InputTokens)
	if err != nil {
		return err
	}
	output, err := appServerTokenCount("outputTokens", *total.OutputTokens)
	if err != nil {
		return err
	}
	cacheRead, err := appServerTokenCount("cachedInputTokens", *total.CachedInputTokens)
	if err != nil {
		return err
	}
	cacheWrite, err := appServerTokenCount("cacheWriteInputTokens", total.CacheWriteInputTokens)
	if err != nil {
		return err
	}
	next := appServerTokenCounts{input: input, output: output, cacheRead: cacheRead, cacheWrite: cacheWrite}
	previous := p.turn.usageTotal
	if next.input < previous.input || next.output < previous.output || next.cacheRead < previous.cacheRead || next.cacheWrite < previous.cacheWrite {
		return errors.New("codex token usage totals regressed")
	}
	p.turn.pendingUsage.InputTokens += next.input - previous.input
	p.turn.pendingUsage.OutputTokens += next.output - previous.output
	p.turn.pendingCacheRead += next.cacheRead - previous.cacheRead
	p.turn.pendingCacheWrite += next.cacheWrite - previous.cacheWrite
	p.turn.usageTotal = next
	return nil
}

func appServerTokenCount(name string, value int64) (int, error) {
	if value < 0 || uint64(value) > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("codex token usage %s is out of range", name)
	}
	return int(value), nil
}

func (p *AppServerProvider) takeResponse(text string) provider.Response {
	response := provider.Response{Text: text}
	if p.turn == nil {
		return response
	}
	response.Usage = p.turn.pendingUsage
	response.Metadata.PromptCache.Supported = true
	response.Metadata.PromptCache.ReadInputTokens = p.turn.pendingCacheRead
	response.Metadata.PromptCache.WriteInputTokens = p.turn.pendingCacheWrite
	response.Metadata.PromptCache.Applied = p.turn.pendingCacheRead > 0 || p.turn.pendingCacheWrite > 0
	p.turn.pendingUsage = provider.Usage{}
	p.turn.pendingCacheRead = 0
	p.turn.pendingCacheWrite = 0
	return response
}

func (p *AppServerProvider) parseDynamicCall(envelope rpcEnvelope) (provider.ToolCall, error) {
	if p.turn == nil || p.turn.pending != nil {
		return provider.ToolCall{}, errors.New("unexpected Codex dynamic tool request")
	}
	var params struct {
		Arguments json.RawMessage `json:"arguments"`
		CallID    string          `json:"callId"`
		Namespace *string         `json:"namespace"`
		ThreadID  string          `json:"threadId"`
		Tool      string          `json:"tool"`
		TurnID    string          `json:"turnId"`
	}
	if err := json.Unmarshal(envelope.Params, &params); err != nil {
		return provider.ToolCall{}, fmt.Errorf("decode Codex dynamic tool request: %w", err)
	}
	if params.Namespace != nil && strings.TrimSpace(*params.Namespace) != "" {
		return provider.ToolCall{}, fmt.Errorf("codex requested unsupported dynamic tool namespace %q", *params.Namespace)
	}
	if err := p.validateTurnScope(params.ThreadID, params.TurnID); err != nil {
		return provider.ToolCall{}, err
	}
	if _, ok := p.turn.tools[params.Tool]; !ok {
		return provider.ToolCall{}, fmt.Errorf("codex requested undeclared dynamic tool %q", params.Tool)
	}
	call := provider.ToolCall{
		ID:        params.CallID,
		Name:      params.Tool,
		Arguments: append(json.RawMessage(nil), params.Arguments...),
	}
	if err := call.Validate(); err != nil {
		return provider.ToolCall{}, fmt.Errorf("invalid Codex dynamic tool request: %w", err)
	}
	if len(call.Arguments) == 0 || !json.Valid(call.Arguments) {
		return provider.ToolCall{}, errors.New("codex dynamic tool arguments are not valid JSON")
	}
	return call, nil
}

func (p *AppServerProvider) validateTurnScope(threadID, turnID string) error {
	if p.turn == nil {
		return errors.New("codex event arrived without an active turn")
	}
	if threadID != p.turn.threadID || turnID != p.turn.turnID {
		return fmt.Errorf("codex event scope %q/%q does not match active turn %q/%q", threadID, turnID, p.turn.threadID, p.turn.turnID)
	}
	return nil
}

func (p *AppServerProvider) takeText() string {
	if p.turn == nil {
		return ""
	}
	return p.turn.takeAgentText()
}

func (p *AppServerProvider) rejectServerRequest(ctx context.Context, envelope rpcEnvelope) error {
	message := fmt.Sprintf("Ouvrier refused non-dynamic Codex server request %q", envelope.Method)
	var response any
	switch envelope.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		response = rpcResult{ID: envelope.ID, Result: map[string]any{"decision": "cancel"}}
	case "item/permissions/requestApproval":
		response = rpcResult{ID: envelope.ID, Result: map[string]any{"permissions": map[string]any{}}}
	case "item/tool/requestUserInput":
		response = rpcResult{ID: envelope.ID, Result: map[string]any{"answers": map[string]any{}}}
	case "mcpServer/elicitation/request":
		response = rpcResult{ID: envelope.ID, Result: map[string]any{"action": "cancel", "content": nil}}
	case "applyPatchApproval", "execCommandApproval":
		response = rpcResult{ID: envelope.ID, Result: map[string]any{"decision": "abort"}}
	default:
		response = rpcErrorResponse{
			ID: envelope.ID,
			Error: rpcError{
				Code:    -32001,
				Message: message,
			},
		}
	}
	if err := p.send(ctx, response); err != nil {
		return fmt.Errorf("%s (send refusal: %w)", message, err)
	}
	return errors.New(message)
}

func isAppServerRequestMethod(method string) bool {
	switch method {
	case "item/commandExecution/requestApproval",
		"item/fileChange/requestApproval",
		"item/tool/requestUserInput",
		"mcpServer/elicitation/request",
		"item/permissions/requestApproval",
		"item/tool/call",
		"account/chatgptAuthTokens/refresh",
		"attestation/generate",
		"currentTime/read",
		"applyPatchApproval",
		"execCommandApproval":
		return true
	default:
		return false
	}
}

func (p *AppServerProvider) sendRPCError(ctx context.Context, id json.RawMessage, code int, message string) error {
	return p.send(ctx, rpcErrorResponse{
		ID: id,
		Error: rpcError{
			Code:    code,
			Message: message,
		},
	})
}

func (p *AppServerProvider) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	p.nextID++
	id := p.nextID
	if err := p.send(ctx, rpcRequest{Method: method, ID: id, Params: params}); err != nil {
		return nil, err
	}
	for {
		envelope, err := p.receive(ctx)
		if err != nil {
			return nil, err
		}
		if envelope.Method != "" {
			if envelope.hasID() {
				if err := p.rejectServerRequest(ctx, envelope); err != nil {
					return nil, err
				}
			}
			if len(p.inbox) >= maxAppServerInbox {
				return nil, fmt.Errorf("codex app-server notification inbox exceeds %d messages", maxAppServerInbox)
			}
			p.inbox = append(p.inbox, envelope)
			continue
		}
		if !envelope.hasID() {
			return nil, errors.New("codex app-server sent a response without an id")
		}
		if strings.TrimSpace(string(envelope.ID)) != fmt.Sprintf("%d", id) {
			return nil, fmt.Errorf("codex app-server response id %s does not match request %d", envelope.ID, id)
		}
		if envelope.Error != nil {
			return nil, fmt.Errorf("codex app-server RPC error %d: %s", envelope.Error.Code, envelope.Error.Message)
		}
		if envelope.Result == nil {
			return nil, errors.New("codex app-server response omitted result")
		}
		return envelope.Result, nil
	}
}

func (p *AppServerProvider) nextEnvelope(ctx context.Context) (rpcEnvelope, error) {
	if len(p.inbox) > 0 {
		envelope := p.inbox[0]
		p.inbox = p.inbox[1:]
		return envelope, nil
	}
	return p.receive(ctx)
}

func (p *AppServerProvider) receive(ctx context.Context) (rpcEnvelope, error) {
	process := p.currentProcess()
	if process == nil {
		return rpcEnvelope{}, errors.New("codex app-server process is not running")
	}
	raw, err := process.Receive(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			shutdownErr := p.shutdownProcess()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return rpcEnvelope{}, errors.Join(ctxErr, shutdownErr)
			}
			return rpcEnvelope{}, errors.Join(err, shutdownErr)
		}
		stderr := strings.TrimSpace(process.Stderr())
		if stderr != "" {
			return rpcEnvelope{}, fmt.Errorf("codex app-server exited (%w): %s", err, stderr)
		}
		return rpcEnvelope{}, err
	}
	var envelope rpcEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return rpcEnvelope{}, fmt.Errorf("decode Codex app-server JSONL: %w", err)
	}
	if len(envelope.JSONRPC) != 0 {
		return rpcEnvelope{}, errors.New("codex app-server message unexpectedly included jsonrpc header")
	}
	if envelope.hasID() && !validRPCID(envelope.ID) {
		return rpcEnvelope{}, errors.New("codex app-server message contained an invalid id")
	}
	return envelope, nil
}

func (p *AppServerProvider) send(ctx context.Context, value any) error {
	process := p.currentProcess()
	if process == nil {
		return errors.New("codex app-server process is not running")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return process.Send(ctx, raw)
}

func (p *AppServerProvider) failProtocol(err error) error {
	if shutdownErr := p.shutdownProcess(); shutdownErr != nil {
		err = errors.Join(err, fmt.Errorf("terminate Codex app-server: %w", shutdownErr))
	}
	return fmt.Errorf("codex app-server provider: %w", err)
}

func (p *AppServerProvider) shutdownProcess() error {
	process := p.detachProcess()
	var err error
	if process != nil {
		err = process.Close()
	}
	p.initialized = false
	p.turn = nil
	p.inbox = nil
	return err
}

// AbortTurn abandons an incomplete stateful turn without permanently closing
// the provider. Dynamic tool calls leave app-server waiting for a matching
// result; callers must invoke AbortTurn when their outer tool loop exits before
// delivering that result. The next Complete call then starts a fresh bounded
// process/thread instead of accidentally continuing stale protocol state.
func (p *AppServerProvider) AbortTurn(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := p.acquire(ctx); err != nil {
		return err
	}
	defer p.release()
	if p.isClosed() {
		return errAppServerClosed
	}
	if p.turn == nil {
		return nil
	}
	if err := p.shutdownProcess(); err != nil {
		return fmt.Errorf("abort Codex app-server turn: %w", err)
	}
	return nil
}

// Close interrupts any active turn and terminates the app-server process.
func (p *AppServerProvider) Close() error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	process := p.process
	p.process = nil
	if process == nil {
		return nil
	}
	// Hard-close is the authoritative cancellation boundary. A best-effort
	// turn/interrupt request could itself block behind the active protocol
	// exchange, whereas closing the bounded transport immediately interrupts
	// both Send and Receive and terminates the whole process group.
	return process.Close()
}

func (p *AppServerProvider) isClosed() bool {
	p.lifecycleMu.Lock()
	closed := p.closed
	p.lifecycleMu.Unlock()
	return closed
}

func (p *AppServerProvider) currentProcess() AppServerProcess {
	p.lifecycleMu.Lock()
	process := p.process
	p.lifecycleMu.Unlock()
	return process
}

func (p *AppServerProvider) installProcess(process AppServerProcess) error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.closed {
		return errAppServerClosed
	}
	if p.process != nil {
		return errors.New("codex app-server process is already running")
	}
	p.process = process
	return nil
}

func (p *AppServerProvider) detachProcess() AppServerProcess {
	p.lifecycleMu.Lock()
	process := p.process
	p.process = nil
	p.lifecycleMu.Unlock()
	return process
}

func renderAppServerInput(messages []provider.Message) string {
	var b strings.Builder
	for _, message := range messages {
		fmt.Fprintf(&b, "%s:\n", strings.ToUpper(string(message.Role)))
		for _, block := range message.Blocks {
			switch block.Type {
			case provider.BlockText:
				b.WriteString(block.Text)
				b.WriteByte('\n')
			case provider.BlockToolCall:
				if block.ToolCall != nil {
					fmt.Fprintf(&b, "[tool call %s %s] %s\n", block.ToolCall.ID, block.ToolCall.Name, block.ToolCall.Arguments)
				}
			case provider.BlockToolResult:
				if block.ToolResult != nil {
					fmt.Fprintf(&b, "[tool result %s %s error=%t] %s\n", block.ToolResult.ToolCallID, block.ToolResult.Name, block.ToolResult.IsError, block.ToolResult.Content)
				}
			}
		}
	}
	return strings.TrimSpace(b.String())
}

type dynamicToolSpec struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type rpcRequest struct {
	Method string `json:"method"`
	ID     int64  `json:"id"`
	Params any    `json:"params"`
}

type rpcNotification struct {
	Method string `json:"method"`
}

type rpcResult struct {
	ID     json.RawMessage `json:"id"`
	Result any             `json:"result"`
}

type rpcErrorResponse struct {
	ID    json.RawMessage `json:"id"`
	Error rpcError        `json:"error"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcEnvelope struct {
	JSONRPC json.RawMessage `json:"jsonrpc"`
	Method  string          `json:"method"`
	ID      json.RawMessage `json:"id"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

func (e rpcEnvelope) hasID() bool {
	id := strings.TrimSpace(string(e.ID))
	return id != "" && id != "null"
}

func validRPCID(id json.RawMessage) bool {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(id))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	switch value.(type) {
	case string, json.Number:
		return true
	default:
		return false
	}
}
