package ovr

import (
	"fmt"

	runtimeplan "ouvrier/internal/runtime"
)

func compileStep(node Node) (runtimeplan.Step, error) {
	pipe, ok := pipeNodeFromNode(node)
	if !ok {
		return runtimeplan.Step{}, ErrInvalidNode
	}
	step := runtimeplan.Step{
		Kind:  runtimeplan.StepPipe,
		Goal:  pipe.goal,
		Model: pipe.config.model,
	}
	if pipe.config.output != nil {
		step.ResultSchema = resultSchemaFromType(pipe.config.output.typ)
	}
	return step, nil
}

func compileTerminal(node Node) (runtimeplan.Terminal, error) {
	switch terminal := node.(type) {
	case replyNode:
		return compileReplyTerminal(terminal), nil
	case *replyNode:
		if terminal == nil {
			return runtimeplan.Terminal{}, ErrInvalidNode
		}
		return compileReplyTerminal(*terminal), nil
	case pushNode:
		return runtimeplan.Terminal{Kind: runtimeplan.TerminalPush}, nil
	case *pushNode:
		if terminal == nil {
			return runtimeplan.Terminal{}, ErrInvalidNode
		}
		return runtimeplan.Terminal{Kind: runtimeplan.TerminalPush}, nil
	case sinkNode:
		return runtimeplan.Terminal{Kind: runtimeplan.TerminalSink}, nil
	case *sinkNode:
		if terminal == nil {
			return runtimeplan.Terminal{}, ErrInvalidNode
		}
		return runtimeplan.Terminal{Kind: runtimeplan.TerminalSink}, nil
	default:
		return runtimeplan.Terminal{}, ErrInvalidNode
	}
}

func compileReplyTerminal(node replyNode) runtimeplan.Terminal {
	terminal := runtimeplan.Terminal{Kind: runtimeplan.TerminalReply}
	if format, ok := node.format.(resultSchemaCarrier); ok {
		terminal.ResultSchema = resultSchemaFromType(format.resultSchemaType())
	}
	return terminal
}

func validateTerminalCompatibility(trigger runtimeplan.TriggerKind, terminal runtimeplan.TerminalKind) error {
	if terminal == runtimeplan.TerminalReply && trigger != runtimeplan.TriggerHTTP {
		return fmt.Errorf("%w: Reply requires an HTTP trigger", ErrIncompatibleTerminal)
	}
	return nil
}

func pipeNodeFromNode(node Node) (pipeNode, bool) {
	switch node := node.(type) {
	case pipeNode:
		return node, true
	case *pipeNode:
		if node == nil {
			return pipeNode{}, false
		}
		return *node, true
	default:
		return pipeNode{}, false
	}
}
