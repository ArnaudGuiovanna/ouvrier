package ovr_test

import (
	"errors"
	"testing"

	"ouvrier"
)

type testReply struct {
	Status string `json:"status"`
}

func TestValidateAcceptsHTTPPipelineWithReply(t *testing.T) {
	err := ovr.Validate(
		ovr.From("POST /tickets/{id}"),
		ovr.Pipe("classify ticket", ovr.Model("anthropic/claude-sonnet-4-6")),
		ovr.Reply(ovr.JSON[testReply]()),
	)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}

func TestValidateAcceptsAsyncPipelineWithSink(t *testing.T) {
	err := ovr.Validate(
		ovr.From(ovr.Cron("0 6 * * *")),
		ovr.Pipe("summarize overnight events", ovr.Model("anthropic/claude-haiku-4-5")),
		ovr.Sink(ovr.Log()),
	)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}

func TestValidateAcceptsWebhookPushTerminal(t *testing.T) {
	err := ovr.Validate(
		ovr.From(ovr.Webhook("github")),
		ovr.Pipe("normalize webhook payload", ovr.Model("anthropic/claude-sonnet-4-6")),
		ovr.Push(ovr.Webhook("https://example.com/hooks/normalized")),
	)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}

func TestValidateRequiresFirstNodeFrom(t *testing.T) {
	err := ovr.Validate(
		ovr.Pipe("classify ticket", ovr.Model("anthropic/claude-sonnet-4-6")),
		ovr.Reply(ovr.JSON[testReply]()),
	)
	if !errors.Is(err, ovr.ErrFirstNodeNotFrom) {
		t.Fatalf("Validate error = %v, want ErrFirstNodeNotFrom", err)
	}
}

func TestValidateRequiresTerminalNode(t *testing.T) {
	err := ovr.Validate(
		ovr.From("GET /tickets"),
		ovr.Pipe("classify ticket", ovr.Model("anthropic/claude-sonnet-4-6")),
	)
	if !errors.Is(err, ovr.ErrTerminalMissing) {
		t.Fatalf("Validate error = %v, want ErrTerminalMissing", err)
	}
}

func TestValidateRequiresPipeModel(t *testing.T) {
	err := ovr.Validate(
		ovr.From(ovr.Stream("kafka://tickets")),
		ovr.Pipe("classify ticket"),
		ovr.Sink(ovr.Log()),
	)
	if !errors.Is(err, ovr.ErrPipeMissingModel) {
		t.Fatalf("Validate error = %v, want ErrPipeMissingModel", err)
	}
}
