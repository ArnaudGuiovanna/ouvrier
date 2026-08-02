//go:build linux

package codex

import (
	"context"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestProviderBoundsInheritedPipesAndKillsDescendants(t *testing.T) {
	p := &Provider{Runner: codexExecAdversarialRunner{mode: "inherited-pipe-parent"}, Bin: "codex-test"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()

	resp, err := p.Complete(ctx, provider.Request{Messages: []provider.Message{provider.UserText("test")}})
	if err == nil || !strings.Contains(err.Error(), "WaitDelay") {
		t.Fatalf("Complete() error = %v, want bounded inherited-pipe failure", err)
	}
	if elapsed := time.Since(started); elapsed > codexExecProcessWait+2*time.Second {
		t.Fatalf("Complete() elapsed = %s, inherited pipes were not bounded", elapsed)
	}
	value, ok := strings.CutPrefix(strings.TrimSpace(resp.Text), "grandchild_pid=")
	if !ok {
		t.Fatalf("response text = %q, want grandchild pid", resp.Text)
	}
	pid, parseErr := strconv.Atoi(value)
	if parseErr != nil || pid <= 0 {
		t.Fatalf("grandchild pid = %q: %v", value, parseErr)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(time.Second)
	for linuxProcessAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if linuxProcessAlive(pid) {
		t.Fatalf("grandchild process %d survived provider completion", pid)
	}
}
