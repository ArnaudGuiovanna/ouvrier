package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
)

func (app *App) runStatusCommand(ctx context.Context, args []string) error {
	if hasHelpFlag(args) {
		printStatusHelp(app.out)
		return nil
	}

	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	url := flags.String("url", defaultAdminURL, "worker base URL")
	token := flags.String("token", "", "admin bearer token (defaults to $PIP_ADMIN_TOKEN)")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("%w: status does not accept positional arguments", ErrUsage)
	}

	client := newAdminClient(*url, resolveAdminToken(*token))

	var payload map[string]any
	if err := client.getJSON(ctx, "/admin/status", &payload); err != nil {
		return err
	}

	printStatusSummary(app.out, payload)
	return nil
}

func resolveAdminToken(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv("PIP_ADMIN_TOKEN")
}

func printStatusSummary(w io.Writer, payload map[string]any) {
	fmt.Fprintf(w, "status:            %s\n", stringField(payload, "status"))
	fmt.Fprintf(w, "sessions:          %s\n", numberField(payload, "sessions"))
	fmt.Fprintf(w, "executions:        %s\n", numberField(payload, "executions"))
	fmt.Fprintf(w, "events:            %s\n", numberField(payload, "events"))

	if byStatus, ok := payload["by_status"].(map[string]any); ok && len(byStatus) > 0 {
		keys := make([]string, 0, len(byStatus))
		for k := range byStatus {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%s", k, formatJSONNumber(byStatus[k])))
		}
		fmt.Fprintf(w, "by_status:         %s\n", joinKV(parts))
	} else {
		fmt.Fprintln(w, "by_status:         -")
	}

	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "schema:")
	fmt.Fprintf(w, "  validation_passed: %s\n", numberField(payload, "schema_validation_passed"))
	fmt.Fprintf(w, "  validation_failed: %s\n", numberField(payload, "schema_validation_failed"))
	fmt.Fprintf(w, "  violations:        %s\n", numberField(payload, "schema_violations"))
	fmt.Fprintf(w, "  repairs_started:   %s\n", numberField(payload, "schema_repairs_started"))
	fmt.Fprintf(w, "  repairs_completed: %s\n", numberField(payload, "schema_repairs_completed"))
	fmt.Fprintf(w, "  repairs_failed:    %s\n", numberField(payload, "schema_repairs_failed"))

	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "llm:")
	fmt.Fprintf(w, "  calls:             %s\n", numberField(payload, "llm_calls"))
	fmt.Fprintf(w, "  failures:          %s\n", numberField(payload, "llm_failures"))
	fmt.Fprintf(w, "  retries:           %s\n", numberField(payload, "llm_retries"))
	fmt.Fprintf(w, "  final_failures:    %s\n", numberField(payload, "llm_final_failures"))
	fmt.Fprintf(w, "  input_tokens:      %s\n", numberField(payload, "input_tokens"))
	fmt.Fprintf(w, "  output_tokens:     %s\n", numberField(payload, "output_tokens"))
	fmt.Fprintf(w, "  cost_usd:          %s\n", numberField(payload, "cost_usd"))
	fmt.Fprintf(w, "  avg_latency_ms:    %s\n", numberField(payload, "average_latency_ms"))

	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "budgets:")
	fmt.Fprintf(w, "  exceeded:          %s\n", numberField(payload, "budget_exceeded"))
	fmt.Fprintf(w, "  exceeded_tokens:   %s\n", numberField(payload, "budget_exceeded_tokens"))
	fmt.Fprintf(w, "  exceeded_cost_usd: %s\n", numberField(payload, "budget_exceeded_cost_usd"))
	fmt.Fprintf(w, "  exceeded_wallclock: %s\n", numberField(payload, "budget_exceeded_wallclock"))
	fmt.Fprintf(w, "  exceeded_iter:     %s\n", numberField(payload, "budget_exceeded_iterations"))

	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "tools:")
	fmt.Fprintf(w, "  calls:             %s\n", numberField(payload, "tool_calls"))
	fmt.Fprintf(w, "  completed:         %s\n", numberField(payload, "tool_calls_completed"))
	fmt.Fprintf(w, "  failures:          %s\n", numberField(payload, "tool_failures"))
	fmt.Fprintf(w, "  perm_allowed:      %s\n", numberField(payload, "permission_allowed"))
	fmt.Fprintf(w, "  perm_denied:       %s\n", numberField(payload, "permission_denied"))
}

func stringField(payload map[string]any, key string) string {
	if v, ok := payload[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return "-"
}

func numberField(payload map[string]any, key string) string {
	v, ok := payload[key]
	if !ok || v == nil {
		return "0"
	}
	return formatJSONNumber(v)
}

func formatJSONNumber(v any) string {
	switch n := v.(type) {
	case json.Number:
		return n.String()
	case float64:
		return trimFloat(n)
	case int:
		return fmt.Sprintf("%d", n)
	case int64:
		return fmt.Sprintf("%d", n)
	case string:
		return n
	default:
		return fmt.Sprintf("%v", v)
	}
}

func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}

func joinKV(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
