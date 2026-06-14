package tui

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

// TestRenderDemo is a manual visual check: go test -run RenderDemo -v
func TestRenderDemo(t *testing.T) {
	if os.Getenv("OVR_DEMO") == "" {
		t.Skip("set OVR_DEMO=1 to print a cockpit frame")
	}
	dir := t.TempDir()
	w := filepath.Join(dir, "ticket-triage")
	writeOperateWorker(t, w, "ticket-triage")

	m := newOperateModel(context.Background(), OperateOptions{Dir: w, Agent: "codex", Driver: operate.ManualDriver{}}).(*operateModel)
	m.Update(tea.WindowSizeMsg{Width: 92, Height: 30})

	for _, p := range []string{"create a worker that receives POST /tickets and triages it", "/workers"} {
		m.submit(p)
		for ev := range m.events {
			m.handleStream(opStreamMsg{ev: ev, ok: true})
		}
		m.handleStream(opStreamMsg{ok: false})
	}
	t.Log("\n" + ansiRE.ReplaceAllString(m.render(), ""))
}
