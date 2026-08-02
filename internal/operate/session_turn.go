package operate

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// sessionTurnContext marks a context that already owns one runtime's
// per-session mutation lane. Governed tools invoked by a prompt inherit this
// marker and do not deadlock by trying to acquire the same lane recursively.
type sessionTurnContext struct {
	runtime   *AgentRuntime
	sessionID string
}

type sessionTurnContextKey struct{}

type sessionTurnLane struct {
	token chan struct{}
	refs  int
}

// acquireSessionTurn serializes complete prompt/tool mutations for a session,
// including the model round trips between transcript appends. The process
// writer lock prevents a second runtime from writing; this in-process lane
// prevents two goroutines owned by the same runtime from interleaving history.
func (r *AgentRuntime) acquireSessionTurn(ctx context.Context, sessionID string) (context.Context, func(), error) {
	if r == nil {
		return ctx, func() {}, fmt.Errorf("operate: nil runtime")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sessionID = strings.TrimSpace(sessionID)
	if !validSessionID(sessionID) {
		return ctx, func() {}, fmt.Errorf("%w: invalid session id %q", ErrSessionNotFound, sessionID)
	}
	if held, _ := ctx.Value(sessionTurnContextKey{}).(sessionTurnContext); held.runtime == r && held.sessionID == sessionID {
		return ctx, func() {}, nil
	}

	r.turnMu.Lock()
	lane := r.turns[sessionID]
	if lane == nil {
		lane = &sessionTurnLane{token: make(chan struct{}, 1)}
		r.turns[sessionID] = lane
	}
	lane.refs++
	r.turnMu.Unlock()
	releaseReference := func() {
		r.turnMu.Lock()
		lane.refs--
		if lane.refs == 0 && r.turns[sessionID] == lane {
			delete(r.turns, sessionID)
		}
		r.turnMu.Unlock()
	}

	select {
	case lane.token <- struct{}{}:
		marked := context.WithValue(ctx, sessionTurnContextKey{}, sessionTurnContext{runtime: r, sessionID: sessionID})
		var once sync.Once
		return marked, func() {
			once.Do(func() {
				<-lane.token
				releaseReference()
			})
		}, nil
	case <-ctx.Done():
		releaseReference()
		return ctx, func() {}, ctx.Err()
	}
}
