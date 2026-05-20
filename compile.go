package ovr

import (
	"fmt"
	"reflect"

	runtimeplan "ouvrier/internal/runtime"
)

func compilePlans(nodes []Node) ([]runtimeplan.Plan, error) {
	if len(nodes) == 0 {
		return nil, ErrPipelineEmpty
	}
	if nodes[0] == nil || nodes[0].nodeKind() != nodeKindFrom {
		return nil, ErrFirstNodeNotFrom
	}

	plans := make([]runtimeplan.Plan, 0, 1)
	for i := 0; i < len(nodes); {
		node := nodes[i]
		if node == nil {
			return nil, fmt.Errorf("node %d: %w", i, ErrInvalidNode)
		}
		if node.nodeKind() != nodeKindFrom {
			return nil, fmt.Errorf("node %d: %w", i, ErrFirstNodeNotFrom)
		}

		plan, next, err := compilePlanAt(nodes, i)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
		i = next
	}
	return plans, nil
}

func compilePlanAt(nodes []Node, start int) (runtimeplan.Plan, int, error) {
	from, ok := fromNodeFromNode(nodes[start])
	if !ok {
		return runtimeplan.Plan{}, 0, fmt.Errorf("node %d: %w", start, ErrInvalidNode)
	}
	if err := nodes[start].validateNode(); err != nil {
		return runtimeplan.Plan{}, 0, fmt.Errorf("node %d: %w", start, err)
	}

	trigger, err := compileTrigger(from.source)
	if err != nil {
		return runtimeplan.Plan{}, 0, fmt.Errorf("node %d: %w", start, err)
	}
	trigger.WorkerPool = from.config.workerPool

	plan := runtimeplan.Plan{Trigger: trigger}
	for i := start + 1; i < len(nodes); i++ {
		node := nodes[i]
		if node == nil {
			return runtimeplan.Plan{}, 0, fmt.Errorf("node %d: %w", i, ErrInvalidNode)
		}
		if err := node.validateNode(); err != nil {
			return runtimeplan.Plan{}, 0, fmt.Errorf("node %d: %w", i, err)
		}

		switch node.nodeKind() {
		case nodeKindFrom:
			return runtimeplan.Plan{}, 0, fmt.Errorf("node %d: %w", i, ErrTerminalMissing)
		case nodeKindPipe:
			step, err := compileStep(node)
			if err != nil {
				return runtimeplan.Plan{}, 0, fmt.Errorf("node %d: %w", i, err)
			}
			plan.Steps = append(plan.Steps, step)
		case nodeKindReply, nodeKindPush, nodeKindSink:
			terminal, err := compileTerminal(node)
			if err != nil {
				return runtimeplan.Plan{}, 0, fmt.Errorf("node %d: %w", i, err)
			}
			if err := validateTerminalCompatibility(trigger.Kind, terminal.Kind); err != nil {
				return runtimeplan.Plan{}, 0, fmt.Errorf("node %d: %w", i, err)
			}
			plan.Terminal = terminal
			if err := reconcileTerminalResultSchema(&plan); err != nil {
				return runtimeplan.Plan{}, 0, fmt.Errorf("node %d: %w", i, err)
			}
			next := i + 1
			if next < len(nodes) {
				if err := validateNextPipelineStart(nodes[next], next); err != nil {
					return runtimeplan.Plan{}, 0, err
				}
			}
			return plan, next, nil
		default:
			return runtimeplan.Plan{}, 0, fmt.Errorf("node %d: %w", i, ErrInvalidNode)
		}
	}

	return runtimeplan.Plan{}, 0, ErrTerminalMissing
}

func reconcileTerminalResultSchema(plan *runtimeplan.Plan) error {
	if plan.Terminal.Kind != runtimeplan.TerminalReply || plan.Terminal.ResultSchema == nil || len(plan.Steps) == 0 {
		return nil
	}
	last := &plan.Steps[len(plan.Steps)-1]
	if last.ResultSchema == nil {
		last.ResultSchema = plan.Terminal.ResultSchema
		return nil
	}
	if !compatibleResultSchemas(last.ResultSchema, plan.Terminal.ResultSchema) {
		return fmt.Errorf("%w: Reply JSON schema %s does not match Pipe Output schema %s",
			ErrIncompatibleTerminal,
			resultSchemaLabel(plan.Terminal.ResultSchema),
			resultSchemaLabel(last.ResultSchema),
		)
	}
	return nil
}

func compatibleResultSchemas(left, right *runtimeplan.ResultSchema) bool {
	if left == nil || right == nil {
		return left == right
	}
	if left.Type != nil || right.Type != nil {
		return left.Type == right.Type
	}
	return reflect.DeepEqual(left.JSONSchema, right.JSONSchema) || left.Name == right.Name
}

func resultSchemaLabel(contract *runtimeplan.ResultSchema) string {
	if contract == nil {
		return "<nil>"
	}
	if contract.Name != "" {
		return contract.Name
	}
	if contract.Type != nil {
		return contract.Type.String()
	}
	return "<unnamed>"
}

func validateNextPipelineStart(node Node, index int) error {
	if node == nil {
		return fmt.Errorf("node %d: %w", index, ErrInvalidNode)
	}
	switch node.nodeKind() {
	case nodeKindFrom:
		return nil
	case nodeKindReply, nodeKindPush, nodeKindSink:
		return fmt.Errorf("node %d: %w", index, ErrMultipleTerminals)
	default:
		return fmt.Errorf("node %d: %w", index, ErrTerminalNotLast)
	}
}
