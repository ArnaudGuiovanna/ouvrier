package ovr

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ouvrier/internal/provider"
)

func TestNewHTTPHandlerServesDirectSSEReply(t *testing.T) {
	handler, err := newHTTPHandler([]Node{
		From("GET /events"),
		Reply(SSE()),
	})
	if err != nil {
		t.Fatalf("newHTTPHandler returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	assertSSEHeader(t, rec)
	if got, want := rec.Body.String(), "event: output\ndata: {\"status\":\"ok\"}\n\n"; got != want {
		t.Fatalf("SSE body = %q, want %q", got, want)
	}
}

func TestNewHTTPHandlerRepliesWithPipelineOutputSSE(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(SSE()),
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
	assertSSEHeader(t, rec)
	body := rec.Body.String()
	for _, want := range []string{
		"event: pipeline_started\n",
		"event: llm_call_started\n",
		"event: pipe_completed\n",
		"event: pipeline_completed\n",
		"event: output\ndata: {\"status\":\"classified\"}\n\n",
		"event: done\ndata: {\"status\":\"completed\"}\n\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("SSE body missing %q:\n%s", want, body)
		}
	}
}

func TestNewHTTPHandlerRedactsSensitiveSSEOutput(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"message":"Authorization: Bearer root-token","safe":"ok"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(SSE()),
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
	body := rec.Body.String()
	if strings.Contains(body, "root-token") {
		t.Fatalf("SSE body leaked token:\n%s", body)
	}
	if !strings.Contains(body, `event: output`+"\n"+`data: {"message":"Authorization: [REDACTED]","safe":"ok"}`+"\n\n") {
		t.Fatalf("SSE body missing redacted output:\n%s", body)
	}
}

func TestNewHTTPHandlerStreamsSSEPipelineErrors(t *testing.T) {
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(SSE()),
	}, httpRuntime{})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	assertSSEHeader(t, rec)
	body := rec.Body.String()
	for _, want := range []string{
		"event: pipeline_failed\n",
		"event: error\ndata: {\"status\":\"provider_not_configured\"}\n\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("SSE body missing %q:\n%s", want, body)
		}
	}
}

func TestNewHTTPHandlerStreamsSSEEventsBeforePipelineCompletes(t *testing.T) {
	scripted := newBlockingProvider()
	released := make(chan struct{})
	release := func() {
		select {
		case <-released:
		default:
			close(released)
			close(scripted.release)
		}
	}
	t.Cleanup(release)

	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(SSE()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/tickets", strings.NewReader(`{"title":"broken"}`))
	if err != nil {
		t.Fatalf("NewRequestWithContext returned error: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do returned error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", contentType)
	}

	reader := bufio.NewReader(resp.Body)
	prefix := readSSEUntil(t, reader, "event: llm_call_started", time.Second)
	select {
	case <-scripted.started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	release()

	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll returned error: %v", err)
	}
	body := prefix + string(rest)
	if !strings.Contains(body, "event: output\ndata: done\n\n") {
		t.Fatalf("SSE body missing final output:\n%s", body)
	}
}

func assertSSEHeader(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	if contentType := rec.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", contentType)
	}
	if cacheControl := rec.Header().Get("Cache-Control"); cacheControl != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", cacheControl)
	}
}

func readSSEUntil(t *testing.T, reader *bufio.Reader, marker string, timeout time.Duration) string {
	t.Helper()

	type readResult struct {
		text string
		err  error
	}
	done := make(chan readResult, 1)
	go func() {
		var body strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				body.WriteString(line)
				if strings.Contains(body.String(), marker) {
					done <- readResult{text: body.String()}
					return
				}
			}
			if err != nil {
				done <- readResult{text: body.String(), err: err}
				return
			}
		}
	}()

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("reading SSE until %q returned error: %v; body=%s", marker, result.err, result.text)
		}
		return result.text
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for SSE marker %q", marker)
		return ""
	}
}
