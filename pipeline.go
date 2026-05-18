package ovr

import (
	"fmt"
	"strings"
)

// PipeOption configures a Pipe node.
type PipeOption interface {
	applyPipe(*pipeConfig)
}

type pipeConfig struct {
	model  string
	tools  []toolSpec
	skills []skillSpec
}

type pipeNode struct {
	goal   string
	config pipeConfig
	err    error
}

// Pipe declares one agent step in a pipeline.
func Pipe(goal string, options ...PipeOption) Node {
	node := pipeNode{goal: strings.TrimSpace(goal)}
	for _, option := range options {
		if option == nil {
			node.err = fmt.Errorf("%w: nil Pipe option", ErrInvalidNode)
			continue
		}
		option.applyPipe(&node.config)
	}
	return node
}

func (n pipeNode) nodeKind() nodeKind {
	return nodeKindPipe
}

func (n pipeNode) validateNode() error {
	if n.err != nil {
		return n.err
	}
	if n.goal == "" {
		return fmt.Errorf("%w: Pipe goal is required", ErrInvalidNode)
	}
	if strings.TrimSpace(n.config.model) == "" {
		return ErrPipeMissingModel
	}
	for _, tool := range n.config.tools {
		if err := tool.validateTool(); err != nil {
			return err
		}
	}
	for _, skill := range n.config.skills {
		if err := skill.validateSkill(); err != nil {
			return err
		}
	}
	return nil
}

type modelOption struct {
	id string
}

// Model selects the explicit LLM model used by a Pipe.
func Model(id string) PipeOption {
	return modelOption{id: strings.TrimSpace(id)}
}

func (o modelOption) applyPipe(config *pipeConfig) {
	config.model = o.id
}
