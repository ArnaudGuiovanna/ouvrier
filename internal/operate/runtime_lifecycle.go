package operate

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// beginRuntimeActivity establishes the shutdown boundary for an operation.
// Registration and Close's transition to closed share lifecycleMu, so a
// WaitGroup Add can never race a zero-count Wait.
func (r *AgentRuntime) beginRuntimeActivity(parent context.Context, key string) (context.Context, func(), error) {
	if r == nil {
		return parent, func() {}, errors.New("operate: nil runtime")
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	r.lifecycleMu.Lock()
	if r.closed {
		r.lifecycleMu.Unlock()
		cancel()
		return parent, func() {}, ErrRuntimeClosed
	}
	r.activitySeq++
	id := r.activitySeq
	if r.activities[key] == nil {
		r.activities[key] = make(map[uint64]context.CancelFunc)
	}
	r.activities[key][id] = cancel
	r.activityWG.Add(1)
	r.lifecycleMu.Unlock()

	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			cancel()
			r.lifecycleMu.Lock()
			delete(r.activities[key], id)
			if len(r.activities[key]) == 0 {
				delete(r.activities, key)
			}
			r.lifecycleMu.Unlock()
			r.activityWG.Done()
		})
	}, nil
}

func (r *AgentRuntime) cancelSessionActivities(sessionID string) (bool, error) {
	if r == nil {
		return false, errors.New("operate: nil runtime")
	}
	r.lifecycleMu.Lock()
	if r.closed {
		r.lifecycleMu.Unlock()
		return false, ErrRuntimeClosed
	}
	cancels := make([]context.CancelFunc, 0, len(r.activities[sessionID]))
	for _, cancel := range r.activities[sessionID] {
		cancels = append(cancels, cancel)
	}
	r.lifecycleMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return len(cancels) != 0, nil
}

func (r *AgentRuntime) acquireModelTurn(ctx context.Context) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx, func() {}, ctx.Err()
	case <-r.modelTurn:
		var once sync.Once
		return ctx, func() { once.Do(func() { r.modelTurn <- struct{}{} }) }, nil
	}
}

func (r *AgentRuntime) closeRuntime() error {
	r.lifecycleMu.Lock()
	r.closed = true
	cancels := make([]context.CancelFunc, 0)
	for _, keyed := range r.activities {
		for _, cancel := range keyed {
			cancels = append(cancels, cancel)
		}
	}
	r.lifecycleMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	r.activityWG.Wait()

	var errs []error
	r.lockMu.Lock()
	for id, lock := range r.locks {
		if err := releaseSessionLock(lock); err != nil {
			errs = append(errs, fmt.Errorf("operate: release session %s lock: %w", id, err))
		}
		delete(r.locks, id)
	}
	r.lockMu.Unlock()
	if closer, ok := r.Options.Model.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			errs = append(errs, fmt.Errorf("operate: close model transport: %w", err))
		}
	}
	return errors.Join(errs...)
}
