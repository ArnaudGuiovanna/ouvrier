package state

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// IdempotencyOutcome is the durable outcome of one reserved idempotency key.
// Pending is deliberately not treated as success: it means the guarded effect
// may still be running or its outcome may be indeterminate after a crash.
type IdempotencyOutcome string

const (
	IdempotencyPending   IdempotencyOutcome = "pending"
	IdempotencySucceeded IdempotencyOutcome = "succeeded"
	IdempotencyFailed    IdempotencyOutcome = "failed"
)

// IdempotencyRecord is the outcome-aware state associated with one key.
type IdempotencyRecord struct {
	Key       string
	ExecID    string
	Outcome   IdempotencyOutcome
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IdempotencyOutcomeStore is an optional extension implemented by Ouvrier's
// built-in stores. The legacy Store reservation method remains source
// compatible for custom stores, while the runtime and ToolExecutor use this
// richer seam when available so failed work can be retried safely.
type IdempotencyOutcomeStore interface {
	Idempotency(context.Context, string) (IdempotencyRecord, bool, error)
	ResolveIdempotency(context.Context, string, string, IdempotencyOutcome) error
	ResolveIdempotencyByExecution(context.Context, string, string, IdempotencyOutcome) error
}

func validateIdempotencyResolution(key, execID string, outcome IdempotencyOutcome) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("idempotency key is required")
	}
	if strings.TrimSpace(execID) == "" {
		return fmt.Errorf("execution ID is required")
	}
	return validateResolvedIdempotencyOutcome(outcome)
}

func validateResolvedIdempotencyOutcome(outcome IdempotencyOutcome) error {
	switch outcome {
	case IdempotencySucceeded, IdempotencyFailed:
		return nil
	default:
		return fmt.Errorf("invalid resolved idempotency outcome %q", outcome)
	}
}
