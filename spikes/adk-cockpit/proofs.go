package adkspike

import "sync"

type proofKey struct {
	invocationID string
	callID       string
	toolName     string
}

// proofTracker correlates function responses with calls that actually crossed
// the governed wrapper. It never writes provenance tokens into model-visible
// content. Keeping this tracker only in memory is an explicit spike limit.
type proofTracker struct {
	mu            sync.RWMutex
	verifiedCalls map[proofKey]struct{}
}

func newProofTracker() *proofTracker {
	return &proofTracker{verifiedCalls: make(map[proofKey]struct{})}
}

func (p *proofTracker) record(invocationID, callID, toolName string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.verifiedCalls[proofKey{
		invocationID: invocationID,
		callID:       callID,
		toolName:     toolName,
	}] = struct{}{}
	p.mu.Unlock()
}

func (p *proofTracker) consumeVerified(invocationID, callID, toolName string) bool {
	if p == nil {
		return false
	}
	key := proofKey{
		invocationID: invocationID,
		callID:       callID,
		toolName:     toolName,
	}
	p.mu.Lock()
	_, ok := p.verifiedCalls[key]
	delete(p.verifiedCalls, key)
	p.mu.Unlock()
	return ok
}

func (p *proofTracker) purgeInvocation(invocationID string) {
	if p == nil || invocationID == "" {
		return
	}
	p.mu.Lock()
	for key := range p.verifiedCalls {
		if key.invocationID == invocationID {
			delete(p.verifiedCalls, key)
		}
	}
	p.mu.Unlock()
}
