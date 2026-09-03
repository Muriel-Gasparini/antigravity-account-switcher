package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/web"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/test/mocks"
)

// TestTier3_QuotaPoller_AutoRestore validates that the background quota daemon
// polls :retrieveUserQuotaSummary and auto-restores an exhausted account when quota refreshes.
func TestTier3_QuotaPoller_AutoRestore(t *testing.T) {
	// Fast poll interval for test responsiveness
	env := setupE2EEnvironment(t, 25*time.Millisecond)
	ctx := context.Background()

	now := time.Now().UTC()
	acc := &domain.Account{
		ID:          "acc-exhausted-daemon",
		Email:       "exhausted_user@example.com",
		AccessToken: "token-exhausted-123",
		IsActive:    true,
		Status:      domain.AccountStatusExhausted, // Initially exhausted
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := env.AccountRepo.Create(ctx, acc); err != nil {
		t.Fatalf("create account: %v", err)
	}

	// 1. Configure mock server with non-zero remaining fraction (replenished quota)
	env.MockGoogle.SetAccountQuota("token-exhausted-123", []mocks.QuotaSummaryBucket{
		{
			BucketID:          "gemini-2.5-pro",
			DisplayName:       "Gemini 2.5 Pro",
			Window:            "DAILY",
			RemainingFraction: 0.90, // 90% quota restored
			RemainingAmount:   900,
			ResetTime:         now.Add(24 * time.Hour),
		},
	})

	// 2. Start the poller daemon
	if err := env.Poller.Start(ctx); err != nil {
		t.Fatalf("start poller: %v", err)
	}

	// 3. Wait for poller to run and auto-restore account
	restored := false
	for i := 0; i < 40; i++ {
		time.Sleep(25 * time.Millisecond)
		updated, err := env.AccountRepo.GetByID(ctx, "acc-exhausted-daemon")
		if err == nil && updated.Status == domain.AccountStatusActive {
			restored = true
			break
		}
	}

	if !restored {
		t.Fatalf("expected account to be auto-restored to 'active' by daemon")
	}

	// 4. Verify quota bucket is saved in SQLite
	buckets, err := env.QuotaRepo.GetByAccountID(ctx, "acc-exhausted-daemon")
	if err != nil {
		t.Fatalf("get quota buckets: %v", err)
	}
	if len(buckets) == 0 {
		t.Fatalf("expected quota bucket saved in DB, got 0")
	}
	if buckets[0].RemainingFraction != 0.90 {
		t.Errorf("expected remaining fraction 0.90, got %f", buckets[0].RemainingFraction)
	}
}

// TestTier3_REST_API_Endpoints validates the dashboard REST endpoints.
func TestTier3_REST_API_Endpoints(t *testing.T) {
	env := setupE2EEnvironment(t, 0)
	ctx := context.Background()

	// Seed 2 accounts
	now := time.Now().UTC()
	acc1 := &domain.Account{
		ID:          "acc-rest-1",
		Email:       "rest1@example.com",
		AccessToken: "tok-rest-1",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	acc2 := &domain.Account{
		ID:          "acc-rest-2",
		Email:       "rest2@example.com",
		AccessToken: "tok-rest-2",
		IsActive:    false,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_ = env.AccountRepo.Create(ctx, acc1)
	_ = env.AccountRepo.Create(ctx, acc2)

	// 1. GET /api/status
	respStatus, err := http.Get(env.ServerURL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	defer respStatus.Body.Close()
	if respStatus.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", respStatus.StatusCode)
	}
	var status web.StatusResponse
	_ = json.NewDecoder(respStatus.Body).Decode(&status)
	if status.TotalAccounts != 2 {
		t.Errorf("expected 2 accounts, got %d", status.TotalAccounts)
	}

	// 2. GET /api/accounts
	respAccs, err := http.Get(env.ServerURL + "/api/accounts")
	if err != nil {
		t.Fatalf("GET /api/accounts: %v", err)
	}
	defer respAccs.Body.Close()
	var accs []*web.AccountWithBuckets
	_ = json.NewDecoder(respAccs.Body).Decode(&accs)
	if len(accs) != 2 {
		t.Fatalf("expected 2 accounts in list, got %d", len(accs))
	}

	// 3. POST /api/accounts/acc-rest-2/select
	respSelect, err := http.Post(env.ServerURL+"/api/accounts/acc-rest-2/select", "application/json", nil)
	if err != nil {
		t.Fatalf("POST select: %v", err)
	}
	defer respSelect.Body.Close()
	if respSelect.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from select, got %d", respSelect.StatusCode)
	}

	active, _ := env.AccountRepo.GetActive(ctx)
	if active == nil || active.ID != "acc-rest-2" {
		t.Errorf("expected active account to be acc-rest-2, got %v", active)
	}

	// 4. GET /api/metrics
	respMetrics, err := http.Get(env.ServerURL + "/api/metrics")
	if err != nil {
		t.Fatalf("GET /api/metrics: %v", err)
	}
	defer respMetrics.Body.Close()
	if respMetrics.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", respMetrics.StatusCode)
	}
}
