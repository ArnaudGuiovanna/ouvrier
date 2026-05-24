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

	workerPool := newCronWorkerPool(plan.Trigger.WorkerPool)
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
			if err := rt.processStreamMessage(runCtx, plan, message); err != nil {
				select {
				case processErr <- err:
					cancel()
				default:
				}
			}
		}(message)
	}
}

func (rt httpRuntime) processStreamMessage(ctx context.Context, plan runtimeplan.Plan, message streamMessage) error {
	result, err := runStreamPlanOnce(ctx, rt, plan, message)
	if err == nil {
		if ackErr := ackStreamMessage(ctx, message); ackErr != nil {
			return rt.emitStreamDeadLetter(ctx, plan, result, message, ackErr)
		}
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.Join(rt.emitStreamDeadLetter(ctx, plan, result, message, err), nackStreamMessage(ctx, message, err))
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
