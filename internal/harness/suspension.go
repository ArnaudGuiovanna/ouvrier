package harness

import (
	"context"
	"errors"

	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

// SuspendedRunError carries the in-memory continuation for a run parked behind
// a human approval. It deliberately does not persist tool arguments or the model
// transcript; the durable store keeps only the redaction-safe approval record.
type SuspendedRunError struct {
	Suspended *tools.SuspendedError
	resume    func(context.Context) (Outcome, error)
}

func NewSuspendedRunError(suspended *tools.SuspendedError, resume func(context.Context) (Outcome, error)) *SuspendedRunError {
	return &SuspendedRunError{Suspended: suspended, resume: resume}
}

func (e *SuspendedRunError) Error() string {
	if e == nil || e.Suspended == nil {
		return "execution suspended for approval"
	}
	return e.Suspended.Error()
}

func (e *SuspendedRunError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Suspended
}

func (e *SuspendedRunError) Resume(ctx context.Context) (Outcome, error) {
	if e == nil || e.resume == nil {
		return Outcome{Status: StatusFailed}, errors.New("suspended run continuation is unavailable")
	}
	return e.resume(ctx)
}

func SuspendedRun(err error) (*SuspendedRunError, bool) {
	var suspended *SuspendedRunError
	if errors.As(err, &suspended) && suspended != nil {
		return suspended, true
	}
	return nil, false
}
