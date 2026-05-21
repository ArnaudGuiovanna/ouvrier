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
	subAgents, err := runtimeSubAgentsFromPipe(pipe.config.subAgents)
	if err != nil {
		return runtimeplan.Step{}, err
	}
	step := runtimeplan.Step{
		Kind:       runtimeplan.StepPipe,
		Goal:       pipe.goal,
		Model:      pipe.config.model,
		Tools:      runtimeToolsFromPipe(pipe.config.tools),
		MCPServers: runtimeMCPServersFromPipe(pipe.config.mcpServers),
		SubAgents:  subAgents,
		Budget: runtimeplan.Budget{
			MaxTokens:    pipe.config.budget.MaxTokens,
			MaxCostUSD:   pipe.config.budget.MaxCostUSD,
			MaxWallClock: pipe.config.budget.MaxWallClock,
		},
		NoCache:         pipe.config.noCache,
		SequentialTools: pipe.config.sequential,
	}
	if pipe.config.output != nil {
		resultSchema, err := resultSchemaFromType(pipe.config.output.typ)
		if err != nil {
			return runtimeplan.Step{}, err
		}
		step.ResultSchema = resultSchema
	}
	if pipe.config.retry != nil {
		step.Retry = &runtimeplan.Retry{
			ProviderRetries: pipe.config.retry.providerRetries,
			Backoff:         pipe.config.retry.backoff,
		}
	}
	return step, nil
}

func runtimeMCPServersFromPipe(servers []mcpSpec) []runtimeplan.MCPServer {
	out := make([]runtimeplan.MCPServer, 0, len(servers))
	for _, server := range servers {
		out = append(out, runtimeplan.MCPServer{Name: server.name})
	}
	return out
}

func runtimeSubAgentsFromPipe(subAgents []subAgentSpec) ([]runtimeplan.SubAgent, error) {
	out := make([]runtimeplan.SubAgent, 0, len(subAgents))
	for _, subAgent := range subAgents {
		if err := subAgent.validateSubAgent(); err != nil {
			return nil, err
		}
		pipeline, err := compileSubAgentPipeline(subAgent.pipeline)
		if err != nil {
			return nil, err
		}
		out = append(out, runtimeplan.SubAgent{
			Name:        subAgent.name,
			Pipeline:    pipeline,
			MaxParallel: subAgent.maxParallel,
			PartialOK:   subAgent.partialOK,
		})
	}
	return out, nil
}

func compileSubAgentPipeline(pipeline PipelineSpec) (runtimeplan.Pipeline, error) {
	if err := pipeline.validateSubAgentPipeline(); err != nil {
		return runtimeplan.Pipeline{}, err
	}
	steps := make([]runtimeplan.Step, 0, len(pipeline.nodes))
	for i, node := range pipeline.nodes {
		step, err := compileStep(node)
		if err != nil {
			return runtimeplan.Pipeline{}, fmt.Errorf("SubAgent pipeline node %d: %w", i, err)
		}
		steps = append(steps, step)
	}
	return runtimeplan.Pipeline{Steps: steps}, nil
}

func runtimeToolsFromPipe(tools []toolSpec) []runtimeplan.Tool {
	out := make([]runtimeplan.Tool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, runtimeplan.Tool{
			Name:             tool.name,
			Description:      tool.description,
			InputSchema:      toolInputSchema(tool),
			ArgumentName:     toolArgumentName(tool),
			GoFunc:           tool.fn,
			Effect:           tool.effect,
			IdempotencyKey:   tool.idempotencyKey,
			SideEffects:      append([]string(nil), tool.sideEffects...),
			RequiresApproval: tool.requiresApproval,
			Timeout:          tool.timeout,
		})
	}
	return out
}

func compileTerminal(node Node) (runtimeplan.Terminal, error) {
	switch terminal := node.(type) {
	case replyNode:
		return compileReplyTerminal(terminal)
	case *replyNode:
		if terminal == nil {
			return runtimeplan.Terminal{}, ErrInvalidNode
		}
		return compileReplyTerminal(*terminal)
	case pushNode:
		return compilePushTerminal(terminal), nil
	case *pushNode:
		if terminal == nil {
			return runtimeplan.Terminal{}, ErrInvalidNode
		}
		return compilePushTerminal(*terminal), nil
	case sinkNode:
		return compileSinkTerminal(terminal), nil
	case *sinkNode:
		if terminal == nil {
			return runtimeplan.Terminal{}, ErrInvalidNode
		}
		return compileSinkTerminal(*terminal), nil
	default:
		return runtimeplan.Terminal{}, ErrInvalidNode
	}
}

func compileReplyTerminal(node replyNode) (runtimeplan.Terminal, error) {
	terminal := runtimeplan.Terminal{Kind: runtimeplan.TerminalReply}
	if format, ok := node.format.(asyncReplyFormat); ok && format.asyncReply() {
		terminal.Async = true
	}
	if format, ok := node.format.(sseReplyFormat); ok && format.sseReply() {
		terminal.SSE = true
	}
	if format, ok := node.format.(resultSchemaCarrier); ok {
		resultSchema, err := resultSchemaFromType(format.resultSchemaType())
		if err != nil {
			return runtimeplan.Terminal{}, err
		}
		terminal.ResultSchema = resultSchema
	}
	return terminal, nil
}

type webhookPushTarget interface {
	pushWebhookURL() string
}

type queuePushTarget interface {
	pushQueueURI() string
}

func compilePushTerminal(node pushNode) runtimeplan.Terminal {
	terminal := runtimeplan.Terminal{Kind: runtimeplan.TerminalPush}
	if target, ok := node.target.(webhookPushTarget); ok {
		terminal.PushWebhookURL = target.pushWebhookURL()
	}
	if target, ok := node.target.(queuePushTarget); ok {
		terminal.PushQueueURI = target.pushQueueURI()
	}
	return terminal
}

type fileSinkTarget interface {
	sinkFilePath() string
}

type logSinkTarget interface {
	sinkLog() bool
}

func compileSinkTerminal(node sinkNode) runtimeplan.Terminal {
	terminal := runtimeplan.Terminal{Kind: runtimeplan.TerminalSink}
	if target, ok := node.target.(fileSinkTarget); ok {
		terminal.SinkFilePath = target.sinkFilePath()
	}
	if target, ok := node.target.(logSinkTarget); ok {
		terminal.SinkLog = target.sinkLog()
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
