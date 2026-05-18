package ovr

import (
	"errors"
	"fmt"
)

var (
	// ErrPipelineEmpty means no pipeline nodes were provided.
	ErrPipelineEmpty = errors.New("pipeline is empty")
	// ErrFirstNodeNotFrom means the pipeline does not start with a From node.
	ErrFirstNodeNotFrom = errors.New("first node must be From")
	// ErrTerminalMissing means the pipeline has no Reply, Push, or Sink node.
	ErrTerminalMissing = errors.New("pipeline must include Reply, Push, or Sink")
	// ErrPipeMissingModel means a Pipe node was declared without Model.
	ErrPipeMissingModel = errors.New("pipe must include Model")
	// ErrInvalidNode means a node was nil, unsupported, or otherwise invalid.
	ErrInvalidNode = errors.New("invalid node")
)

// Validate checks a pipeline declaration without starting any runtime work.
func Validate(nodes ...Node) error {
	return validatePipeline(nodes)
}

func validatePipeline(nodes []Node) error {
	if len(nodes) == 0 {
		return ErrPipelineEmpty
	}
	if nodes[0] == nil || nodes[0].nodeKind() != nodeKindFrom {
		return ErrFirstNodeNotFrom
	}

	hasTerminal := false
	for i, node := range nodes {
		if node == nil {
			return fmt.Errorf("node %d: %w", i, ErrInvalidNode)
		}
		if err := node.validateNode(); err != nil {
			return fmt.Errorf("node %d: %w", i, err)
		}
		switch node.nodeKind() {
		case nodeKindReply, nodeKindPush, nodeKindSink:
			hasTerminal = true
		}
	}

	if !hasTerminal {
		return ErrTerminalMissing
	}
	return nil
}
