package acp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

type sessionResponse struct {
	SessionID     string                `json:"sessionId"`
	ConfigOptions []sessionConfigOption `json:"configOptions"`
}

type sessionConfigOption struct {
	ID           string                `json:"id"`
	CurrentValue any                   `json:"currentValue"`
	Options      []sessionConfigChoice `json:"options"`
}

type sessionConfigChoice struct {
	Value any `json:"value"`
}

func governedSessionParams(req operate.TurnRequest) map[string]interface{} {
	return map[string]interface{}{
		"cwd":        req.CWD,
		"mcpServers": []interface{}{},
		"_meta": map[string]interface{}{
			"claudeCode": map[string]interface{}{
				"options": map[string]interface{}{
					"settingSources": []string{},
					"tools":          []string{},
					"disallowedTools": []string{
						"Agent", "AskUserQuestion", "Bash", "BashOutput", "EnterPlanMode",
						"ExitPlanMode", "Glob", "Grep", "KillShell", "NotebookEdit", "Read",
						"Skill", "Task", "TaskCreate", "TaskGet", "TaskList", "TaskUpdate",
						"TodoWrite", "WebFetch", "WebSearch",
					},
				},
			},
		},
	}
}

func (c *client) enforceGovernedMode(ctx context.Context, session sessionResponse) error {
	if strings.TrimSpace(session.SessionID) == "" {
		return errors.New("create session: ACP agent returned an empty sessionId")
	}
	var safeMode any
	modeAvailable := false
	for _, option := range session.ConfigOptions {
		if option.ID == "mode" {
			modeAvailable = true
			var ok bool
			safeMode, ok = safeACPModeValue(option)
			if !ok {
				return errors.New("create session: ACP agent does not expose a safe controllable permission mode")
			}
			break
		}
	}
	if !modeAvailable {
		return errors.New("create session: ACP agent does not expose a controllable permission mode")
	}
	var response struct {
		ConfigOptions []interface{} `json:"configOptions"`
	}
	if err := c.call(ctx, "session/set_config_option", map[string]interface{}{
		"sessionId": session.SessionID,
		"configId":  "mode",
		"value":     safeMode,
	}, &response); err != nil {
		return fmt.Errorf("enforce default ACP permission mode: %w", err)
	}
	return nil
}

func safeACPModeValue(option sessionConfigOption) (any, bool) {
	// Codex ACP calls the constrained mode "read-only"; Claude ACP calls it
	// "default". Prefer those exact adapter-owned values and reject unknown
	// mode catalogs instead of guessing at their privilege semantics.
	for _, wanted := range []string{"read-only", "default"} {
		for _, candidate := range option.Options {
			if value, ok := candidate.Value.(string); ok && value == wanted {
				return wanted, true
			}
		}
	}
	return nil, false
}
