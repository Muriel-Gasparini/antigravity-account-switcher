package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// refreshServer is a fake token endpoint that counts requests and holds each
// one until release is closed.
type refreshServer struct {
	*httptest.Server
	hits    atomic.Int32
	release chan struct{}
}

func newRefreshServer(t *testing.T) *refreshServer {
	t.Helper()
	rs := &refreshServer{release: make(chan struct{})}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := rs.hits.Add(1)
		<-rs.release
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("access-%d", n),
			"expires_in":   3600,
		})
	}))
	t.Cleanup(func() {
		select {
		case <-rs.release:
		default:
			close(rs.release)
		}
		rs.Close()
	})
	return rs
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (g *refreshGroup) waiting(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.calls[key]; ok {
		return c.waiters
	}
	return 0
}

type refreshResult struct {
	resp *TokenResponse
	err  error
}

func TestRefreshToken_ConcurrentCallsShareOneRequest(t *testing.T) {
	rs := newRefreshServer(t)
	svc := NewOAuthService(nil, WithTokenURL(rs.URL+"/token"))

	const callers = 8
	results := make(chan refreshResult, callers)
	for i := 0; i < callers; i++ {
		go func() {
			resp, err := svc.RefreshToken(context.Background(), "rt-1")
			results <- refreshResult{resp, err}
		}()
	}

	eventually(t, "all callers to join one refresh", func() bool {
		return rs.hits.Load() >= 1 && svc.refreshes.waiting("rt-1") == callers-1
	})
	close(rs.release)

	for i := 0; i < callers; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("RefreshToken: %v", r.err)
		}
		if r.resp.AccessToken != "access-1" {
			t.Errorf("access token = %q, want access-1", r.resp.AccessToken)
		}
	}
	if got := rs.hits.Load(); got != 1 {
		t.Errorf("token endpoint hit %d times, want 1", got)
	}
}

func TestRefreshToken_DistinctRefreshTokensAreNotMerged(t *testing.T) {
	rs := newRefreshServer(t)
	svc := NewOAuthService(nil, WithTokenURL(rs.URL+"/token"))

	var wg sync.WaitGroup
	for _, rt := range []string{"rt-a", "rt-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.RefreshToken(context.Background(), rt); err != nil {
				t.Errorf("RefreshToken(%s): %v", rt, err)
			}
		}()
	}
	eventually(t, "one request per account", func() bool { return rs.hits.Load() == 2 })
	close(rs.release)
	wg.Wait()
}

func TestRefreshToken_SequentialCallsRefreshAgain(t *testing.T) {
	rs := newRefreshServer(t)
	close(rs.release)
	svc := NewOAuthService(nil, WithTokenURL(rs.URL+"/token"))

	for i := 1; i <= 2; i++ {
		resp, err := svc.RefreshToken(context.Background(), "rt-1")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if want := fmt.Sprintf("access-%d", i); resp.AccessToken != want {
			t.Errorf("call %d: access token = %q, want %q", i, resp.AccessToken, want)
		}
	}
}

func TestRefreshToken_LeaderCancelDoesNotFailWaiters(t *testing.T) {
	rs := newRefreshServer(t)
	svc := NewOAuthService(nil, WithTokenURL(rs.URL+"/token"))

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leader := make(chan refreshResult, 1)
	go func() {
		resp, err := svc.RefreshToken(leaderCtx, "rt-1")
		leader <- refreshResult{resp, err}
	}()
	eventually(t, "leader request to reach the endpoint", func() bool { return rs.hits.Load() == 1 })

	waiter := make(chan refreshResult, 1)
	go func() {
		resp, err := svc.RefreshToken(context.Background(), "rt-1")
		waiter <- refreshResult{resp, err}
	}()
	eventually(t, "waiter to join", func() bool { return svc.refreshes.waiting("rt-1") == 1 })

	cancelLeader()
	if r := <-leader; !errors.Is(r.err, context.Canceled) {
		t.Errorf("leader err = %v, want context.Canceled", r.err)
	}

	close(rs.release)
	r := <-waiter
	if r.err != nil {
		t.Fatalf("waiter failed after leader cancel: %v", r.err)
	}
	if r.resp.AccessToken != "access-1" {
		t.Errorf("waiter access token = %q, want access-1", r.resp.AccessToken)
	}
	if got := rs.hits.Load(); got != 1 {
		t.Errorf("token endpoint hit %d times, want 1", got)
	}
}
