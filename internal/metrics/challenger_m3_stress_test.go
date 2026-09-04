package metrics

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
)

// setupStressDB initializes a real SQLite database for metrics stress testing.
func setupStressDB(t *testing.T) (*sqlite.DB, *sqlite.AccountRepository, *sqlite.MetricsRepository, *Service) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "stress_metrics.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	accRepo := sqlite.NewAccountRepository(db)
	metricsRepo := sqlite.NewMetricsRepository(db)
	svc := NewService(metricsRepo, accRepo)

	return db, accRepo, metricsRepo, svc
}

// TestChallenger_Metrics_TimelineZeroFilling_WideDateRanges verifies zero-filling
// accuracy across multiple date windows (1, 14, 30, 90, 365, 1000 days),
// non-positive inputs, and validates strict 1-day step continuity and boundaries.
func TestChallenger_Metrics_TimelineZeroFilling_WideDateRanges(t *testing.T) {
	_, accRepo, _, svc := setupStressDB(t)
	ctx := context.Background()

	acc := &domain.Account{
		ID:          "acc-wide-timeline",
		Email:       "wide@example.com",
		Status:      domain.AccountStatusActive,
		IsActive:    true,
		TokenExpiry: time.Now().Add(1 * time.Hour),
	}
	if err := accRepo.Create(ctx, acc); err != nil {
		t.Fatalf("accRepo.Create: %v", err)
	}

	now := time.Now().UTC()
	todayStr := now.Format("2006-01-02")
	threeDaysAgoStr := now.AddDate(0, 0, -3).Format("2006-01-02")
	fiftyDaysAgoStr := now.AddDate(0, 0, -50).Format("2006-01-02")
	threeHundredDaysAgoStr := now.AddDate(0, 0, -300).Format("2006-01-02")

	// Record metrics at specific scattered intervals
	seedMetrics := []struct {
		timestamp time.Time
		tokens    int64
	}{
		{now, 1000},
		{now.AddDate(0, 0, -3), 500},
		{now.AddDate(0, 0, -50), 250},
		{now.AddDate(0, 0, -300), 100},
	}
	for _, sm := range seedMetrics {
		if err := svc.Record(ctx, &domain.TokenMetric{
			AccountID:        acc.ID,
			RequestPath:      "/v1internal:streamGenerateContent",
			PromptTokens:     sm.tokens / 2,
			CandidatesTokens: sm.tokens / 2,
			TotalTokens:      sm.tokens,
			Timestamp:        sm.timestamp,
		}); err != nil {
			t.Fatalf("Record failed: %v", err)
		}
	}

	testWindows := []struct {
		inputDays    int
		expectedDays int
	}{
		{-5, 14},     // Non-positive defaults to 14
		{0, 14},      // Zero defaults to 14
		{1, 1},       // Today only
		{7, 7},       // 1 week
		{14, 14},     // 2 weeks
		{30, 30},     // 1 month
		{90, 90},     // 1 quarter
		{365, 365},   // 1 year
		{1000, 1000}, // ~3 years
	}

	for _, tc := range testWindows {
		t.Run(fmt.Sprintf("days=%d", tc.inputDays), func(t *testing.T) {
			timeline, err := svc.GetDailyUsage(ctx, acc.ID, tc.inputDays, true)
			if err != nil {
				t.Fatalf("GetDailyUsage(%d, zeroFill=true) failed: %v", tc.inputDays, err)
			}

			if len(timeline) != tc.expectedDays {
				t.Fatalf("expected timeline len %d, got %d", tc.expectedDays, len(timeline))
			}

			// Invariant 1: Last item must be today
			lastItem := timeline[len(timeline)-1]
			if lastItem.Date != todayStr {
				t.Errorf("last item date = %q, want today %q", lastItem.Date, todayStr)
			}
			if lastItem.TotalTokens != 1000 {
				t.Errorf("today's tokens = %d, want 1000", lastItem.TotalTokens)
			}

			// Invariant 2: First item must be now - (expectedDays-1)
			firstExpected := now.AddDate(0, 0, -(tc.expectedDays - 1)).Format("2006-01-02")
			if timeline[0].Date != firstExpected {
				t.Errorf("first item date = %q, want %q", timeline[0].Date, firstExpected)
			}

			// Invariant 3: Strict chronological ordering and exactly 1-day step
			for i := 0; i < len(timeline)-1; i++ {
				currDate, err1 := time.Parse("2006-01-02", timeline[i].Date)
				nextDate, err2 := time.Parse("2006-01-02", timeline[i+1].Date)
				if err1 != nil || err2 != nil {
					t.Fatalf("invalid date format in timeline: %s, %s", timeline[i].Date, timeline[i+1].Date)
				}
				diff := nextDate.Sub(currDate)
				// Calendar days can have DST shifts in some timezones, but in UTC it's exactly 24h
				if diff != 24*time.Hour {
					t.Errorf("gap between %s and %s is %v, expected 24h", timeline[i].Date, timeline[i+1].Date, diff)
				}
			}

			// Invariant 4: Check token values for known populated days vs zero-filled days
			dateMap := make(map[string]*domain.DailyTokenUsage, len(timeline))
			for _, item := range timeline {
				dateMap[item.Date] = item
			}

			if tc.expectedDays >= 4 {
				if d3, ok := dateMap[threeDaysAgoStr]; ok {
					if d3.TotalTokens != 500 {
						t.Errorf("tokens at 3 days ago = %d, want 500", d3.TotalTokens)
					}
				} else {
					t.Errorf("expected %s in timeline", threeDaysAgoStr)
				}
			}

			if tc.expectedDays >= 51 {
				if d50, ok := dateMap[fiftyDaysAgoStr]; ok {
					if d50.TotalTokens != 250 {
						t.Errorf("tokens at 50 days ago = %d, want 250", d50.TotalTokens)
					}
				}
			}

			if tc.expectedDays >= 301 {
				if d300, ok := dateMap[threeHundredDaysAgoStr]; ok {
					if d300.TotalTokens != 100 {
						t.Errorf("tokens at 300 days ago = %d, want 100", d300.TotalTokens)
					}
				}
			}
		})
	}
}

// TestChallenger_Metrics_MultiAccount_ConcurrentQueriesAndWrites stresses the metrics
// engine with 10 accounts and dozens of concurrent goroutines mixing writes and reads.
func TestChallenger_Metrics_MultiAccount_ConcurrentQueriesAndWrites(t *testing.T) {
	_, accRepo, _, svc := setupStressDB(t)
	ctx := context.Background()

	const numAccounts = 10
	accountIDs := make([]string, numAccounts)
	for i := 0; i < numAccounts; i++ {
		id := fmt.Sprintf("acc-stress-%02d", i)
		accountIDs[i] = id
		acc := &domain.Account{
			ID:          id,
			Email:       fmt.Sprintf("user%02d@stress.test", i),
			Status:      domain.AccountStatusActive,
			IsActive:    i == 0,
			TokenExpiry: time.Now().Add(24 * time.Hour),
		}
		if err := accRepo.Create(ctx, acc); err != nil {
			t.Fatalf("Create account %s failed: %v", id, err)
		}
	}

	// Pre-seed some metrics
	now := time.Now().UTC()
	for _, id := range accountIDs {
		_ = svc.Record(ctx, &domain.TokenMetric{
			AccountID:           id,
			RequestPath:         "/v1internal:streamGenerateContent",
			PromptTokens:        150,
			CandidatesTokens:    50,
			TotalTokens:         200,
			CachedContentTokens: 20,
			ThoughtsTokens:      10,
			Timestamp:           now.Add(-2 * time.Hour),
		})
	}

	// Spin up 30 concurrent goroutines executing simultaneous read/write workloads
	const numWorkers = 30
	var wg sync.WaitGroup
	errCh := make(chan error, numWorkers*10)

	stopCh := make(chan struct{})
	time.AfterFunc(1500*time.Millisecond, func() {
		close(stopCh)
	})

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))

			for {
				select {
				case <-stopCh:
					return
				default:
				}

				targetAcc := accountIDs[r.Intn(numAccounts)]
				op := r.Intn(6)

				switch op {
				case 0: // Write / Record
					err := svc.Record(ctx, &domain.TokenMetric{
						AccountID:        targetAcc,
						RequestPath:      "/v1internal:streamGenerateContent",
						PromptTokens:     int64(r.Intn(500) + 50),
						CandidatesTokens: int64(r.Intn(200) + 10),
						Timestamp:        now.Add(-time.Duration(r.Intn(100)) * time.Hour),
					})
					if err != nil {
						errCh <- fmt.Errorf("Record error: %w", err)
						return
					}

				case 1: // GetSummary for specific account
					periods := []domain.MetricPeriod{domain.PeriodDay, domain.PeriodWeek, domain.PeriodMonth, domain.PeriodLifetime}
					p := periods[r.Intn(len(periods))]
					summary, err := svc.GetSummary(ctx, targetAcc, p)
					if err != nil {
						errCh <- fmt.Errorf("GetSummary error: %w", err)
						return
					}
					if summary == nil {
						errCh <- errors.New("GetSummary returned nil summary")
						return
					}

				case 2: // GetGlobalSummary
					periods := []domain.MetricPeriod{domain.PeriodDay, domain.PeriodWeek, domain.PeriodMonth, domain.PeriodLifetime}
					p := periods[r.Intn(len(periods))]
					summary, err := svc.GetGlobalSummary(ctx, p)
					if err != nil {
						errCh <- fmt.Errorf("GetGlobalSummary error: %w", err)
						return
					}
					if summary == nil {
						errCh <- errors.New("GetGlobalSummary returned nil summary")
						return
					}

				case 3: // GetAccountBreakdown
					breakdown, err := svc.GetAccountBreakdown(ctx, domain.PeriodDay)
					if err != nil {
						errCh <- fmt.Errorf("GetAccountBreakdown error: %w", err)
						return
					}
					if len(breakdown) != numAccounts {
						errCh <- fmt.Errorf("GetAccountBreakdown expected %d accounts, got %d", numAccounts, len(breakdown))
						return
					}

				case 4: // GetDailyUsage with zeroFill
					days := r.Intn(60) + 1
					timeline, err := svc.GetDailyUsage(ctx, targetAcc, days, true)
					if err != nil {
						errCh <- fmt.Errorf("GetDailyUsage error: %w", err)
						return
					}
					if len(timeline) != days {
						errCh <- fmt.Errorf("GetDailyUsage expected %d items, got %d", days, len(timeline))
						return
					}

				case 5: // GetDashboardPayload
					payload, err := svc.GetDashboardPayload(ctx, 14)
					if err != nil {
						errCh <- fmt.Errorf("GetDashboardPayload error: %w", err)
						return
					}
					if payload == nil || payload.Summary.Today == nil || len(payload.Timeline) != 14 {
						errCh <- fmt.Errorf("GetDashboardPayload invalid response: %+v", payload)
						return
					}
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent worker error: %v", err)
	}
}

// TestChallenger_Metrics_PeriodNormalization tests edge cases in period strings.
func TestChallenger_Metrics_PeriodNormalization(t *testing.T) {
	tests := []struct {
		name    string
		input   domain.MetricPeriod
		want    domain.MetricPeriod
		wantErr bool
	}{
		{"empty string defaults to lifetime", "", domain.PeriodLifetime, false},
		{"uppercase DAY", "DAY", domain.PeriodDay, false},
		{"mixed case Today", " Today ", domain.PeriodDay, false},
		{"this_week alias", "this_week", domain.PeriodWeek, false},
		{"WEEKLY", "WEEKLY", domain.PeriodWeek, false},
		{"this_month alias", "this_month", domain.PeriodMonth, false},
		{"monthly", "monthly", domain.PeriodMonth, false},
		{"all_time alias", "all_time", domain.PeriodLifetime, false},
		{"TOTAL alias", "TOTAL", domain.PeriodLifetime, false},
		{"invalid period", "century", "", true},
		{"numeric period", "2026", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizePeriod(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("NormalizePeriod(%q) expected error, got nil", tc.input)
				}
				if !errors.Is(err, domain.ErrInvalidPeriod) {
					t.Errorf("NormalizePeriod(%q) expected ErrInvalidPeriod, got %v", tc.input, err)
				}
			} else {
				if err != nil {
					t.Fatalf("NormalizePeriod(%q) unexpected error: %v", tc.input, err)
				}
				if got != tc.want {
					t.Errorf("NormalizePeriod(%q) = %q, want %q", tc.input, got, tc.want)
				}
			}
		})
	}
}

// TestChallenger_Metrics_AccountValidation verifies GetSummary errors when account does not exist.
func TestChallenger_Metrics_AccountValidation(t *testing.T) {
	_, _, _, svc := setupStressDB(t)
	ctx := context.Background()

	_, err := svc.GetSummary(ctx, "non-existent-account-id", domain.PeriodDay)
	if !errors.Is(err, domain.ErrAccountNotFound) {
		t.Errorf("expected ErrAccountNotFound, got %v", err)
	}

	_, err = svc.GetDailyUsage(ctx, "non-existent-account-id", 7, true)
	if !errors.Is(err, domain.ErrAccountNotFound) {
		t.Errorf("expected ErrAccountNotFound, got %v", err)
	}
}
