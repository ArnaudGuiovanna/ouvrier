package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"ouvrier/internal/policy"
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

func reserveIdempotency(ctx context.Context, tool registeredTool, raw json.RawMessage) error {
	if normalizeEffect(tool.metadata.Effect) != policy.EffectIdempotent {
		return nil
	}
	expression := strings.TrimSpace(tool.metadata.IdempotencyKey)
	if expression == "" {
		return errors.New("idempotent tool requires an idempotency key")
	}

	reservation, ok := idempotencyFromContext(ctx)
	if !ok {
		return nil
	}
	key, err := idempotencyReservationKey(tool.name, expression, raw)
	if err != nil {
		return err
	}
	existing, reserved, err := reservation.store.ReserveIdempotency(ctx, key, reservation.execID)
	if err != nil {
		return fmt.Errorf("reserve idempotency key: %w", err)
	}
	if !reserved {
		return fmt.Errorf("idempotency key already reserved for execution %s", existing)
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
