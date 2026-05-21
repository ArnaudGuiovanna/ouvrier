package ovr

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"ouvrier/internal/provider"
)

type recordedNATSPublish struct {
	Subject string
	Payload string
	Connect string
}

func TestNewHTTPHandlerPushesPipelineOutputToQueue(t *testing.T) {
	queueURI, publishes := newNATSPublishRecorder(t, "tickets.classified")
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Push(Queue(queueURI)),
	}, httpRuntime{provider: scripted, toolExecutor: outputAllowedExecutor("queue")})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	publish := assertNATSPublish(t, publishes)
	if publish.Subject != "tickets.classified" || publish.Payload != `{"status":"classified"}` {
		t.Fatalf("publish = %+v, want classified ticket payload", publish)
	}
}

func TestNewHTTPHandlerPushesDirectInputToQueue(t *testing.T) {
	queueURI, publishes := newNATSPublishRecorder(t, "events.created")
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /events"),
		Push(Queue(queueURI)),
	}, httpRuntime{toolExecutor: outputAllowedExecutor("queue")})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{"event":"created"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	publish := assertNATSPublish(t, publishes)
	if publish.Subject != "events.created" || publish.Payload != `{"event":"created"}` {
		t.Fatalf("publish = %+v, want direct event payload", publish)
	}
}

func TestNewHTTPHandlerReturnsFailureWhenQueuePushFails(t *testing.T) {
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /events"),
		Push(Queue("nats://127.0.0.1:1/events.created")),
	}, httpRuntime{toolExecutor: outputAllowedExecutor("queue")})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{"event":"created"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
}

func newNATSPublishRecorder(t *testing.T, subject string) (string, <-chan recordedNATSPublish) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})

	publishes := make(chan recordedNATSPublish, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if err := recordNATSPublish(conn, publishes); err != nil {
			t.Errorf("recordNATSPublish returned error: %v", err)
		}
	}()

	return fmt.Sprintf("nats://%s/%s", listener.Addr().String(), subject), publishes
}

func recordNATSPublish(conn net.Conn, publishes chan<- recordedNATSPublish) error {
	if _, err := io.WriteString(conn, "INFO {}\r\n"); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	connectLine, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	pubLine, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	fields := strings.Fields(strings.TrimSpace(pubLine))
	if len(fields) != 3 || fields[0] != "PUB" {
		return fmt.Errorf("PUB line = %q", pubLine)
	}
	payloadSize, err := strconv.Atoi(fields[2])
	if err != nil {
		return err
	}
	payload := make([]byte, payloadSize+2)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return err
	}
	pingLine, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(pingLine) != "PING" {
		return fmt.Errorf("line after payload = %q, want PING", pingLine)
	}
	if _, err := io.WriteString(conn, "PONG\r\n"); err != nil {
		return err
	}
	publishes <- recordedNATSPublish{
		Subject: fields[1],
		Payload: string(payload[:payloadSize]),
		Connect: strings.TrimSpace(connectLine),
	}
	return nil
}

func assertNATSPublish(t *testing.T, publishes <-chan recordedNATSPublish) recordedNATSPublish {
	t.Helper()

	select {
	case publish := <-publishes:
		if !strings.HasPrefix(publish.Connect, "CONNECT ") {
			t.Fatalf("CONNECT line = %q", publish.Connect)
		}
		return publish
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for NATS publish")
		return recordedNATSPublish{}
	}
}
