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
)

type idempotencyStore interface {
	ReserveIdempotency(context.Context, string, string) (string, bool, error)
}

type idempotencyContext struct {
	store  idempotencyStore
	execID string
}

type idempotencyContextKey struct{}

func ContextWithIdempotencyStore(ctx context.Context, store idempotencyStore, execID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if store == nil || strings.TrimSpace(execID) == "" {
		return ctx
	}
	return context.WithValue(ctx, idempotencyContextKey{}, idempotencyContext{
		store:  store,
		execID: strings.TrimSpace(execID),
	})
}

// reserveIdempotency reserves an idempotent tool call's key before execution.
// It returns skip=true when the reservation is already held by THIS execution
// — a durable-run replay (#40) or an in-run duplicate re-issuing the same
// call — which the executor dedupes as a success instead of the historical
// error: the work is already done under this exec id. A reservation held by a
// different execution remains an error.
func reserveIdempotency(ctx context.Context, tool registeredTool, raw json.RawMessage) (bool, error) {
	if normalizeEffect(tool.metadata.Effect) != policy.EffectIdempotent {
		return false, nil
	}
	expression := strings.TrimSpace(tool.metadata.IdempotencyKey)
	if expression == "" {
		return false, errors.New("idempotent tool requires an idempotency key")
	}

	reservation, ok := idempotencyFromContext(ctx)
	if !ok {
		return false, errors.New("idempotent tool requires an idempotency StateStore")
	}
	key, err := idempotencyReservationKey(tool.name, expression, raw)
	if err != nil {
		return false, err
	}
	existing, reserved, err := reservation.store.ReserveIdempotency(ctx, key, reservation.execID)
	if err != nil {
		return false, fmt.Errorf("reserve idempotency key: %w", err)
	}
	if !reserved {
		if existing == reservation.execID {
			return true, nil
		}
		return false, fmt.Errorf("idempotency key already reserved for execution %s", existing)
	}
	return false, nil
}

func idempotencyFromContext(ctx context.Context) (idempotencyContext, bool) {
	if ctx == nil {
		return idempotencyContext{}, false
	}
	reservation, ok := ctx.Value(idempotencyContextKey{}).(idempotencyContext)
	return reservation, ok && reservation.store != nil && reservation.execID != ""
}

func idempotencyReservationKey(toolName, expression string, raw json.RawMessage) (string, error) {
	value, err := resolveIdempotencyValue(raw, expression)
	if err != nil {
		return "", err
	}
	material := strings.TrimSpace(toolName) + "\x00" + strings.TrimSpace(expression) + "\x00" + string(value)
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
