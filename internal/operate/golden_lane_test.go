package operate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

// This is the narrow integration lane between the coding cockpit and the
// shipped worker runtime: a model can only scaffold/write through governed
// tools, and completion is accepted only after real audit, build, checksum,
// and local runtime evidence for the exact same source tree.
func TestGoldenLaneConstructsAuditsBuildsAndRunsWorker(t *testing.T) {
	parent := t.TempDir()
	target := goruntime.GOOS + "/" + goruntime.GOARCH
	model := &scriptedModel{steps: []provider.Response{
		{
			StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{{
				ID: "scaffold", Name: "scaffold_worker",
				Arguments: json.RawMessage(`{"name":"golden-worker","trigger":"POST /tickets","model":"openai/gpt-5.5"}`),
			}},
		},
		{
			StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{{
				ID: "write", Name: "write_worker_file",
				Arguments: json.RawMessage(`{"path":"worker_evidence.go","content":"package main\n\nconst cockpitGoldenEvidence = \"governed-write\"\n"}`),
			}},
		},
		{
			StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{{
				ID: "audit", Name: "audit_worker", Arguments: json.RawMessage(`{}`),
			}},
		},
		{
			StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{{
				ID: "build", Name: "build_worker",
				Arguments: json.RawMessage(fmt.Sprintf(`{"target":%q}`, target)),
			}},
		},
		{Text: "Worker constructed, audited, built, and ready for local verification.", StopReason: provider.StopEndTurn},
	}}

	rt, err := NewAgentRuntime(RuntimeOptions{
		Dir: parent, Driver: ManualDriver{}, Model: model, ModelID: "test/cockpit", Target: target,
		HeadlessPosture: PostureAutoSafe,
	})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	started, err := rt.Start(context.Background(), RuntimeStartRequest{Dir: parent})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	turn, err := rt.Prompt(context.Background(), started.Session.ID, "build a production-ready ticket worker")
	if err != nil {
		entries, transcriptErr := ReadTranscript(started.Session.TranscriptPath)
		t.Fatalf("Prompt() error = %v\nfinal=%s\ntranscript_error=%v\ntranscript=%+v", err, turn.Final, transcriptErr, entries)
	}
	if model.i != len(model.steps) || !strings.Contains(turn.Final, "audited") {
		t.Fatalf("model steps/final = %d/%q, want evidence-backed completion", model.i, turn.Final)
	}

	workerDir := filepath.Join(parent, "golden-worker")
	if data, err := os.ReadFile(filepath.Join(workerDir, "worker_evidence.go")); err != nil || !bytes.Contains(data, []byte("governed-write")) {
		t.Fatalf("governed source write = %q, %v", data, err)
	}
	var audit AuditReport
	readJSONArtifact(t, started.Session.AuditPath, &audit)
	if !audit.Passed || !isSHA256(audit.SourceSHA256) || !LatestAuditPassedFor(started.Session.AuditPath, workerDir) {
		t.Fatalf("audit evidence = %+v, want current passing source-bound report", audit)
	}
	var build BuildArtifact
	readJSONArtifact(t, started.Session.BuildPath, &build)
	if !build.AuditPassed || build.SourceSHA256 != audit.SourceSHA256 || !isSHA256(build.SHA256) || !isSHA256(build.AuditSHA256) {
		t.Fatalf("build evidence = %+v, want audit/source-bound checksums", build)
	}
	actualSHA, err := fileSHA256(build.BinaryPath)
	if err != nil || actualSHA != build.SHA256 {
		t.Fatalf("binary checksum = %q, %v; artifact records %q", actualSHA, err, build.SHA256)
	}
	current, err := stableCandidateSourceSnapshot(workerDir)
	if err != nil || current.SHA256 != build.SourceSHA256 {
		t.Fatalf("current source = %+v, %v; build source = %s", current, err, build.SourceSHA256)
	}

	if goruntime.GOOS == "linux" && goruntime.GOARCH == "amd64" {
		assertWorkerHealth(t, build.BinaryPath)
	}
}

func readJSONArtifact(t *testing.T, path string, dst any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func assertWorkerHealth(t *testing.T, binary string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local health port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release local health port: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binary)
	const adminToken = "golden-lane-admin-token"
	cmd.Env = append(os.Environ(), "OUVRIER_ADDR="+addr, "OUVRIER_ENV=production", "OUVRIER_ADMIN_TOKEN="+adminToken)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start built worker: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	exited := false
	defer func() {
		cancel()
		if exited {
			return
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	lastResponse := "no HTTP response"
	for time.Now().Before(deadline) {
		req, requestErr := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/admin/health", nil)
		if requestErr == nil {
			req.Header.Set("Authorization", "Bearer "+adminToken)
		}
		resp, requestErr := client.Do(req)
		if requestErr == nil {
			lastResponse = resp.Status
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case processErr := <-done:
			exited = true
			t.Fatalf("built worker exited before health check: %v\n%s", processErr, output.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("built worker did not become healthy at %s (%s)\n%s", addr, lastResponse, output.String())
}
