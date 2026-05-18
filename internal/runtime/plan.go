package runtime

import (
	"encoding/json"
	"reflect"
)

type TriggerKind string

const (
	TriggerHTTP    TriggerKind = "http"
	TriggerCron    TriggerKind = "cron"
	TriggerWebhook TriggerKind = "webhook"
	TriggerStream  TriggerKind = "stream"
)

type Trigger struct {
	Kind   TriggerKind
	Method string
	Path   string
	Expr   string
	Value  string
	URI    string
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
	ResultSchema *ResultSchema
}

type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	GoFunc      any
}

type TerminalKind string

const (
	TerminalReply TerminalKind = "reply"
	TerminalPush  TerminalKind = "push"
	TerminalSink  TerminalKind = "sink"
)

type Terminal struct {
	Kind         TerminalKind
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
