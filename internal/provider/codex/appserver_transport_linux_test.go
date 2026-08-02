//go:build linux

package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDefaultAppServerTransportKillsWholeProcessGroup(t *testing.T) {
	mode := ""
	if len(os.Args) > 1 {
		mode = os.Args[len(os.Args)-1]
	}
	switch mode {
	case "appserver-tree-grandchild":
		for {
			time.Sleep(time.Hour)
		}
	case "appserver-tree-parent":
		child := exec.Command(
			os.Args[0],
			"-test.run=^TestDefaultAppServerTransportKillsWholeProcessGroup$",
			"--",
			"appserver-tree-grandchild",
		)
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			t.Fatalf("start process-tree grandchild: %v", err)
		}
		fmt.Printf("{\"pid\":%d}\n", child.Process.Pid)
		for {
			time.Sleep(time.Hour)
		}
	}

	process, err := (defaultAppServerTransport{}).Start(
		os.Args[0],
		"-test.run=^TestDefaultAppServerTransportKillsWholeProcessGroup$",
		"--",
		"appserver-tree-parent",
	)
	if err != nil {
		t.Fatalf("start process-tree helper: %v", err)
	}
	message, err := process.Receive(context.Background())
	if err != nil {
		_ = process.Close()
		t.Fatalf("receive grandchild pid: %v", err)
	}
	var announcement struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(message, &announcement); err != nil || announcement.PID <= 0 {
		_ = process.Close()
		t.Fatalf("invalid grandchild announcement %q: %#v, %v", message, announcement, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(announcement.PID, syscall.SIGKILL) })

	started := time.Now()
	if err := process.Close(); err != nil {
		t.Fatalf("close process-tree helper: %v", err)
	}
	if elapsed := time.Since(started); elapsed > appServerCloseTimeout+250*time.Millisecond {
		t.Fatalf("Close exceeded its bound: %s", elapsed)
	}

	deadline := time.Now().Add(time.Second)
	for linuxProcessAlive(announcement.PID) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if linuxProcessAlive(announcement.PID) {
		t.Fatalf("grandchild process %d survived app-server Close", announcement.PID)
	}
}

func linuxProcessAlive(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err == nil {
		// A zombie is already dead; it merely awaits reaping by its new parent.
		if suffix := string(data); strings.Contains(suffix, ") Z ") {
			return false
		}
	}
	err = syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
