package ovr

import "encoding/json"

type adminPlansResponse struct {
	Status string              `json:"status"`
	Plans  []adminPlanResponse `json:"plans"`
}

type adminCapabilitiesResponse struct {
	Status       string              `json:"status"`
	Capabilities []adminPlanResponse `json:"capabilities"`
}

type adminPlanResponse struct {
	ID       string                    `json:"id"`
	Trigger  adminPlanTriggerResponse  `json:"trigger"`
	Steps    []adminStepResponse       `json:"steps"`
	Terminal adminPlanTerminalResponse `json:"terminal"`
}

type adminPlanTriggerResponse struct {
	Kind              string `json:"kind"`
	Method            string `json:"method,omitempty"`
	Path              string `json:"path,omitempty"`
	Expr              string `json:"expr,omitempty"`
	Value             string `json:"value,omitempty"`
	URI               string `json:"uri,omitempty"`
	WorkerPool        int    `json:"worker_pool,omitempty"`
	IdempotencyHeader string `json:"idempotency_header,omitempty"`
	SignatureEnv      string `json:"signature_env,omitempty"`
	SignatureHeader   string `json:"signature_header,omitempty"`
	DLQTarget         string `json:"dlq_target,omitempty"`
	MaxAttempts       int    `json:"max_attempts,omitempty"`
	MaxInFlight       int    `json:"max_in_flight,omitempty"`
	AckPolicy         string `json:"ack_policy,omitempty"`
}

type adminStepResponse struct {
	Index           int                        `json:"index"`
	Kind            string                     `json:"kind"`
	Goal            string                     `json:"goal,omitempty"`
	Model           string                     `json:"model,omitempty"`
	Fallback        []string                   `json:"fallback,omitempty"`
	Tools           []adminToolResponse        `json:"tools,omitempty"`
	Bash            []adminBashToolResponse    `json:"bash,omitempty"`
	Skills          []string                   `json:"skills,omitempty"`
	MCPServers      []string                   `json:"mcp_servers,omitempty"`
	SubAgents       []adminSubAgentResponse    `json:"subagents,omitempty"`
	Branches        int                        `json:"branches,omitempty"`
	MapSteps        int                        `json:"map_steps,omitempty"`
	Concurrency     int                        `json:"concurrency,omitempty"`
	PartialOK       bool                       `json:"partial_ok,omitempty"`
	ResultSchema    *adminResultSchemaResponse `json:"result_schema,omitempty"`
	Retry           *adminRetryResponse        `json:"retry,omitempty"`
	Budget          adminBudgetResponse        `json:"budget"`
	NoCache         bool                       `json:"no_cache,omitempty"`
	SequentialTools bool                       `json:"sequential_tools,omitempty"`
}

type adminToolResponse struct {
	Name             string          `json:"name"`
	Description      string          `json:"description,omitempty"`
	InputSchema      json.RawMessage `json:"input_schema,omitempty"`
	ArgumentName     string          `json:"argument_name,omitempty"`
	Effect           string          `json:"effect,omitempty"`
	IdempotencyKey   string          `json:"idempotency_key,omitempty"`
	SideEffects      []string        `json:"side_effects,omitempty"`
	RequiresApproval bool            `json:"requires_approval,omitempty"`
	TimeoutMS        int64           `json:"timeout_ms,omitempty"`
}

type adminBashToolResponse struct {
	Name                string   `json:"name"`
	SandboxRoot         string   `json:"sandbox_root,omitempty"`
	AllowedEnv          []string `json:"allowed_env,omitempty"`
	TimeoutMS           int64    `json:"timeout_ms,omitempty"`
	MaxOutputBytes      int      `json:"max_output_bytes,omitempty"`
	UnsafeHostExecution bool     `json:"unsafe_host_execution,omitempty"`
}

type adminSubAgentResponse struct {
	Name        string `json:"name"`
	MaxParallel int    `json:"max_parallel,omitempty"`
	PartialOK   bool   `json:"partial_ok,omitempty"`
	Steps       int    `json:"steps"`
}

type adminResultSchemaResponse struct {
	Name       string          `json:"name,omitempty"`
	Type       string          `json:"type,omitempty"`
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
}

type adminRetryResponse struct {
	ProviderRetries int   `json:"provider_retries"`
	BackoffMS       int64 `json:"backoff_ms,omitempty"`
}

type adminBudgetResponse struct {
	MaxIterations  int     `json:"max_iterations,omitempty"`
	MaxTokens      int     `json:"max_tokens,omitempty"`
	MaxCostUSD     float64 `json:"max_cost_usd,omitempty"`
	MaxWallClockMS int64   `json:"max_wallclock_ms,omitempty"`
}

type adminPlanTerminalResponse struct {
	Kind           string                     `json:"kind"`
	Async          bool                       `json:"async,omitempty"`
	SSE            bool                       `json:"sse,omitempty"`
	ResultSchema   *adminResultSchemaResponse `json:"result_schema,omitempty"`
	SinkLog        bool                       `json:"sink_log,omitempty"`
	SinkFilePath   string                     `json:"sink_file_path,omitempty"`
	PushWebhookURL string                     `json:"push_webhook_url,omitempty"`
	PushQueueURI   string                     `json:"push_queue_uri,omitempty"`
}
