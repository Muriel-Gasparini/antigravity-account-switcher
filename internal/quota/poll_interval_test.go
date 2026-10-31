package quota

import (
	"context"
	"testing"
	"time"
)

// A zero or negative poll interval (for example quota_interval "0s" in the
// config file or --poll-interval 0) must not reach time.NewTicker, which
// panics on non-positive durations and would take the whole process down.
func TestNewPoller_NonPositiveIntervalFallsBackToDefault(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second, -5 * time.Minute} {
		_, accRepo, quotaRepo := setupTestStore(t)

		p, err := NewPoller(accRepo, quotaRepo, WithPollInterval(interval))
		if err != nil {
			t.Fatalf("NewPoller(interval=%v): %v", interval, err)
		}
		if p.cfg.PollInterval != DefaultPollInterval {
			t.Fatalf("interval %v: PollInterval = %v, want default %v", interval, p.cfg.PollInterval, DefaultPollInterval)
		}

		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("Start(interval=%v): %v", interval, err)
		}
		if err := p.Stop(); err != nil {
			t.Fatalf("Stop(interval=%v): %v", interval, err)
		}
	}
}
