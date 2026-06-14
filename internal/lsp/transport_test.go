package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	type msg struct {
		Hello string `json:"hello"`
	}
	var buf bytes.Buffer
	if err := writeMessage(&buf, msg{Hello: "world"}); err != nil {
		t.Fatalf("writeMessage: %v", err)
	}
	got, err := readMessage(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	var out msg
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Hello != "world" {
		t.Errorf("want hello=world, got %q", out.Hello)
	}
}

func TestExtraHeader(t *testing.T) {
	body := []byte(`{"x":1}`)
	raw := fmt.Sprintf("Content-Length: %d\r\nContent-Type: application/json\r\n\r\n%s", len(body), body)
	r := bufio.NewReader(bytes.NewReader([]byte(raw)))
	got, err := readMessage(r)
	if err != nil {
		t.Fatalf("readMessage with extra header: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("want %s, got %s", body, got)
	}
}
