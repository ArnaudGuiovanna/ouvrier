package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

const defaultLogsLast = 20

func (app *App) runLogsCommand(ctx context.Context, args []string) error {
	if hasHelpFlag(args) {
		printLogsHelp(app.out)
		return nil
	}

	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	urlFlag := flags.String("url", defaultAdminURL, "worker base URL")
	token := flags.String("token", "", "admin bearer token (defaults to $PIP_ADMIN_TOKEN)")
	last := flags.Int("last", defaultLogsLast, "number of executions to fetch")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("%w: logs does not accept positional arguments", ErrUsage)
	}
	if *last <= 0 {
		return fmt.Errorf("%w: --last must be positive", ErrUsage)
	}

	client := newAdminClient(*urlFlag, resolveAdminToken(*token))

	query := url.Values{}
	query.Set("last", strconv.Itoa(*last))

	var payload map[string]any
	if err := client.getJSON(ctx, "/admin/traces?"+query.Encode(), &payload); err != nil {
		return err
	}

	printLogsTable(app.out, payload)
	return nil
}

func printLogsTable(w io.Writer, payload map[string]any) {
	traces, _ := payload["traces"].([]any)
	if len(traces) == 0 {
		fmt.Fprintln(w, "no traces")
		return
	}

	fmt.Fprintf(w, "%-36s  %-10s  %12s  %10s  %s\n", "EXEC_ID", "LAST_KIND", "EVENTS", "LATENCY_MS", "ERROR")
	for _, raw := range traces {
		trace, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		execID := stringField(trace, "exec_id")
		if execID == "-" {
			execID = stringField(trace, "trace_key")
		}
		fmt.Fprintf(
			w,
			"%-36s  %-10s  %12s  %10s  %s\n",
			truncate(execID, 36),
			truncate(traceLastKind(trace), 10),
			numberField(trace, "events"),
			numberField(trace, "average_latency_ms"),
			summarizeTraceError(trace),
		)
	}
}

func traceLastKind(trace map[string]any) string {
	kind := stringField(trace, "last_kind")
	// Trim the noisy event family prefix when present (e.g. "pipe.completed" → "completed")
	if dot := strings.LastIndex(kind, "."); dot >= 0 && dot < len(kind)-1 {
		kind = kind[dot+1:]
	}
	return kind
}

func summarizeTraceError(trace map[string]any) string {
	failures := numberField(trace, "llm_failures")
	toolFailures := numberField(trace, "tool_failures")
	violations := numberField(trace, "schema_violations")
	if failures == "0" && toolFailures == "0" && violations == "0" {
		return ""
	}
	parts := make([]string, 0, 3)
	if failures != "0" {
		parts = append(parts, "llm_fail="+failures)
	}
	if toolFailures != "0" {
		parts = append(parts, "tool_fail="+toolFailures)
	}
	if violations != "0" {
		parts = append(parts, "schema_violations="+violations)
	}
	return strings.Join(parts, ",")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
