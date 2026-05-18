package runtime

import (
	"encoding/json"
	"reflect"

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
	Kind       TriggerKind
	Method     string
	Path       string
	Expr       string
	Value      string
	URI        string
	WorkerPool int
}

type StepKind string

const (
	StepPipe StepKind = "pipe"
)

type Step struct {
	Kind         StepKind
	Goal         string
	Model        string
	Tools        []Tool
	MCPServers   []MCPServer
	ResultSchema *ResultSchema
}

type Tool struct {
	Name             string
	Description      string
	InputSchema      json.RawMessage
	GoFunc           any
	Effect           policy.Effect
	IdempotencyKey   string
	SideEffects      []string
	RequiresApproval bool
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
	Kind         TerminalKind
	Async        bool
	ResultSchema *ResultSchema
}

type Plan struct {
	Trigger  Trigger
	Steps    []Step
	Terminal Terminal
}

type ResultSchema struct {
	Name string
	Type reflect.Type
}
