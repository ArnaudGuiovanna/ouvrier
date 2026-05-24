package ovr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
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

func TestNewHTTPHandlerSequentialToolsRunsSubAgentToolCallsOneAtATime(t *testing.T) {
	scripted := newParallelSubAgentProvider(1)
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
			SequentialTools(),
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
		t.Fatalf("timed out waiting for first subagent call; max active = %d", scripted.maxActive())
	}
	if scripted.waitForMaxActiveAtLeast(2, 100*time.Millisecond) {
		cancel()
		scripted.releaseChildren()
		waitForParallelSubAgentHandler(t, done)
		t.Fatalf("SequentialTools allowed %d concurrent subagent calls", scripted.maxActive())
	}
	scripted.releaseChildren()
	waitForParallelSubAgentHandler(t, done)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if violation := scripted.violation(); violation != "" {
		t.Fatal(violation)
	}
	if maxActive := scripted.maxActive(); maxActive != 1 {
		t.Fatalf("max active subagent calls = %d, want exactly 1", maxActive)
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

func TestNewHTTPHandlerPropagatesRequestCancellationToSubAgentChildTask(t *testing.T) {
	scripted := newCancelPropagationSubAgentProvider()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
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
			SubAgent("translate", translator, MaxParallel(1)),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/emails", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rec, req)
	}()

	if !scripted.waitForChildStart(300 * time.Millisecond) {
		cancel()
		waitForParallelSubAgentHandler(t, done)
		t.Fatal("timed out waiting for subagent child provider call")
	}
	cancel()
	if !scripted.waitForChildDone(300 * time.Millisecond) {
		waitForParallelSubAgentHandler(t, done)
		t.Fatal("timed out waiting for subagent child cancellation")
	}
	waitForParallelSubAgentHandler(t, done)

	if err := scripted.childErr(); err != context.Canceled {
		t.Fatalf("child provider error = %v, want context.Canceled", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d after cancellation, body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	assertParallelSubAgentEvent(t, stream.List(), events.EventTaskStarted)
	assertParallelSubAgentEvent(t, stream.List(), events.EventTaskFailed)
}

func TestNewHTTPHandlerPreservesPartialOKSubAgentToolResultOrderWhenChildTaskFails(t *testing.T) {
	scripted := newFailingOrderedSubAgentProvider()
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
			SubAgent("translate", translator, MaxParallel(3), PartialOK()),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/emails", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	rootRequests := scripted.rootRequestsSnapshot()
	if len(rootRequests) != 2 {
		t.Fatalf("root provider requests = %d, want initial request and request after ToolResults", len(rootRequests))
	}
	gotResults := parallelSubAgentRawToolResults(rootRequests[1])
	wantResults := []parallelSubAgentRawToolResult{
		{ID: "call_1", IsError: false, Content: `{"text":"translated one"}`},
		{ID: "call_2", IsError: true, Content: `"translation failed for two"`},
		{ID: "call_3", IsError: false, Content: `{"text":"translated three"}`},
	}
	if !reflect.DeepEqual(gotResults, wantResults) {
		t.Fatalf("ToolResults = %+v, want %+v", gotResults, wantResults)
	}
	if got := scripted.childCompletionOrder(); reflect.DeepEqual(got, []string{"one", "two", "three"}) {
		t.Fatalf("child completion order = %v, want out-of-order completions to exercise ordered ToolResults", got)
	}
}

func TestNewHTTPHandlerFailsParentPipeWhenSubAgentChildTaskFailsWithoutPartialOK(t *testing.T) {
	scripted := newFailingOrderedSubAgentProvider()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
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
			SubAgent("translate", translator, MaxParallel(3)),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/emails", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	rootRequests := scripted.rootRequestsSnapshot()
	if len(rootRequests) != 1 {
		t.Fatalf("root provider requests = %d, want no follow-up after fatal child failure", len(rootRequests))
	}
	assertParallelSubAgentEvent(t, stream.List(), events.EventTaskFailed)
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

type cancelPropagationSubAgentProvider struct {
	mu      sync.Mutex
	rootN   int
	child   error
	started chan struct{}
	done    chan struct{}
	once    sync.Once
}

func newCancelPropagationSubAgentProvider() *cancelPropagationSubAgentProvider {
	return &cancelPropagationSubAgentProvider{
		started: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (p *cancelPropagationSubAgentProvider) Name() string {
	return "cancel-propagation-subagent-scripted"
}

func (p *cancelPropagationSubAgentProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	switch req.Model {
	case parallelSubAgentRootModel:
		p.mu.Lock()
		p.rootN++
		callNumber := p.rootN
		p.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return provider.Response{}, err
		}
		if callNumber != 1 {
			return provider.Response{}, fmt.Errorf("unexpected root provider call %d", callNumber)
		}
		return provider.Response{
			Text:       "need translation",
			StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{{
				ID:        "call_cancel",
				Name:      "translate",
				Arguments: []byte(`{"input":"cancel me"}`),
			}},
		}, nil
	case parallelSubAgentChildModel:
		p.once.Do(func() {
			close(p.started)
		})
		<-ctx.Done()
		p.mu.Lock()
		p.child = ctx.Err()
		p.mu.Unlock()
		close(p.done)
		return provider.Response{}, ctx.Err()
	default:
		return provider.Response{}, fmt.Errorf("unexpected model %q", req.Model)
	}
}

func (p *cancelPropagationSubAgentProvider) waitForChildStart(timeout time.Duration) bool {
	select {
	case <-p.started:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (p *cancelPropagationSubAgentProvider) waitForChildDone(timeout time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (p *cancelPropagationSubAgentProvider) childErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.child
}

type failingOrderedSubAgentProvider struct {
	mu           sync.Mutex
	rootRequests []provider.Request
	childDone    []string
}

func newFailingOrderedSubAgentProvider() *failingOrderedSubAgentProvider {
	return &failingOrderedSubAgentProvider{}
}

func (p *failingOrderedSubAgentProvider) Name() string {
	return "failing-ordered-subagent-scripted"
}

func (p *failingOrderedSubAgentProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
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

func (p *failingOrderedSubAgentProvider) completeRoot(req provider.Request) (provider.Response, error) {
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
			},
		}, nil
	case 2:
		return provider.Response{Text: `{"status":"ok"}`, StopReason: provider.StopEndTurn}, nil
	default:
		return provider.Response{}, fmt.Errorf("unexpected root provider call %d", callNumber)
	}
}

func (p *failingOrderedSubAgentProvider) completeChild(ctx context.Context, req provider.Request) (provider.Response, error) {
	input := ""
	if len(req.Messages) > 0 {
		input = req.Messages[0].Text()
	}
	if delay := failingOrderedSubAgentDelay(input); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return provider.Response{}, ctx.Err()
		}
	}
	p.mu.Lock()
	p.childDone = append(p.childDone, input)
	p.mu.Unlock()
	if input == "two" {
		return provider.Response{}, errors.New("translation failed for two")
	}
	content, err := json.Marshal(httpSubAgentReply{Text: "translated " + input})
	if err != nil {
		return provider.Response{}, err
	}
	return provider.Response{Text: string(content), StopReason: provider.StopEndTurn}, nil
}

func (p *failingOrderedSubAgentProvider) rootRequestsSnapshot() []provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.Request(nil), p.rootRequests...)
}

func (p *failingOrderedSubAgentProvider) childCompletionOrder() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.childDone...)
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

func (p *parallelSubAgentProvider) waitForMaxActiveAtLeast(min int, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		p.mu.Lock()
		maxSeen := p.maxSeen
		p.mu.Unlock()
		if maxSeen >= min {
			return true
		}
		select {
		case <-deadline.C:
			return false
		case <-ticker.C:
		}
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

func failingOrderedSubAgentDelay(input string) time.Duration {
	switch input {
	case "one":
		return 60 * time.Millisecond
	case "two":
		return 20 * time.Millisecond
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

type parallelSubAgentRawToolResult struct {
	ID      string
	IsError bool
	Content string
}

func parallelSubAgentRawToolResults(req provider.Request) []parallelSubAgentRawToolResult {
	var results []parallelSubAgentRawToolResult
	for _, message := range req.Messages {
		if message.Role != provider.RoleTool {
			continue
		}
		for _, block := range message.Blocks {
			if block.ToolResult == nil {
				continue
			}
			results = append(results, parallelSubAgentRawToolResult{
				ID:      block.ToolResult.ToolCallID,
				IsError: block.ToolResult.IsError,
				Content: string(block.ToolResult.Content),
			})
		}
	}
	return results
}

func assertParallelSubAgentEvent(t *testing.T, recorded []events.Event, kind events.EventKind) {
	t.Helper()
	for _, event := range recorded {
		if event.Kind == kind && event.Payload["subagent"] == "translate" {
			return
		}
	}
	t.Fatalf("events = %+v, want %s for translate subagent", recorded, kind)
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
