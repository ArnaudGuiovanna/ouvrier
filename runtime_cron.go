package ovr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	runtimeplan "ouvrier/internal/runtime"
)

type cronSchedule interface {
	Next(time.Time) time.Time
}

type intervalCronSchedule struct {
	interval time.Duration
}

func (s intervalCronSchedule) Next(after time.Time) time.Time {
	return after.Add(s.interval)
}

type fieldCronSchedule struct {
	minute cronField
	hour   cronField
	day    cronField
	month  cronField
	dow    cronField
}

func (s fieldCronSchedule) Next(after time.Time) time.Time {
	candidate := after.UTC().Truncate(time.Minute).Add(time.Minute)
	deadline := candidate.AddDate(5, 0, 0)
	for !candidate.After(deadline) {
		if s.matches(candidate) {
			return candidate
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}
}

func (s fieldCronSchedule) matches(t time.Time) bool {
	if !s.minute.matches(t.Minute()) || !s.hour.matches(t.Hour()) || !s.month.matches(int(t.Month())) {
		return false
	}
	dayMatches := s.day.matches(t.Day())
	dowMatches := s.dow.matches(int(t.Weekday()))
	if s.day.restricted() && s.dow.restricted() {
		return dayMatches || dowMatches
	}
	return dayMatches && dowMatches
}

type cronField struct {
	values map[int]struct{}
	all    bool
}

func (f cronField) matches(value int) bool {
	if f.all {
		return true
	}
	_, ok := f.values[value]
	return ok
}

func (f cronField) restricted() bool {
	return !f.all
}

func parseCronSchedule(expr string) (cronSchedule, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, errors.New("cron expression is required")
	}
	if rest, ok := strings.CutPrefix(expr, "@every "); ok {
		interval, err := time.ParseDuration(strings.TrimSpace(rest))
		if err != nil || interval <= 0 {
			return nil, fmt.Errorf("@every interval must be a positive duration")
		}
		return intervalCronSchedule{interval: interval}, nil
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron expression must have 5 fields")
	}
	minute, err := parseCronField(fields[0], 0, 59, nil)
	if err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	hour, err := parseCronField(fields[1], 0, 23, nil)
	if err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	day, err := parseCronField(fields[2], 1, 31, nil)
	if err != nil {
		return nil, fmt.Errorf("day-of-month: %w", err)
	}
	month, err := parseCronField(fields[3], 1, 12, nil)
	if err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	dow, err := parseCronField(fields[4], 0, 7, func(value int) int {
		if value == 7 {
			return 0
		}
		return value
	})
	if err != nil {
		return nil, fmt.Errorf("day-of-week: %w", err)
	}
	return fieldCronSchedule{minute: minute, hour: hour, day: day, month: month, dow: dow}, nil
}

func parseCronField(raw string, min, max int, normalize func(int) int) (cronField, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return cronField{}, errors.New("field is required")
	}
	if raw == "*" {
		return cronField{all: true}, nil
	}
	field := cronField{values: map[int]struct{}{}}
	for _, part := range strings.Split(raw, ",") {
		if err := addCronFieldPart(&field, strings.TrimSpace(part), min, max, normalize); err != nil {
			return cronField{}, err
		}
	}
	if len(field.values) == 0 {
		return cronField{}, errors.New("field has no values")
	}
	return field, nil
}

func addCronFieldPart(field *cronField, part string, min, max int, normalize func(int) int) error {
	if part == "" {
		return errors.New("empty list item")
	}
	rangePart, stepPart, hasStep := strings.Cut(part, "/")
	step := 1
	if hasStep {
		parsed, err := strconv.Atoi(stepPart)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("invalid step %q", stepPart)
		}
		step = parsed
	}
	start, end, err := cronFieldRange(rangePart, min, max)
	if err != nil {
		return err
	}
	for rawValue := start; rawValue <= end; rawValue += step {
		value := rawValue
		if normalize != nil {
			value = normalize(value)
		}
		field.values[value] = struct{}{}
	}
	return nil
}

func cronFieldRange(raw string, min, max int) (int, int, error) {
	if raw == "*" {
		return min, max, nil
	}
	left, right, hasRange := strings.Cut(raw, "-")
	if !hasRange {
		value, err := parseCronFieldValue(left, min, max)
		return value, value, err
	}
	start, err := parseCronFieldValue(left, min, max)
	if err != nil {
		return 0, 0, err
	}
	end, err := parseCronFieldValue(right, min, max)
	if err != nil {
		return 0, 0, err
	}
	if start > end {
		return 0, 0, fmt.Errorf("range %d-%d is inverted", start, end)
	}
	return start, end, nil
}

func parseCronFieldValue(raw string, min, max int) (int, error) {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", raw)
	}
	if value < min || value > max {
		return 0, fmt.Errorf("value %d outside %d-%d", value, min, max)
	}
	return value, nil
}

func serveCronPlans(rt httpRuntime, plans []runtimeplan.Plan) error {
	schedules := make([]cronSchedule, len(plans))
	for i, plan := range plans {
		if plan.Trigger.Kind != runtimeplan.TriggerCron {
			return fmt.Errorf("%w: only cron triggers are supported by cron runtime", ErrRunNotImplemented)
		}
		schedule, err := parseCronSchedule(plan.Trigger.Expr)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidNode, err)
		}
		schedules[i] = schedule
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	for i, plan := range plans {
		wg.Add(1)
		go func(plan runtimeplan.Plan, schedule cronSchedule) {
			defer wg.Done()
			runCronLoop(ctx, rt, plan, schedule)
		}(plan, schedules[i])
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

func runCronLoop(ctx context.Context, rt httpRuntime, plan runtimeplan.Plan, schedule cronSchedule) {
	workerPool := newCronWorkerPool(plan.Trigger.WorkerPool)
	for {
		next := schedule.Next(time.Now())
		if next.IsZero() {
			return
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			if !acquireCronWorker(ctx, workerPool) {
				return
			}
			go func(scheduledAt time.Time) {
				defer releaseCronWorker(workerPool)
				_, _ = runCronPlanOnce(ctx, rt, plan, scheduledAt)
			}(next)
		}
	}
}

func newCronWorkerPool(limit int) chan struct{} {
	if limit <= 0 {
		limit = 1
	}
	return make(chan struct{}, limit)
}

func acquireCronWorker(ctx context.Context, workerPool chan struct{}) bool {
	select {
	case workerPool <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func releaseCronWorker(workerPool chan struct{}) {
	<-workerPool
}

func runCronPlanOnce(ctx context.Context, rt httpRuntime, plan runtimeplan.Plan, scheduledAt time.Time) (planRunResult, error) {
	if plan.Trigger.Kind != runtimeplan.TriggerCron {
		return planRunResult{}, fmt.Errorf("%w: expected cron trigger", ErrRunNotImplemented)
	}
	input, err := cronInput(plan, scheduledAt)
	if err != nil {
		return planRunResult{}, err
	}
	result, err := rt.runPlanResult(ctx, plan, input)
	if err != nil {
		return result, err
	}
	switch plan.Terminal.Kind {
	case runtimeplan.TerminalPush:
		err = rt.applyPushTerminal(ctx, plan.Terminal, result, result.Output)
	case runtimeplan.TerminalSink:
		payloadKey := "output"
		if len(plan.Steps) == 0 {
			payloadKey = "input"
		}
		err = rt.applySinkTerminal(ctx, plan.Terminal, result, payloadKey)
	case runtimeplan.TerminalReply:
		err = fmt.Errorf("%w: Reply requires an HTTP trigger", ErrIncompatibleTerminal)
	}
	return result, err
}

func cronInput(plan runtimeplan.Plan, scheduledAt time.Time) (string, error) {
	payload := map[string]string{
		"trigger":      "cron",
		"expr":         plan.Trigger.Expr,
		"scheduled_at": scheduledAt.UTC().Format(time.RFC3339Nano),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
