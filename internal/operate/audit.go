package operate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const auditGateTimeout = 2 * time.Minute

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
	At                time.Time    `json:"at"`
	Workspace         string       `json:"workspace"`
	SourceSHA256      string       `json:"source_sha256"`
	SourceFiles       int          `json:"source_files"`
	SourceBytes       int64        `json:"source_bytes"`
	Toolchain         string       `json:"toolchain"`
	LocalReplacements int          `json:"local_replacements"`
	Results           []GateResult `json:"results"`
	Passed            bool         `json:"passed"`
}

// CommandRunner is the command-execution seam for audit gates.
type CommandRunner func(ctx context.Context, dir string, name string, args []string) (stdout, stderr string, err error)

// AuditRunner executes deterministic audit gates in a candidate workspace.
type AuditRunner struct {
	RunCommand CommandRunner
	Build      func(ctx context.Context, dir string, out, errOut io.Writer) error
	Now        func() time.Time
	Redactor   Redactor
}

// NewAuditRunner returns the default audit runner.
func NewAuditRunner() AuditRunner {
	// The nil execution seams are intentional: Run installs the production
	// sandbox after it has resolved and fingerprinted the concrete workspace.
	// Tests may still inject both seams explicitly without requiring bwrap.
	return AuditRunner{Now: time.Now}
}

// Run executes the v0.4 required gates.
func (r AuditRunner) Run(ctx context.Context, dir string) (AuditReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return AuditReport{}, fmt.Errorf("operate: resolve audit dir: %w", err)
	}
	r.Redactor, err = productionRedactor(abs, "", "", r.Redactor)
	if err != nil {
		return AuditReport{}, err
	}
	before, err := stableCandidateSourceSnapshot(abs)
	if err != nil {
		return AuditReport{}, fmt.Errorf("operate: fingerprint worker before audit: %w", err)
	}

	// Compiling or testing a candidate can execute attacker-controlled code.
	// Default audit execution therefore fails closed unless the OS sandbox can
	// prove the requested isolation guarantees. Explicitly injected seams are
	// retained for deterministic unit tests and specialized trusted harnesses.
	var sandbox *auditSandbox
	if (r.RunCommand == nil) != (r.Build == nil) {
		return AuditReport{}, fmt.Errorf("operate: incomplete custom audit runner: command and build seams must be supplied together")
	}
	if r.RunCommand == nil {
		sandbox, err = newAuditSandbox(ctx, abs, before)
		if err != nil {
			return AuditReport{}, err
		}
		defer func() { _ = sandbox.Close() }()
	}
	if r.RunCommand == nil {
		r.RunCommand = func(commandCtx context.Context, commandDir, name string, args []string) (string, string, error) {
			if name == "go" && len(args) > 0 && (args[0] == "test" || args[0] == "vet") {
				return sandbox.RunGo(commandCtx, commandDir, args)
			}
			return defaultAuditCommandRunner(commandCtx, commandDir, name, args)
		}
	}
	if r.Build == nil {
		r.Build = sandbox.Build
	}

	report := AuditReport{
		At:                r.Now().UTC(),
		Workspace:         before.Workspace,
		SourceSHA256:      before.SHA256,
		SourceFiles:       before.Files,
		SourceBytes:       before.Bytes,
		Toolchain:         before.Toolchain,
		LocalReplacements: before.LocalReplacements,
		Passed:            true,
	}
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
	if sandbox != nil {
		if err := sandbox.Close(); err != nil {
			return AuditReport{}, err
		}
	}
	after, err := stableCandidateSourceSnapshot(abs)
	if err != nil {
		return AuditReport{}, fmt.Errorf("operate: fingerprint worker after audit: %w", err)
	}
	if before != after {
		report.Passed = false
		report.Results = append(report.Results, GateResult{
			Name:     "source immutability",
			Status:   GateFail,
			Error:    "worker source changed while audit gates were running; the audit is bound to the pre-gate snapshot and cannot authorize a build",
			Duration: 0,
		})
	} else {
		report.Results = append(report.Results, GateResult{
			Name: "source immutability", Status: GatePass,
			Output: "pre-gate and post-gate source fingerprints match",
		})
	}
	return report, nil
}

func (r AuditRunner) gateGitDiffCheck(ctx context.Context, dir string) GateResult {
	start := time.Now()
	args, prepareErr := hardenedGitArgs("diff", "--check", "--", ".")
	if prepareErr != nil {
		return GateResult{Name: "git diff --check", Status: GateFail, Error: prepareErr.Error(), Duration: time.Since(start)}
	}
	stdout, stderr, err := r.RunCommand(ctx, dir, "git", args)
	if err != nil && strings.Contains(strings.ToLower(stdout+"\n"+stderr), "not a git repository") {
		return GateResult{
			Name: "git diff --check", Status: GateSkip,
			Output:   "worker is not a Git worktree; source fingerprinting still binds audit and build evidence",
			Duration: time.Since(start),
		}
	}
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
	gateCtx, cancel := auditTimeoutContext(ctx)
	defer cancel()
	stdout, stderr, err := r.RunCommand(gateCtx, dir, "go", []string{"test", "./..."})
	return commandGateResult("go test ./...", start, stdout, stderr, err)
}

func (r AuditRunner) gateGoVet(ctx context.Context, dir string) GateResult {
	start := time.Now()
	gateCtx, cancel := auditTimeoutContext(ctx)
	defer cancel()
	stdout, stderr, err := r.RunCommand(gateCtx, dir, "go", []string{"vet", "./..."})
	return commandGateResult("go vet ./...", start, stdout, stderr, err)
}

func (r AuditRunner) gateBuild(ctx context.Context, dir string) GateResult {
	start := time.Now()
	gateCtx, cancel := auditTimeoutContext(ctx)
	defer cancel()
	out := newBoundedOutput(maxAuditStreamBytes, "audit build stdout")
	errOut := newBoundedOutput(maxAuditStreamBytes, "audit build stderr")
	err := r.Build(gateCtx, dir, out, errOut)
	return commandGateResult("ouvrier build --static --target linux/amd64", start, out.String(), errOut.String(), err)
}

func auditTimeoutContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, auditGateTimeout)
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
	summary, err := scanBoundedWorkerSecrets(ctx, dir, r.Redactor)
	if err != nil {
		return GateResult{
			Name: "secret scan", Status: GateFail,
			Error:    "cannot safely inspect bounded worker source: " + r.Redactor.Redact(err.Error()),
			Duration: time.Since(start),
		}
	}
	return GateResult{
		Name: "secret scan", Status: GatePass,
		Output:   fmt.Sprintf("no credential-shaped material in %d safe source files (%d bytes)", summary.files, summary.bytes),
		Duration: time.Since(start),
	}
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
	if name == "git" {
		return runPreparedGit(ctx, dir, args)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	stdout := newBoundedOutput(maxAuditStreamBytes, "audit command stdout")
	stderr := newBoundedOutput(maxAuditStreamBytes, "audit command stderr")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
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
func WriteAuditReport(path string, report AuditReport, redactors ...Redactor) error {
	report = sanitizeAuditReport(mergedOptionalRedactor(redactors), report)
	return writeJSONArtifact(path, "audit report", report)
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
