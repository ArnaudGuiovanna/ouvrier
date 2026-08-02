package operate

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
)

const (
	evidenceWorkerState  = "worker_diff_or_state"
	evidencePassingAudit = "passing_audit"
	evidenceBuildSHA256  = "build_artifact_sha256"
)

// agentCompletionGate is reconstructed from the durable transcript at every
// model turn. Mutation attempts activate it before execution, so an
// interrupted/denied/failed write cannot be forgotten by a later Prompt.
// Model text never enters this state machine.
type agentCompletionGate struct {
	required            bool
	workerObserved      bool
	auditObserved       bool
	buildObserved       bool
	auditSourceSHA256   string
	auditArtifactSHA256 string
}

func completionGateFromTranscript(entries []TranscriptEntry, session *Session) agentCompletionGate {
	gate := agentCompletionGate{}
	for _, entry := range entries {
		switch entry.Kind {
		case TranscriptToolCall:
			gate.observeAttempt(entry.ToolName)
		case TranscriptToolResult:
			result := ToolResult{Data: entry.Output}
			if summary, ok := entry.Output["summary"].(string); ok {
				result.Summary = summary
			}
			var runErr error
			if message, ok := entry.Output["error"].(string); ok && strings.TrimSpace(message) != "" {
				runErr = errors.New(message)
			}
			gate.observe(entry.ToolName, result, runErr)
		}
	}
	gate.reconcilePersistedEvidence(session)
	return gate
}

func (g *agentCompletionGate) observeAttempt(tool string) {
	if !isMutationTool(tool) {
		return
	}
	g.required = true
	g.workerObserved = false
	g.auditObserved = false
	g.buildObserved = false
	g.auditSourceSHA256 = ""
	g.auditArtifactSHA256 = ""
}

func (g *agentCompletionGate) requireMutationIntent() { g.observeAttempt("write_worker_file") }

func isMutationTool(tool string) bool {
	switch tool {
	case "scaffold_worker", "patch_worker", "fix_worker", "write_worker_file", "remove_worker_file":
		return true
	default:
		return false
	}
}

func (g *agentCompletionGate) observe(tool string, result ToolResult, runErr error) {
	if runErr != nil {
		if !g.required {
			return
		}
		switch tool {
		case "audit_worker":
			g.auditObserved = false
			g.buildObserved = false
			g.auditSourceSHA256 = ""
			g.auditArtifactSHA256 = ""
		case "build_worker":
			g.buildObserved = false
		}
		return
	}

	switch tool {
	case "scaffold_worker", "patch_worker", "fix_worker", "write_worker_file", "remove_worker_file":
		observed := mutationResultObserved(tool, result.Data)
		if !observed {
			// A successful no-op (for example ManualDriver making no edits) is
			// not a construction/repair completion claim and does not activate
			// the gate. Only Ouvrier-observed state or diff evidence does.
			return
		}
		// Any later mutation invalidates evidence collected for the previous
		// candidate. The mutation's own structured result may establish the
		// initial file/diff proof, but audit and build must be observed again.
		g.required = true
		g.workerObserved = true
		g.auditObserved = false
		g.buildObserved = false
		g.auditSourceSHA256 = ""
		g.auditArtifactSHA256 = ""
	case "diff_worker":
		// A diff is inspection evidence, never mutation provenance. It may be
		// empty outside Git, or may describe edits that predate this session.
		// Only the structured result of an executed Ouvrier mutation tool can
		// satisfy workerObserved.
	case "audit_worker":
		if g.required {
			passed, _ := result.Data["passed"].(bool)
			sourceSHA := resultDataString(result.Data, "source_sha256")
			auditSHA := resultDataString(result.Data, "audit_sha256")
			g.auditObserved = passed && resultDataString(result.Data, "audit_path") != "" &&
				isSHA256(sourceSHA) && isSHA256(auditSHA)
			g.buildObserved = false
			if g.auditObserved {
				g.auditSourceSHA256 = sourceSHA
				g.auditArtifactSHA256 = auditSHA
			} else {
				g.auditSourceSHA256 = ""
				g.auditArtifactSHA256 = ""
			}
		}
	case "build_worker":
		if g.required {
			auditPassed, _ := result.Data["audit_passed"].(bool)
			g.buildObserved = g.auditObserved && auditPassed &&
				resultDataString(result.Data, "binary_path") != "" &&
				isSHA256(resultDataString(result.Data, "sha256")) &&
				resultDataString(result.Data, "source_sha256") == g.auditSourceSHA256 &&
				resultDataString(result.Data, "audit_sha256") == g.auditArtifactSHA256
		}
	}
}

func (g *agentCompletionGate) reconcilePersistedEvidence(session *Session) {
	if session == nil || !g.required || !g.complete() {
		return
	}
	audit, err := CurrentAuditEvidence(session.AuditPath, session.Dir)
	if err != nil || audit.Report.SourceSHA256 != g.auditSourceSHA256 || audit.ArtifactSHA256 != g.auditArtifactSHA256 {
		g.auditObserved = false
		g.buildObserved = false
		return
	}
	data, err := os.ReadFile(session.BuildPath)
	if err != nil {
		g.buildObserved = false
		return
	}
	var artifact BuildArtifact
	if json.Unmarshal(data, &artifact) != nil || !artifact.AuditPassed ||
		artifact.SourceSHA256 != audit.Report.SourceSHA256 || artifact.AuditSHA256 != audit.ArtifactSHA256 ||
		!isSHA256(artifact.SHA256) || strings.TrimSpace(artifact.BinaryPath) == "" {
		g.buildObserved = false
		return
	}
	binarySHA, err := fileSHA256(artifact.BinaryPath)
	if err != nil || binarySHA != artifact.SHA256 {
		g.buildObserved = false
	}
}

func mutationIntent(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" || strings.HasPrefix(lower, "/") {
		return false
	}
	for _, marker := range []string{
		"create", "scaffold", "construct", "implement", "add ", "write ", "edit ", "modify", "change", "update", "patch", "fix", "repair", "remove", "delete",
		"crée", "creer", "créer", "constru", "fabri", "implément", "implement", "ajout", "écri", "ecri", "modifi", "change", "met à jour", "corrig", "répar", "repar", "supprim",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func (g agentCompletionGate) complete() bool {
	return !g.required || g.workerObserved && g.auditObserved && g.buildObserved
}

func (g agentCompletionGate) missingEvidence() []string {
	if !g.required {
		return nil
	}
	missing := make([]string, 0, 3)
	if !g.workerObserved {
		missing = append(missing, evidenceWorkerState)
	}
	if !g.auditObserved {
		missing = append(missing, evidencePassingAudit)
	}
	if !g.buildObserved {
		missing = append(missing, evidenceBuildSHA256)
	}
	return missing
}

func (g agentCompletionGate) request() (string, error) {
	missing := g.missingEvidence()
	requiredTools := make([]string, 0, len(missing))
	for _, evidence := range missing {
		switch evidence {
		case evidenceWorkerState:
			requiredTools = append(requiredTools, "write_worker_file")
		case evidencePassingAudit:
			requiredTools = append(requiredTools, "audit_worker")
		case evidenceBuildSHA256:
			requiredTools = append(requiredTools, "build_worker")
		}
	}
	payload := struct {
		Type            string   `json:"type"`
		Outcome         string   `json:"outcome"`
		MissingEvidence []string `json:"missing_evidence"`
		RequiredTools   []string `json:"required_tools"`
		Instruction     string   `json:"instruction"`
	}{
		Type:            "ouvrier_completion_evidence_request",
		Outcome:         "incomplete",
		MissingEvidence: missing,
		RequiredTools:   requiredTools,
		Instruction:     "Continue the tool loop and obtain every missing proof through executed Ouvrier tools; model text is not completion evidence.",
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func mutationResultObserved(tool string, data map[string]any) bool {
	switch tool {
	case "scaffold_worker":
		return resultDataString(data, "name") != "" && resultDataString(data, "dir") != ""
	case "patch_worker", "fix_worker":
		return resultDataString(data, "diff_path") != "" && resultHasChangedFiles(data)
	case "write_worker_file", "remove_worker_file":
		changed, _ := data["changed"].(bool)
		return changed && resultDataString(data, "path") != "" && isSHA256(resultDataString(data, "source_sha256"))
	default:
		return false
	}
}

func resultHasChangedFiles(data map[string]any) bool {
	if data == nil {
		return false
	}
	switch files := data["changed_files"].(type) {
	case []string:
		for _, file := range files {
			if strings.TrimSpace(file) != "" {
				return true
			}
		}
	case []any:
		for _, file := range files {
			if text, ok := file.(string); ok && strings.TrimSpace(text) != "" {
				return true
			}
		}
	}
	return false
}

func resultDataString(data map[string]any, key string) string {
	if data == nil {
		return ""
	}
	value, _ := data[key].(string)
	return strings.TrimSpace(value)
}

func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
