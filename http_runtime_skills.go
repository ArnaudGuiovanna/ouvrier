package ovr

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/sandbox"
	"github.com/ArnaudGuiovanna/ouvrier/internal/skills"
)

func (rt httpRuntime) systemPromptForStep(ctx context.Context, step runtimeplan.Step, scope planRunScope) (string, error) {
	if len(step.Skills) == 0 {
		return step.Goal, nil
	}
	workspace, err := rt.skillWorkspace()
	if err != nil {
		return "", err
	}

	var builder strings.Builder
	builder.WriteString(step.Goal)
	builder.WriteString("\n\n## Skills\n")

	for _, skill := range step.Skills {
		loaded, err := skills.Load(ctx, workspace, skill.Name)
		if err != nil {
			return "", err
		}
		if err := rt.emitSkillLoaded(ctx, scope, loaded); err != nil {
			return "", err
		}
		builder.WriteString("\n### ")
		builder.WriteString(loaded.Name)
		builder.WriteString("\n\nDescription: ")
		builder.WriteString(loaded.Description)
		builder.WriteString("\n\n")
		builder.WriteString(loaded.Body)
		builder.WriteByte('\n')
	}

	return builder.String(), nil
}

func (rt httpRuntime) skillWorkspace() (*sandbox.Sandbox, error) {
	if rt.sandbox != nil {
		return rt.sandbox, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("skill workspace: %w", err)
	}
	workspace, err := sandbox.New(cwd)
	if err != nil {
		return nil, fmt.Errorf("skill workspace: %w", err)
	}
	return workspace, nil
}

func (rt httpRuntime) emitSkillLoaded(ctx context.Context, scope planRunScope, loaded skills.LoadedSkill) error {
	payload := map[string]any{
		"name":        loaded.Name,
		"description": loaded.Description,
		"path":        "skills/" + loaded.Name + "/SKILL.md",
	}
	if scope.parentSession != nil {
		return rt.emitSessionEvent(ctx, *scope.parentSession, events.EventSkillLoaded, payload)
	}
	return rt.emitRuntimeEvent(ctx, planRunResult{}, events.EventSkillLoaded, payload)
}
