package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const samplePipYAML = `name: demo
version: 0.1.0

deploy:
  ssh:
    host: ops@example.com
    path: /opt/demo
    service: demo.service
  docker:
    image: demo:0.1.0

env:
  required:
    - ANTHROPIC_API_KEY
    - MOODLE_BASE_URL

healthcheck:
  path: /admin/health
  interval: 30s
`

const sampleMainGo = `package main

import (
	ovr "github.com/ArnaudGuiovanna/ouvrier"
)

func main() {
	ovr.Run(":8080",
		ovr.From("POST /tickets"),
		ovr.Pipe("triage",
			ovr.Model("anthropic/claude-sonnet-4-6"),
		),
	)
}
`

func writeProjectFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte(samplePipYAML), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(sampleMainGo), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	for _, sub := range []string{"tools", "skills"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "tools", "load_ticket.go"), []byte("package tools\n"), 0o644); err != nil {
		t.Fatalf("write tool: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills", "moodle-fsrs"), 0o755); err != nil {
		t.Fatalf("mkdir skill: %v", err)
	}
}

func TestRunShowSummarizesPipYAML(t *testing.T) {
	dir := t.TempDir()
	writeProjectFixture(t, dir)

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"show", "--dir", dir}); err != nil {
		t.Fatalf("Run(show) error = %v", err)
	}
	if got := errOut.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}

	stdout := out.String()
	for _, want := range []string{
		"name:     demo",
		"version:  0.1.0",
		"trigger:  POST /tickets",
		"model:    anthropic/claude-sonnet-4-6",
		"deploy:   ssh, docker",
		"ANTHROPIC_API_KEY",
		"MOODLE_BASE_URL",
		"health:   /admin/health",
		"tools:    load_ticket.go",
		"skills:   moodle-fsrs",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("show output missing %q in:\n%s", want, stdout)
		}
	}
}

func TestRunShowJSONPrintsMachineReadableSummary(t *testing.T) {
	dir := t.TempDir()
	writeProjectFixture(t, dir)
	manifest := `{
  "name": "demo",
  "description": "Demo worker",
  "events": ["pi.agent_end"],
  "outcomes": ["feedback"],
  "admin_url": "http://127.0.0.1:8080"
}`
	if err := os.WriteFile(filepath.Join(dir, workerManifestFilename), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"show", "--dir", dir, "--json"}); err != nil {
		t.Fatalf("Run(show --json) error = %v", err)
	}
	if got := errOut.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}

	var body showJSONSummary
	if err := json.Unmarshal(out.Bytes(), &body); err != nil {
		t.Fatalf("show --json output is not JSON: %v\n%s", err, out.String())
	}
	if body.Name != "demo" || body.Trigger != "POST /tickets" || body.Model != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("summary = %+v, want demo trigger/model", body)
	}
	if len(body.Tools) != 1 || body.Tools[0] != "load_ticket.go" {
		t.Fatalf("tools = %v, want load_ticket.go", body.Tools)
	}
	if body.Manifest == nil || body.Manifest.Name != "demo" || body.Manifest.Events[0] != "pi.agent_end" {
		t.Fatalf("manifest = %+v, want parsed worker manifest", body.Manifest)
	}
}

func TestRunShowReportsMissingPipYAML(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))

	err := app.Run(context.Background(), []string{"show", "--dir", dir})
	if !errors.Is(err, ErrPipYAMLMissing) {
		t.Fatalf("Run(show) error = %v, want ErrPipYAMLMissing", err)
	}
	if got := out.String(); got != "" {
		t.Fatalf("stdout = %q, want empty", got)
	}
}

func TestDetectMainTriggerSupportsStructuredTriggers(t *testing.T) {
	cases := []struct {
		name string
		main string
		want string
	}{
		{
			name: "cron",
			main: `package main

import ovr "github.com/ArnaudGuiovanna/ouvrier"

func main() {
	ovr.Run(":8080", ovr.From(ovr.Cron("@every 1h")))
}
`,
			want: "cron @every 1h",
		},
		{
			name: "webhook",
			main: `package main

import ovr "github.com/ArnaudGuiovanna/ouvrier"

func main() {
	ovr.Run(":8080", ovr.From(ovr.Webhook("github")))
}
`,
			want: "webhook github",
		},
		{
			name: "stream",
			main: `package main

import ovr "github.com/ArnaudGuiovanna/ouvrier"

func main() {
	ovr.Run(":8080", ovr.From(ovr.Stream("kafka://tickets")))
}
`,
			want: "stream kafka://tickets",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(tc.main), 0o644); err != nil {
				t.Fatalf("write main.go: %v", err)
			}
			if got := detectMainTrigger(dir); got != tc.want {
				t.Fatalf("detectMainTrigger() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunShowHelpPrintsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"show", "--help"}); err != nil {
		t.Fatalf("Run(show --help) error = %v", err)
	}
	if !strings.Contains(out.String(), "Usage: ouvrier show") {
		t.Fatalf("show --help missing usage; got:\n%s", out.String())
	}
}

func TestParsePipYAMLHandlesQuotedAndComments(t *testing.T) {
	doc := `# comment
name: "quoted"
version: '0.2.0'
deploy:
  ssh: # nested target
    host: a@b
  docker:
    image: foo
env:
  required:
    - "ONE"
    - TWO
healthcheck:
  path: /admin/health
`
	got := parsePipYAML(doc)
	if got.Name != "quoted" {
		t.Fatalf("Name = %q, want quoted", got.Name)
	}
	if got.Version != "0.2.0" {
		t.Fatalf("Version = %q, want 0.2.0", got.Version)
	}
	if len(got.Deploy) != 2 || got.Deploy[0] != "ssh" || got.Deploy[1] != "docker" {
		t.Fatalf("Deploy = %v, want [ssh docker]", got.Deploy)
	}
	if len(got.EnvReq) != 2 || got.EnvReq[0] != "ONE" || got.EnvReq[1] != "TWO" {
		t.Fatalf("EnvReq = %v, want [ONE TWO]", got.EnvReq)
	}
	if got.Health != "/admin/health" {
		t.Fatalf("Health = %q, want /admin/health", got.Health)
	}
}
