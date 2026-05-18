package ovr

import (
	"errors"
	"testing"
)

func TestWorkerPoolOptionStoresTriggerConcurrency(t *testing.T) {
	node := From("POST /jobs", WorkerPool(3))

	from, ok := node.(fromNode)
	if !ok {
		t.Fatalf("From returned %T, want fromNode", node)
	}
	if from.config.workerPool != 3 {
		t.Fatalf("worker pool = %d, want 3", from.config.workerPool)
	}
}

func TestWorkerPoolOptionRejectsInvalidLimit(t *testing.T) {
	err := Validate(
		From("POST /jobs", WorkerPool(0)),
		Reply(Accepted()),
	)
	if !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("Validate error = %v, want ErrInvalidNode", err)
	}
}
