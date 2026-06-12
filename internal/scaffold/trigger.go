package scaffold

import (
	"fmt"
	"strconv"
	"strings"

	ovr "github.com/ArnaudGuiovanna/ouvrier"
)

// triggerKind classifies the trigger category a scaffold should generate.
type triggerKind int

const (
	triggerHTTP triggerKind = iota
	triggerCron
	triggerWebhook
	triggerStream
)

// triggerSpec is the parsed result of a --trigger string. It carries enough
// information to render a compiling main.go for every supported trigger
// category while keeping the existing HTTP behaviour unchanged.
type triggerSpec struct {
	kind triggerKind
	// display is the canonical human-readable trigger string echoed back in
	// the README and pip.yaml comments (e.g. "POST /tickets",
	// "cron 0 6 * * *", "webhook github", "stream kafka://tickets").
	display string
	// fromArg is the Go expression handed to ovr.From(...) in the generated
	// main.go. For HTTP triggers this is a quoted route literal; for the other
	// categories it is an ovr.Cron/ovr.Webhook/ovr.Stream constructor call.
	fromArg string
	// terminalExpr is the terminal pipeline node expression (a Reply or Sink).
	terminalExpr string
	// usesReplyType reports whether the generated main.go needs the
	// ticketReply helper type (only the JSON Reply terminal uses it).
	usesReplyType bool
}

// TriggerRender is the validated rendering surface for a supported trigger.
// FromArg is the expression passed to ovr.From(...). TerminalExpr is the
// terminal node expression paired with that trigger.
type TriggerRender struct {
	Display       string
	FromArg       string
	TerminalExpr  string
	UsesReplyType bool
}

// RenderTrigger validates a supported trigger string and returns the Go
// snippets needed to add it to an Ouvrier worker.
func RenderTrigger(trigger string) (TriggerRender, error) {
	spec, err := parseScaffoldTrigger(trigger)
	if err != nil {
		return TriggerRender{}, err
	}
	if err := validateScaffoldTrigger(spec); err != nil {
		return TriggerRender{}, fmt.Errorf("%w: trigger %q is not supported: %w", ErrInvalidConfig, spec.display, err)
	}
	return TriggerRender{
		Display:       spec.display,
		FromArg:       spec.fromArg,
		TerminalExpr:  spec.terminalExpr,
		UsesReplyType: spec.usesReplyType,
	}, nil
}

// parseScaffoldTrigger normalizes and classifies a --trigger string. It accepts
// HTTP routes ("POST /tickets"), cron expressions (bare "0 6 * * *" or prefixed
// "cron 0 6 * * *"), webhook providers ("webhook github"), and stream URIs
// ("stream kafka://tickets").
func parseScaffoldTrigger(trigger string) (triggerSpec, error) {
	trigger = strings.TrimSpace(trigger)
	if trigger == "" {
		return triggerSpec{}, fmt.Errorf("%w: trigger is required", ErrInvalidConfig)
	}

	fields := strings.Fields(trigger)
	keyword := strings.ToLower(fields[0])

	switch keyword {
	case "cron":
		return parseCronScaffoldTrigger(strings.TrimSpace(trigger[len(fields[0]):]))
	case "webhook":
		return parseWebhookScaffoldTrigger(strings.TrimSpace(trigger[len(fields[0]):]))
	case "stream":
		return parseStreamScaffoldTrigger(strings.TrimSpace(trigger[len(fields[0]):]))
	case "get", "post":
		return parseHTTPScaffoldTrigger(trigger)
	}

	// No keyword: disambiguate bare forms. A leading scheme like "kafka://"
	// is a stream; otherwise treat it as a bare cron expression.
	if isStreamURI(trigger) {
		return parseStreamScaffoldTrigger(trigger)
	}
	if looksLikeCron(trigger) {
		return parseCronScaffoldTrigger(trigger)
	}

	return triggerSpec{}, fmt.Errorf("%w: --trigger accepts HTTP routes (\"POST /tickets\"), cron (\"0 6 * * *\" or \"cron 0 6 * * *\"), webhook (\"webhook github\"), or stream (\"stream kafka://tickets\"); got %q", ErrInvalidConfig, trigger)
}

func parseHTTPScaffoldTrigger(trigger string) (triggerSpec, error) {
	route, err := normalizeHTTPScaffoldTrigger(trigger)
	if err != nil {
		return triggerSpec{}, err
	}
	return triggerSpec{
		kind:          triggerHTTP,
		display:       route,
		fromArg:       strconv.Quote(route),
		terminalExpr:  "ovr.Reply(ovr.JSON[ticketReply]())",
		usesReplyType: true,
	}, nil
}

func parseCronScaffoldTrigger(expr string) (triggerSpec, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return triggerSpec{}, fmt.Errorf("%w: --trigger cron requires an expression, e.g. \"cron 0 6 * * *\"", ErrInvalidConfig)
	}
	return triggerSpec{
		kind:         triggerCron,
		display:      "cron " + expr,
		fromArg:      "ovr.Cron(" + strconv.Quote(expr) + ")",
		terminalExpr: "ovr.Sink(ovr.Log())",
	}, nil
}

func parseWebhookScaffoldTrigger(provider string) (triggerSpec, error) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return triggerSpec{}, fmt.Errorf("%w: --trigger webhook requires a provider, e.g. \"webhook github\"", ErrInvalidConfig)
	}
	if len(strings.Fields(provider)) != 1 {
		return triggerSpec{}, fmt.Errorf("%w: --trigger webhook provider must be a single token, e.g. \"webhook github\"", ErrInvalidConfig)
	}
	return triggerSpec{
		kind:         triggerWebhook,
		display:      "webhook " + provider,
		fromArg:      "ovr.Webhook(" + strconv.Quote(provider) + ")",
		terminalExpr: "ovr.Sink(ovr.Log())",
	}, nil
}

func parseStreamScaffoldTrigger(uri string) (triggerSpec, error) {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return triggerSpec{}, fmt.Errorf("%w: --trigger stream requires a URI, e.g. \"stream kafka://tickets\"", ErrInvalidConfig)
	}
	if len(strings.Fields(uri)) != 1 {
		return triggerSpec{}, fmt.Errorf("%w: --trigger stream URI must be a single token, e.g. \"stream kafka://tickets\"", ErrInvalidConfig)
	}
	return triggerSpec{
		kind:         triggerStream,
		display:      "stream " + uri,
		fromArg:      "ovr.Stream(" + strconv.Quote(uri) + ")",
		terminalExpr: "ovr.Sink(ovr.Log())",
	}, nil
}

// isStreamURI reports whether s carries a supported stream scheme prefix.
func isStreamURI(s string) bool {
	for _, scheme := range []string{"kafka://", "nats://", "redis://"} {
		if strings.HasPrefix(strings.ToLower(s), scheme) {
			return true
		}
	}
	return false
}

// looksLikeCron reports whether s plausibly resembles a cron expression so the
// bare (keyword-less) form can be disambiguated from typos. Strict validation
// is deferred to ovr.Validate.
func looksLikeCron(s string) bool {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "@") {
		return true
	}
	fields := strings.Fields(s)
	return len(fields) == 5 || len(fields) == 6
}

// validateScaffoldTrigger runs the parsed trigger through the real ovr
// validation so the scaffold never emits a worker the runtime would reject.
func validateScaffoldTrigger(spec triggerSpec) error {
	var source any
	switch spec.kind {
	case triggerHTTP:
		source = spec.display
	case triggerCron:
		source = ovr.Cron(strings.TrimSpace(strings.TrimPrefix(spec.display, "cron")))
	case triggerWebhook:
		source = ovr.Webhook(strings.TrimSpace(strings.TrimPrefix(spec.display, "webhook")))
	case triggerStream:
		source = ovr.Stream(strings.TrimSpace(strings.TrimPrefix(spec.display, "stream")))
	}

	var terminal ovr.Node
	switch spec.kind {
	case triggerHTTP:
		terminal = ovr.Reply(ovr.JSON[map[string]any]())
	default:
		terminal = ovr.Sink(ovr.Log())
	}

	return ovr.Validate(
		ovr.From(source),
		ovr.Pipe("validate scaffold trigger", ovr.Model("anthropic/claude-sonnet-4-6")),
		terminal,
	)
}
