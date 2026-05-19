package harness

import (
	"context"

	runtimecore "ouvrier/internal/runtime"
)

type sessionContextKey struct{}

func contextWithSession(ctx context.Context, session runtimecore.Session) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, sessionContextKey{}, session)
}

func SessionFromContext(ctx context.Context) (runtimecore.Session, bool) {
	if ctx == nil {
		return runtimecore.Session{}, false
	}
	session, ok := ctx.Value(sessionContextKey{}).(runtimecore.Session)
	return session, ok
}
