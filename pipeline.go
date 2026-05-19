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
	model      string
	output     *outputSpec
	tools      []toolSpec
	skills     []skillSpec
	mcpServers []mcpSpec
	subAgents  []subAgentSpec
	err        error
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
	if n.config.err != nil {
		return n.config.err
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
	for _, mcpServer := range n.config.mcpServers {
		if err := mcpServer.validateMCP(); err != nil {
			return err
		}
	}
	for _, subAgent := range n.config.subAgents {
		if err := subAgent.validateSubAgent(); err != nil {
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

func (c *pipeConfig) setErr(err error) {
	if c.err == nil {
		c.err = err
	}
}

// PipelineSpec is a child pipeline declaration used by SubAgent.
type PipelineSpec struct {
	nodes []Node
	err   error
}

// Pipeline declares a child pipeline that can be exposed to a Pipe as a SubAgent.
func Pipeline(nodes ...Node) PipelineSpec {
	return PipelineSpec{nodes: append([]Node(nil), nodes...)}
}

// SubAgentOption configures a SubAgent registered on a Pipe.
type SubAgentOption interface {
	applySubAgent(*subAgentSpec)
}

const defaultSubAgentMaxParallel = 5

type subAgentSpec struct {
	name        string
	pipeline    PipelineSpec
	maxParallel int
	err         error
}

type subAgentPipeOption struct {
	spec subAgentSpec
}

// SubAgent exposes a child pipeline as a governed tool for a Pipe.
func SubAgent(name string, pipeline PipelineSpec, options ...SubAgentOption) PipeOption {
	spec := subAgentSpec{
		name:        strings.TrimSpace(name),
		pipeline:    pipeline,
		maxParallel: defaultSubAgentMaxParallel,
	}
	for _, option := range options {
		if option == nil {
			spec.setErr(fmt.Errorf("%w: nil SubAgent option", ErrInvalidNode))
			continue
		}
		option.applySubAgent(&spec)
	}
	return subAgentPipeOption{spec: spec}
}

func (o subAgentPipeOption) applyPipe(config *pipeConfig) {
	config.subAgents = append(config.subAgents, o.spec)
}

type maxParallelOption struct {
	limit int
}

// MaxParallel bounds concurrent invocations of a SubAgent.
func MaxParallel(limit int) SubAgentOption {
	return maxParallelOption{limit: limit}
}

func (o maxParallelOption) applySubAgent(spec *subAgentSpec) {
	if o.limit <= 0 {
		spec.setErr(fmt.Errorf("%w: SubAgent MaxParallel must be greater than zero", ErrInvalidNode))
		return
	}
	if o.limit > defaultSubAgentMaxParallel {
		spec.setErr(fmt.Errorf("%w: SubAgent MaxParallel cannot exceed %d", ErrInvalidNode, defaultSubAgentMaxParallel))
		return
	}
	spec.maxParallel = o.limit
}

func (s subAgentSpec) validateSubAgent() error {
	if s.err != nil {
		return s.err
	}
	if s.name == "" {
		return fmt.Errorf("%w: SubAgent name is required", ErrInvalidNode)
	}
	if s.maxParallel <= 0 {
		return fmt.Errorf("%w: SubAgent MaxParallel must be greater than zero", ErrInvalidNode)
	}
	if s.maxParallel > defaultSubAgentMaxParallel {
		return fmt.Errorf("%w: SubAgent MaxParallel cannot exceed %d", ErrInvalidNode, defaultSubAgentMaxParallel)
	}
	return s.pipeline.validateSubAgentPipeline()
}

func (s *subAgentSpec) setErr(err error) {
	if s.err == nil {
		s.err = err
	}
}

func (p PipelineSpec) validateSubAgentPipeline() error {
	if p.err != nil {
		return p.err
	}
	if len(p.nodes) == 0 {
		return fmt.Errorf("%w: SubAgent pipeline must include at least one Pipe", ErrInvalidNode)
	}
	for i, node := range p.nodes {
		if node == nil {
			return fmt.Errorf("%w: SubAgent pipeline node %d is nil", ErrInvalidNode, i)
		}
		if node.nodeKind() != nodeKindPipe {
			return fmt.Errorf("%w: SubAgent pipeline node %d must be Pipe", ErrInvalidNode, i)
		}
		if err := node.validateNode(); err != nil {
			return fmt.Errorf("SubAgent pipeline node %d: %w", i, err)
		}
	}
	return nil
}
