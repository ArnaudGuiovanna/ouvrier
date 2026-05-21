package ovr_test

import (
	"errors"
	"strings"
	"testing"

	"ouvrier"
)

func TestRunValidatesPipelineBeforeStartingRuntime(t *testing.T) {
	err := ovr.Run(":8080", ovr.Pipe("missing trigger", ovr.Model("anthropic/claude-sonnet-4-6")))
	if !errors.Is(err, ovr.ErrFirstNodeNotFrom) {
		t.Fatalf("Run error = %v, want ErrFirstNodeNotFrom", err)
	}
}

func TestRunAttemptsToListenForValidHTTPPipeline(t *testing.T) {
	t.Setenv("OUVRIER_STATE_BACKEND", "memory")

	err := ovr.Run(
		"127.0.0.1:bad-port",
		ovr.From("GET /health"),
		ovr.Reply(ovr.JSON[testReply]()),
	)
	if err == nil {
		t.Fatal("Run returned nil, want listen error")
	}
	if errors.Is(err, ovr.ErrRunNotImplemented) {
		t.Fatalf("Run error = %v, no longer want ErrRunNotImplemented for HTTP pipelines", err)
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Fatalf("Run error = %v, want listen context", err)
	}
}

func TestRunAttemptsToListenForValidWebhookPipeline(t *testing.T) {
	t.Setenv("OUVRIER_STATE_BACKEND", "memory")

	err := ovr.Run(
		"127.0.0.1:bad-port",
		ovr.From(ovr.Webhook("github")),
		ovr.Sink(ovr.Log()),
	)
	if err == nil {
		t.Fatal("Run returned nil, want listen error")
	}
	if errors.Is(err, ovr.ErrRunNotImplemented) {
		t.Fatalf("Run error = %v, no longer want ErrRunNotImplemented for webhook pipelines", err)
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Fatalf("Run error = %v, want listen context", err)
	}
}
