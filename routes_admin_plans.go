package ovr

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
)

func (rt httpRuntime) serveAdminPlans(w http.ResponseWriter, req *http.Request) {
	if !rt.authorizeAdmin(w, req) {
		return
	}
	writeJSON(w, http.StatusOK, adminPlansResponse{
		Status: "ok",
		Plans:  adminPlanResponses(rt.adminPlans),
	})
}

func (rt httpRuntime) serveAdminCapabilities(w http.ResponseWriter, req *http.Request) {
	if !rt.authorizeAdmin(w, req) {
		return
	}
	writeJSON(w, http.StatusOK, adminCapabilitiesResponse{
		Status:       "ok",
		Capabilities: adminPlanResponses(rt.adminPlans),
	})
}

func adminPlanResponses(routes []adminPlanRoute) []adminPlanResponse {
	plans := make([]adminPlanResponse, 0, len(routes))
	for i, route := range routes {
		plans = append(plans, adminPlanResponseFromPlan(i+1, route.plan))
	}
	return plans
}

func adminPlanResponseFromPlan(index int, plan runtimeplan.Plan) adminPlanResponse {
	steps := make([]adminStepResponse, 0, len(plan.Steps))
	for i, step := range plan.Steps {
		steps = append(steps, adminStepResponseFromStep(i+1, step))
	}
	return adminPlanResponse{
		ID:       fmt.Sprintf("plan_%d", index),
		Trigger:  adminTriggerResponseFromPlan(plan.Trigger),
		Steps:    steps,
		Terminal: adminTerminalResponseFromPlan(plan.Terminal),
	}
}

func adminTriggerResponseFromPlan(trigger runtimeplan.Trigger) adminPlanTriggerResponse {
	return adminPlanTriggerResponse{
		Kind:              string(trigger.Kind),
		Method:            trigger.Method,
		Path:              trigger.Path,
		Expr:              trigger.Expr,
		Value:             trigger.Value,
		URI:               events.RedactText(trigger.URI),
		WorkerPool:        trigger.WorkerPool,
		IdempotencyHeader: trigger.IdempotencyHeader,
		SignatureEnv:      trigger.SignatureEnv,
		SignatureHeader:   trigger.SignatureHeader,
		DLQTarget:         events.RedactText(trigger.DLQTarget),
		MaxAttempts:       trigger.MaxAttempts,
		MaxInFlight:       trigger.MaxInFlight,
		AckPolicy:         trigger.AckPolicy,
	}
}

func adminStepResponseFromStep(index int, step runtimeplan.Step) adminStepResponse {
	tools := make([]adminToolResponse, 0, len(step.Tools))
	for _, tool := range step.Tools {
		tools = append(tools, adminToolResponseFromTool(tool))
	}
	skills := make([]string, 0, len(step.Skills))
	for _, skill := range step.Skills {
		skills = append(skills, skill.Name)
	}
	mcpServers := make([]string, 0, len(step.MCPServers))
	for _, server := range step.MCPServers {
		mcpServers = append(mcpServers, server.Name)
	}
	bashTools := make([]adminBashToolResponse, 0, len(step.Bash))
	for _, bash := range step.Bash {
		bashTools = append(bashTools, adminBashToolResponseFromTool(bash))
	}
	subAgents := make([]adminSubAgentResponse, 0, len(step.SubAgents))
	for _, subAgent := range step.SubAgents {
		subAgents = append(subAgents, adminSubAgentResponse{
			Name:        subAgent.Name,
			MaxParallel: subAgent.MaxParallel,
			PartialOK:   subAgent.PartialOK,
			Steps:       len(subAgent.Pipeline.Steps),
		})
	}
	return adminStepResponse{
		Index:           index,
		Kind:            string(step.Kind),
		Goal:            step.Goal,
		Model:           step.Model,
		Fallback:        append([]string(nil), step.Fallback...),
		Tools:           tools,
		Bash:            bashTools,
		Skills:          skills,
		MCPServers:      mcpServers,
		SubAgents:       subAgents,
		Branches:        len(step.Branches),
		MapSteps:        len(step.MapPipeline.Steps),
		Concurrency:     step.Concurrency,
		PartialOK:       step.PartialOK,
		ResultSchema:    adminResultSchemaResponseFromPlan(step.ResultSchema),
		Retry:           adminRetryResponseFromPlan(step.Retry),
		Budget:          adminBudgetResponseFromPlan(step.Budget),
		NoCache:         step.NoCache,
		SequentialTools: step.SequentialTools,
	}
}

func adminToolResponseFromTool(tool runtimeplan.Tool) adminToolResponse {
	return adminToolResponse{
		Name:             tool.Name,
		Description:      tool.Description,
		InputSchema:      cloneRawMessage(tool.InputSchema),
		ArgumentName:     tool.ArgumentName,
		Effect:           string(tool.Effect),
		IdempotencyKey:   tool.IdempotencyKey,
		SideEffects:      append([]string(nil), tool.SideEffects...),
		RequiresApproval: tool.RequiresApproval,
		TimeoutMS:        durationMillis(tool.Timeout),
	}
}

func adminBashToolResponseFromTool(tool runtimeplan.BashTool) adminBashToolResponse {
	return adminBashToolResponse{
		Name:                tool.Name,
		SandboxRoot:         events.RedactText(tool.SandboxRoot),
		AllowedEnv:          append([]string(nil), tool.AllowedEnv...),
		TimeoutMS:           durationMillis(tool.Timeout),
		MaxOutputBytes:      tool.MaxOutputBytes,
		UnsafeHostExecution: tool.UnsafeHostExecution,
	}
}

func adminTerminalResponseFromPlan(terminal runtimeplan.Terminal) adminPlanTerminalResponse {
	return adminPlanTerminalResponse{
		Kind:           string(terminal.Kind),
		Async:          terminal.Async,
		SSE:            terminal.SSE,
		ResultSchema:   adminResultSchemaResponseFromPlan(terminal.ResultSchema),
		SinkLog:        terminal.SinkLog,
		SinkFilePath:   events.RedactText(terminal.SinkFilePath),
		PushWebhookURL: events.RedactText(terminal.PushWebhookURL),
		PushQueueURI:   events.RedactText(terminal.PushQueueURI),
	}
}

func adminResultSchemaResponseFromPlan(schema *runtimeplan.ResultSchema) *adminResultSchemaResponse {
	if schema == nil {
		return nil
	}
	response := &adminResultSchemaResponse{Name: schema.Name, JSONSchema: cloneRawMessage(schema.JSONSchema)}
	if schema.Type != nil {
		response.Type = schema.Type.String()
	}
	return response
}

func adminRetryResponseFromPlan(retry *runtimeplan.Retry) *adminRetryResponse {
	if retry == nil {
		return nil
	}
	return &adminRetryResponse{ProviderRetries: retry.ProviderRetries, BackoffMS: durationMillis(retry.Backoff)}
}

func adminBudgetResponseFromPlan(budget runtimeplan.Budget) adminBudgetResponse {
	return adminBudgetResponse{
		MaxIterations:  budget.MaxIterations,
		MaxTokens:      budget.MaxTokens,
		MaxCostUSD:     budget.MaxCostUSD,
		MaxWallClockMS: durationMillis(budget.MaxWallClock),
	}
}

func durationMillis(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return duration.Milliseconds()
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}
