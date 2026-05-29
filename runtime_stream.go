package ovr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
)

type streamMessage struct {
	ID       string
	Body     string
	Metadata map[string]string
	ack      func(context.Context) error
	nack     func(context.Context, error) error
}

type streamReceiver interface {
	Receive(context.Context, string) (streamMessage, error)
}

type unsupportedStreamReceiver struct{}

func (unsupportedStreamReceiver) Receive(ctx context.Context, rawURI string) (streamMessage, error) {
	if err := ctx.Err(); err != nil {
		return streamMessage{}, err
	}
	scheme := "unknown"
	if parsed, err := url.Parse(rawURI); err == nil && parsed.Scheme != "" {
		scheme = parsed.Scheme
	}
	return streamMessage{}, fmt.Errorf("%w: stream receiver for %s is not configured", ErrRunNotImplemented, scheme)
}

func serveStreamPlans(addr string, rt httpRuntime, plans []runtimeplan.Plan) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return serveStreamPlansWithContext(ctx, addr, rt, plans)
}

func serveStreamPlansWithContext(ctx context.Context, addr string, rt httpRuntime, plans []runtimeplan.Plan) error {
	for _, plan := range plans {
		if plan.Trigger.Kind != runtimeplan.TriggerStream {
			return fmt.Errorf("%w: only stream triggers are supported by stream runtime", ErrRunNotImplemented)
		}
		if err := validateStreamURI(plan.Trigger.URI); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidNode, err)
		}
	}
	handler, err := newAdminHandlerWithRuntime(plans, rt)
	if err != nil {
		return err
	}
	return runSupervisedRuntimes(ctx,
		func(ctx context.Context) error {
			return serveHTTPWithContext(ctx, addr, handler)
		},
		func(ctx context.Context) error {
			return runStreamPlans(ctx, rt, plans)
		},
	)
}

func runStreamPlans(ctx context.Context, rt httpRuntime, plans []runtimeplan.Plan) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if closer, ok := rt.streamReceiver.(interface{ Close() error }); ok {
		defer closer.Close()
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, len(plans))
	var wg sync.WaitGroup
	for _, plan := range plans {
		plan := plan
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runStreamLoop(runCtx, rt, plan); err != nil {
				select {
				case errCh <- err:
					cancel()
				default:
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		select {
		case err := <-errCh:
			return err
		default:
			return nil
		}
	case err := <-errCh:
		cancel()
		<-done
		return err
	case <-ctx.Done():
		cancel()
		<-done
		return nil
	}
}

func runStreamLoop(ctx context.Context, rt httpRuntime, plan runtimeplan.Plan) error {
	if plan.Trigger.Kind != runtimeplan.TriggerStream {
		return fmt.Errorf("%w: expected stream trigger", ErrRunNotImplemented)
	}
	receiver := rt.streamReceiver
	if receiver == nil {
		receiver = unsupportedStreamReceiver{}
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	workerPool := newCronWorkerPool(streamInFlightLimit(plan.Trigger))
	attempts := newStreamAttemptTracker()
	processErr := make(chan error, 1)
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		select {
		case err := <-processErr:
			cancel()
			return err
		default:
		}

		message, err := receiver.Receive(runCtx, plan.Trigger.URI)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				select {
				case processErr := <-processErr:
					return processErr
				default:
					return nil
				}
			}
			return err
		}
		if !acquireCronWorker(runCtx, workerPool) {
			return nil
		}
		wg.Add(1)
		go func(message streamMessage) {
			defer wg.Done()
			defer releaseCronWorker(workerPool)
			if err := rt.processStreamMessage(runCtx, plan, message, attempts); err != nil {
				select {
				case processErr <- err:
					cancel()
				default:
				}
			}
		}(message)
	}
}

// streamInFlightLimit returns the bound on concurrently processed stream
// messages. StreamMaxInFlight is the dedicated backpressure knob; WorkerPool is
// honoured for backwards compatibility when MaxInFlight is unset.
func streamInFlightLimit(trigger runtimeplan.Trigger) int {
	if trigger.MaxInFlight > 0 {
		return trigger.MaxInFlight
	}
	return trigger.WorkerPool
}

// streamManualAck reports whether the trigger uses the manual acknowledgement
// policy, under which the runtime never auto-acks a successfully processed
// delivery.
func streamManualAck(trigger runtimeplan.Trigger) bool {
	return strings.EqualFold(strings.TrimSpace(trigger.AckPolicy), string(StreamAckManual))
}

func (rt httpRuntime) processStreamMessage(ctx context.Context, plan runtimeplan.Plan, message streamMessage, attempts *streamAttemptTracker) error {
	attempt := attempts.next(message.ID)

	result, err := runStreamPlanOnce(ctx, rt, plan, message)
	if err == nil {
		attempts.clear(message.ID)
		// Under the manual ack policy the handler owns acknowledgement: the
		// runtime does not auto-ack, so the broker keeps the delivery until the
		// source's own ack closure is invoked elsewhere.
		if streamManualAck(plan.Trigger) {
			return nil
		}
		if ackErr := ackStreamMessage(ctx, message); ackErr != nil {
			return rt.deadLetterStreamMessage(ctx, plan, result, message, ackErr, attempt)
		}
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	maxAttempts := plan.Trigger.MaxAttempts
	if maxAttempts <= 0 {
		// No retry budget configured: preserve the historical behaviour of
		// dead-lettering on the first failure.
		attempts.clear(message.ID)
		return errors.Join(
			rt.deadLetterStreamMessage(ctx, plan, result, message, err, attempt),
			nackStreamMessage(ctx, message, err),
		)
	}
	if attempt < maxAttempts {
		// Within the retry budget: nack so the broker redelivers and emit a
		// redelivery event for observability.
		return errors.Join(
			rt.emitStreamRedelivered(ctx, plan, result, message, err, attempt),
			nackStreamMessage(ctx, message, err),
		)
	}
	// Exhausted the retry budget: dead-letter the poisoned message.
	attempts.clear(message.ID)
	return rt.deadLetterStreamMessage(ctx, plan, result, message, err, attempt)
}

// deadLetterStreamMessage routes the poisoned message to the configured DLQ
// target (when one is set) and emits the stream_dead_lettered event. When a DLQ
// target is configured the source delivery is acked so the broker stops
// redelivering it; otherwise the message is left for the broker to handle.
func (rt httpRuntime) deadLetterStreamMessage(ctx context.Context, plan runtimeplan.Plan, result planRunResult, message streamMessage, deliveryErr error, attempt int) error {
	target := strings.TrimSpace(plan.Trigger.DLQTarget)
	if target == "" || rt.streamDLQ == nil {
		return rt.emitStreamDeadLetter(ctx, plan, result, message, deliveryErr, attempt)
	}
	routeErr := rt.streamDLQ.Route(ctx, target, message)
	if routeErr != nil {
		// Could not move it to the DLQ: nack so the broker keeps the message.
		return errors.Join(routeErr, nackStreamMessage(ctx, message, deliveryErr))
	}
	// The message now lives in the DLQ; ack the source so it is not
	// redelivered, then record the dead-letter event.
	return errors.Join(
		ackStreamMessage(ctx, message),
		rt.emitStreamDeadLetter(ctx, plan, result, message, deliveryErr, attempt),
	)
}

func ackStreamMessage(ctx context.Context, message streamMessage) error {
	if message.ack == nil {
		return nil
	}
	return message.ack(ctx)
}

func nackStreamMessage(ctx context.Context, message streamMessage, deliveryErr error) error {
	if message.nack == nil {
		return nil
	}
	return message.nack(ctx, deliveryErr)
}

func runStreamPlanOnce(ctx context.Context, rt httpRuntime, plan runtimeplan.Plan, message streamMessage) (planRunResult, error) {
	if plan.Trigger.Kind != runtimeplan.TriggerStream {
		return planRunResult{}, fmt.Errorf("%w: expected stream trigger", ErrRunNotImplemented)
	}
	input, err := streamInput(plan, message)
	if err != nil {
		return planRunResult{}, err
	}
	session, duplicate, err := rt.reserveStreamIdempotency(ctx, plan, message)
	if err != nil {
		return planRunResult{}, err
	}
	if duplicate {
		return planRunResultFromInput(input, session), nil
	}

	result := planRunResultFromInput(input, session)
	directExecution := false
	if len(plan.Steps) == 0 && result.HasSession {
		if err := rt.startPipelineExecution(ctx, result.Session, plan); err != nil {
			return result, err
		}
		directExecution = true
	}
	if len(plan.Steps) > 0 {
		result, err = rt.runPlanResultWithSession(ctx, plan, input, session)
		if err != nil {
			return result, err
		}
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
	if err != nil {
		if result.HasSession {
			err = errors.Join(err, rt.finishPipelineExecution(ctx, result.Session, plan, "failed", err))
		}
		return result, err
	}
	if directExecution {
		err = rt.finishPipelineExecution(ctx, result.Session, plan, "completed", nil)
	}
	return result, err
}

func streamInput(plan runtimeplan.Plan, message streamMessage) (string, error) {
	payload := map[string]any{
		"trigger": "stream",
		"uri":     streamDisplayURI(plan.Trigger.URI),
	}
	if strings.TrimSpace(message.ID) != "" {
		payload["id"] = strings.TrimSpace(message.ID)
	}
	if strings.TrimSpace(message.Body) != "" {
		var decoded any
		if err := json.Unmarshal([]byte(message.Body), &decoded); err == nil {
			payload["body"] = decoded
		} else {
			payload["body"] = message.Body
		}
	}
	if len(message.Metadata) > 0 {
		metadata := make(map[string]string, len(message.Metadata))
		for key, value := range message.Metadata {
			key = strings.TrimSpace(key)
			if key != "" {
				metadata[key] = value
			}
		}
		if len(metadata) > 0 {
			payload["metadata"] = metadata
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func streamDisplayURI(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	parsed.User = nil
	return parsed.String()
}
