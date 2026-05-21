package ovr

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ouvrier/internal/events"
	"ouvrier/internal/provider"
)

func TestParseCronScheduleNextDailyTime(t *testing.T) {
	schedule, err := parseCronSchedule("0 6 * * *")
	if err != nil {
		t.Fatalf("parseCronSchedule returned error: %v", err)
	}

	next := schedule.Next(time.Date(2026, 5, 21, 5, 30, 0, 0, time.UTC))
	want := time.Date(2026, 5, 21, 6, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("Next = %s, want %s", next, want)
	}
}

func TestParseCronScheduleSupportsRangesStepsAndSundaySeven(t *testing.T) {
	schedule, err := parseCronSchedule("*/15 9-17 * * 1-5,7")
	if err != nil {
		t.Fatalf("parseCronSchedule returned error: %v", err)
	}

	next := schedule.Next(time.Date(2026, 5, 24, 8, 59, 0, 0, time.UTC)) // Sunday.
	want := time.Date(2026, 5, 24, 9, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("Next = %s, want %s", next, want)
	}
}

func TestValidateRejectsInvalidCronExpression(t *testing.T) {
	err := Validate(
		From(Cron("not-a-cron")),
		Sink(Log()),
	)
	if !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("Validate error = %v, want ErrInvalidNode", err)
	}
}

func TestRunCronPlanOnceRunsPipelineAndLogsOutput(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"cron"}`, StopReason: provider.StopEndTurn},
	}
	plans, err := compilePlans([]Node{
		From(Cron("0 6 * * *")),
		Pipe("summarize overnight events", Model("anthropic/claude-haiku-4-5")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	result, err := runCronPlanOnce(context.Background(), httpRuntime{provider: scripted, eventStream: stream}, plans[0], time.Date(2026, 5, 21, 6, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("runCronPlanOnce returned error: %v", err)
	}

	if result.Output != `{"status":"cron"}` {
		t.Fatalf("output = %q, want cron provider output", result.Output)
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	input := decodeProviderInput(t, scripted.requests[0].Messages[0].Text())
	assertRawJSONField(t, input, "trigger", `"cron"`)
	assertRawJSONField(t, input, "expr", `"0 6 * * *"`)
	assertRawJSONField(t, input, "scheduled_at", `"2026-05-21T06:00:00Z"`)
	assertSinkLoggedEvent(t, stream, "output", `{"status":"cron"}`)
}

func TestRunCronPlanOncePushesPipelineOutputToWebhook(t *testing.T) {
	posts := make(chan string, 1)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer req.Body.Close()
		var body struct {
			Status string `json:"status"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Errorf("webhook body is not JSON: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		posts <- body.Status
		w.WriteHeader(http.StatusAccepted)
	}))
	defer webhook.Close()

	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"cron"}`, StopReason: provider.StopEndTurn},
	}
	plans, err := compilePlans([]Node{
		From(Cron("@every 1h")),
		Pipe("summarize overnight events", Model("anthropic/claude-haiku-4-5")),
		Push(Webhook(webhook.URL)),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	_, err = runCronPlanOnce(context.Background(), httpRuntime{
		provider:     scripted,
		toolExecutor: outputAllowedExecutor("webhook"),
	}, plans[0], time.Date(2026, 5, 21, 6, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("runCronPlanOnce returned error: %v", err)
	}

	select {
	case status := <-posts:
		if status != "cron" {
			t.Fatalf("webhook status = %q, want cron", status)
		}
	case <-time.After(time.Second):
		t.Fatal("webhook did not receive cron output")
	}
}

func TestRunnerRejectsMixedHTTPAndCronRuntimeForNow(t *testing.T) {
	t.Setenv("OUVRIER_STATE_BACKEND", "memory")

	err := NewRunner().Run(
		"127.0.0.1:bad-port",
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
		From(Cron("@every 1h")),
		Sink(Log()),
	)
	if !errors.Is(err, ErrRunNotImplemented) {
		t.Fatalf("Run error = %v, want ErrRunNotImplemented", err)
	}
}

func TestPlansTriggerKindDetectsMixedTriggers(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
		From(Cron("@every 1h")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	if got := plansTriggerKind(plans); got != "" {
		t.Fatalf("plansTriggerKind = %q, want empty mixed trigger kind", got)
	}
}

func TestParseCronEverySchedule(t *testing.T) {
	schedule, err := parseCronSchedule("@every 10s")
	if err != nil {
		t.Fatalf("parseCronSchedule returned error: %v", err)
	}

	next := schedule.Next(time.Date(2026, 5, 21, 6, 0, 0, 0, time.UTC))
	if !strings.HasSuffix(next.Format(time.RFC3339), "06:00:10Z") {
		t.Fatalf("Next = %s, want +10s", next)
	}
}
