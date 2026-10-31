package oauth

import (
	"context"
	"sync"
)

// refreshCall is one in-flight token refresh shared by every caller that asked
// for the same refresh token while it was running.
type refreshCall struct {
	done    chan struct{}
	resp    *TokenResponse
	err     error
	waiters int
}

// refreshGroup collapses concurrent refreshes of the same refresh token into a
// single upstream request. It only dedupes calls that overlap in time; it does
// not cache results.
type refreshGroup struct {
	mu    sync.Mutex
	calls map[string]*refreshCall
}

// do runs fn once per key at a time. The first caller starts fn on a context
// detached from its own cancellation (bounded by DefaultHTTPTimeout) so that one
// caller giving up does not fail the others. Each caller still returns as soon
// as its own ctx is done.
func (g *refreshGroup) do(ctx context.Context, key string, fn func(context.Context) (*TokenResponse, error)) (*TokenResponse, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*refreshCall)
	}
	c, running := g.calls[key]
	if running {
		c.waiters++
	} else {
		c = &refreshCall{done: make(chan struct{})}
		g.calls[key] = c
	}
	g.mu.Unlock()

	if !running {
		go func() {
			runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultHTTPTimeout)
			defer cancel()
			c.resp, c.err = fn(runCtx)

			g.mu.Lock()
			delete(g.calls, key)
			g.mu.Unlock()
			close(c.done)
		}()
	}

	select {
	case <-c.done:
		if c.err != nil {
			return nil, c.err
		}
		resp := *c.resp
		return &resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
