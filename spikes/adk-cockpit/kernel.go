package adkspike

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagents/loopagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	adktool "google.golang.org/adk/v2/tool"
)

// RepairConfig is intentionally closed over the Ouvrier product boundary.
// Callers provide a model and declarative tool metadata; they cannot inject an
// arbitrary ADK agent, workflow or raw ADK tool.
type RepairConfig struct {
	AppName       string
	UserID        string
	Model         model.LLM
	Tools         []ToolSpec
	Executor      GovernedExecutor
	MaxIterations uint
}

// Kernel is the minimal ADK runner/session adapter exercised by this spike.
type Kernel struct {
	appName         string
	userID          string
	runner          *runner.Runner
	sessions        session.Service
	completionTools map[string]bool
	proofs          *proofTracker
}

// NewRepairKernel builds the LLM agent and bounded LoopAgent internally. This
// is the only product constructor in the spike.
func NewRepairKernel(cfg RepairConfig) (*Kernel, error) {
	if cfg.Model == nil {
		return nil, errors.New("adk spike: model is required")
	}
	if cfg.Executor == nil {
		return nil, errors.New("adk spike: governed executor is required")
	}
	if cfg.MaxIterations == 0 {
		return nil, errors.New("adk spike: MaxIterations must be greater than zero")
	}
	if len(cfg.Tools) == 0 {
		return nil, errors.New("adk spike: at least one governed tool is required")
	}

	proofs := newProofTracker()
	completionTools := make(map[string]bool, len(cfg.Tools))
	seen := make(map[string]struct{}, len(cfg.Tools))
	tools := make([]adktool.Tool, 0, len(cfg.Tools))
	hasCompletionTool := false
	for _, declared := range cfg.Tools {
		declared.Name = strings.TrimSpace(declared.Name)
		if declared.Name == "" {
			return nil, errors.New("adk spike: tool name is required")
		}
		if _, exists := seen[declared.Name]; exists {
			return nil, fmt.Errorf("adk spike: duplicate tool %q", declared.Name)
		}
		seen[declared.Name] = struct{}{}
		completionTools[declared.Name] = declared.CompletesWhenVerified
		hasCompletionTool = hasCompletionTool || declared.CompletesWhenVerified
		wrapped, err := newGovernedTool(declared, cfg.Executor, proofs)
		if err != nil {
			return nil, err
		}
		tools = append(tools, wrapped)
	}
	if !hasCompletionTool {
		return nil, errors.New("adk spike: at least one completion evidence tool is required")
	}

	stage, err := llmagent.New(llmagent.Config{
		Name:  "ouvrier_worker_builder",
		Model: cfg.Model,
		Tools: tools,
	})
	if err != nil {
		return nil, fmt.Errorf("build Ouvrier worker agent: %w", err)
	}
	root, err := loopagent.New(loopagent.Config{
		AgentConfig: agent.Config{
			Name:      "ouvrier_worker_repair_loop",
			SubAgents: []agent.Agent{stage},
		},
		MaxIterations: cfg.MaxIterations,
	})
	if err != nil {
		return nil, fmt.Errorf("build bounded Ouvrier repair loop: %w", err)
	}

	appName := strings.TrimSpace(cfg.AppName)
	if appName == "" {
		appName = "ouvrier-cockpit"
	}
	userID := strings.TrimSpace(cfg.UserID)
	if userID == "" {
		userID = "operator"
	}
	sessions := session.InMemoryService()
	adkRunner, err := runner.New(runner.Config{
		AppName:        appName,
		Agent:          root,
		SessionService: sessions,
	})
	if err != nil {
		return nil, fmt.Errorf("build ADK runner: %w", err)
	}
	return &Kernel{
		appName:         appName,
		userID:          userID,
		runner:          adkRunner,
		sessions:        sessions,
		completionTools: completionTools,
		proofs:          proofs,
	}, nil
}

func (k *Kernel) CreateSession(ctx context.Context, id string) error {
	if k == nil || k.sessions == nil {
		return errors.New("adk spike: nil kernel")
	}
	id, err := normalizeSessionID(id)
	if err != nil {
		return err
	}
	_, err = k.sessions.Create(ctx, &session.CreateRequest{
		AppName:   k.appName,
		UserID:    k.userID,
		SessionID: id,
	})
	return err
}

// Run streams normalized events. Its final live event is always EventOutcome.
// EventFinal is reserved for a verified, governed completion proof and is
// never inferred from ADK's model-response finality flag.
func (k *Kernel) Run(ctx context.Context, sessionID, prompt string) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		if k == nil || k.runner == nil {
			err := errors.New("adk spike: nil kernel")
			if !yield(Event{Kind: EventOutcome, Outcome: OutcomeFailed}, nil) {
				return
			}
			yield(Event{}, err)
			return
		}
		resolvedID, err := normalizeSessionID(sessionID)
		if err != nil {
			if !yield(outcomeEvent("", sessionID, OutcomeFailed), nil) {
				return
			}
			yield(Event{}, err)
			return
		}

		message := genai.NewContentFromText(prompt, genai.RoleUser)
		lastInvocationID := ""
		verified := false
		defer func() {
			k.proofs.purgeInvocation(lastInvocationID)
		}()
		for raw, runErr := range k.runner.Run(
			ctx,
			k.userID,
			resolvedID,
			message,
			agent.RunConfig{StreamingMode: agent.StreamingModeSSE},
			runner.WithYieldUserMessage(),
		) {
			if runErr != nil {
				outcome := classifyErrorOutcome(ctx, runErr)
				if !yield(outcomeEvent(lastInvocationID, resolvedID, outcome), nil) {
					return
				}
				yield(Event{}, runErr)
				return
			}
			events, normalizeErr := normalizeEvent(raw, k.completionTools, k.proofs)
			if normalizeErr != nil {
				if !yield(outcomeEvent(lastInvocationID, resolvedID, OutcomeFailed), nil) {
					return
				}
				yield(Event{}, normalizeErr)
				return
			}
			for _, event := range events {
				if event.InvocationID != "" {
					lastInvocationID = event.InvocationID
				}
				verified = verified || event.Kind == EventFinal
				if !yield(event, nil) {
					return
				}
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			if !yield(outcomeEvent(lastInvocationID, resolvedID, OutcomeCancelled), nil) {
				return
			}
			yield(Event{}, ctxErr)
			return
		}
		outcome := OutcomeExhausted
		if verified {
			outcome = OutcomeVerified
		}
		yield(outcomeEvent(lastInvocationID, resolvedID, outcome), nil)
	}
}

// ReplayPersisted replays completed events stored by the current in-memory ADK
// service. It intentionally excludes live partial deltas and synthetic outcome
// events; neither is claimed as durable by this spike.
func (k *Kernel) ReplayPersisted(ctx context.Context, sessionID string) ([]Event, error) {
	if k == nil || k.sessions == nil {
		return nil, errors.New("adk spike: nil kernel")
	}
	resolvedID, err := normalizeSessionID(sessionID)
	if err != nil {
		return nil, err
	}
	response, err := k.sessions.Get(ctx, &session.GetRequest{
		AppName:   k.appName,
		UserID:    k.userID,
		SessionID: resolvedID,
	})
	if err != nil {
		return nil, err
	}
	var events []Event
	for raw := range response.Session.Events().All() {
		if raw == nil || raw.Partial {
			continue
		}
		// Replay cannot recertify completion: the in-memory proof tracker is
		// deliberately live-only and a durable Ouvrier proof journal is still
		// a production NO-GO criterion.
		normalized, err := normalizeEvent(raw, k.completionTools, nil)
		if err != nil {
			return nil, err
		}
		events = append(events, normalized...)
	}
	return events, nil
}

func normalizeSessionID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("adk spike: session ID is required")
	}
	return id, nil
}
