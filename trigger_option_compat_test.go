package ovr_test

import (
	"errors"
	"testing"

	ovr "github.com/ArnaudGuiovanna/ouvrier"
)

func TestValidateRejectsStreamOptionOnHTTPTrigger(t *testing.T) {
	err := ovr.Validate(
		ovr.From("POST /tickets", ovr.StreamDLQ("kafka://dead", 3)),
		ovr.Pipe("triage", ovr.Model("anthropic/claude-sonnet-4-6")),
		ovr.Sink(ovr.Log()),
	)
	if !errors.Is(err, ovr.ErrIncompatibleTriggerOption) {
		t.Fatalf("Validate error = %v, want ErrIncompatibleTriggerOption", err)
	}
}

func TestValidateRejectsSignatureOnStreamTrigger(t *testing.T) {
	err := ovr.Validate(
		ovr.From(ovr.Stream("nats://events"), ovr.VerifySignature("SECRET_ENV", "X-Signature")),
		ovr.Pipe("handle", ovr.Model("anthropic/claude-sonnet-4-6")),
		ovr.Sink(ovr.Log()),
	)
	if !errors.Is(err, ovr.ErrIncompatibleTriggerOption) {
		t.Fatalf("Validate error = %v, want ErrIncompatibleTriggerOption", err)
	}
}

func TestValidateAllowsSignatureOnWebhookTrigger(t *testing.T) {
	err := ovr.Validate(
		ovr.From(ovr.Webhook("github"), ovr.VerifySignature("GH_SECRET", "X-Hub-Signature-256")),
		ovr.Pipe("handle", ovr.Model("anthropic/claude-sonnet-4-6")),
		ovr.Sink(ovr.Log()),
	)
	if err != nil {
		t.Fatalf("Validate webhook+signature = %v, want nil", err)
	}
}

func TestValidateAllowsStreamDLQOnStreamTrigger(t *testing.T) {
	err := ovr.Validate(
		ovr.From(ovr.Stream("nats://events"), ovr.StreamDLQ("nats://dead", 5)),
		ovr.Pipe("handle", ovr.Model("anthropic/claude-sonnet-4-6")),
		ovr.Sink(ovr.Log()),
	)
	if err != nil {
		t.Fatalf("Validate stream+DLQ = %v, want nil", err)
	}
}

func TestValidateRejectsModelWithoutProviderPrefix(t *testing.T) {
	err := ovr.Validate(
		ovr.From("POST /tickets"),
		ovr.Pipe("triage", ovr.Model("claude-sonnet-4-6")),
		ovr.Sink(ovr.Log()),
	)
	if !errors.Is(err, ovr.ErrInvalidModelForm) {
		t.Fatalf("Validate error = %v, want ErrInvalidModelForm", err)
	}
}

func TestValidateAcceptsProviderQualifiedModel(t *testing.T) {
	err := ovr.Validate(
		ovr.From("POST /tickets"),
		ovr.Pipe("triage", ovr.Model("anthropic/claude-sonnet-4-6")),
		ovr.Sink(ovr.Log()),
	)
	if err != nil {
		t.Fatalf("Validate provider-qualified model = %v, want nil", err)
	}
}
