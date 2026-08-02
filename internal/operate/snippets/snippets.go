// Package snippets is the embedded Ouvrier API + snippet pack: the single source
// of truth shared by the agent (read_ouvrier_api / search_ouvrier_docs) and the
// IDE (snippet palette + API panel). No TUI or LSP imports.
package snippets

import (
	"embed"
	"sort"
	"strings"
)

//go:embed pack/*.md
var packFS embed.FS

// Snippet is one Ouvrier API primitive snippet.
type Snippet struct {
	Prefix string // completion trigger, e.g. "ovr-tool"
	Title  string // short label
	Group  string // trigger | pipeline | governance | reply
	Body   string // Go code; ${1:...} are tab stops
	Doc    string // one or two lines explaining it
}

// snippetData is the canonical snippet set, grounded in the real ovr package API.
// From() accepts a string ("METHOD /path") or typed sources (ovr.Cron, ovr.Webhook, ovr.Stream).
// SubAgent takes a name and a PipelineSpec (from ovr.Pipeline(...)).
var snippetData = []Snippet{
	// ── trigger ───────────────────────────────────────────────────────────────
	{
		Prefix: "ovr-run",
		Title:  "ovr.Run",
		Group:  "trigger",
		Body:   "ovr.Run(\"${1::8080}\",\n\t${2:nodes},\n)",
		Doc:    "Start an HTTP/cron/webhook/stream worker.",
	},
	{
		Prefix: "ovr-http",
		Title:  "ovr.From HTTP",
		Group:  "trigger",
		Body:   "ovr.From(\"${1:POST} ${2:/path}\")",
		Doc:    "HTTP trigger.",
	},
	{
		Prefix: "ovr-cron",
		Title:  "ovr.From Cron",
		Group:  "trigger",
		Body:   "ovr.From(ovr.Cron(\"${1:0 6 * * *}\"))",
		Doc:    "Cron schedule trigger.",
	},
	{
		Prefix: "ovr-webhook",
		Title:  "ovr.From Webhook",
		Group:  "trigger",
		Body:   "ovr.From(ovr.Webhook(\"${1:github}\"))",
		Doc:    "Inbound webhook trigger.",
	},
	{
		Prefix: "ovr-stream",
		Title:  "ovr.From Stream",
		Group:  "trigger",
		Body:   "ovr.From(ovr.Stream(\"${1:kafka://broker/topic}\"))",
		Doc:    "Stream trigger (kafka/redis/nats).",
	},

	// ── pipeline ──────────────────────────────────────────────────────────────
	{
		Prefix: "ovr-pipe",
		Title:  "ovr.Pipe",
		Group:  "pipeline",
		Body:   "ovr.Pipe(\"${1:goal}\",\n\tovr.Model(\"${2:anthropic/claude-sonnet-4-6}\"),\n\tovr.Output[${3:Result}](),\n)",
		Doc:    "An LLM agent step with a goal, model, and typed output.",
	},
	{
		Prefix: "ovr-model",
		Title:  "ovr.Model",
		Group:  "pipeline",
		Body:   "ovr.Model(\"${1:anthropic/claude-sonnet-4-6}\")",
		Doc:    "Select the model for a pipe.",
	},
	{
		Prefix: "ovr-output",
		Title:  "ovr.Output",
		Group:  "pipeline",
		Body:   "ovr.Output[${1:Result}]()",
		Doc:    "Declare the typed structured result of a pipe.",
	},
	{
		Prefix: "ovr-subagent",
		Title:  "ovr.SubAgent",
		Group:  "pipeline",
		// SubAgent(name string, pipeline PipelineSpec, options ...SubAgentOption)
		Body: "ovr.SubAgent(\"${1:name}\", ovr.Pipeline(${2:pipe}))",
		Doc:  "A governed child agent.",
	},

	// ── governance ────────────────────────────────────────────────────────────
	{
		Prefix: "ovr-tool",
		Title:  "ovr.Tool",
		Group:  "governance",
		Body:   "ovr.Tool(\"${1:name}\", ${2:fn},\n\tovr.ReadOnly(),\n\tovr.Describe(\"${3:what it does}\"),\n)",
		Doc:    "Expose a Go function to the agent through the governed executor.",
	},
	{
		Prefix: "ovr-readonly",
		Title:  "ovr.ReadOnly",
		Group:  "governance",
		Body:   "ovr.ReadOnly()",
		Doc:    "Mark a tool as a pure read (no side effects).",
	},
	{
		Prefix: "ovr-sideeffect",
		Title:  "ovr.SideEffecting",
		Group:  "governance",
		// SideEffecting takes variadic labels; at least one is required.
		Body: "ovr.SideEffecting(\"${1:label}\")",
		Doc:  "Mark a tool that mutates the world.",
	},
	{
		Prefix: "ovr-idempotent",
		Title:  "ovr.Idempotent",
		Group:  "governance",
		Body:   "ovr.Idempotent(\"${1:request.id}\")",
		Doc:    "Mark a tool safe to retry using a dot-separated JSON argument path.",
	},
	{
		Prefix: "ovr-approval",
		Title:  "ovr.RequiresApproval",
		Group:  "governance",
		Body:   "ovr.RequiresApproval()",
		Doc:    "Require human approval before the tool runs.",
	},
	{
		Prefix: "ovr-describe",
		Title:  "ovr.Describe",
		Group:  "governance",
		Body:   "ovr.Describe(\"${1:what it does}\")",
		Doc:    "Document a tool for the model.",
	},

	// ── reply ─────────────────────────────────────────────────────────────────
	{
		Prefix: "ovr-reply-json",
		Title:  "ovr.Reply JSON",
		Group:  "reply",
		Body:   "ovr.Reply(ovr.JSON[${1:Result}]())",
		Doc:    "Return a typed JSON HTTP response.",
	},
	{
		Prefix: "ovr-reply-accepted",
		Title:  "ovr.Reply Accepted",
		Group:  "reply",
		Body:   "ovr.Reply(ovr.Accepted())",
		Doc:    "Return 202 Accepted for async work.",
	},
	{
		Prefix: "ovr-sink",
		Title:  "ovr.Sink",
		Group:  "reply",
		Body:   "ovr.Sink(ovr.Log())",
		Doc:    "Terminate a non-HTTP worker by logging the outcome.",
	},
	{
		Prefix: "ovr-push",
		Title:  "ovr.Push",
		Group:  "reply",
		// Push takes a PushTarget; Webhook and Queue are the concrete types.
		Body: "ovr.Push(${1:ovr.Webhook(\"https://example.com/results\")})",
		Doc:  "Push the outcome to a webhook/queue.",
	},
}

// groupOrder defines stable sort priority for groups.
var groupOrder = map[string]int{
	"trigger":    0,
	"pipeline":   1,
	"governance": 2,
	"reply":      3,
}

func loadAll() []Snippet {
	out := make([]Snippet, len(snippetData))
	copy(out, snippetData)
	sort.SliceStable(out, func(i, j int) bool {
		gi := groupOrder[out[i].Group]
		gj := groupOrder[out[j].Group]
		if gi != gj {
			return gi < gj
		}
		return out[i].Prefix < out[j].Prefix
	})
	return out
}

// All returns every snippet in stable order (by group then prefix).
func All() []Snippet { return loadAll() }

// Search returns snippets whose prefix, title, or doc contains q (case-insensitive).
// An empty query returns all snippets.
func Search(q string) []Snippet {
	if q == "" {
		return All()
	}
	q = strings.ToLower(q)
	var out []Snippet
	for _, s := range loadAll() {
		if strings.Contains(strings.ToLower(s.Prefix), q) ||
			strings.Contains(strings.ToLower(s.Title), q) ||
			strings.Contains(strings.ToLower(s.Doc), q) {
			out = append(out, s)
		}
	}
	return out
}

// Primitives returns a compact human reference (one line per primitive) extracted
// from the "## Primitives" section of the embedded ouvrier-core.md.
func Primitives() []string {
	data, err := packFS.ReadFile("pack/ouvrier-core.md")
	if err != nil {
		return nil
	}
	var out []string
	inSection := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "## Primitives" {
			inSection = true
			continue
		}
		if inSection {
			if strings.HasPrefix(strings.TrimSpace(line), "## ") {
				break
			}
			if strings.HasPrefix(strings.TrimSpace(line), "- ") {
				out = append(out, strings.TrimSpace(line))
			}
		}
	}
	return out
}

// DocMatch is one grep-style result from the embedded markdown docs.
type DocMatch struct {
	Source string
	Text   string
	Line   int
}

// SearchDocs greps all lines of the embedded pack markdown for q (case-insensitive).
func SearchDocs(q string) []DocMatch {
	if q == "" {
		return nil
	}
	q = strings.ToLower(q)
	entries, err := packFS.ReadDir("pack")
	if err != nil {
		return nil
	}
	var out []DocMatch
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := packFS.ReadFile("pack/" + entry.Name())
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(strings.ToLower(line), q) {
				out = append(out, DocMatch{
					Source: entry.Name(),
					Text:   strings.TrimSpace(line),
					Line:   i + 1,
				})
			}
		}
	}
	return out
}
