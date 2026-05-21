package ovr

import (
	"fmt"
	"runtime"
)

// ParallelOption configures a Parallel composition step.
type ParallelOption interface {
	applyParallel(*parallelConfig)
}

type parallelConfig struct {
	partialOK bool
	err       error
}

type parallelNode struct {
	nodes  []Node
	config parallelConfig
	err    error
}

// Parallel fans out the current input to multiple Pipe branches.
func Parallel(items ...any) Node {
	node := parallelNode{}
	for _, item := range items {
		switch typed := item.(type) {
		case nil:
			node.setErr(fmt.Errorf("%w: nil Parallel item", ErrInvalidNode))
		case Node:
			node.nodes = append(node.nodes, typed)
		case ParallelOption:
			typed.applyParallel(&node.config)
		default:
			node.setErr(fmt.Errorf("%w: unsupported Parallel item %T", ErrInvalidNode, item))
		}
	}
	return node
}

func (n parallelNode) nodeKind() nodeKind {
	return nodeKindParallel
}

func (n parallelNode) validateNode() error {
	if n.err != nil {
		return n.err
	}
	if n.config.err != nil {
		return n.config.err
	}
	if len(n.nodes) == 0 {
		return fmt.Errorf("%w: Parallel requires at least one Pipe", ErrInvalidNode)
	}
	for i, node := range n.nodes {
		if err := validateCompositionPipeNode(node, "Parallel", i); err != nil {
			return err
		}
	}
	return nil
}

func (n *parallelNode) setErr(err error) {
	if n.err == nil {
		n.err = err
	}
}

// MapOption configures a Map composition step.
type MapOption interface {
	applyMap(*mapConfig)
}

type mapConfig struct {
	concurrency int
	partialOK   bool
	err         error
}

type mapNode struct {
	nodes  []Node
	config mapConfig
	err    error
}

// Map applies a Pipe sub-pipeline to each element of the previous JSON array.
func Map(items ...any) Node {
	node := mapNode{config: mapConfig{concurrency: runtime.NumCPU()}}
	for _, item := range items {
		switch typed := item.(type) {
		case nil:
			node.setErr(fmt.Errorf("%w: nil Map item", ErrInvalidNode))
		case Node:
			node.nodes = append(node.nodes, typed)
		case MapOption:
			typed.applyMap(&node.config)
		default:
			node.setErr(fmt.Errorf("%w: unsupported Map item %T", ErrInvalidNode, item))
		}
	}
	if node.config.concurrency <= 0 {
		node.config.concurrency = 1
	}
	return node
}

func (n mapNode) nodeKind() nodeKind {
	return nodeKindMap
}

func (n mapNode) validateNode() error {
	if n.err != nil {
		return n.err
	}
	if n.config.err != nil {
		return n.config.err
	}
	if len(n.nodes) == 0 {
		return fmt.Errorf("%w: Map requires at least one Pipe", ErrInvalidNode)
	}
	for i, node := range n.nodes {
		if err := validateCompositionPipeNode(node, "Map", i); err != nil {
			return err
		}
	}
	return nil
}

func (n *mapNode) setErr(err error) {
	if n.err == nil {
		n.err = err
	}
}

type concurrencyOption struct {
	limit int
}

// Concurrency bounds concurrent Map item executions.
func Concurrency(limit int) MapOption {
	return concurrencyOption{limit: limit}
}

func (o concurrencyOption) applyMap(config *mapConfig) {
	if o.limit <= 0 {
		config.setErr(fmt.Errorf("%w: Map Concurrency must be greater than zero", ErrInvalidNode))
		return
	}
	config.concurrency = o.limit
}

func (c *mapConfig) setErr(err error) {
	if c.err == nil {
		c.err = err
	}
}

func validateCompositionPipeNode(node Node, scope string, index int) error {
	if node == nil {
		return fmt.Errorf("%w: %s item %d is nil", ErrInvalidNode, scope, index)
	}
	if node.nodeKind() != nodeKindPipe {
		return fmt.Errorf("%w: %s item %d must be Pipe", ErrInvalidNode, scope, index)
	}
	pipe, ok := pipeNodeFromNode(node)
	if !ok {
		return fmt.Errorf("%w: %s item %d must be Pipe", ErrInvalidNode, scope, index)
	}
	if err := pipe.validatePipeNodeWithSubAgentContext(0, nil); err != nil {
		return fmt.Errorf("%s item %d: %w", scope, index, err)
	}
	return nil
}
