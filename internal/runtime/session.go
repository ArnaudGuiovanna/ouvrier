package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Budget struct {
	MaxIterations int
	MaxTokens     int
	MaxCostUSD    float64
}

type Session struct {
	ExecID          string
	SessionID       string
	ParentSessionID string
	TraceID         string
	Model           string
	StartedAt       time.Time
	Budget          Budget
}

type SessionOption func(*sessionConfig) error

type sessionConfig struct {
	execID          string
	sessionID       string
	parentSessionID string
	traceID         string
	budget          Budget
	now             func() time.Time
}

func NewSession(model string, opts ...SessionOption) (Session, error) {
	cfg := defaultSessionConfig()
	if err := applySessionOptions(&cfg, opts); err != nil {
		return Session{}, err
	}
	return buildSession(strings.TrimSpace(model), cfg)
}

func NewChildSession(parent Session, model string, opts ...SessionOption) (Session, error) {
	if parent.ExecID == "" || parent.SessionID == "" || parent.TraceID == "" {
		return Session{}, errors.New("parent session must include exec, session, and trace IDs")
	}
	cfg := defaultSessionConfig()
	cfg.execID = parent.ExecID
	cfg.parentSessionID = parent.SessionID
	cfg.traceID = parent.TraceID
	cfg.budget = parent.Budget
	if err := applySessionOptions(&cfg, opts); err != nil {
		return Session{}, err
	}
	return buildSession(strings.TrimSpace(model), cfg)
}

func WithSessionIDs(execID, sessionID, traceID string) SessionOption {
	return func(cfg *sessionConfig) error {
		if execID = strings.TrimSpace(execID); execID != "" {
			cfg.execID = execID
		}
		if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
			cfg.sessionID = sessionID
		}
		if traceID = strings.TrimSpace(traceID); traceID != "" {
			cfg.traceID = traceID
		}
		return nil
	}
}

func WithSessionBudget(budget Budget) SessionOption {
	return func(cfg *sessionConfig) error {
		cfg.budget = budget
		return nil
	}
}

func WithSessionClock(now func() time.Time) SessionOption {
	return func(cfg *sessionConfig) error {
		if now == nil {
			return errors.New("session clock is required")
		}
		cfg.now = now
		return nil
	}
}

func defaultSessionConfig() sessionConfig {
	return sessionConfig{now: time.Now}
}

func applySessionOptions(cfg *sessionConfig, opts []SessionOption) error {
	for _, opt := range opts {
		if opt == nil {
			return errors.New("nil session option")
		}
		if err := opt(cfg); err != nil {
			return err
		}
	}
	return nil
}

func buildSession(model string, cfg sessionConfig) (Session, error) {
	if model == "" {
		return Session{}, errors.New("session model is required")
	}
	return Session{
		ExecID:          ensureID("exec", cfg.execID),
		SessionID:       ensureID("sess", cfg.sessionID),
		ParentSessionID: cfg.parentSessionID,
		TraceID:         ensureID("trace", cfg.traceID),
		Model:           model,
		StartedAt:       cfg.now().UTC(),
		Budget:          cfg.budget,
	}, nil
}

func ensureID(prefix, current string) string {
	if current != "" {
		return current
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(random)
}
