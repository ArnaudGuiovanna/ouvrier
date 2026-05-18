package mcpclient

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ouvrier/internal/provider"
	"ouvrier/internal/tools"
)

type lookupInput struct {
	Query string `json:"query" jsonschema:"query to look up"`
}

type lookupOutput struct {
	Answer string `json:"answer"`
}

func TestSessionRegistersSDKToolsWithExecutor(t *testing.T) {
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "v1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "lookup", Description: "Lookup data."},
		func(ctx context.Context, req *mcp.CallToolRequest, input lookupInput) (*mcp.CallToolResult, lookupOutput, error) {
			if input.Query != "ouvrier" {
				t.Fatalf("query = %q, want ouvrier", input.Query)
			}
			return nil, lookupOutput{Answer: "workers"}, nil
		})

	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server Connect returned error: %v", err)
	}
	defer serverSession.Close()

	session, err := NewConnector().Connect(ctx, Server{
		Name:      "moodle-mcp",
		Transport: clientTransport,
	})
	if err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	defer session.Close()

	executor := tools.NewExecutor()
	specs, err := session.RegisterTools(ctx, executor)
	if err != nil {
		t.Fatalf("RegisterTools returned error: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("tool specs = %d, want 1", len(specs))
	}
	if specs[0].Name != "moodle-mcp__lookup" {
		t.Fatalf("tool name = %q, want moodle-mcp__lookup", specs[0].Name)
	}
	if specs[0].Description != "Lookup data." {
		t.Fatalf("description = %q, want Lookup data.", specs[0].Description)
	}
	if len(specs[0].InputSchema) == 0 {
		t.Fatal("InputSchema is empty")
	}

	result, err := executor.Execute(ctx, provider.ToolCall{
		ID:        "call_1",
		Name:      "moodle-mcp__lookup",
		Arguments: []byte(`{"query":"ouvrier"}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("IsError = true, content=%s", result.Content)
	}
	var output lookupOutput
	if err := json.Unmarshal(result.Content, &output); err != nil {
		t.Fatalf("tool content is not lookupOutput: %v", err)
	}
	if output.Answer != "workers" {
		t.Fatalf("answer = %q, want workers", output.Answer)
	}
}
