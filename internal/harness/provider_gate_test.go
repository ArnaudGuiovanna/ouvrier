package harness

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderGateBoundsConcurrencyPerProvider(t *testing.T) {
	gate := NewProviderGate(map[string]int{"anthropic": 2})

	var inFlight, maxInFlight int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, err := gate.Acquire(context.Background(), "anthropic")
			if err != nil {
				t.Errorf("Acquire returned error: %v", err)
				return
			}
			defer release()
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				prev := atomic.LoadInt32(&maxInFlight)
				if cur <= prev || atomic.CompareAndSwapInt32(&maxInFlight, prev, cur) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
		}()
	}
	close(start)
	wg.Wait()

	if maxInFlight > 2 {
		t.Fatalf("max in-flight = %d, want <= 2", maxInFlight)
	}
}

func TestProviderGateUnboundedProviderDoesNotBlock(t *testing.T) {
	gate := NewProviderGate(map[string]int{"anthropic": 1})

	// openai has no configured limit, so concurrent acquisitions never block.
	release1, err := gate.Acquire(context.Background(), "openai")
	if err != nil {
		t.Fatalf("Acquire returned error: %v", err)
	}
	defer release1()
	release2, err := gate.Acquire(context.Background(), "openai")
	if err != nil {
		t.Fatalf("Acquire returned error: %v", err)
	}
	release2()
}

func TestProviderGateOneProviderDoesNotStallOthers(t *testing.T) {
	gate := NewProviderGate(map[string]int{"anthropic": 1})

	// Saturate anthropic without releasing.
	releaseA, err := gate.Acquire(context.Background(), "anthropic")
	if err != nil {
		t.Fatalf("Acquire anthropic returned error: %v", err)
	}
	defer releaseA()

	done := make(chan struct{})
	go func() {
		release, err := gate.Acquire(context.Background(), "openai")
		if err == nil {
			release()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("openai acquire stalled behind saturated anthropic provider")
	}
}

func TestNewProviderGateNilWhenNoPositiveLimits(t *testing.T) {
	if gate := NewProviderGate(map[string]int{"anthropic": 0, "openai": -1}); gate != nil {
		t.Fatalf("gate = %v, want nil when no positive limits", gate)
	}
	if gate := NewProviderGate(nil); gate != nil {
		t.Fatalf("gate = %v, want nil for empty map", gate)
	}
}
