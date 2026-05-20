package tools

import (
	"context"
	"time"

	"ouvrier/internal/policy"
)

type retryContext struct {
	maxRetries int
	backoff    time.Duration
}

type retryContextKey struct{}

type ToolRetryAudit struct {
	ToolName   string
	ToolCallID string
	Attempt    int
	MaxRetries int
	Effect     policy.Effect
	Err        error
}

type ToolRetryObserver func(context.Context, ToolRetryAudit) error

type retryObserverContextKey struct{}

func ContextWithToolRetry(ctx context.Context, maxRetries int, backoff time.Duration) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if maxRetries <= 0 {
		return ctx
	}
	if backoff < 0 {
		backoff = 0
	}
	return context.WithValue(ctx, retryContextKey{}, retryContext{
		maxRetries: maxRetries,
		backoff:    backoff,
	})
}

func ContextWithToolRetryObserver(ctx context.Context, observer ToolRetryObserver) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, retryObserverContextKey{}, observer)
}

func retryFromContext(ctx context.Context) retryContext {
	if ctx == nil {
		return retryContext{}
	}
	retry, _ := ctx.Value(retryContextKey{}).(retryContext)
	return retry
}

func observeToolRetry(ctx context.Context, audit ToolRetryAudit) error {
	if ctx == nil {
		return nil
	}
	observer, ok := ctx.Value(retryObserverContextKey{}).(ToolRetryObserver)
	if !ok || observer == nil {
		return nil
	}
	return observer(ctx, audit)
}

func waitToolRetryBackoff(ctx context.Context, backoff time.Duration, attempt int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if backoff <= 0 {
		return nil
	}
	delay := backoff * time.Duration(1<<attempt)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
