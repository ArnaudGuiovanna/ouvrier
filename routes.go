package ovr

import (
	"fmt"

	runtimeplan "ouvrier/internal/runtime"
)

type httpRoute struct {
	method   string
	path     string
	terminal runtimeplan.TerminalKind
	hasPipe  bool
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
			method:   plan.Trigger.Method,
			path:     plan.Trigger.Path,
			terminal: plan.Terminal.Kind,
			hasPipe:  len(plan.Steps) > 0,
		})
	}
	return routes, nil
}
