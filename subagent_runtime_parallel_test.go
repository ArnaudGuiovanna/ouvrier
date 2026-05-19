package ovr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"ouvrier/internal/provider"
)

const (
	parallelSubAgentRootModel  = "anthropic/claude-sonnet-4-6"
	parallelSubAgentChildModel = "anthropic/claude-haiku-4-5"
)

func TestNewHTTPHandlerRunsSubAgentToolCallsInBoundedParallelAndPreservesResultOrder(t *testing.T) {
	scripted := newParallelSubAgentProvider(2)
	translator := Pipeline(
		Pipe("translate text",
			Model(parallelSubAgentChildModel),
			Output[httpSubAgentReply](),
		),
	)
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /emails"),
		Pipe("draft multilingual email",
			Model(parallelSubAgentRootModel),
			SubAgent("translate", translator, MaxParallel(2)),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/emails", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rec, req)
	}()

	if !scripted.waitForTwoActive(300 * time.Millisecond) {
		cancel()
		waitForParallelSubAgentHandler(t, done)
		t.Fatalf("timed out waiting for 2 active subagent calls; max active = %d", scripted.maxActive())
	}
	scripted.releaseChildren()
	waitForParallelSubAgentHandler(t, done)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if violation := scripted.violation(); violation != "" {
		t.Fatal(violation)
	}
	if maxActive := scripted.maxActive(); maxActive != 2 {
		t.Fatalf("max active subagent calls = %d, want exactly 2", maxActive)
	}
	if got := scripted.childCompletionOrder(); reflect.DeepEqual(got, []string{"one", "two", "three", "four"}) {
		t.Fatalf("child completion order = %v, want out-of-order completions to exercise ordered ToolResults", got)
	}

	rootRequests := scripted.rootRequestsSnapshot()
	if len(rootRequests) != 2 {
		t.Fatalf("root provider requests = %d, want initial request and request after ToolResults", len(rootRequests))
	}
	gotResults, err := parallelSubAgentToolResults(rootRequests[1])
	if err != nil {
		t.Fatalf("decode root ToolResults: %v", err)
	}
	wantResults := []parallelSubAgentToolResult{
		{ID: "call_1", Text: "translated one"},
		{ID: "call_2", Text: "translated two"},
		{ID: "call_3", Text: "translated three"},
		{ID: "call_4", Text: "translated four"},
	}
	if !reflect.DeepEqual(gotResults, wantResults) {
		t.Fatalf("ToolResults = %+v, want %+v", gotResults, wantResults)
	}
}

type parallelSubAgentProvider struct {
	mu sync.Mutex

	maxAllowed       int
	active           int
	maxSeen          int
	violationMessage string

	rootRequests  []provider.Request
	childRequests []provider.Request
	childDone     []string

	twoActiveOnce sync.Once
	twoActive     chan struct{}
	releaseOnce   sync.Once
	release       chan struct{}
}

func newParallelSubAgentProvider(maxAllowed int) *parallelSubAgentProvider {
	return &parallelSubAgentProvider{
		maxAllowed: maxAllowed,
		twoActive:  make(chan struct{}),
		release:    make(chan struct{}),
	}
}

func (p *parallelSubAgentProvider) Name() string {
	return "parallel-subagent-scripted"
}

func (p *parallelSubAgentProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	if err := ctx.Err(); err != nil {
		return provider.Response{}, err
	}
	switch req.Model {
	case parallelSubAgentRootModel:
		return p.completeRoot(req)
	case parallelSubAgentChildModel:
		return p.completeChild(ctx, req)
	default:
		return provider.Response{}, fmt.Errorf("unexpected model %q", req.Model)
	}
}

func (p *parallelSubAgentProvider) completeRoot(req provider.Request) (provider.Response, error) {
	p.mu.Lock()
	p.rootRequests = append(p.rootRequests, cloneParallelSubAgentRequest(req))
	callNumber := len(p.rootRequests)
	p.mu.Unlock()

	switch callNumber {
	case 1:
		return provider.Response{
			Text:       "need translations",
			StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{
				{ID: "call_1", Name: "translate", Arguments: []byte(`{"input":"one"}`)},
				{ID: "call_2", Name: "translate", Arguments: []byte(`{"input":"two"}`)},
				{ID: "call_3", Name: "translate", Arguments: []byte(`{"input":"three"}`)},
				{ID: "call_4", Name: "translate", Arguments: []byte(`{"input":"four"}`)},
			},
		}, nil
	case 2:
		return provider.Response{
			Text:       `{"status":"ok"}`,
			StopReason: provider.StopEndTurn,
		}, nil
	default:
		return provider.Response{}, fmt.Errorf("unexpected root provider call %d", callNumber)
	}
}

func (p *parallelSubAgentProvider) completeChild(ctx context.Context, req provider.Request) (provider.Response, error) {
	input := ""
	if len(req.Messages) > 0 {
		input = req.Messages[0].Text()
	}

	p.childStarted(req)
	defer p.childFinished(input)

	select {
	case <-p.release:
	case <-ctx.Done():
		return provider.Response{}, ctx.Err()
	}

	if delay := parallelSubAgentDelay(input); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return provider.Response{}, ctx.Err()
		}
	}

	content, err := json.Marshal(httpSubAgentReply{Text: "translated " + input})
	if err != nil {
		return provider.Response{}, err
	}
	return provider.Response{Text: string(content), StopReason: provider.StopEndTurn}, nil
}

func (p *parallelSubAgentProvider) childStarted(req provider.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.childRequests = append(p.childRequests, cloneParallelSubAgentRequest(req))
	p.active++
	if p.active > p.maxSeen {
		p.maxSeen = p.active
	}
	if p.active > p.maxAllowed && p.violationMessage == "" {
		p.violationMessage = fmt.Sprintf("active subagent calls = %d, want at most %d", p.active, p.maxAllowed)
	}
	if p.active == p.maxAllowed {
		p.twoActiveOnce.Do(func() {
			close(p.twoActive)
		})
	}
}

func (p *parallelSubAgentProvider) childFinished(input string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.childDone = append(p.childDone, input)
	p.active--
}

func (p *parallelSubAgentProvider) waitForTwoActive(timeout time.Duration) bool {
	select {
	case <-p.twoActive:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (p *parallelSubAgentProvider) releaseChildren() {
	p.releaseOnce.Do(func() {
		close(p.release)
	})
}

func (p *parallelSubAgentProvider) maxActive() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxSeen
}

func (p *parallelSubAgentProvider) violation() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.violationMessage
}

func (p *parallelSubAgentProvider) childCompletionOrder() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.childDone...)
}

func (p *parallelSubAgentProvider) rootRequestsSnapshot() []provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.Request(nil), p.rootRequests...)
}

func parallelSubAgentDelay(input string) time.Duration {
	switch input {
	case "one":
		return 80 * time.Millisecond
	case "three":
		return 40 * time.Millisecond
	default:
		return 0
	}
}

type parallelSubAgentToolResult struct {
	ID   string
	Text string
}

func parallelSubAgentToolResults(req provider.Request) ([]parallelSubAgentToolResult, error) {
	var results []parallelSubAgentToolResult
	for _, message := range req.Messages {
		if message.Role != provider.RoleTool {
			continue
		}
		for _, block := range message.Blocks {
			if block.ToolResult == nil {
				continue
			}
			var payload httpSubAgentReply
			if err := json.Unmarshal(block.ToolResult.Content, &payload); err != nil {
				return nil, err
			}
			results = append(results, parallelSubAgentToolResult{
				ID:   block.ToolResult.ToolCallID,
				Text: payload.Text,
			})
		}
	}
	return results, nil
}

func waitForParallelSubAgentHandler(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not return after request cancellation")
	}
}

func cloneParallelSubAgentRequest(req provider.Request) provider.Request {
	cloned := req
	cloned.Messages = append([]provider.Message(nil), req.Messages...)
	for i := range cloned.Messages {
		cloned.Messages[i].Blocks = append([]provider.Block(nil), req.Messages[i].Blocks...)
	}
	cloned.Tools = append([]provider.ToolSpec(nil), req.Tools...)
	return cloned
}
