package harness_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/harness"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

// namedProvider is a scripted provider that reports a configurable name so
// fallback tests can route different models to different providers.
type namedProvider struct {
	name      string
	response  provider.Response
	err       error
	callCount int
}

func (p *namedProvider) Name() string { return p.name }

func (p *namedProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	p.callCount++
	if p.err != nil {
		return provider.Response{}, p.err
	}
	return p.response, nil
}

func newResolver(t *testing.T, byProvider map[string]provider.Provider) func(string) (provider.Provider, error) {
	t.Helper()
	return func(model string) (provider.Provider, error) {
		ref, err := provider.ParseModelID(model)
		if err != nil {
			return nil, err
		}
		p, ok := byProvider[ref.Provider]
		if !ok {
			return nil, errors.New("no provider for " + ref.Provider)
		}
		return p, nil
	}
}

func TestRunFallsThroughOnTransientFailure(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	primary := &namedProvider{name: "anthropic", err: provider.RateLimitError(errors.New("429"))}
	secondary := &namedProvider{name: "openai", response: provider.Response{Text: "done", StopReason: provider.StopEndTurn}}
	resolver := newResolver(t, map[string]provider.Provider{
		"anthropic": primary,
		"openai":    secondary,
	})

	h, err := harness.New(primary,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithFallback("openai/gpt-4o"),
		harness.WithProviderResolver(resolver),
		harness.WithProviderRetries(0),
		harness.WithEventStream(stream),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Status != harness.StatusCompleted {
		t.Fatalf("Status = %q, want completed", out.Status)
	}
	if out.Text != "done" {
		t.Fatalf("Text = %q, want done from fallback model", out.Text)
	}
	if primary.callCount != 1 || secondary.callCount != 1 {
		t.Fatalf("call counts primary=%d secondary=%d, want 1/1", primary.callCount, secondary.callCount)
	}

	event, ok := findEvent(stream.List(), events.EventModelFallback)
	if !ok {
		t.Fatalf("events = %+v, want model fallback event", stream.List())
	}
	if event.Payload["from_model"] != "anthropic/claude-sonnet-4-6" ||
		event.Payload["to_model"] != "openai/gpt-4o" ||
		event.Payload["error_kind"] != string(provider.ErrorRateLimit) {
		t.Fatalf("event payload = %+v, want fallback metadata", event.Payload)
	}
	if _, ok := event.Payload["messages"]; ok {
		t.Fatalf("event payload = %+v, must not include request messages", event.Payload)
	}
}

func TestRunDoesNotFallThroughOnPermanentFailure(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	permanent := provider.PermanentError(errors.New("model removed"))
	primary := &namedProvider{name: "anthropic", err: permanent}
	secondary := &namedProvider{name: "openai", response: provider.Response{Text: "done", StopReason: provider.StopEndTurn}}
	resolver := newResolver(t, map[string]provider.Provider{
		"anthropic": primary,
		"openai":    secondary,
	})

	h, err := harness.New(primary,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithFallback("openai/gpt-4o"),
		harness.WithProviderResolver(resolver),
		harness.WithProviderRetries(0),
		harness.WithEventStream(stream),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if !errors.Is(err, permanent) {
		t.Fatalf("Run error = %v, want permanent error", err)
	}
	if out.Status != harness.StatusFailed {
		t.Fatalf("Status = %q, want failed", out.Status)
	}
	if primary.callCount != 1 {
		t.Fatalf("primary calls = %d, want 1", primary.callCount)
	}
	if secondary.callCount != 0 {
		t.Fatalf("secondary calls = %d, want 0 (no fallthrough on permanent)", secondary.callCount)
	}
	if _, ok := findEvent(stream.List(), events.EventModelFallback); ok {
		t.Fatalf("events = %+v, want no fallback event on permanent failure", stream.List())
	}
}

func TestRunFallsThroughChainUntilSuccess(t *testing.T) {
	primary := &namedProvider{name: "anthropic", err: provider.TransientError(errors.New("503"))}
	secondary := &namedProvider{name: "openai", err: provider.RateLimitError(errors.New("429"))}
	tertiary := &namedProvider{name: "groq", response: provider.Response{Text: "ok", StopReason: provider.StopEndTurn}}
	resolver := newResolver(t, map[string]provider.Provider{
		"anthropic": primary,
		"openai":    secondary,
		"groq":      tertiary,
	})

	h, err := harness.New(primary,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithFallback("openai/gpt-4o", "groq/llama-3.1"),
		harness.WithProviderResolver(resolver),
		harness.WithProviderRetries(0),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Text != "ok" {
		t.Fatalf("Text = %q, want ok from third model", out.Text)
	}
	if primary.callCount != 1 || secondary.callCount != 1 || tertiary.callCount != 1 {
		t.Fatalf("call counts = %d/%d/%d, want 1/1/1", primary.callCount, secondary.callCount, tertiary.callCount)
	}
}

// concurrencyProvider tracks the maximum number of overlapping Complete calls.
type concurrencyProvider struct {
	name     string
	inFlight int32
	maxSeen  int32
	hold     time.Duration
}

func (p *concurrencyProvider) Name() string { return p.name }

func (p *concurrencyProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	cur := atomic.AddInt32(&p.inFlight, 1)
	for {
		prev := atomic.LoadInt32(&p.maxSeen)
		if cur <= prev || atomic.CompareAndSwapInt32(&p.maxSeen, prev, cur) {
			break
		}
	}
	time.Sleep(p.hold)
	atomic.AddInt32(&p.inFlight, -1)
	return provider.Response{Text: "done", StopReason: provider.StopEndTurn}, nil
}

func TestProviderGateBoundsHarnessConcurrency(t *testing.T) {
	p := &concurrencyProvider{name: "anthropic", hold: 10 * time.Millisecond}
	gate := harness.NewProviderGate(map[string]int{"anthropic": 2})

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := harness.New(p,
				harness.WithModel("anthropic/claude-sonnet-4-6"),
				harness.WithProviderGate(gate),
			)
			if err != nil {
				t.Errorf("New returned error: %v", err)
				return
			}
			if _, err := h.Run(context.Background(), "payload"); err != nil {
				t.Errorf("Run returned error: %v", err)
			}
		}()
	}
	wg.Wait()

	if max := atomic.LoadInt32(&p.maxSeen); max > 2 {
		t.Fatalf("max concurrent provider calls = %d, want <= 2 (budget bound)", max)
	}
}

func TestRunReturnsLastErrorWhenAllFallbacksFail(t *testing.T) {
	primary := &namedProvider{name: "anthropic", err: provider.TransientError(errors.New("503"))}
	lastErr := provider.RateLimitError(errors.New("429 exhausted"))
	secondary := &namedProvider{name: "openai", err: lastErr}
	resolver := newResolver(t, map[string]provider.Provider{
		"anthropic": primary,
		"openai":    secondary,
	})

	h, err := harness.New(primary,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithFallback("openai/gpt-4o"),
		harness.WithProviderResolver(resolver),
		harness.WithProviderRetries(0),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if !errors.Is(err, lastErr) {
		t.Fatalf("Run error = %v, want last fallback error", err)
	}
	if out.Status != harness.StatusFailed {
		t.Fatalf("Status = %q, want failed", out.Status)
	}
}
