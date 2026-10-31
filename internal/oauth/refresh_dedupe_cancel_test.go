package oauth

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// A caller whose context is already cancelled must not start a refresh: with
// rotating refresh tokens the detached request would burn the old token and
// nobody would receive the new one.
func TestRefreshGroup_CancelledContextStartsNoRequest(t *testing.T) {
	var g refreshGroup
	var calls int32
	release := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := g.do(ctx, "rt-1", func(context.Context) (*TokenResponse, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return &TokenResponse{AccessToken: "new"}, nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// A started flight registers itself before do returns, so this is
	// deterministic: no entry means no request was started.
	g.mu.Lock()
	inFlight := len(g.calls)
	g.mu.Unlock()
	if inFlight != 0 {
		t.Fatalf("%d refresh flight(s) started for a cancelled caller, want 0", inFlight)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("refresh ran %d time(s) for a cancelled caller, want 0", n)
	}
}
