package ovr

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"sync"
	"syscall"

	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
)

func plansRunnableTogether(plans []runtimeplan.Plan) bool {
	if len(plans) == 0 {
		return false
	}
	kinds := make(map[runtimeplan.TriggerKind]struct{})
	for _, plan := range plans {
		switch plan.Trigger.Kind {
		case runtimeplan.TriggerHTTP, runtimeplan.TriggerWebhook, runtimeplan.TriggerCron, runtimeplan.TriggerStream:
			kinds[plan.Trigger.Kind] = struct{}{}
		default:
			return false
		}
	}
	return len(kinds) > 1
}

func serveMixedPlans(addr string, rt httpRuntime, plans []runtimeplan.Plan) error {
	groups, err := runtimePlanGroups(plans)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	runners := make([]func(context.Context) error, 0, 4)
	if adminAddr := adminAddrFromEnv(); adminAddr != "" {
		publicHandler, adminHandler, err := newSplitHTTPHandlersFromRoutesAndPlans(groups.httpRoutes, plans, rt)
		if err != nil {
			return err
		}
		runners = append(runners,
			func(ctx context.Context) error {
				return serveHTTPWithContext(ctx, addr, publicHandler)
			},
			func(ctx context.Context) error {
				return serveHTTPWithContext(ctx, adminAddr, adminHandler)
			},
		)
	} else {
		handler, err := newHTTPHandlerFromRoutesAndPlans(groups.httpRoutes, plans, rt)
		if err != nil {
			return err
		}
		runners = append(runners, func(ctx context.Context) error {
			return serveHTTPWithContext(ctx, addr, handler)
		})
	}
	if len(groups.cronPlans) > 0 {
		cronPlans := append([]runtimeplan.Plan(nil), groups.cronPlans...)
		schedules := append([]cronSchedule(nil), groups.cronSchedules...)
		runners = append(runners, func(ctx context.Context) error {
			return runCronPlansWithSchedules(ctx, rt, cronPlans, schedules)
		})
	}
	if len(groups.streamPlans) > 0 {
		streamPlans := append([]runtimeplan.Plan(nil), groups.streamPlans...)
		runners = append(runners, func(ctx context.Context) error {
			return runStreamPlans(ctx, rt, streamPlans)
		})
	}
	return runSupervisedRuntimes(ctx, runners...)
}

type runtimeGroups struct {
	httpRoutes    []httpRoute
	cronPlans     []runtimeplan.Plan
	cronSchedules []cronSchedule
	streamPlans   []runtimeplan.Plan
}

func runtimePlanGroups(plans []runtimeplan.Plan) (runtimeGroups, error) {
	var groups runtimeGroups
	for _, plan := range plans {
		switch plan.Trigger.Kind {
		case runtimeplan.TriggerHTTP:
			groups.httpRoutes = append(groups.httpRoutes, httpRouteFromHTTPPlan(plan))
		case runtimeplan.TriggerWebhook:
			route, err := httpRouteFromWebhookPlan(plan)
			if err != nil {
				return runtimeGroups{}, err
			}
			groups.httpRoutes = append(groups.httpRoutes, route)
		case runtimeplan.TriggerCron:
			schedule, err := parseCronSchedule(plan.Trigger.Expr)
			if err != nil {
				return runtimeGroups{}, fmt.Errorf("%w: %v", ErrInvalidNode, err)
			}
			groups.cronPlans = append(groups.cronPlans, plan)
			groups.cronSchedules = append(groups.cronSchedules, schedule)
		case runtimeplan.TriggerStream:
			if err := validateStreamURI(plan.Trigger.URI); err != nil {
				return runtimeGroups{}, fmt.Errorf("%w: %v", ErrInvalidNode, err)
			}
			groups.streamPlans = append(groups.streamPlans, plan)
		default:
			return runtimeGroups{}, fmt.Errorf("%w: mixed runtime contains unsupported trigger %q", ErrRunNotImplemented, plan.Trigger.Kind)
		}
	}
	return groups, nil
}

func runSupervisedRuntimes(ctx context.Context, runners ...func(context.Context) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, len(runners))
	var wg sync.WaitGroup
	for _, runner := range runners {
		runner := runner
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runner(runCtx); err != nil && !errors.Is(err, context.Canceled) {
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
