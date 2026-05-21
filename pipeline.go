package ovr

import (
	"fmt"
	"strings"
	"time"
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
	retry      *retrySpec
	budget     Budget
	noCache    bool
	sequential bool
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
	return n.validatePipeNodeWithSubAgentContext(0, nil)
}

func (n pipeNode) validatePipeNodeWithSubAgentContext(depth int, stack []string) error {
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
		if err := subAgent.validateSubAgentWithContext(depth+1, stack); err != nil {
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

type timeoutOption struct {
	duration time.Duration
	err      error
}

// Timeout configures a Pipe wall-clock budget using a Go duration string.
func Timeout(value string) PipeOption {
	duration, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return timeoutOption{err: fmt.Errorf("%w: Pipe timeout must be a valid duration", ErrInvalidNode)}
	}
	if duration <= 0 {
		return timeoutOption{err: fmt.Errorf("%w: Pipe timeout must be greater than zero", ErrInvalidNode)}
	}
	return timeoutOption{duration: duration}
}

func (o timeoutOption) applyPipe(config *pipeConfig) {
	if o.err != nil {
		config.setErr(o.err)
		return
	}
	config.budget.MaxWallClock = o.duration
}

type maxTokensOption struct {
	max int
}

// MaxTokens configures a Pipe token budget.
func MaxTokens(max int) PipeOption {
	return maxTokensOption{max: max}
}

func (o maxTokensOption) applyPipe(config *pipeConfig) {
	if o.max <= 0 {
		config.setErr(fmt.Errorf("%w: Pipe MaxTokens must be greater than zero", ErrInvalidNode))
		return
	}
	config.budget.MaxTokens = o.max
}

type maxCostUSDOption struct {
	max float64
}

// MaxCostUSD configures a Pipe cost budget in US dollars.
func MaxCostUSD(max float64) PipeOption {
	return maxCostUSDOption{max: max}
}

func (o maxCostUSDOption) applyPipe(config *pipeConfig) {
	if o.max <= 0 {
		config.setErr(fmt.Errorf("%w: Pipe MaxCostUSD must be greater than zero", ErrInvalidNode))
		return
	}
	config.budget.MaxCostUSD = o.max
}

type noCacheOption struct{}

// NoCache disables provider prompt-cache hints for one Pipe.
func NoCache() PipeOption {
	return noCacheOption{}
}

func (noCacheOption) applyPipe(config *pipeConfig) {
	config.noCache = true
}

type sequentialToolsOption struct{}

// SequentialTools forces tool calls from one provider turn to execute one at a time.
func SequentialTools() PipeOption {
	return sequentialToolsOption{}
}

func (sequentialToolsOption) applyPipe(config *pipeConfig) {
	config.sequential = true
}

// BackoffPolicy configures the delay between retry attempts.
type BackoffPolicy interface {
	retryBackoff() time.Duration
}

const defaultExponentialRetryBackoff = 100 * time.Millisecond

type exponentialBackoffPolicy struct {
	base time.Duration
}

// ExponentialBackoff configures retry attempts with an exponential delay.
func ExponentialBackoff() BackoffPolicy {
	return exponentialBackoffPolicy{base: defaultExponentialRetryBackoff}
}

func (p exponentialBackoffPolicy) retryBackoff() time.Duration {
	return p.base
}

type retrySpec struct {
	providerRetries int
	backoff         time.Duration
}

type retryOption struct {
	spec retrySpec
	err  error
}

// Retry configures transient provider retries and retry-safe tool retries.
// Tool retries only apply to ReadOnly or Idempotent tools.
func Retry(max int, policies ...BackoffPolicy) PipeOption {
	option := retryOption{spec: retrySpec{providerRetries: max}}
	if max < 0 {
		option.err = fmt.Errorf("%w: Retry count must be greater than or equal to zero", ErrInvalidNode)
		return option
	}
	if len(policies) > 1 {
		option.err = fmt.Errorf("%w: Retry accepts at most one backoff policy", ErrInvalidNode)
		return option
	}
	for _, policy := range policies {
		if policy == nil {
			option.err = fmt.Errorf("%w: Retry backoff policy is required", ErrInvalidNode)
			return option
		}
		option.spec.backoff = policy.retryBackoff()
	}
	return option
}

func (o retryOption) applyPipe(config *pipeConfig) {
	if o.err != nil {
		config.setErr(o.err)
		return
	}
	if config.retry != nil {
		config.setErr(fmt.Errorf("%w: Pipe retry declared more than once", ErrInvalidNode))
		return
	}
	config.retry = &o.spec
}

// PipelineSpec is a child pipeline declaration used by SubAgent.
type PipelineSpec struct {
	nodes []Node
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
const defaultSubAgentMaxDepth = 8

type subAgentSpec struct {
	name        string
	pipeline    PipelineSpec
	maxParallel int
	partialOK   bool
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

type partialOKOption struct{}

// PartialOK lets a SubAgent return ordered error results for failed child tasks
// or lets Parallel/Map return ordered error outcomes instead of failing
// the whole parent pipeline immediately.
func PartialOK() partialOKOption {
	return partialOKOption{}
}

func (o partialOKOption) applySubAgent(spec *subAgentSpec) {
	spec.partialOK = true
}

func (o partialOKOption) applyParallel(config *parallelConfig) {
	config.partialOK = true
}

func (o partialOKOption) applyMap(config *mapConfig) {
	config.partialOK = true
}

func (s subAgentSpec) validateSubAgent() error {
	return s.validateSubAgentWithContext(1, nil)
}

func (s subAgentSpec) validateSubAgentWithContext(depth int, stack []string) error {
	if s.err != nil {
		return s.err
	}
	if s.name == "" {
		return fmt.Errorf("%w: SubAgent name is required", ErrInvalidNode)
	}
	if depth > defaultSubAgentMaxDepth {
		return fmt.Errorf("%w: SubAgent depth cannot exceed %d", ErrInvalidNode, defaultSubAgentMaxDepth)
	}
	if containsSubAgentName(stack, s.name) {
		return fmt.Errorf("%w: SubAgent cycle detected for %q", ErrInvalidNode, s.name)
	}
	if s.maxParallel <= 0 {
		return fmt.Errorf("%w: SubAgent MaxParallel must be greater than zero", ErrInvalidNode)
	}
	if s.maxParallel > defaultSubAgentMaxParallel {
		return fmt.Errorf("%w: SubAgent MaxParallel cannot exceed %d", ErrInvalidNode, defaultSubAgentMaxParallel)
	}
	return s.pipeline.validateSubAgentPipelineWithContext(depth, append(stack, s.name))
}

func (s *subAgentSpec) setErr(err error) {
	if s.err == nil {
		s.err = err
	}
}

func (p PipelineSpec) validateSubAgentPipeline() error {
	return p.validateSubAgentPipelineWithContext(0, nil)
}

func (p PipelineSpec) validateSubAgentPipelineWithContext(depth int, stack []string) error {
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
		pipe, ok := pipeNodeFromNode(node)
		if !ok {
			return fmt.Errorf("%w: SubAgent pipeline node %d must be Pipe", ErrInvalidNode, i)
		}
		if err := pipe.validatePipeNodeWithSubAgentContext(depth, stack); err != nil {
			return fmt.Errorf("SubAgent pipeline node %d: %w", i, err)
		}
	}
	return nil
}

func containsSubAgentName(stack []string, name string) bool {
	for _, current := range stack {
		if current == name {
			return true
		}
	}
	return false
}
