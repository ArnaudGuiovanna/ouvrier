package ovr

import "fmt"

type httpRoute struct {
	method   string
	path     string
	terminal nodeKind
	hasPipe  bool
}

func httpRoutesFromNodes(nodes []Node) ([]httpRoute, error) {
	if err := validatePipeline(nodes); err != nil {
		return nil, err
	}

	trigger, ok := httpTriggerFromNode(nodes[0])
	if !ok {
		return nil, fmt.Errorf("%w: only HTTP triggers are supported by this runtime slice", ErrRunNotImplemented)
	}

	route := httpRoute{
		method: trigger.method,
		path:   trigger.path,
	}
	for _, node := range nodes[1:] {
		switch node.nodeKind() {
		case nodeKindPipe:
			route.hasPipe = true
		case nodeKindReply, nodeKindPush, nodeKindSink:
			route.terminal = node.nodeKind()
			return []httpRoute{route}, nil
		}
	}

	return nil, ErrTerminalMissing
}

func httpTriggerFromNode(node Node) (httpTrigger, bool) {
	switch node := node.(type) {
	case fromNode:
		trigger, ok := node.source.(httpTrigger)
		return trigger, ok
	case *fromNode:
		trigger, ok := node.source.(httpTrigger)
		return trigger, ok
	default:
		return httpTrigger{}, false
	}
}
