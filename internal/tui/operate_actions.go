package tui

import (
	"bytes"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

func (m *operateModel) startAction(name string, cmd tea.Cmd) (*operateModel, tea.Cmd) {
	if m.err != nil {
		m.log = appendBounded(m.log, name+": "+m.err.Error())
		return m, nil
	}
	if m.harness == nil || m.session == nil {
		m.log = appendBounded(m.log, name+": no active operate session")
		return m, nil
	}
	if m.running != "" {
		m.log = appendBounded(m.log, name+": "+m.running+" already running")
		return m, nil
	}
	m.running = name
	m.log = appendBounded(m.log, name+" started")
	return m, cmd
}

func (m *operateModel) reviewCmd() tea.Cmd {
	return func() tea.Msg {
		report, err := m.harness.ReviewWorker(m.ctx, m.session, m.workspace, operate.ReviewWholeWorker, "")
		if err != nil {
			return operateActionDone{action: "review", session: m.session, err: err}
		}
		line := fmt.Sprintf("review done: %d finding(s)", len(report.Findings))
		if strings.TrimSpace(report.Summary) != "" {
			line += " - " + strings.TrimSpace(report.Summary)
		}
		return operateActionDone{action: "review", session: m.session, lines: []string{line}}
	}
}

func (m *operateModel) patchCmd() tea.Cmd {
	return func() tea.Msg {
		if strings.TrimSpace(m.opts.Goal) == "" {
			return operateActionDone{action: "patch", session: m.session, err: fmt.Errorf("operate patch requires --goal in terminal mode")}
		}
		report, err := m.harness.PatchWorker(m.ctx, m.session, m.workspace, m.opts.Goal)
		if err != nil {
			return operateActionDone{action: "patch", session: m.session, err: err}
		}
		lines := []string{"patch done: " + strings.TrimSpace(report.Summary)}
		if len(report.ChangedFiles) > 0 {
			lines = append(lines, "changed: "+strings.Join(report.ChangedFiles, ", "))
		}
		return operateActionDone{action: "patch", session: m.session, lines: lines}
	}
}

func (m *operateModel) fixCmd() tea.Cmd {
	return func() tea.Msg {
		report, err := m.harness.FixWorker(m.ctx, m.session, m.workspace, "")
		if err != nil {
			return operateActionDone{action: "fix", session: m.session, err: err}
		}
		lines := []string{"fix done: " + strings.TrimSpace(report.Summary)}
		if len(report.ChangedFiles) > 0 {
			lines = append(lines, "changed: "+strings.Join(report.ChangedFiles, ", "))
		}
		return operateActionDone{action: "fix", session: m.session, lines: lines}
	}
}

func (m *operateModel) auditCmd() tea.Cmd {
	return func() tea.Msg {
		report, err := m.harness.RunAudit(m.ctx, m.session, operate.CandidateDir(m.session, m.workspace))
		if err != nil {
			return operateActionDone{action: "audit", session: m.session, err: err}
		}
		status := "failed"
		if report.Passed {
			status = "passed"
		}
		return operateActionDone{action: "audit", session: m.session, lines: []string{fmt.Sprintf("audit %s: %d gate(s)", status, len(report.Results))}}
	}
}

func (m *operateModel) buildCmd() tea.Cmd {
	return func() tea.Msg {
		auditPassed := operate.LatestAuditPassed(m.session.AuditPath)
		if !auditPassed && !m.opts.AllowFail {
			return operateActionDone{action: "build", session: m.session, err: fmt.Errorf("build requires passing audit or --allow-failed")}
		}
		var out, errOut bytes.Buffer
		artifact, err := m.harness.Build(m.ctx, m.session, operate.CandidateDir(m.session, m.workspace), m.opts.Target, auditPassed, operate.ProgressWriter{Out: &out, Err: &errOut})
		if err != nil {
			return operateActionDone{action: "build", session: m.session, err: err}
		}
		return operateActionDone{action: "build", session: m.session, lines: compactLines("build done: "+artifact.BinaryPath, out.String(), errOut.String())}
	}
}

func (m *operateModel) transferCmd() tea.Cmd {
	return func() tea.Msg {
		if strings.TrimSpace(m.opts.Env) == "" {
			return operateActionDone{action: "transfer", session: m.session, err: fmt.Errorf("transfer requires --env")}
		}
		var out, errOut bytes.Buffer
		report, err := m.harness.Transfer(m.ctx, m.session, operate.TransferRequest{
			Dir:          operate.CandidateDir(m.session, m.workspace),
			Env:          m.opts.Env,
			EnvFile:      m.opts.EnvFile,
			Target:       m.opts.Target,
			Keep:         m.opts.Keep,
			AllowFailed:  m.opts.AllowFail,
			AuditPassed:  operate.LatestAuditPassed(m.session.AuditPath),
			ReviewPassed: operate.ReviewPassed(m.session.ReviewPath),
		}, operate.ProgressWriter{Out: &out, Err: &errOut})
		if err != nil {
			return operateActionDone{action: "transfer", session: m.session, err: err}
		}
		return operateActionDone{action: "transfer", session: m.session, lines: compactLines("transfer done: "+report.Request.Env, out.String(), errOut.String())}
	}
}
