package operate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

// GateStatus is one audit gate outcome.
type GateStatus string

const (
	GatePass GateStatus = "pass"
	GateFail GateStatus = "fail"
	GateWarn GateStatus = "warn"
	GateSkip GateStatus = "skip"
)

// GateResult is the structured result of one deterministic audit gate.
type GateResult struct {
	Name     string        `json:"name"`
	Status   GateStatus    `json:"status"`
	Output   string        `json:"output,omitempty"`
	Error    string        `json:"error,omitempty"`
	Duration time.Duration `json:"duration"`
}

// AuditReport is persisted as audit.json.
type AuditReport struct {
	At        time.Time    `json:"at"`
	Workspace string       `json:"workspace"`
	Results   []GateResult `json:"results"`
	Passed    bool         `json:"passed"`
}

// CommandRunner is the command-execution seam for audit gates.
type CommandRunner func(ctx context.Context, dir string, name string, args []string) (stdout, stderr string, err error)

// AuditRunner executes deterministic audit gates in a candidate workspace.
type AuditRunner struct {
	RunCommand CommandRunner
	Build      func(ctx context.Context, dir string, out, errOut *bytes.Buffer) error
	Now        func() time.Time
}

// NewAuditRunner returns the default audit runner.
func NewAuditRunner() AuditRunner {
	return AuditRunner{
		RunCommand: defaultAuditCommandRunner,
		Build: func(ctx context.Context, dir string, out, errOut *bytes.Buffer) error {
			_, err := deploy.StaticBuild(ctx, dir, "linux/amd64", out, errOut, deploy.DefaultGoRunner)
			return err
		},
		Now: time.Now,
	}
}

// Run executes the v0.4 required gates.
func (r AuditRunner) Run(ctx context.Context, dir string) (AuditReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r.RunCommand == nil {
		r.RunCommand = defaultAuditCommandRunner
	}
	if r.Build == nil {
		r.Build = NewAuditRunner().Build
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return AuditReport{}, fmt.Errorf("operate: resolve audit dir: %w", err)
	}

	report := AuditReport{At: r.Now().UTC(), Workspace: abs, Passed: true}
	gates := []func(context.Context, string) GateResult{
		r.gateGitDiffCheck,
		r.gateGofmt,
		r.gateGoTest,
		r.gateGoVet,
		r.gateBuild,
		gateManifestCoherence,
		r.gateSecretScan,
	}
	for _, gate := range gates {
		result := gate(ctx, abs)
		report.Results = append(report.Results, result)
		if result.Status == GateFail {
			report.Passed = false
		}
	}
	return report, nil
}

func (r AuditRunner) gateGitDiffCheck(ctx context.Context, dir string) GateResult {
	start := time.Now()
	stdout, stderr, err := r.RunCommand(ctx, dir, "git", []string{"diff", "--check"})
	return commandGateResult("git diff --check", start, stdout, stderr, err)
}

func (r AuditRunner) gateGofmt(ctx context.Context, dir string) GateResult {
	start := time.Now()
	files, err := goFiles(dir)
	if err != nil {
		return GateResult{Name: "gofmt", Status: GateFail, Error: err.Error(), Duration: time.Since(start)}
	}
	if len(files) == 0 {
		return GateResult{Name: "gofmt", Status: GateSkip, Output: "no Go files", Duration: time.Since(start)}
	}
	args := append([]string{"-l"}, files...)
	stdout, stderr, err := r.RunCommand(ctx, dir, "gofmt", args)
	result := commandGateResult("gofmt", start, stdout, stderr, err)
	if result.Status == GatePass && strings.TrimSpace(stdout) != "" {
		result.Status = GateFail
		result.Output = strings.TrimSpace(stdout)
		result.Error = "files are not gofmt-formatted"
	}
	return result
}

func (r AuditRunner) gateGoTest(ctx context.Context, dir string) GateResult {
	start := time.Now()
	stdout, stderr, err := r.RunCommand(ctx, dir, "go", []string{"test", "./..."})
	return commandGateResult("go test ./...", start, stdout, stderr, err)
}

func (r AuditRunner) gateGoVet(ctx context.Context, dir string) GateResult {
	start := time.Now()
	stdout, stderr, err := r.RunCommand(ctx, dir, "go", []string{"vet", "./..."})
	return commandGateResult("go vet ./...", start, stdout, stderr, err)
}

func (r AuditRunner) gateBuild(ctx context.Context, dir string) GateResult {
	start := time.Now()
	var out, errOut bytes.Buffer
	err := r.Build(ctx, dir, &out, &errOut)
	return commandGateResult("ouvrier build --static --target linux/amd64", start, out.String(), errOut.String(), err)
}

func gateManifestCoherence(_ context.Context, dir string) GateResult {
	start := time.Now()
	ws, err := DetectWorkspace(dir)
	if err != nil {
		return GateResult{Name: "manifest coherence", Status: GateFail, Error: err.Error(), Duration: time.Since(start)}
	}
	var problems []string
	if ws.Name == "" {
		problems = append(problems, "missing worker name")
	}
	if _, err := os.Stat(ws.ManifestPath); err != nil {
		problems = append(problems, "missing ouvrier.worker.json")
	}
	if len(ws.Events) == 0 {
		problems = append(problems, "manifest events is empty")
	}
	if len(ws.Outcomes) == 0 {
		problems = append(problems, "manifest outcomes is empty")
	}
	if len(problems) > 0 {
		return GateResult{Name: "manifest coherence", Status: GateFail, Error: strings.Join(problems, "; "), Duration: time.Since(start)}
	}
	return GateResult{Name: "manifest coherence", Status: GatePass, Output: "pip.yaml, main.go, and worker manifest detected", Duration: time.Since(start)}
}

func (r AuditRunner) gateSecretScan(ctx context.Context, dir string) GateResult {
	start := time.Now()
	stdout, _, err := r.RunCommand(ctx, dir, "git", []string{"diff", "--cached", "--", "."})
	if err != nil || strings.TrimSpace(stdout) == "" {
		stdout, _, _ = r.RunCommand(ctx, dir, "git", []string{"diff", "--", "."})
	}
	text := stdout
	needles := []string{"OPENAI_API_KEY=", "ANTHROPIC_API_KEY=", "OUVRIER_ADMIN_TOKEN=", "AWS_SECRET_ACCESS_KEY=", "PRIVATE KEY"}
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return GateResult{Name: "secret scan", Status: GateFail, Error: "candidate diff contains token-shaped text: " + needle, Duration: time.Since(start)}
		}
	}
	return GateResult{Name: "secret scan", Status: GatePass, Output: "no obvious secrets in candidate diff", Duration: time.Since(start)}
}

func commandGateResult(name string, start time.Time, stdout, stderr string, err error) GateResult {
	out := strings.TrimSpace(stdout)
	if e := strings.TrimSpace(stderr); e != "" {
		if out != "" {
			out += "\n"
		}
		out += e
	}
	result := GateResult{Name: name, Status: GatePass, Output: out, Duration: time.Since(start)}
	if err != nil {
		result.Status = GateFail
		result.Error = err.Error()
	}
	return result
}

func defaultAuditCommandRunner(ctx context.Context, dir string, name string, args []string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func goFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			switch name {
			case ".git", ".ouvrier", "vendor", "bin", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, ".go") {
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			files = append(files, rel)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

// WriteAuditReport persists report as indented JSON.
func WriteAuditReport(path string, report AuditReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("operate: encode audit report: %w", err)
	}
	return writeAtomic(path, append(data, '\n'), 0o600)
}

// LoadAuditReport reads a persisted audit report.
func LoadAuditReport(path string) (AuditReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AuditReport{}, err
	}
	var report AuditReport
	if err := json.Unmarshal(data, &report); err != nil {
		return AuditReport{}, err
	}
	return report, nil
}
