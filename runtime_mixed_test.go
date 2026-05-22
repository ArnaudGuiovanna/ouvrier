package ovr

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRuntimePlanGroupsSplitsSupportedTriggerKinds(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
		From(Webhook("github")),
		Sink(Log()),
		From(Cron("@every 1h")),
		Sink(Log()),
		From(Stream("nats://127.0.0.1:4222/tickets.created")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	groups, err := runtimePlanGroups(plans)
	if err != nil {
		t.Fatalf("runtimePlanGroups returned error: %v", err)
	}
	if len(groups.httpRoutes) != 2 {
		t.Fatalf("http routes = %d, want HTTP plus Webhook routes", len(groups.httpRoutes))
	}
	if len(groups.cronPlans) != 1 || len(groups.cronSchedules) != 1 {
		t.Fatalf("cron plans/schedules = %d/%d, want 1/1", len(groups.cronPlans), len(groups.cronSchedules))
	}
	if len(groups.streamPlans) != 1 {
		t.Fatalf("stream plans = %d, want 1", len(groups.streamPlans))
	}
}

func TestRunSupervisedRuntimesCancelsSiblingsOnFailure(t *testing.T) {
	boom := errors.New("boom")
	canceled := make(chan struct{})

	err := runSupervisedRuntimes(context.Background(),
		func(ctx context.Context) error {
			<-ctx.Done()
			close(canceled)
			return nil
		},
		func(ctx context.Context) error {
			return boom
		},
	)
	if !errors.Is(err, boom) {
		t.Fatalf("runSupervisedRuntimes error = %v, want boom", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("sibling runtime was not canceled")
	}
}

func TestRunSupervisedRuntimesReturnsNilOnParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		<-started
		cancel()
	}()

	err := runSupervisedRuntimes(ctx, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(done)
		return nil
	})
	if err != nil {
		t.Fatalf("runSupervisedRuntimes returned error: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner did not observe cancellation")
	}
}
