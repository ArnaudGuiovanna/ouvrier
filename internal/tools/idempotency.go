package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/policy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

type idempotencyStore interface {
	ReserveIdempotency(context.Context, string, string) (string, bool, error)
}

type idempotencyContext struct {
	store     idempotencyStore
	execID    string
	namespace string
}

type idempotencyContextKey struct{}
type idempotencyReplayContextKey struct{}

type idempotencyClaim struct {
	store  state.IdempotencyOutcomeStore
	key    string
	execID string
}

func ContextWithIdempotencyStore(ctx context.Context, store idempotencyStore, execID string) context.Context {
	return ContextWithIdempotencyStoreNamespace(ctx, store, execID, "")
}

// ContextWithIdempotencyStoreNamespace isolates otherwise identical tool
// names and business keys belonging to different Pipe definitions. Namespace
// is internal harness metadata; the public Tool syntax stays unchanged.
func ContextWithIdempotencyStoreNamespace(ctx context.Context, store idempotencyStore, execID, namespace string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if store == nil || strings.TrimSpace(execID) == "" {
		return ctx
	}
	return context.WithValue(ctx, idempotencyContextKey{}, idempotencyContext{
		store:     store,
		execID:    strings.TrimSpace(execID),
		namespace: strings.TrimSpace(namespace),
	})
}

// IdempotencyNamespaceFromContext returns the current internal Pipe scope.
// It is used by governed child pipelines to derive, rather than discard, the
// parent's isolation boundary.
func IdempotencyNamespaceFromContext(ctx context.Context) string {
	reservation, ok := idempotencyFromContext(ctx)
	if !ok {
		return ""
	}
	return reservation.namespace
}

// ContextWithIdempotencyReplay marks a governed durable replay. It allows the
// same execution to re-enter a pending idempotent reservation: the tool's
// declared stable key is the deduplication contract. Ordinary in-flight
// duplicates remain blocked while pending.
func ContextWithIdempotencyReplay(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, idempotencyReplayContextKey{}, true)
}

// reserveIdempotency reserves an idempotent tool call's key before execution.
// It returns skip=true only when THIS execution already resolved the key as
// succeeded. Pending calls are never mistaken for completed work; a governed
// durable replay may re-enter its own pending idempotent claim because the
// stable key is the tool's explicit replay-safety contract. Failed claims are
// atomically made available to a retry. A succeeded key owned by another
// execution remains a hard duplicate error for compatibility.
func reserveIdempotency(ctx context.Context, tool registeredTool, raw json.RawMessage) (bool, *idempotencyClaim, error) {
	if normalizeEffect(tool.metadata.Effect) != policy.EffectIdempotent {
		return false, nil, nil
	}
	expression := strings.TrimSpace(tool.metadata.IdempotencyKey)
	if expression == "" {
		return false, nil, errors.New("idempotent tool requires an idempotency key")
	}

	reservation, ok := idempotencyFromContext(ctx)
	if !ok {
		return false, nil, errors.New("idempotent tool requires an idempotency StateStore")
	}
	key, err := idempotencyReservationKeyInNamespace(reservation.namespace, tool.name, expression, raw)
	if err != nil {
		return false, nil, err
	}
	existing, reserved, err := reservation.store.ReserveIdempotency(ctx, key, reservation.execID)
	if err != nil {
		return false, nil, fmt.Errorf("reserve idempotency key: %w", err)
	}
	outcomes, outcomeAware := reservation.store.(state.IdempotencyOutcomeStore)
	if reserved {
		if outcomeAware {
			return false, &idempotencyClaim{store: outcomes, key: key, execID: reservation.execID}, nil
		}
		return false, nil, nil
	}
	if !outcomeAware {
		if existing == reservation.execID {
			return true, nil, nil
		}
		return false, nil, fmt.Errorf("idempotency key already reserved for execution %s", existing)
	}
	record, found, err := outcomes.Idempotency(ctx, key)
	if err != nil {
		return false, nil, fmt.Errorf("read idempotency outcome: %w", err)
	}
	if !found {
		return false, nil, errors.New("idempotency reservation disappeared after conflict")
	}
	switch record.Outcome {
	case state.IdempotencySucceeded:
		if record.ExecID == reservation.execID {
			return true, nil, nil
		}
		return false, nil, fmt.Errorf("idempotency key already succeeded for execution %s", record.ExecID)
	case state.IdempotencyPending:
		if record.ExecID == reservation.execID && idempotencyReplayFromContext(ctx) {
			return false, &idempotencyClaim{store: outcomes, key: key, execID: reservation.execID}, nil
		}
		return false, nil, fmt.Errorf("idempotency key is pending for execution %s", record.ExecID)
	case state.IdempotencyFailed:
		return false, nil, errors.New("failed idempotency reservation was not made available for retry")
	default:
		return false, nil, fmt.Errorf("unknown idempotency outcome %q", record.Outcome)
	}
}

func idempotencyReplayFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	replay, _ := ctx.Value(idempotencyReplayContextKey{}).(bool)
	return replay
}

func (c *idempotencyClaim) resolve(ctx context.Context, outcome state.IdempotencyOutcome) error {
	if c == nil || c.store == nil {
		return nil
	}
	if err := c.store.ResolveIdempotency(ctx, c.key, c.execID, outcome); err != nil {
		return fmt.Errorf("resolve idempotency key: %w", err)
	}
	return nil
}

func idempotencyFromContext(ctx context.Context) (idempotencyContext, bool) {
	if ctx == nil {
		return idempotencyContext{}, false
	}
	reservation, ok := ctx.Value(idempotencyContextKey{}).(idempotencyContext)
	return reservation, ok && reservation.store != nil && reservation.execID != ""
}

func idempotencyReservationKey(toolName, expression string, raw json.RawMessage) (string, error) {
	return idempotencyReservationKeyInNamespace("", toolName, expression, raw)
}

func idempotencyReservationKeyInNamespace(namespace, toolName, expression string, raw json.RawMessage) (string, error) {
	value, err := resolveIdempotencyValue(raw, expression)
	if err != nil {
		return "", err
	}
	material := strings.TrimSpace(namespace) + "\x00" + strings.TrimSpace(toolName) + "\x00" + strings.TrimSpace(expression) + "\x00" + string(value)
	sum := sha256.Sum256([]byte(material))
	return "tool:" + strings.TrimSpace(toolName) + ":" + strings.TrimSpace(expression) + ":" + hex.EncodeToString(sum[:]), nil
}

func resolveIdempotencyValue(raw json.RawMessage, expression string) (json.RawMessage, error) {
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return nil, errors.New("resolve idempotency key: expression is required")
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = []byte(`{}`)
	}

	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("resolve idempotency key %q: decode arguments: %w", expression, err)
	}
	current := value
	for _, part := range strings.Split(expression, ".") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("resolve idempotency key %q: empty path segment", expression)
		}
		object, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("resolve idempotency key %q: %q is not an object", expression, part)
		}
		next, ok := object[part]
		if !ok {
			return nil, fmt.Errorf("resolve idempotency key %q: missing field %q", expression, part)
		}
		if next == nil {
			return nil, fmt.Errorf("resolve idempotency key %q: field %q is null", expression, part)
		}
		current = next
	}
	encoded, err := json.Marshal(current)
	if err != nil {
		return nil, fmt.Errorf("resolve idempotency key %q: encode value: %w", expression, err)
	}
	return encoded, nil
}
