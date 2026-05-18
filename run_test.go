package ovr_test

import (
	"errors"
	"testing"

	"ouvrier"
)

func TestRunValidatesPipelineBeforeStartingRuntime(t *testing.T) {
	err := ovr.Run(":8080", ovr.Pipe("missing trigger", ovr.Model("anthropic/claude-sonnet-4-6")))
	if !errors.Is(err, ovr.ErrFirstNodeNotFrom) {
		t.Fatalf("Run error = %v, want ErrFirstNodeNotFrom", err)
	}
}

func TestRunReturnsExplicitNotImplementedForValidPipeline(t *testing.T) {
	err := ovr.Run(
		":8080",
		ovr.From("POST /tickets"),
		ovr.Pipe("classify ticket", ovr.Model("anthropic/claude-sonnet-4-6")),
		ovr.Reply(ovr.JSON[testReply]()),
	)
	if !errors.Is(err, ovr.ErrRunNotImplemented) {
		t.Fatalf("Run error = %v, want ErrRunNotImplemented", err)
	}
}
