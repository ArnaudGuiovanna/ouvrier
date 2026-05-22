package ovr

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDefaultStreamReceiverReceivesNATSMessage(t *testing.T) {
	uri, err := url.Parse("nats://127.0.0.1:4222/tickets.created")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	client, server := net.Pipe()
	defer client.Close()
	done := serveTestNATSConn(t, server, "tickets.created", `{"event":"nats"}`)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	message, err := receiveNATSStreamFromConn(ctx, client, uri)
	if err != nil {
		t.Fatalf("receiveNATSStreamFromConn returned error: %v", err)
	}
	if message.Body != `{"event":"nats"}` {
		t.Fatalf("body = %q, want nats event", message.Body)
	}
	if message.Metadata["subject"] != "tickets.created" {
		t.Fatalf("subject metadata = %q, want tickets.created", message.Metadata["subject"])
	}
	waitTestNATSServer(t, done)
}

func TestDefaultHTTPRuntimeInstallsConcreteStreamReceiver(t *testing.T) {
	rt := defaultHTTPRuntime()
	if rt.streamReceiver == nil {
		t.Fatal("stream receiver is nil, want default receiver")
	}
	if _, ok := rt.streamReceiver.(*defaultStreamReceiver); !ok {
		t.Fatalf("stream receiver = %T, want *defaultStreamReceiver", rt.streamReceiver)
	}
}

func TestParseNATSMSGLine(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		wantSubject string
		wantSize    int
		wantErr     bool
	}{
		{name: "plain", line: "MSG tickets.created 1 15", wantSubject: "tickets.created", wantSize: 15},
		{name: "reply", line: "MSG tickets.created 1 _INBOX.reply 15", wantSubject: "tickets.created", wantSize: 15},
		{name: "bad size", line: "MSG tickets.created 1 nope", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subject, size, err := parseNATSMSGLine(tt.line)
			if tt.wantErr {
				if err == nil {
					t.Fatal("parseNATSMSGLine returned nil error, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseNATSMSGLine returned error: %v", err)
			}
			if subject != tt.wantSubject || size != tt.wantSize {
				t.Fatalf("subject=%q size=%d, want %q/%d", subject, size, tt.wantSubject, tt.wantSize)
			}
		})
	}
}

func serveTestNATSConn(t *testing.T, conn net.Conn, subject, payload string) <-chan error {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		defer close(done)
		defer conn.Close()
		if _, err := io.WriteString(conn, "INFO {}\r\n"); err != nil {
			done <- err
			return
		}

		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				done <- err
				return
			}
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "SUB ") {
				if !strings.Contains(line, subject) {
					done <- fmt.Errorf("SUB line = %q, want subject %q", line, subject)
					return
				}
				if _, err := fmt.Fprintf(conn, "MSG %s 1 %d\r\n%s\r\n", subject, len(payload), payload); err != nil {
					done <- err
					return
				}
				return
			}
		}
	}()
	return done
}

func waitTestNATSServer(t *testing.T, done <-chan error) {
	t.Helper()

	select {
	case err, ok := <-done:
		if ok && err != nil {
			t.Fatalf("test NATS server returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("test NATS server did not finish")
	}
}

func TestDefaultStreamReceiverKeepsUnsupportedSchemesExplicit(t *testing.T) {
	_, err := newDefaultStreamReceiver().Receive(context.Background(), "mqtt://tickets")
	if err == nil || !strings.Contains(err.Error(), "unsupported stream scheme") {
		t.Fatalf("Receive error = %v, want explicit unsupported scheme", err)
	}
}

func TestNATSStreamSubjectParsingUsesQueueURIRules(t *testing.T) {
	uri, err := url.Parse("nats://127.0.0.1:4222?subject=tickets.created")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	subject, err := natsQueueSubject(uri)
	if err != nil {
		t.Fatalf("natsQueueSubject returned error: %v", err)
	}
	if subject != "tickets.created" {
		t.Fatalf("subject = %q, want tickets.created", subject)
	}
}
