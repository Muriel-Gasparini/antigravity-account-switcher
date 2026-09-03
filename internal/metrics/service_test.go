package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
)

type mockAccountRepo struct {
	accounts []*domain.Account
	getByID  map[string]*domain.Account
	err      error
}

func (m *mockAccountRepo) Create(ctx context.Context, acc *domain.Account) error { return nil }
func (m *mockAccountRepo) GetByID(ctx context.Context, id string) (*domain.Account, error) {
	if m.err != nil {
		return nil, m.err
	}
	if acc, ok := m.getByID[id]; ok {
		return acc, nil
	}
	return nil, domain.ErrAccountNotFound
}
func (m *mockAccountRepo) GetByEmail(ctx context.Context, email string) (*domain.Account, error) {
	return nil, nil
}
func (m *mockAccountRepo) GetActive(ctx context.Context) (*domain.Account, error) { return nil, nil }
func (m *mockAccountRepo) List(ctx context.Context) ([]*domain.Account, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.accounts, nil
}
func (m *mockAccountRepo) SetActive(ctx context.Context, id string) error                 { return nil }
func (m *mockAccountRepo) UpdateStatus(ctx context.Context, id string, status domain.AccountStatus) error {
	return nil
}
func (m *mockAccountRepo) UpdateToken(ctx context.Context, id string, accessToken string, expiry time.Time) error {
	return nil
}
func (m *mockAccountRepo) UpdateRefreshToken(ctx context.Context, id string, refreshToken string) error {
	return nil
}
func (m *mockAccountRepo) Delete(ctx context.Context, id string) error { return nil }
func (m *mockAccountRepo) GetNextAvailable(ctx context.Context, excludeID string) (*domain.Account, error) {
	return nil, nil
}

type mockMetricsRepo struct {
	summaryRes         *domain.AggregatedMetrics
	summariesRes       map[string]*domain.AggregatedMetrics
	dailyHistoryRes    []*domain.DailyTokenUsage
	recorded           []*domain.TokenMetric
	summaryCalls       []struct{ accID, period string }
	summariesCalls     []string
	dailyHistoryCalls  []struct{ accID string; days int }
	recordErr          error
	summaryErr         error
	summariesErr       error
	dailyHistoryErr    error
}

func (m *mockMetricsRepo) Record(ctx context.Context, metric *domain.TokenMetric) error {
	if m.recordErr != nil {
		return m.recordErr
	}
	m.recorded = append(m.recorded, metric)
	return nil
}

func (m *mockMetricsRepo) GetSummary(ctx context.Context, accountID string, period string) (*domain.AggregatedMetrics, error) {
	m.summaryCalls = append(m.summaryCalls, struct{ accID, period string }{accountID, period})
	if m.summaryErr != nil {
		return nil, m.summaryErr
	}
	if m.summaryRes != nil {
		return m.summaryRes, nil
	}
	return &domain.AggregatedMetrics{}, nil
}

func (m *mockMetricsRepo) GetAccountSummaries(ctx context.Context, period string) (map[string]*domain.AggregatedMetrics, error) {
	m.summariesCalls = append(m.summariesCalls, period)
	if m.summariesErr != nil {
		return nil, m.summariesErr
	}
	return m.summariesRes, nil
}

func (m *mockMetricsRepo) GetDailyHistory(ctx context.Context, accountID string, days int) ([]*domain.DailyTokenUsage, error) {
	m.dailyHistoryCalls = append(m.dailyHistoryCalls, struct{ accID string; days int }{accountID, days})
	if m.dailyHistoryErr != nil {
		return nil, m.dailyHistoryErr
	}
	return m.dailyHistoryRes, nil
}

func TestNormalizePeriod(t *testing.T) {
	tests := []struct {
		input    domain.MetricPeriod
		expected domain.MetricPeriod
		wantErr  bool
	}{
		{"day", domain.PeriodDay, false},
		{"DAY", domain.PeriodDay, false},
		{" daily ", domain.PeriodDay, false},
		{"today", domain.PeriodDay, false},
		{"week", domain.PeriodWeek, false},
		{"weekly", domain.PeriodWeek, false},
		{"this_week", domain.PeriodWeek, false},
		{"month", domain.PeriodMonth, false},
		{"monthly", domain.PeriodMonth, false},
		{"this_month", domain.PeriodMonth, false},
		{"lifetime", domain.PeriodLifetime, false},
		{"total", domain.PeriodLifetime, false},
		{"all", domain.PeriodLifetime, false},
		{"all_time", domain.PeriodLifetime, false},
		{"", domain.PeriodLifetime, false},
		{"invalid", "", true},
		{"year", "", true},
	}

	for _, tc := range tests {
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
				t.Errorf("NormalizePeriod(%q) unexpected error: %v", tc.input, err)
			}
			if got != tc.expected {
				t.Errorf("NormalizePeriod(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		}
	}
}

func TestService_GetSummary(t *testing.T) {
	accRepo := &mockAccountRepo{
		getByID: map[string]*domain.Account{
			"acc-1": {ID: "acc-1", Email: "test@example.com"},
		},
	}
	metricsRepo := &mockMetricsRepo{
		summaryRes: &domain.AggregatedMetrics{
			TotalTokens:   1500,
			TotalRequests: 10,
		},
	}
	svc := NewService(metricsRepo, accRepo)

	ctx := context.Background()

	// Existing account
	res, err := svc.GetSummary(ctx, "acc-1", domain.PeriodDay)
	if err != nil {
		t.Fatalf("GetSummary failed: %v", err)
	}
	if res.TotalTokens != 1500 {
		t.Errorf("expected 1500 tokens, got %d", res.TotalTokens)
	}

	// Non-existing account
	_, err = svc.GetSummary(ctx, "acc-unknown", domain.PeriodDay)
	if !errors.Is(err, domain.ErrAccountNotFound) {
		t.Errorf("expected ErrAccountNotFound, got %v", err)
	}

	// Invalid period
	_, err = svc.GetSummary(ctx, "acc-1", "bad_period")
	if !errors.Is(err, domain.ErrInvalidPeriod) {
		t.Errorf("expected ErrInvalidPeriod, got %v", err)
	}
}

func TestService_GetGlobalSummary(t *testing.T) {
	metricsRepo := &mockMetricsRepo{
		summaryRes: &domain.AggregatedMetrics{
			TotalTokens: 5000,
		},
	}
	svc := NewService(metricsRepo, nil)

	res, err := svc.GetGlobalSummary(context.Background(), domain.PeriodWeek)
	if err != nil {
		t.Fatalf("GetGlobalSummary failed: %v", err)
	}
	if res.TotalTokens != 5000 {
		t.Errorf("expected 5000 tokens, got %d", res.TotalTokens)
	}
	if len(metricsRepo.summaryCalls) != 1 || metricsRepo.summaryCalls[0].accID != "" {
		t.Errorf("expected global call with empty account ID, got: %+v", metricsRepo.summaryCalls)
	}
}

func TestService_GetAccountBreakdown(t *testing.T) {
	acc1 := &domain.Account{ID: "acc-1", Email: "a@test.com", Status: domain.AccountStatusActive, IsActive: true}
	acc2 := &domain.Account{ID: "acc-2", Email: "b@test.com", Status: domain.AccountStatusExhausted, IsActive: false}
	acc3 := &domain.Account{ID: "acc-3", Email: "c@test.com", Status: domain.AccountStatusActive, IsActive: false}

	accRepo := &mockAccountRepo{
		accounts: []*domain.Account{acc1, acc2, acc3},
	}

	metricsRepo := &mockMetricsRepo{
		summariesRes: map[string]*domain.AggregatedMetrics{
			"acc-1": {TotalTokens: 1000, TotalRequests: 5},
			"acc-2": {TotalTokens: 500, TotalRequests: 2},
			// acc-3 has no usage
		},
	}

	svc := NewService(metricsRepo, accRepo)

	breakdown, err := svc.GetAccountBreakdown(context.Background(), domain.PeriodDay)
	if err != nil {
		t.Fatalf("GetAccountBreakdown failed: %v", err)
	}

	if len(breakdown) != 3 {
		t.Fatalf("expected 3 accounts in breakdown, got %d", len(breakdown))
	}

	// Verify acc-1
	if breakdown[0].AccountID != "acc-1" || breakdown[0].Metrics.TotalTokens != 1000 || !breakdown[0].IsActive {
		t.Errorf("unexpected acc-1 data: %+v", breakdown[0])
	}
	// Verify acc-2
	if breakdown[1].AccountID != "acc-2" || breakdown[1].Metrics.TotalTokens != 500 || breakdown[1].Status != domain.AccountStatusExhausted {
		t.Errorf("unexpected acc-2 data: %+v", breakdown[1])
	}
	// Verify acc-3 (zero usage)
	if breakdown[2].AccountID != "acc-3" || breakdown[2].Metrics.TotalTokens != 0 || breakdown[2].Metrics.TotalRequests != 0 {
		t.Errorf("expected zero usage for acc-3, got %+v", breakdown[2].Metrics)
	}
}

func TestService_GetDailyUsage_ZeroFilled(t *testing.T) {
	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	threeDaysAgo := now.AddDate(0, 0, -3).Format("2006-01-02")

	metricsRepo := &mockMetricsRepo{
		dailyHistoryRes: []*domain.DailyTokenUsage{
			{Date: threeDaysAgo, TotalTokens: 300, RequestCount: 2},
			{Date: today, TotalTokens: 600, RequestCount: 4},
		},
	}

	svc := NewService(metricsRepo, nil)

	// Test zeroFill = false
	raw, err := svc.GetDailyUsage(context.Background(), "", 5, false)
	if err != nil {
		t.Fatalf("GetDailyUsage without zeroFill failed: %v", err)
	}
	if len(raw) != 2 {
		t.Errorf("expected 2 raw entries, got %d", len(raw))
	}

	// Test zeroFill = true
	filled, err := svc.GetDailyUsage(context.Background(), "", 5, true)
	if err != nil {
		t.Fatalf("GetDailyUsage with zeroFill failed: %v", err)
	}
	if len(filled) != 5 {
		t.Fatalf("expected 5 filled entries for 5 days, got %d", len(filled))
	}

	// Chronological order verification
	for i := 0; i < len(filled)-1; i++ {
		if filled[i].Date >= filled[i+1].Date {
			t.Errorf("timeline not chronological: %s followed by %s", filled[i].Date, filled[i+1].Date)
		}
	}

	// Today must be the last entry and have 600 tokens
	last := filled[len(filled)-1]
	if last.Date != today || last.TotalTokens != 600 {
		t.Errorf("last entry mismatch: got %+v, want date %s and 600 tokens", last, today)
	}
}

func TestService_GetDashboardPayload(t *testing.T) {
	accRepo := &mockAccountRepo{
		accounts: []*domain.Account{
			{ID: "acc-1", Email: "primary@example.com", Status: domain.AccountStatusActive, IsActive: true},
		},
	}
	metricsRepo := &mockMetricsRepo{
		summaryRes: &domain.AggregatedMetrics{TotalTokens: 1200},
		summariesRes: map[string]*domain.AggregatedMetrics{
			"acc-1": {TotalTokens: 1200},
		},
		dailyHistoryRes: []*domain.DailyTokenUsage{
			{Date: time.Now().UTC().Format("2006-01-02"), TotalTokens: 1200},
		},
	}

	svc := NewService(metricsRepo, accRepo)

	payload, err := svc.GetDashboardPayload(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetDashboardPayload failed: %v", err)
	}

	if payload.Summary.Today == nil || payload.Summary.ThisWeek == nil || payload.Summary.ThisMonth == nil || payload.Summary.AllTime == nil {
		t.Error("summary cards not fully populated")
	}
	if len(payload.ByAccount) != 1 {
		t.Errorf("expected 1 account in breakdown, got %d", len(payload.ByAccount))
	}
	if len(payload.Timeline) != 7 {
		t.Errorf("expected 7 timeline points, got %d", len(payload.Timeline))
	}
}

func TestService_Record(t *testing.T) {
	metricsRepo := &mockMetricsRepo{}
	svc := NewService(metricsRepo, nil)

	metric := &domain.TokenMetric{
		AccountID:   "acc-1",
		TotalTokens: 250,
	}
	err := svc.Record(context.Background(), metric)
	if err != nil {
		t.Fatalf("Record failed: %v", err)
	}
	if len(metricsRepo.recorded) != 1 || metricsRepo.recorded[0].TotalTokens != 250 {
		t.Errorf("Record did not properly forward metric to repo: %+v", metricsRepo.recorded)
	}
}

func TestService_IntegrationWithRealSQLite(t *testing.T) {
	dbPath := t.TempDir() + "/test_metrics_svc.db"
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open sqlite DB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	accRepo := sqlite.NewAccountRepository(db)
	metricsRepo := sqlite.NewMetricsRepository(db)
	svc := NewService(metricsRepo, accRepo)

	ctx := context.Background()

	// Create 2 accounts
	acc1 := &domain.Account{
		ID:           "acc-real-1",
		Email:        "alpha@example.com",
		RefreshToken: "rt1",
		AccessToken:  "at1",
		Status:       domain.AccountStatusActive,
		IsActive:     true,
	}
	acc2 := &domain.Account{
		ID:           "acc-real-2",
		Email:        "beta@example.com",
		RefreshToken: "rt2",
		AccessToken:  "at2",
		Status:       domain.AccountStatusActive,
		IsActive:     false,
	}
	if err := accRepo.Create(ctx, acc1); err != nil {
		t.Fatalf("accRepo.Create: %v", err)
	}
	if err := accRepo.Create(ctx, acc2); err != nil {
		t.Fatalf("accRepo.Create: %v", err)
	}

	now := time.Now().UTC()

	// Record metrics for acc1
	if err := svc.Record(ctx, &domain.TokenMetric{
		AccountID:           "acc-real-1",
		RequestPath:         "/v1internal:streamGenerateContent",
		PromptTokens:        100,
		CandidatesTokens:    50,
		TotalTokens:         150,
		CachedContentTokens: 20,
		ThoughtsTokens:      10,
		Timestamp:           now,
	}); err != nil {
		t.Fatalf("svc.Record: %v", err)
	}

	// Record metrics for acc2
	if err := svc.Record(ctx, &domain.TokenMetric{
		AccountID:        "acc-real-2",
		RequestPath:      "/v1internal:streamGenerateContent",
		PromptTokens:     200,
		CandidatesTokens: 100,
		TotalTokens:      300,
		Timestamp:        now.Add(-1 * time.Hour),
	}); err != nil {
		t.Fatalf("svc.Record: %v", err)
	}

	// 1. Test GetAccountSummaries direct on sqlite repo
	summaries, err := metricsRepo.GetAccountSummaries(ctx, "day")
	if err != nil {
		t.Fatalf("GetAccountSummaries: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("expected 2 accounts in summaries, got %d", len(summaries))
	}
	if summaries["acc-real-1"].TotalTokens != 150 {
		t.Errorf("acc-real-1 total tokens = %d, want 150", summaries["acc-real-1"].TotalTokens)
	}
	if summaries["acc-real-1"].TotalCachedContentTokens != 20 {
		t.Errorf("acc-real-1 cached content tokens = %d, want 20", summaries["acc-real-1"].TotalCachedContentTokens)
	}
	if summaries["acc-real-1"].TotalThoughtsTokens != 10 {
		t.Errorf("acc-real-1 thoughts tokens = %d, want 10", summaries["acc-real-1"].TotalThoughtsTokens)
	}

	// 2. Test GetAccountBreakdown via Service
	breakdown, err := svc.GetAccountBreakdown(ctx, domain.PeriodDay)
	if err != nil {
		t.Fatalf("GetAccountBreakdown: %v", err)
	}
	if len(breakdown) != 2 {
		t.Fatalf("expected 2 accounts in breakdown, got %d", len(breakdown))
	}

	// 3. Test GetDailyUsage with zeroFill
	dailyUsage, err := svc.GetDailyUsage(ctx, "acc-real-1", 7, true)
	if err != nil {
		t.Fatalf("GetDailyUsage: %v", err)
	}
	if len(dailyUsage) != 7 {
		t.Fatalf("expected 7 days of daily usage, got %d", len(dailyUsage))
	}
	todayUsage := dailyUsage[len(dailyUsage)-1]
	if todayUsage.TotalTokens != 150 || todayUsage.CachedContentTokens != 20 || todayUsage.ThoughtsTokens != 10 {
		t.Errorf("unexpected today usage: %+v", todayUsage)
	}

	// 4. Test GetDashboardPayload
	payload, err := svc.GetDashboardPayload(ctx, 14)
	if err != nil {
		t.Fatalf("GetDashboardPayload: %v", err)
	}
	if payload.Summary.Today.TotalTokens != 450 {
		t.Errorf("expected 450 total tokens today across pool, got %d", payload.Summary.Today.TotalTokens)
	}
}
