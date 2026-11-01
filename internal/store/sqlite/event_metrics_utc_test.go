package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

// Period cutoffs and ORDER BY compare the stored timestamp as text, so an
// offset value such as 12:00+14:00 sorts by its wall clock instead of its
// instant. Writes must normalise to UTC.
var plus14 = time.FixedZone("UTC+14", 14*3600)

func TestMetricsRepository_SummaryIgnoresTimezoneOffset(t *testing.T) {
	tests := []struct {
		name    string
		age     time.Duration
		wantDay int64
	}{
		{"30h old with +14:00 offset is outside the day window", 30 * time.Hour, 0},
		{"1h old with +14:00 offset is inside the day window", time.Hour, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, accRepo, _, metricsRepo, _ := setupTestStore(t)
			ctx := context.Background()
			if err := accRepo.Create(ctx, &domain.Account{ID: "acc-1", Email: "a@example.com", RefreshToken: "rt"}); err != nil {
				t.Fatalf("create account: %v", err)
			}
			err := metricsRepo.Record(ctx, &domain.TokenMetric{
				AccountID:        "acc-1",
				RequestPath:      "/v1internal:generateContent",
				PromptTokens:     10,
				CandidatesTokens: 5,
				Timestamp:        time.Now().Add(-tt.age).In(plus14),
			})
			if err != nil {
				t.Fatalf("Record: %v", err)
			}

			sum, err := metricsRepo.GetSummary(ctx, "", "day")
			if err != nil {
				t.Fatalf("GetSummary: %v", err)
			}
			if sum.TotalRequests != tt.wantDay {
				t.Errorf("GetSummary(day).TotalRequests = %d, want %d", sum.TotalRequests, tt.wantDay)
			}

			perAccount, err := metricsRepo.GetAccountSummaries(ctx, "day")
			if err != nil {
				t.Fatalf("GetAccountSummaries: %v", err)
			}
			var got int64
			if m := perAccount["acc-1"]; m != nil {
				got = m.TotalRequests
			}
			if got != tt.wantDay {
				t.Errorf("GetAccountSummaries(day)[acc-1].TotalRequests = %d, want %d", got, tt.wantDay)
			}
		})
	}
}

func TestEventRepository_ListRecentOrdersByInstant(t *testing.T) {
	_, accRepo, _, _, eventRepo := setupTestStore(t)
	ctx := context.Background()
	if err := accRepo.Create(ctx, &domain.Account{ID: "acc-1", Email: "a@example.com", RefreshToken: "rt"}); err != nil {
		t.Fatalf("create account: %v", err)
	}

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// "earlier" happened 2h before "later" but its +14:00 wall clock (02:00 the
	// next day) sorts after "later" as text.
	earlier := &domain.ProxyEvent{Type: domain.EventTypeError, AccountID: "acc-1", Message: "earlier", Timestamp: base.Add(-2 * time.Hour).In(plus14)}
	later := &domain.ProxyEvent{Type: domain.EventTypeError, AccountID: "acc-1", Message: "later", Timestamp: base}
	for _, ev := range []*domain.ProxyEvent{earlier, later} {
		if err := eventRepo.Record(ctx, ev); err != nil {
			t.Fatalf("Record %s: %v", ev.Message, err)
		}
	}

	got, err := eventRepo.ListRecent(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(got) != 2 || got[0].Message != "later" || got[1].Message != "earlier" {
		t.Fatalf("ListRecent order = %v, want [later earlier]", messages(got))
	}
	if !got[1].Timestamp.Equal(earlier.Timestamp) {
		t.Errorf("round-tripped timestamp = %v, want same instant as %v", got[1].Timestamp, earlier.Timestamp)
	}
}

func TestRecordedTimestampsAreStoredAsUTC(t *testing.T) {
	ts := time.Date(2026, 1, 1, 12, 0, 0, 0, plus14) // 2025-12-31T22:00:00Z
	const want = "2025-12-31T22:00:00Z"

	tests := []struct {
		name   string
		record func(t *testing.T) string
	}{
		{"token_metrics.timestamp", func(t *testing.T) string {
			db, accRepo, _, metricsRepo, _ := setupTestStore(t)
			ctx := context.Background()
			if err := accRepo.Create(ctx, &domain.Account{ID: "acc-1", Email: "a@example.com", RefreshToken: "rt"}); err != nil {
				t.Fatalf("create account: %v", err)
			}
			if err := metricsRepo.Record(ctx, &domain.TokenMetric{AccountID: "acc-1", Timestamp: ts}); err != nil {
				t.Fatalf("Record: %v", err)
			}
			var got string
			if err := db.QueryRowContext(ctx, `SELECT timestamp FROM token_metrics`).Scan(&got); err != nil {
				t.Fatalf("read back: %v", err)
			}
			return got
		}},
		{"proxy_events.created_at", func(t *testing.T) string {
			db, accRepo, _, _, eventRepo := setupTestStore(t)
			ctx := context.Background()
			if err := accRepo.Create(ctx, &domain.Account{ID: "acc-1", Email: "a@example.com", RefreshToken: "rt"}); err != nil {
				t.Fatalf("create account: %v", err)
			}
			if err := eventRepo.Record(ctx, &domain.ProxyEvent{Type: domain.EventTypeError, AccountID: "acc-1", Message: "m", Timestamp: ts}); err != nil {
				t.Fatalf("Record: %v", err)
			}
			var got string
			if err := db.QueryRowContext(ctx, `SELECT created_at FROM proxy_events`).Scan(&got); err != nil {
				t.Fatalf("read back: %v", err)
			}
			return got
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.record(t); got != want {
				t.Errorf("stored %s = %q, want %q", tt.name, got, want)
			}
		})
	}
}

func messages(evs []*domain.ProxyEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Message
	}
	return out
}
