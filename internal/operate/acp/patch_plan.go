package acp

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

const (
	maxPatchPlanFiles = 64
	maxPatchFileBytes = 1 << 20
	maxPatchPlanBytes = 16 << 20
	patchPlanSchema   = `{"type":"object","properties":{"summary":{"type":"string","minLength":1},"files":{"type":"array","minItems":1,"maxItems":64,"items":{"type":"object","properties":{"path":{"type":"string","minLength":1},"content":{"type":"string","maxLength":1048576}},"required":["path","content"],"additionalProperties":false}}},"required":["summary","files"],"additionalProperties":false}`
)

type patchPlan struct {
	Summary string          `json:"summary"`
	Files   []patchPlanFile `json:"files"`
}

type patchPlanFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func applyPatchPlan(req operate.TurnRequest, result operate.TurnResult, sink operate.EventSink) (operate.TurnResult, error) {
	plan, err := decodePatchPlan(result.FinalMessage, req.CWD)
	if err != nil {
		return result, fmt.Errorf("decode governed ACP patch plan: %w", err)
	}
	seen := make(map[string]struct{}, len(plan.Files))
	changed := make([]string, 0, len(plan.Files))
	total := 0
	for _, file := range plan.Files {
		requested := strings.TrimSpace(file.Path)
		clean := filepath.Clean(requested)
		path := filepath.ToSlash(clean)
		if requested == "" || clean == "." || filepath.IsAbs(clean) || clean == ".." ||
			strings.HasPrefix(clean, ".."+string(filepath.Separator)) || sensitiveACPPath(clean) {
			return result, fmt.Errorf("ACP patch plan contains unsafe path %q", req.Redactor.Redact(requested))
		}
		if _, duplicate := seen[path]; duplicate {
			return result, fmt.Errorf("ACP patch plan repeats path %q", req.Redactor.Redact(path))
		}
		seen[path] = struct{}{}
		if len(file.Content) > maxPatchFileBytes {
			return result, fmt.Errorf("ACP patch plan file %q exceeds %d bytes", req.Redactor.Redact(path), maxPatchFileBytes)
		}
		if !utf8.ValidString(file.Content) {
			return result, fmt.Errorf("ACP patch plan file %q is not valid UTF-8 text", req.Redactor.Redact(path))
		}
		if total > maxPatchPlanBytes-len(file.Content) {
			return result, fmt.Errorf("ACP patch plan exceeds %d bytes", maxPatchPlanBytes)
		}
		total += len(file.Content)
		changed = append(changed, path)
	}

	workspace := operate.Workspace{Dir: req.CWD}
	for i, file := range plan.Files {
		path := changed[i]
		if err := emitPatchEvent(sink, operate.Event{At: time.Now().UTC(), Kind: operate.EventCommandStarted, Command: "write_worker_file", Path: req.Redactor.Redact(path), Metadata: map[string]interface{}{"transport": "acp/v1", "source": "structured_patch_plan"}}); err != nil {
			return result, err
		}
		if err := operate.WriteWorkerFile(workspace, path, file.Content); err != nil {
			_ = emitPatchEvent(sink, operate.Event{At: time.Now().UTC(), Kind: operate.EventCommandFinished, Command: "write_worker_file", Path: req.Redactor.Redact(path), ExitCode: 1, Metadata: map[string]interface{}{"transport": "acp/v1", "source": "structured_patch_plan"}})
			return result, fmt.Errorf("apply ACP patch file %q: %s", req.Redactor.Redact(path), req.Redactor.Redact(err.Error()))
		}
		if err := emitPatchEvent(sink, operate.Event{At: time.Now().UTC(), Kind: operate.EventCommandFinished, Command: "write_worker_file", Path: req.Redactor.Redact(path), Metadata: map[string]interface{}{"transport": "acp/v1", "source": "structured_patch_plan"}}); err != nil {
			return result, err
		}
		if err := emitPatchEvent(sink, operate.Event{At: time.Now().UTC(), Kind: operate.EventFileChanged, Path: req.Redactor.Redact(path), Metadata: map[string]interface{}{"transport": "acp/v1", "source": "structured_patch_plan"}}); err != nil {
			return result, err
		}
	}

	normalized, err := json.Marshal(struct {
		Summary      string   `json:"summary"`
		ChangedFiles []string `json:"changed_files"`
	}{Summary: req.Redactor.Redact(strings.TrimSpace(plan.Summary)), ChangedFiles: changed})
	if err != nil {
		return result, err
	}
	result.FinalMessage = string(normalized)
	return result, nil
}

func decodePatchPlan(raw, cwd string) (patchPlan, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		if newline := strings.IndexByte(raw, '\n'); newline >= 0 {
			raw = raw[newline+1:]
		}
		raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "```"))
	}
	plan, strictErr := decodeStrictPatchPlan(raw)
	if strictErr == nil {
		return validatePatchPlanShape(plan)
	}
	plan, envelopeErr := decodeToollessFullFileEnvelope(raw, cwd)
	if envelopeErr == nil {
		return validatePatchPlanShape(plan)
	}
	return patchPlan{}, fmt.Errorf("strict plan: %v; canonical full-file envelope: %v", strictErr, envelopeErr)
}

func decodeStrictPatchPlan(raw string) (patchPlan, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var plan patchPlan
	if err := decoder.Decode(&plan); err != nil {
		return patchPlan{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return patchPlan{}, fmt.Errorf("multiple JSON values")
		}
		return patchPlan{}, fmt.Errorf("trailing JSON data: %w", err)
	}
	return plan, nil
}

func validatePatchPlanShape(plan patchPlan) (patchPlan, error) {
	if strings.TrimSpace(plan.Summary) == "" {
		return patchPlan{}, fmt.Errorf("summary is required")
	}
	if len(plan.Files) == 0 || len(plan.Files) > maxPatchPlanFiles {
		return patchPlan{}, fmt.Errorf("files must contain between 1 and %d entries", maxPatchPlanFiles)
	}
	return plan, nil
}

func decodeToollessFullFileEnvelope(raw, cwd string) (patchPlan, error) {
	type fullFileEdit struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	raw = strings.TrimSpace(raw)
	if strings.Count(raw, "\n")+1 > maxPatchPlanFiles {
		return patchPlan{}, fmt.Errorf("full-file envelope exceeds %d records", maxPatchPlanFiles)
	}
	lines := strings.Split(raw, "\n")
	files := make([]patchPlanFile, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "* ") {
			return patchPlan{}, fmt.Errorf("expected one or more '* {JSON}' records")
		}
		decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(strings.TrimPrefix(line, "* "))))
		decoder.DisallowUnknownFields()
		var edit fullFileEdit
		if err := decoder.Decode(&edit); err != nil {
			return patchPlan{}, fmt.Errorf("decode record: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			if err == nil {
				return patchPlan{}, fmt.Errorf("record contains multiple JSON values")
			}
			return patchPlan{}, fmt.Errorf("record has trailing JSON data: %w", err)
		}
		path, err := stageRelativePath(cwd, edit.FilePath)
		if err != nil {
			return patchPlan{}, err
		}
		files = append(files, patchPlanFile{Path: path, Content: edit.Content})
	}
	return patchPlan{
		Summary: fmt.Sprintf("Applied %d governed ACP full-file edit(s).", len(files)),
		Files:   files,
	}, nil
}

func stageRelativePath(cwd, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", fmt.Errorf("full-file record path is required")
	}
	if !filepath.IsAbs(requested) {
		return requested, nil
	}
	root, err := filepath.Abs(strings.TrimSpace(cwd))
	if err != nil || strings.TrimSpace(cwd) == "" {
		return "", fmt.Errorf("resolve staged root for full-file record")
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(requested))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("full-file record path escapes the staged worker")
	}
	return rel, nil
}

func emitPatchEvent(sink operate.EventSink, event operate.Event) error {
	if sink == nil {
		return nil
	}
	return sink.Event(event)
}
