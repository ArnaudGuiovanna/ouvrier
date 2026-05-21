package runtime

import (
	"encoding/json"
	"reflect"
	"time"

	"ouvrier/internal/policy"
)

type TriggerKind string

const (
	TriggerHTTP    TriggerKind = "http"
	TriggerCron    TriggerKind = "cron"
	TriggerWebhook TriggerKind = "webhook"
	TriggerStream  TriggerKind = "stream"
)

type Trigger struct {
	Kind              TriggerKind
	Method            string
	Path              string
	Expr              string
	Value             string
	URI               string
	WorkerPool        int
	IdempotencyHeader string
	SignatureEnv      string
	SignatureHeader   string
}

type StepKind string

const (
	StepPipe StepKind = "pipe"
)

type Step struct {
	Kind            StepKind
	Goal            string
	Model           string
	Tools           []Tool
	MCPServers      []MCPServer
	SubAgents       []SubAgent
	ResultSchema    *ResultSchema
	Retry           *Retry
	Budget          Budget
	SequentialTools bool
}

type Retry struct {
	ProviderRetries int
	Backoff         time.Duration
}

type SubAgent struct {
	Name        string
	Pipeline    Pipeline
	MaxParallel int
	PartialOK   bool
}

type Pipeline struct {
	Steps []Step
}

type Tool struct {
	Name             string
	Description      string
	InputSchema      json.RawMessage
	ArgumentName     string
	GoFunc           any
	Effect           policy.Effect
	IdempotencyKey   string
	SideEffects      []string
	RequiresApproval bool
	Timeout          time.Duration
}

type MCPServer struct {
	Name string
}

type TerminalKind string

const (
	TerminalReply TerminalKind = "reply"
	TerminalPush  TerminalKind = "push"
	TerminalSink  TerminalKind = "sink"
)

type Terminal struct {
	Kind           TerminalKind
	Async          bool
	ResultSchema   *ResultSchema
	SinkLog        bool
	SinkFilePath   string
	PushWebhookURL string
}

type Plan struct {
	Trigger  Trigger
	Steps    []Step
	Terminal Terminal
}

type ResultSchema struct {
	Name       string
	Type       reflect.Type
	JSONSchema json.RawMessage
}
