package ovr

import (
	"fmt"

	runtimeplan "ouvrier/internal/runtime"
)

type httpRoute struct {
	method     string
	path       string
	plan       runtimeplan.Plan
	runtime    httpRuntime
	workerPool chan struct{}
}

func httpRoutesFromNodes(nodes []Node) ([]httpRoute, error) {
	plans, err := compilePlans(nodes)
	if err != nil {
		return nil, err
	}

	routes := make([]httpRoute, 0, len(plans))
	for _, plan := range plans {
		if plan.Trigger.Kind != runtimeplan.TriggerHTTP {
			return nil, fmt.Errorf("%w: only HTTP triggers are supported by this runtime slice", ErrRunNotImplemented)
		}
		routes = append(routes, httpRoute{
			method:     plan.Trigger.Method,
			path:       plan.Trigger.Path,
			plan:       plan,
			workerPool: newWorkerPool(plan.Trigger.WorkerPool),
		})
	}
	return routes, nil
}

func newWorkerPool(limit int) chan struct{} {
	if limit <= 0 {
		return nil
	}
	return make(chan struct{}, limit)
}
