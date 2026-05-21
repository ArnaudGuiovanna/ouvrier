package harness

import (
	"context"

	runtimecore "ouvrier/internal/runtime"
)

type sessionContextKey struct{}
type budgetLedgerContextKey struct{}

func contextWithSession(ctx context.Context, session runtimecore.Session) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, sessionContextKey{}, session)
}

func contextWithExecution(ctx context.Context, session runtimecore.Session, ledger *BudgetLedger) context.Context {
	ctx = contextWithSession(ctx, session)
	if ledger == nil {
		return ctx
	}
	return context.WithValue(ctx, budgetLedgerContextKey{}, ledger)
}

func SessionFromContext(ctx context.Context) (runtimecore.Session, bool) {
	if ctx == nil {
		return runtimecore.Session{}, false
	}
	session, ok := ctx.Value(sessionContextKey{}).(runtimecore.Session)
	return session, ok
}

func BudgetLedgerFromContext(ctx context.Context) (*BudgetLedger, bool) {
	if ctx == nil {
		return nil, false
	}
	ledger, ok := ctx.Value(budgetLedgerContextKey{}).(*BudgetLedger)
	return ledger, ok && ledger != nil
}
