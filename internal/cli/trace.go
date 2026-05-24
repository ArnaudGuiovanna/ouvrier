package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
)

func (app *App) runTraceCommand(ctx context.Context, args []string) error {
	if hasHelpFlag(args) {
		printTraceHelp(app.out)
		return nil
	}

	flags := flag.NewFlagSet("trace", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	urlFlag := flags.String("url", defaultAdminURL, "worker base URL")
	token := flags.String("token", "", "admin bearer token (defaults to $PIP_ADMIN_TOKEN)")
	// Move flag tokens ahead of any positional <exec-id> so the std flag
	// package parses them. Without this, `trace exec-42 --url ...` would
	// stop at the positional and ignore --url.
	if err := flags.Parse(reorderFlagsFirst(args)); err != nil {
		return fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("%w: trace requires exactly one <exec-id> argument", ErrUsage)
	}

	execID := strings.TrimSpace(flags.Arg(0))
	if execID == "" {
		return fmt.Errorf("%w: <exec-id> cannot be empty", ErrUsage)
	}

	client := newAdminClient(*urlFlag, resolveAdminToken(*token))

	var payload map[string]any
	endpoint := "/admin/traces/" + url.PathEscape(execID)
	if err := client.getJSON(ctx, endpoint, &payload); err != nil {
		return err
	}

	printTraceDetail(app.out, execID, payload)
	return nil
}

func printTraceDetail(w io.Writer, execID string, payload map[string]any) {
	fmt.Fprintf(w, "exec_id:           %s\n", execID)

	if exec, ok := payload["execution"].(map[string]any); ok && exec != nil {
		fmt.Fprintf(w, "status:            %s\n", stringField(exec, "status"))
		fmt.Fprintf(w, "started_at:        %s\n", stringField(exec, "started_at"))
		fmt.Fprintf(w, "completed_at:      %s\n", stringField(exec, "completed_at"))
		if tid := stringField(exec, "trace_id"); tid != "-" {
			fmt.Fprintf(w, "trace_id:          %s\n", tid)
		}
	}
	fmt.Fprintf(w, "sessions:          %s\n", numberField(payload, "sessions"))
	fmt.Fprintf(w, "schema_violations: %s\n", numberField(payload, "schema_violations"))
	fmt.Fprintf(w, "last_event_id:     %s\n", numberField(payload, "last_event_id"))

	events, _ := payload["events"].([]any)
	fmt.Fprintf(w, "events:            %d\n", len(events))
	if len(events) == 0 {
		return
	}

	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "timeline:")
	fmt.Fprintf(w, "  %-10s  %-24s  %-32s  %s\n", "ID", "AT", "KIND", "DETAIL")
	for _, raw := range events {
		event, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fmt.Fprintf(
			w,
			"  %-10s  %-24s  %-32s  %s\n",
			numberField(event, "id"),
			truncate(stringField(event, "at"), 24),
			truncate(stringField(event, "kind"), 32),
			summarizeEventPayload(event),
		)
	}

	if violations, ok := payload["schema_violation_details"].([]any); ok && len(violations) > 0 {
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "schema_violations:")
		for _, raw := range violations {
			v, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			fmt.Fprintf(w, "  %s  %s: %s\n", stringField(v, "at"), stringField(v, "schema_name"), stringField(v, "error"))
		}
	}
}

func summarizeEventPayload(event map[string]any) string {
	payload, ok := event["payload"].(map[string]any)
	if !ok || len(payload) == 0 {
		return ""
	}
	// Show a stable, deduplicated set of keys with short values.
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := payload[k]
		formatted := formatPayloadValue(v)
		if formatted == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, formatted))
		if len(parts) >= 4 {
			break
		}
	}
	return strings.Join(parts, " ")
}

func formatPayloadValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return truncate(strings.ReplaceAll(t, "\n", " "), 32)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case map[string]any, []any:
		// Don't recurse into nested structures here; the trace command is a
		// summary view, not a JSON dump.
		return "{...}"
	default:
		return truncate(formatJSONNumber(t), 24)
	}
}
