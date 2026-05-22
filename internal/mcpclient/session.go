package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ouvrier/internal/policy"
	"ouvrier/internal/provider"
	"ouvrier/internal/tools"
)

var ErrInvalidServer = errors.New("invalid MCP server")

type Server struct {
	Name      string
	Transport mcp.Transport
}

type Connector struct {
	implementation *mcp.Implementation
}

func NewConnector() *Connector {
	return &Connector{implementation: &mcp.Implementation{Name: "ouvrier", Version: "v0.1.0"}}
}

func (c *Connector) Connect(ctx context.Context, server Server) (*Session, error) {
	name := strings.TrimSpace(server.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidServer)
	}
	if server.Transport == nil {
		return nil, fmt.Errorf("%w: transport is required", ErrInvalidServer)
	}
	implementation := c.implementation
	if implementation == nil {
		implementation = &mcp.Implementation{Name: "ouvrier", Version: "v0.1.0"}
	}
	client := mcp.NewClient(implementation, nil)
	session, err := client.Connect(ctx, server.Transport, nil)
	if err != nil {
		return nil, err
	}
	return &Session{serverName: name, sdk: session}, nil
}

type Session struct {
	serverName string
	sdk        *mcp.ClientSession
}

func (s *Session) RegisterTools(ctx context.Context, executor *tools.Executor) ([]provider.ToolSpec, error) {
	if executor == nil {
		return nil, errors.New("tool executor is required")
	}
	remoteTools, err := s.listTools(ctx)
	if err != nil {
		return nil, err
	}

	specs := make([]provider.ToolSpec, 0, len(remoteTools))
	for _, remoteTool := range remoteTools {
		if remoteTool == nil {
			continue
		}
		spec, err := providerToolSpec(s.serverName, remoteTool)
		if err != nil {
			return nil, err
		}
		handler := mcpToolHandler{
			session:    s,
			localName:  spec.Name,
			remoteName: remoteTool.Name,
		}
		if err := executor.RegisterHandler(spec.Name, handler, tools.WithMetadata(tools.Metadata{
			Kind:        tools.ToolKindMCP,
			Target:      s.serverName,
			Effect:      policy.EffectSideEffecting,
			SideEffects: []string{"mcp:" + s.serverName},
		})); err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

func (s *Session) Close() error {
	if s == nil || s.sdk == nil {
		return nil
	}
	return s.sdk.Close()
}

func (s *Session) listTools(ctx context.Context) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	var cursor string
	for {
		result, err := s.sdk.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		tools = append(tools, result.Tools...)
		if result.NextCursor == "" {
			return tools, nil
		}
		cursor = result.NextCursor
	}
}

func providerToolSpec(serverName string, tool *mcp.Tool) (provider.ToolSpec, error) {
	schema := []byte(`{"type":"object"}`)
	if tool.InputSchema != nil {
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return provider.ToolSpec{}, fmt.Errorf("marshal MCP tool schema %q: %w", tool.Name, err)
		}
		schema = encoded
	}
	return provider.ToolSpec{
		Name:        LocalToolName(serverName, tool.Name),
		Description: strings.TrimSpace(tool.Description),
		InputSchema: schema,
	}, nil
}

type mcpToolHandler struct {
	session    *Session
	localName  string
	remoteName string
}

func (h mcpToolHandler) Execute(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
	args, err := decodeArguments(call.Arguments)
	if err != nil {
		return provider.ToolResult{}, err
	}
	result, err := h.session.sdk.CallTool(ctx, &mcp.CallToolParams{
		Name:      h.remoteName,
		Arguments: args,
	})
	if err != nil {
		return provider.ToolResult{}, err
	}
	content, err := callToolContent(result)
	if err != nil {
		return provider.ToolResult{}, err
	}
	return provider.ToolResult{
		ToolCallID: call.ID,
		Name:       h.localName,
		Content:    content,
		IsError:    result != nil && result.IsError,
	}, nil
}

func decodeArguments(raw json.RawMessage) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("decode MCP tool arguments: %w", err)
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, nil
}

func callToolContent(result *mcp.CallToolResult) (json.RawMessage, error) {
	if result == nil {
		return []byte(`null`), nil
	}
	if result.StructuredContent != nil {
		return json.Marshal(result.StructuredContent)
	}
	if len(result.Content) == 0 {
		return []byte(`null`), nil
	}

	texts := make([]string, 0, len(result.Content))
	raw := make([]json.RawMessage, 0, len(result.Content))
	allText := true
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			texts = append(texts, text.Text)
		} else {
			allText = false
		}
		encoded, err := content.MarshalJSON()
		if err != nil {
			return nil, err
		}
		raw = append(raw, encoded)
	}
	if allText {
		return json.Marshal(strings.Join(texts, "\n"))
	}
	return json.Marshal(raw)
}
