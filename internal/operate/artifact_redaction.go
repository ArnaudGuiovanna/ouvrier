package operate

import (
	"encoding/json"
	"fmt"
)

// writeJSONArtifact is the single serializer for durable JSON reports. Report
// specific helpers sanitize free-form model/command text before it reaches this
// boundary; provenance fields such as source hashes remain byte-exact.
func writeJSONArtifact(path, label string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("operate: encode %s: %w", label, err)
	}
	return writeAtomic(path, append(data, '\n'), 0o600)
}

func sanitizeAuditReport(redactor Redactor, report AuditReport) AuditReport {
	for i := range report.Results {
		report.Results[i].Name = redactor.Redact(report.Results[i].Name)
		report.Results[i].Output = redactor.Redact(report.Results[i].Output)
		report.Results[i].Error = redactor.Redact(report.Results[i].Error)
	}
	return report
}

func sanitizeReviewReport(redactor Redactor, report ReviewReport) ReviewReport {
	report.Summary = redactor.Redact(report.Summary)
	report.Raw = redactor.Redact(report.Raw)
	for i := range report.Findings {
		report.Findings[i] = redactFinding(redactor, report.Findings[i])
	}
	return report
}

func sanitizePatchReport(redactor Redactor, report PatchReport) PatchReport {
	report.Goal = redactor.Redact(report.Goal)
	report.Subject = redactor.Redact(report.Subject)
	report.Summary = redactor.Redact(report.Summary)
	report.Raw = redactor.Redact(report.Raw)
	report.ChangedFiles = redactStrings(redactor, report.ChangedFiles)
	report.Diff.ChangedFiles = redactStrings(redactor, report.Diff.ChangedFiles)
	report.Diff.Diff = redactor.Redact(report.Diff.Diff)
	report.Diff.Status = redactor.Redact(report.Diff.Status)
	return report
}

func sanitizeTransferReport(redactor Redactor, report TransferReport) TransferReport {
	report.Error = redactor.Redact(report.Error)
	report.Request.Dir = redactor.Redact(report.Request.Dir)
	report.Request.Env = redactor.Redact(report.Request.Env)
	report.Request.EnvFile = redactor.Redact(report.Request.EnvFile)
	report.Request.Target = redactor.Redact(report.Request.Target)
	report.Request.AuditPath = redactor.Redact(report.Request.AuditPath)
	report.Request.ReviewPath = redactor.Redact(report.Request.ReviewPath)
	return report
}

func mergedOptionalRedactor(redactors []Redactor) Redactor {
	if len(redactors) == 0 {
		return Redactor{}
	}
	return MergeRedactors(redactors...)
}
