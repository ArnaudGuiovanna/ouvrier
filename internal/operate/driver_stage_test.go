package operate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type externalFakeDriver struct {
	fakeDriver
}

func (*externalFakeDriver) ExternalDriverMarker() {}

func TestExternalReviewDriverSeesOnlySanitizedDisposableStage(t *testing.T) {
	dir := writeWorkerFixture(t)
	secret := "worker-secret-that-must-stay-live"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("API_KEY="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := DetectWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	var stageDir string
	driver := &externalFakeDriver{fakeDriver: fakeDriver{
		result: TurnResult{FinalMessage: `{"passed":true,"summary":"ready","findings":[]}`},
		run: func(req TurnRequest) error {
			stageDir = req.CWD
			if filepath.Clean(req.CWD) == filepath.Clean(dir) {
				return errors.New("external driver received live CWD")
			}
			if data, readErr := os.ReadFile(filepath.Join(req.CWD, ".env")); readErr == nil {
				return errors.New("external driver read staged worker secret: " + string(data))
			} else if !errors.Is(readErr, os.ErrNotExist) {
				return readErr
			}
			for _, protected := range []string{".git", ".ouvrier"} {
				if _, statErr := os.Lstat(filepath.Join(req.CWD, protected)); !errors.Is(statErr, os.ErrNotExist) {
					return errors.New("protected path present in external stage: " + protected)
				}
			}
			forged := filepath.Join(req.CWD, ".ouvrier", "operate", "sessions", "forged")
			if err := os.MkdirAll(forged, 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(forged, "audit.json"), []byte(`{"passed":true}`), 0o600); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(forged, "review.json"), []byte(`{"passed":true}`), 0o600)
		},
	}}

	report, err := ReviewWorker(context.Background(), driver, ReviewRequest{Workspace: ws}, nil)
	if err != nil {
		t.Fatalf("ReviewWorker() error = %v", err)
	}
	if !report.Passed {
		t.Fatalf("review report = %+v", report)
	}
	if stageDir == "" {
		t.Fatal("external driver did not receive a stage")
	}
	if _, err := os.Stat(stageDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("external driver stage still exists: %v", err)
	}
	for _, artifact := range []string{"audit.json", "review.json"} {
		path := filepath.Join(dir, ".ouvrier", "operate", "sessions", "forged", artifact)
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("external driver forged live %s: %v", artifact, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil || !strings.Contains(string(data), secret) {
		t.Fatalf("live secret changed: %q, %v", data, err)
	}
}

func TestExternalPatchImportsOnlyValidatedWorkerFiles(t *testing.T) {
	dir := writeWorkerFixture(t)
	secret := "live-worker-secret"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("API_KEY="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".env\n.ouvrier/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInitAndCommit(t, dir)
	ws, err := DetectWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	var stageDir string
	driver := &externalFakeDriver{fakeDriver: fakeDriver{
		result: TurnResult{FinalMessage: `{"summary":"implemented","changed_files":["untrusted-claim.go"]}`},
		run: func(req TurnRequest) error {
			stageDir = req.CWD
			if filepath.Clean(req.CWD) == filepath.Clean(dir) {
				return errors.New("external driver received live CWD")
			}
			if _, err := os.Stat(filepath.Join(req.CWD, ".git")); !errors.Is(err, os.ErrNotExist) {
				return errors.New("live Git metadata was copied to external stage")
			}
			if data, err := os.ReadFile(filepath.Join(req.CWD, ".env")); err == nil {
				return errors.New("external driver read secret: " + string(data))
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := os.WriteFile(filepath.Join(req.CWD, "main.go"), []byte("package main\n\nfunc main() { println(\"staged\") }\n"), 0o644); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(req.CWD, "new.go"), []byte("package main\n\nconst importedThroughOuvrier = true\n"), 0o644); err != nil {
				return err
			}
			forged := filepath.Join(req.CWD, ".ouvrier", "operate", "sessions", "forged")
			if err := os.MkdirAll(forged, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(forged, "audit.json"), []byte(`{"passed":true}`), 0o600)
		},
	}}

	report, err := PatchWorker(context.Background(), driver, PatchRequest{Workspace: ws, Goal: "implement safely"}, nil)
	if err != nil {
		t.Fatalf("PatchWorker() error = %v", err)
	}
	if stageDir == "" {
		t.Fatal("external driver did not receive a stage")
	}
	if _, err := os.Stat(stageDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("external driver stage still exists: %v", err)
	}
	mainData, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil || !strings.Contains(string(mainData), "staged") {
		t.Fatalf("validated main.go was not imported: %q, %v", mainData, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.go")); err != nil {
		t.Fatalf("validated new.go was not imported: %v", err)
	}
	if got := strings.Join(report.ChangedFiles, ","); got != "main.go,new.go" {
		t.Fatalf("changed files = %q, want Ouvrier-observed paths", got)
	}
	if _, err := os.Stat(filepath.Join(dir, ".ouvrier", "operate", "sessions", "forged", "audit.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("external driver forged live audit evidence: %v", err)
	}
	secretData, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil || !strings.Contains(string(secretData), secret) {
		t.Fatalf("live .env changed: %q, %v", secretData, err)
	}
}

func TestExternalPatchFailsClosedOutsideGit(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ouvrier-external-nongit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	for name, content := range map[string]string{
		"pip.yaml":            "name: demo\nversion: 0.1.0\n",
		"main.go":             "package main\n\nfunc main() {}\n",
		"ouvrier.worker.json": `{"name":"demo","events":["POST /tickets"],"outcomes":["triage"]}` + "\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := DetectWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	driver := &externalFakeDriver{fakeDriver: fakeDriver{run: func(TurnRequest) error {
		runs++
		return nil
	}}}
	_, err = PatchWorker(context.Background(), driver, PatchRequest{Workspace: ws, Goal: "change it"}, nil)
	if err == nil || !strings.Contains(err.Error(), "requires a Git worktree") {
		t.Fatalf("PatchWorker() error = %v", err)
	}
	if runs != 0 {
		t.Fatalf("external driver runs = %d, want zero", runs)
	}
}

func TestExternalPatchRejectsLiveMutationDuringTurn(t *testing.T) {
	dir := writeWorkerFixture(t)
	gitInitAndCommit(t, dir)
	ws, err := DetectWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	driver := &externalFakeDriver{fakeDriver: fakeDriver{
		result: TurnResult{FinalMessage: `{"summary":"attempted","changed_files":["main.go"]}`},
		run: func(req TurnRequest) error {
			if err := os.WriteFile(filepath.Join(req.CWD, "main.go"), []byte("package main\n\nfunc main() { println(\"stage\") }\n"), 0o644); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: escaped\nversion: 0.1.0\n"), 0o644)
		},
	}}

	_, err = PatchWorker(context.Background(), driver, PatchRequest{Workspace: ws, Goal: "change it"}, nil)
	if err == nil || !strings.Contains(err.Error(), "changed during external driver turn") {
		t.Fatalf("PatchWorker() error = %v", err)
	}
	mainData, readErr := os.ReadFile(filepath.Join(dir, "main.go"))
	if readErr != nil || strings.Contains(string(mainData), "stage") {
		t.Fatalf("staged mutation reached live worker: %q, %v", mainData, readErr)
	}
}
