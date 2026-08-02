package operate

import (
	"strings"
	"sync"
	"testing"
)

func TestBoundedOutputDrainsAndReportsTruncation(t *testing.T) {
	output := newBoundedOutput(8, "stdout")
	if n, err := output.Write([]byte("abcdefghijkl")); err != nil || n != 12 {
		t.Fatalf("Write() = %d, %v; want full producer consumption", n, err)
	}
	text := output.String()
	if !strings.HasPrefix(text, "abcdefgh") || !strings.Contains(text, "retained 8 of 12 bytes") {
		t.Fatalf("String() = %q", text)
	}
}

func TestBoundedOutputConcurrentWritersStayBounded(t *testing.T) {
	output := newBoundedOutput(128, "audit")
	var writers sync.WaitGroup
	for range 32 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			_, _ = output.Write([]byte(strings.Repeat("x", 64)))
		}()
	}
	writers.Wait()
	text := output.String()
	if !strings.Contains(text, "retained 128 of 2048 bytes") {
		t.Fatalf("String() = %q", text)
	}
}
