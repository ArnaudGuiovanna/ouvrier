package ovr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

type compositionReply struct {
	Step string `json:"step"`
}

type compositionPartialReply struct {
	OK     bool              `json:"ok"`
	Output *compositionReply `json:"output,omitempty"`
	Error  string            `json:"error,omitempty"`
}

type compositionProvider struct {
	mu        sync.Mutex
	requests  []provider.Request
	active    int
	maxActive int
}

func (p *compositionProvider) Name() string {
	return "composition"
}

func (p *compositionProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	p.started(req)
	defer p.finished()

	switch req.Model {
	case "composition/quality":
		time.Sleep(20 * time.Millisecond)
		return provider.Response{Text: `{"step":"quality"}`, StopReason: provider.StopEndTurn}, nil
	case "composition/compliance":
		return provider.Response{Text: `{"step":"compliance"}`, StopReason: provider.StopEndTurn}, nil
	case "composition/fail":
		return provider.Response{}, errors.New("branch failed")
	case "composition/list":
		return provider.Response{Text: `[{"id":"a"},{"id":"b"},{"id":"c"}]`, StopReason: provider.StopEndTurn}, nil
	case "composition/item":
		id := itemID(req.Messages[0].Text())
		return provider.Response{Text: fmt.Sprintf(`{"step":%q}`, id), StopReason: provider.StopEndTurn}, nil
	case "composition/budget":
		return provider.Response{
			Text:       `{"step":"budget"}`,
			StopReason: provider.StopEndTurn,
			Usage:      provider.Usage{InputTokens: 2},
		}, nil
	default:
		return provider.Response{}, fmt.Errorf("unexpected model %s", req.Model)
	}
}

func (p *compositionProvider) started(req provider.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if req.Model == "composition/item" {
		p.active++
		if p.active > p.maxActive {
			p.maxActive = p.active
		}
	}
}

func (p *compositionProvider) finished() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active > 0 {
		p.active--
	}
}

func (p *compositionProvider) maxItemActive() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxActive
}

func TestNewHTTPHandlerRunsParallelPipesWithOrderedOutputs(t *testing.T) {
	scripted := &compositionProvider{}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Parallel(
			Pipe("quality", Model("composition/quality")),
			Pipe("compliance", Model("composition/compliance")),
		),
		Reply(JSON[[]compositionReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	var replies []compositionReply
	if err := json.Unmarshal([]byte(body.Output), &replies); err != nil {
		t.Fatalf("output is not composition replies: %v; output=%s", err, body.Output)
	}
	if got := replySteps(replies); strings.Join(got, ",") != "quality,compliance" {
		t.Fatalf("reply order = %+v, want declared order", got)
	}
}

func TestNewHTTPHandlerParallelPartialOKReturnsOrderedErrorOutcomes(t *testing.T) {
	scripted := &compositionProvider{}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Parallel(
			Pipe("quality", Model("composition/quality")),
			Pipe("fail", Model("composition/fail")),
			PartialOK(),
		),
		Reply(JSON[[]compositionPartialReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	var replies []compositionPartialReply
	if err := json.Unmarshal([]byte(body.Output), &replies); err != nil {
		t.Fatalf("output is not partial replies: %v; output=%s", err, body.Output)
	}
	if len(replies) != 2 || !replies[0].OK || replies[0].Output.Step != "quality" {
		t.Fatalf("partial replies = %+v, want first success", replies)
	}
	if replies[1].OK || !strings.Contains(replies[1].Error, "branch failed") {
		t.Fatalf("partial replies = %+v, want second error", replies)
	}
}

func TestNewHTTPHandlerMapsJSONArrayWithBoundedConcurrencyAndOrderedOutputs(t *testing.T) {
	scripted := &compositionProvider{}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("list", Model("composition/list")),
		Map(
			Concurrency(1),
			Pipe("item", Model("composition/item")),
		),
		Reply(JSON[[]compositionReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	var replies []compositionReply
	if err := json.Unmarshal([]byte(body.Output), &replies); err != nil {
		t.Fatalf("output is not composition replies: %v; output=%s", err, body.Output)
	}
	if got := replySteps(replies); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("reply order = %+v, want input order", got)
	}
	if scripted.maxItemActive() > 1 {
		t.Fatalf("max active item providers = %d, want <= 1", scripted.maxItemActive())
	}
}

func TestNewHTTPHandlerParallelFailFastCancelsSiblingBranch(t *testing.T) {
	scripted := newCancelCompositionProvider()
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Parallel(
			Pipe("slow", Model("composition/slow")),
			Pipe("fail", Model("composition/fail")),
		),
		Reply(JSON[[]compositionReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	select {
	case <-scripted.slowCanceled:
	default:
		t.Fatal("slow branch was not canceled after sibling failure")
	}
}

func TestNewHTTPHandlerParallelChildBudgetFailureFailsComposition(t *testing.T) {
	scripted := &compositionProvider{}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Parallel(
			Pipe("budget", Model("composition/budget"), MaxTokens(1)),
			Pipe("quality", Model("composition/quality")),
		),
		Reply(JSON[[]compositionReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
}

func itemID(input string) string {
	var raw struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(input), &raw)
	return raw.ID
}

func replySteps(replies []compositionReply) []string {
	steps := make([]string, 0, len(replies))
	for _, reply := range replies {
		steps = append(steps, reply.Step)
	}
	return steps
}

type cancelCompositionProvider struct {
	slowStarted  chan struct{}
	slowCanceled chan struct{}
}

func newCancelCompositionProvider() *cancelCompositionProvider {
	return &cancelCompositionProvider{
		slowStarted:  make(chan struct{}),
		slowCanceled: make(chan struct{}),
	}
}

func (p *cancelCompositionProvider) Name() string {
	return "composition-cancel"
}

func (p *cancelCompositionProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	switch req.Model {
	case "composition/slow":
		close(p.slowStarted)
		<-ctx.Done()
		close(p.slowCanceled)
		return provider.Response{}, ctx.Err()
	case "composition/fail":
		select {
		case <-p.slowStarted:
		case <-ctx.Done():
			return provider.Response{}, ctx.Err()
		}
		return provider.Response{}, errors.New("branch failed")
	default:
		return provider.Response{}, fmt.Errorf("unexpected model %s", req.Model)
	}
}
