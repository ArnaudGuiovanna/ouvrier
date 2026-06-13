package console

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tunnel"
)

// TestDeployStreamsJSONL drives the deploy endpoint with a fake deploy engine
// that writes a few progress lines and then fails, and asserts the response is
// newline-delimited JSON ending in a done record carrying the error.
func TestDeployStreamsJSONL(t *testing.T) {
	mgr := newFakeManager("admintok")
	defer mgr.Close()
	dir := writeFleet(t)
	fakeDeploy := func(ctx context.Context, opts deploy.EnvOpts, p deploy.ProgressWriter) error {
		if opts.EnvName != "staging" {
			t.Errorf("deploy env = %q, want staging", opts.EnvName)
		}
		p.Out.Write([]byte("step 1: build\n"))
		p.Out.Write([]byte("step 2: upload\n"))
		p.Err.Write([]byte("warning: slow link\n"))
		return errors.New("health gate failed")
	}
	srv, err := NewServer(Options{
		Addr:         "127.0.0.1:0",
		Dir:          dir,
		FleetPath:    dir + "/deployments.json",
		sessionToken: testToken,
		deploy:       fakeDeploy,
		newManager: func(_ []deploy.Deployment, _ tunnel.Options) (Manager, error) {
			return mgr, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/workers/staging/deploy", nil)
	req.Host = "127.0.0.1"
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	defer resp.Body.Close()

	var outLines, errLines int
	var doneErr string
	doneSeen := false
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("non-JSONL line %q: %v", sc.Text(), err)
		}
		switch {
		case rec["done"] == true:
			doneSeen = true
			if e, ok := rec["error"].(string); ok {
				doneErr = e
			}
		case rec["stream"] == "out":
			outLines++
		case rec["stream"] == "err":
			errLines++
		}
	}
	if outLines != 2 {
		t.Fatalf("out lines = %d, want 2", outLines)
	}
	if errLines != 1 {
		t.Fatalf("err lines = %d, want 1", errLines)
	}
	if !doneSeen {
		t.Fatal("missing done record")
	}
	if doneErr != "health gate failed" {
		t.Fatalf("done error = %q, want health gate failed", doneErr)
	}
}
