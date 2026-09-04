package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

func TestMetricsRepository_RecordAndSummary(t *testing.T) {
	_, accRepo, _, metricsRepo, _ := setupTestStore(t)
	ctx := context.Background()

	// 1. Summary on empty table should return 0 counts without error
	emptySummary, err := metricsRepo.GetSummary(ctx, "", "lifetime")
	if err != nil {
		t.Fatalf("GetSummary on empty table failed: %v", err)
	}
	if emptySummary.TotalTokens != 0 || emptySummary.TotalRequests != 0 {
		t.Errorf("expected 0 metrics on empty table, got %+v", emptySummary)
	}

	// Create test accounts
	_ = accRepo.Create(ctx, &domain.Account{ID: "acc-m1", Email: "m1@example.com", RefreshToken: "rt1"})
	_ = accRepo.Create(ctx, &domain.Account{ID: "acc-m2", Email: "m2@example.com", RefreshToken: "rt2"})

	now := time.Now().UTC()

	// Insert metrics with various timestamps
	records := []*domain.TokenMetric{
		// 1 hour ago (within day, week, month, lifetime)
		{
			AccountID:           "acc-m1",
			RequestPath:         "/v1internal:streamGenerateContent",
			PromptTokens:        100,
			CandidatesTokens:    50,
			TotalTokens:         150,
			CachedContentTokens: 20,
			ThoughtsTokens:      10,
			Timestamp:           now.Add(-1 * time.Hour),
		},
		// 3 days ago (outside day, within week, month, lifetime)
		{
			AccountID:        "acc-m1",
			RequestPath:      "/v1internal:streamGenerateContent",
			PromptTokens:     200,
			CandidatesTokens: 100,
			TotalTokens:      300,
			Timestamp:        now.Add(-3 * 24 * time.Hour),
		},
		// 15 days ago (outside day & week, within month, lifetime)
		{
			AccountID:        "acc-m1",
			RequestPath:      "/v1internal:generateContent",
			PromptTokens:     400,
			CandidatesTokens: 200,
			TotalTokens:      600,
			Timestamp:        now.Add(-15 * 24 * time.Hour),
		},
		// 45 days ago (only within lifetime)
		{
			AccountID:        "acc-m1",
			RequestPath:      "/v1internal:generateContent",
			PromptTokens:     800,
			CandidatesTokens: 400,
			TotalTokens:      1200,
			Timestamp:        now.Add(-45 * 24 * time.Hour),
		},
		// acc-m2 metric 2 hours ago
		{
			AccountID:        "acc-m2",
			RequestPath:      "/v1internal:streamGenerateContent",
			PromptTokens:     50,
			CandidatesTokens: 25,
			// TotalTokens omitted to test auto-calculation
			Timestamp: now.Add(-2 * time.Hour),
		},
	}

	for _, rec := range records {
		if err := metricsRepo.Record(ctx, rec); err != nil {
			t.Fatalf("Record failed: %v", err)
		}
		if rec.ID == 0 {
			t.Errorf("expected non-zero ID after Record, got 0")
		}
		if rec.TotalTokens == 0 {
			t.Errorf("expected auto-calculated TotalTokens, got 0")
		}
	}

	// Test Period: "day" for acc-m1
	dayM1, err := metricsRepo.GetSummary(ctx, "acc-m1", "day")
	if err != nil {
		t.Fatalf("GetSummary(acc-m1, day) failed: %v", err)
	}
	if dayM1.TotalRequests != 1 || dayM1.TotalTokens != 150 {
		t.Errorf("expected acc-m1 day: 1 request, 150 tokens; got requests=%d, tokens=%d", dayM1.TotalRequests, dayM1.TotalTokens)
	}

	// Test Period: "week" for acc-m1 (1h ago + 3d ago = 150 + 300 = 450 tokens, 2 requests)
	weekM1, err := metricsRepo.GetSummary(ctx, "acc-m1", "week")
	if err != nil {
		t.Fatalf("GetSummary(acc-m1, week) failed: %v", err)
	}
	if weekM1.TotalRequests != 2 || weekM1.TotalTokens != 450 {
		t.Errorf("expected acc-m1 week: 2 requests, 450 tokens; got requests=%d, tokens=%d", weekM1.TotalRequests, weekM1.TotalTokens)
	}

	// Test Period: "month" for acc-m1 (1h + 3d + 15d = 150 + 300 + 600 = 1050 tokens, 3 requests)
	monthM1, err := metricsRepo.GetSummary(ctx, "acc-m1", "month")
	if err != nil {
		t.Fatalf("GetSummary(acc-m1, month) failed: %v", err)
	}
	if monthM1.TotalRequests != 3 || monthM1.TotalTokens != 1050 {
		t.Errorf("expected acc-m1 month: 3 requests, 1050 tokens; got requests=%d, tokens=%d", monthM1.TotalRequests, monthM1.TotalTokens)
	}

	// Test Period: "lifetime" for acc-m1 (1h + 3d + 15d + 45d = 150 + 300 + 600 + 1200 = 2250 tokens, 4 requests)
	lifetimeM1, err := metricsRepo.GetSummary(ctx, "acc-m1", "lifetime")
	if err != nil {
		t.Fatalf("GetSummary(acc-m1, lifetime) failed: %v", err)
	}
	if lifetimeM1.TotalRequests != 4 || lifetimeM1.TotalTokens != 2250 {
		t.Errorf("expected acc-m1 lifetime: 4 requests, 2250 tokens; got requests=%d, tokens=%d", lifetimeM1.TotalRequests, lifetimeM1.TotalTokens)
	}

	// Test Global (all accounts) Period: "day" (acc-m1 150 + acc-m2 75 = 225 tokens, 2 requests)
	globalDay, err := metricsRepo.GetSummary(ctx, "", "day")
	if err != nil {
		t.Fatalf("GetSummary(global, day) failed: %v", err)
	}
	if globalDay.TotalRequests != 2 || globalDay.TotalTokens != 225 {
		t.Errorf("expected global day: 2 requests, 225 tokens; got requests=%d, tokens=%d", globalDay.TotalRequests, globalDay.TotalTokens)
	}

	// Invalid period returns error
	if _, err := metricsRepo.GetSummary(ctx, "", "invalid_period"); err == nil {
		t.Fatalf("expected error for invalid period, got nil")
	}
}

func TestMetricsRepository_GetDailyHistory(t *testing.T) {
	_, accRepo, _, metricsRepo, _ := setupTestStore(t)
	ctx := context.Background()

	_ = accRepo.Create(ctx, &domain.Account{ID: "acc-hist", Email: "hist@example.com", RefreshToken: "rt"})

	now := time.Now().UTC()
	// Insert 2 records today (guaranteed same calendar day), 1 record yesterday
	_ = metricsRepo.Record(ctx, &domain.TokenMetric{
		AccountID:        "acc-hist",
		PromptTokens:     50,
		CandidatesTokens: 25,
		Timestamp:        now,
	})
	_ = metricsRepo.Record(ctx, &domain.TokenMetric{
		AccountID:        "acc-hist",
		PromptTokens:     100,
		CandidatesTokens: 50,
		Timestamp:        now,
	})
	_ = metricsRepo.Record(ctx, &domain.TokenMetric{
		AccountID:        "acc-hist",
		PromptTokens:     200,
		CandidatesTokens: 100,
		Timestamp:        now.AddDate(0, 0, -1),
	})

	history, err := metricsRepo.GetDailyHistory(ctx, "acc-hist", 7)
	if err != nil {
		t.Fatalf("GetDailyHistory failed: %v", err)
	}

	if len(history) < 2 {
		t.Fatalf("expected at least 2 daily history records, got %d", len(history))
	}

	// Last item is today
	today := history[len(history)-1]
	if today.RequestCount != 2 || today.TotalTokens != 225 {
		t.Errorf("expected today 2 requests, 225 tokens; got requests=%d, tokens=%d", today.RequestCount, today.TotalTokens)
	}
}

func TestMetricsRepository_GetDailyHistoryInLocation(t *testing.T) {
	_, accRepo, _, metricsRepo, _ := setupTestStore(t)
	ctx := context.Background()

	_ = accRepo.Create(ctx, &domain.Account{ID: "acc-tz", Email: "tz@example.com", RefreshToken: "rt"})

	// Scenario: A request happens at 00:05 UTC on Sept 4.
	// In UTC: Sept 4.
	// In Brazil (UTC-3): Sept 3, 21:05.
	tUTC := time.Date(2026, 9, 4, 0, 5, 0, 0, time.UTC)
	_ = metricsRepo.Record(ctx, &domain.TokenMetric{
		AccountID:        "acc-tz",
		PromptTokens:     500,
		CandidatesTokens: 200,
		TotalTokens:      700,
		Timestamp:        tUTC,
	})

	spLoc := time.FixedZone("America/Sao_Paulo", -3*3600)

	// In America/Sao_Paulo (UTC-3), strftime with -3 hours must group this under 2026-09-03
	spHistory, err := metricsRepo.GetDailyHistoryInLocation(ctx, "acc-tz", 7, spLoc)
	if err != nil {
		t.Fatalf("GetDailyHistoryInLocation(spLoc) failed: %v", err)
	}
	if len(spHistory) == 0 {
		t.Fatalf("expected spHistory to have at least 1 record")
	}
	lastItemSP := spHistory[len(spHistory)-1]
	if lastItemSP.Date != "2026-09-03" {
		t.Errorf("expected date in SP timezone to be 2026-09-03, got %q", lastItemSP.Date)
	}
	if lastItemSP.TotalTokens != 700 {
		t.Errorf("expected 700 tokens in SP, got %d", lastItemSP.TotalTokens)
	}

	// In UTC, strftime must group this under 2026-09-04
	utcHistory, err := metricsRepo.GetDailyHistoryInLocation(ctx, "acc-tz", 7, time.UTC)
	if err != nil {
		t.Fatalf("GetDailyHistoryInLocation(UTC) failed: %v", err)
	}
	if len(utcHistory) == 0 {
		t.Fatalf("expected utcHistory to have at least 1 record")
	}
	lastItemUTC := utcHistory[len(utcHistory)-1]
	if lastItemUTC.Date != "2026-09-04" {
		t.Errorf("expected date in UTC to be 2026-09-04, got %q", lastItemUTC.Date)
	}
	if lastItemUTC.TotalTokens != 700 {
		t.Errorf("expected 700 tokens in UTC, got %d", lastItemUTC.TotalTokens)
	}
}
