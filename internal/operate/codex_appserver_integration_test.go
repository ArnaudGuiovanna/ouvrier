package operate

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	codexprovider "github.com/ArnaudGuiovanna/ouvrier/internal/provider/codex"
)

func TestCodexAppServerToolCallsCrossGovernedGoldenLane(t *testing.T) {
	dir := writeWorkerFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module codex-golden-worker\n\ngo 1.25.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	process := &operateAppServerProcess{received: [][]byte{
		[]byte(`{"id":1,"result":{"serverInfo":{"name":"codex","version":"test"}}}`),
		[]byte(`{"id":2,"result":{"thread":{"id":"thread-1"}}}`),
		[]byte(`{"id":3,"result":{"turn":{"id":"turn-1","status":"inProgress"}}}`),
		[]byte(`{"method":"item/tool/call","id":"write-rpc","params":{"arguments":{"path":"generated.go","content":"package main\n\nconst generatedThroughCodex = true\n"},"callId":"write-call","threadId":"thread-1","tool":"write_worker_file","turnId":"turn-1"}}`),
		[]byte(`{"method":"item/tool/call","id":"audit-rpc","params":{"arguments":{},"callId":"audit-call","threadId":"thread-1","tool":"audit_worker","turnId":"turn-1"}}`),
		[]byte(`{"method":"item/tool/call","id":"build-rpc","params":{"arguments":{},"callId":"build-call","threadId":"thread-1","tool":"build_worker","turnId":"turn-1"}}`),
		[]byte(`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"id":"message-1","type":"agentMessage","text":"Structured worker evidence complete."}}}`),
		[]byte(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}}`),
	}}
	appServer := codexprovider.NewAppServer("", dir)
	appServer.Transport = operateAppServerTransport{process: process}
	model := NewProviderModel(appServer, "codex")
	runtime, err := NewAgentRuntime(RuntimeOptions{
		Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "codex",
		HeadlessPosture: PostureAutoSafe,
	})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	turn, err := runtime.Prompt(context.Background(), started.Session.ID, "construct and verify this worker")
	if err != nil {
		entries, transcriptErr := ReadTranscript(started.Session.TranscriptPath)
		t.Fatalf("Prompt() error = %v\nfinal=%s\ntranscript_error=%v\ntranscript=%+v\napp_server_sent=%s",
			err, turn.Final, transcriptErr, entries, joinRawJSON(process.Sent()))
	}
	if turn.Final != "Structured worker evidence complete." {
		t.Fatalf("turn.Final = %q", turn.Final)
	}
	entries, err := ReadTranscript(started.Session.TranscriptPath)
	if err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}
	for _, tool := range []string{"write_worker_file", "audit_worker", "build_worker"} {
		if !transcriptHasTool(entries, tool) {
			t.Fatalf("governed transcript lacks %s: %+v", tool, entries)
		}
	}
	durableGate := completionGateFromTranscript(entries, started.Session)
	if !durableGate.complete() {
		t.Fatalf("durable completion evidence is incomplete after the app-server turn: missing=%v gate=%+v", durableGate.missingEvidence(), durableGate)
	}
	for _, entry := range entries {
		if entry.Kind == TranscriptStatus && metaString(entry.Metadata, "completion_gate") == "incomplete" {
			t.Fatalf("completion gate requested another model turn after current governed evidence: %+v", entry)
		}
	}
	if !LatestAuditPassedFor(started.Session.AuditPath, dir) {
		t.Fatal("Codex-driven audit evidence is not current and passing")
	}
	var artifact BuildArtifact
	readJSONArtifact(t, started.Session.BuildPath, &artifact)
	if !artifact.AuditPassed || !isSHA256(artifact.SHA256) {
		t.Fatalf("Codex-driven build artifact = %+v", artifact)
	}

	responses := process.Sent()
	for _, method := range []string{"thread/start", "turn/start"} {
		if got := countAppServerMethod(responses, method); got != 1 {
			t.Fatalf("app-server method %q count = %d, want one governed model turn; sent=%s", method, got, joinRawJSON(responses))
		}
	}
	for _, id := range []string{"write-rpc", "audit-rpc", "build-rpc"} {
		if !successfulAppServerToolResponse(responses, id) {
			t.Fatalf("missing successful dynamic-tool response %q in %s", id, joinRawJSON(responses))
		}
	}
}

func countAppServerMethod(messages [][]byte, want string) int {
	count := 0
	for _, message := range messages {
		var envelope struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(message, &envelope) == nil && envelope.Method == want {
			count++
		}
	}
	return count
}

type operateAppServerTransport struct {
	process *operateAppServerProcess
}

func (t operateAppServerTransport) LookPath(string) (string, error) { return "codex-test", nil }
func (t operateAppServerTransport) Start(string, ...string) (codexprovider.AppServerProcess, error) {
	return t.process, nil
}

type operateAppServerProcess struct {
	mu       sync.Mutex
	received [][]byte
	sent     [][]byte
	closed   bool
}

func (p *operateAppServerProcess) Send(_ context.Context, message []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return io.ErrClosedPipe
	}
	p.sent = append(p.sent, append([]byte(nil), message...))
	return nil
}

func (p *operateAppServerProcess) Receive(ctx context.Context) ([]byte, error) {
	p.mu.Lock()
	if len(p.received) > 0 {
		message := append([]byte(nil), p.received[0]...)
		p.received = p.received[1:]
		p.mu.Unlock()
		return message, nil
	}
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, io.EOF
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, io.EOF
	}
}

func (*operateAppServerProcess) Stderr() string { return "" }

func (p *operateAppServerProcess) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

func (p *operateAppServerProcess) Sent() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, len(p.sent))
	for i := range p.sent {
		out[i] = append([]byte(nil), p.sent[i]...)
	}
	return out
}

func successfulAppServerToolResponse(messages [][]byte, wantID string) bool {
	for _, message := range messages {
		var envelope struct {
			ID     string `json:"id"`
			Result struct {
				Success bool `json:"success"`
			} `json:"result"`
		}
		if json.Unmarshal(message, &envelope) == nil && envelope.ID == wantID && envelope.Result.Success {
			return true
		}
	}
	return false
}

func joinRawJSON(messages [][]byte) string {
	data, _ := json.Marshal(messages)
	return string(data)
}

var _ provider.Provider = (*codexprovider.AppServerProvider)(nil)
