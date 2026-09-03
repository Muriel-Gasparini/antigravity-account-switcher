package quota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/test/mocks"
)

// TestChallenger_Poller_Lifecycle_Sequential verifies that sequential Start/Stop
// cycles properly terminate tickers and clean up goroutines without leakage.
func TestChallenger_Poller_Lifecycle_Sequential(t *testing.T) {
	_, accRepo, quotaRepo := setupTestStore(t)
	ctx := context.Background()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"groups":[]}`))
	}))
	defer server.Close()

	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	baseGoroutines := runtime.NumGoroutine()

	poller, err := NewPoller(accRepo, quotaRepo,
		WithBaseURL(server.URL),
		WithPollInterval(20*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewPoller failed: %v", err)
	}

	// 20 sequential Start / Stop cycles
	for i := 0; i < 20; i++ {
		if err := poller.Start(ctx); err != nil {
			t.Fatalf("Start cycle %d failed: %v", i, err)
		}
		if !poller.IsRunning() {
			t.Fatalf("cycle %d: poller reported not running", i)
		}
		time.Sleep(5 * time.Millisecond)
		if err := poller.Stop(); err != nil {
			t.Fatalf("Stop cycle %d failed: %v", i, err)
		}
		if poller.IsRunning() {
			t.Fatalf("cycle %d: poller reported still running after Stop", i)
		}
	}

	// Idempotent Stop
	if err := poller.Stop(); err != nil {
		t.Fatalf("idempotent Stop failed: %v", err)
	}

	// Start with canceled context
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := poller.Start(canceledCtx); err != nil {
		t.Fatalf("Start with canceled context failed: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	_ = poller.Stop()

	// Verify goroutine count
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	finalGoroutines := runtime.NumGoroutine()
	if delta := finalGoroutines - baseGoroutines; delta > 2 {
		t.Errorf("potential goroutine leak detected: base=%d, final=%d (delta=%d)",
			baseGoroutines, finalGoroutines, delta)
	}
}

// TestChallenger_Poller_Lifecycle_ConcurrentStartStop_ExposesRace stresses
// concurrent calls to Start and Stop, demonstrating whether race conditions or channel panics occur.
func TestChallenger_Poller_Lifecycle_ConcurrentStartStop_ExposesRace(t *testing.T) {
	_, accRepo, quotaRepo := setupTestStore(t)
	ctx := context.Background()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"groups":[]}`))
	}))
	defer server.Close()

	poller, err := NewPoller(accRepo, quotaRepo,
		WithBaseURL(server.URL),
		WithPollInterval(20*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewPoller failed: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = poller.Start(ctx)
		}()
		go func() {
			defer wg.Done()
			_ = poller.Stop()
		}()
	}
	wg.Wait()
	_ = poller.Stop()
}

// TestChallenger_Poller_RapidQuotaReset_Prong1AndProng2 stresses auto-restore:
// Prong 1 (DB-level reset_time elapsed) and Prong 2 (API poll non-zero remaining fraction),
// along with multi-bucket gating and rapid state cycling.
func TestChallenger_Poller_RapidQuotaReset_Prong1AndProng2(t *testing.T) {
	_, accRepo, quotaRepo := setupTestStore(t)
	ctx := context.Background()
	broadcaster := &mockBroadcaster{}

	mockPA := mocks.NewMockGoogleServer()
	defer mockPA.Close()

	// 1. Create 5 exhausted accounts
	accountIDs := make([]string, 5)
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("acc-stress-reset-%d", i)
		accountIDs[i] = id
		acc := &domain.Account{
			ID:          id,
			Email:       fmt.Sprintf("user%d@example.com", i),
			AccessToken: fmt.Sprintf("token-%d", i),
			TokenExpiry: time.Now().Add(1 * time.Hour),
			Status:      domain.AccountStatusExhausted,
		}
		if err := accRepo.Create(ctx, acc); err != nil {
			t.Fatalf("Create account %s: %v", id, err)
		}

		mockPA.SetAccountQuota(fmt.Sprintf("token-%d", i), []mocks.QuotaSummaryBucket{
			{
				BucketID:          "gemini-2.5-pro",
				DisplayName:       "Gemini 2.5 Pro",
				RemainingFraction: 0.8,
				RemainingAmount:   800,
				ResetTime:         time.Now().Add(24 * time.Hour),
			},
		})
	}

	poller, err := NewPoller(accRepo, quotaRepo,
		WithBaseURL(mockPA.URL),
		WithEventBroadcaster(broadcaster),
	)
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}

	// Poll once - Prong 2 should restore all 5 accounts to active
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce failed: %v", err)
	}

	for _, id := range accountIDs {
		acc, err := accRepo.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("GetByID %s: %v", id, err)
		}
		if acc.Status != domain.AccountStatusActive {
			t.Errorf("expected account %s to be active, got %s", id, acc.Status)
		}
	}

	// 2. Test Prong 1 (DB-level reset_time elapsed when API fails)
	failingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failingServer.Close()

	pFail, _ := NewPoller(accRepo, quotaRepo,
		WithBaseURL(failingServer.URL),
		WithEventBroadcaster(broadcaster),
	)

	past := time.Now().Add(-10 * time.Minute)
	for _, id := range accountIDs {
		if err := accRepo.UpdateStatus(ctx, id, domain.AccountStatusExhausted); err != nil {
			t.Fatalf("UpdateStatus exhausted: %v", err)
		}
		if err := quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
			{
				AccountID:         id,
				BucketID:          "gemini-2.5-pro",
				DisplayName:       "Gemini 2.5 Pro",
				Window:            domain.QuotaWindowDaily,
				RemainingFraction: 0.0,
				ResetTime:         past,
			},
		}); err != nil {
			t.Fatalf("UpsertBuckets: %v", err)
		}
	}

	// Run PollOnce - API returns 500, but Prong 1 must restore all accounts from DB
	_ = pFail.PollOnce(ctx)

	for _, id := range accountIDs {
		acc, err := accRepo.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("GetByID %s: %v", id, err)
		}
		if acc.Status != domain.AccountStatusActive {
			t.Errorf("Prong 1: expected account %s to be restored to active, got %s", id, acc.Status)
		}
	}

	// 3. Multi-bucket gating: account with 2 buckets where only 1 has reset
	multiAccID := "acc-multi-bucket"
	accMulti := &domain.Account{
		ID:          multiAccID,
		Email:       "multi@example.com",
		AccessToken: "token-multi",
		TokenExpiry: time.Now().Add(1 * time.Hour),
		Status:      domain.AccountStatusExhausted,
	}
	_ = accRepo.Create(ctx, accMulti)

	// Bucket 1 passed reset, Bucket 2 NOT passed reset
	_ = quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
		{
			AccountID:         multiAccID,
			BucketID:          "bucket-daily",
			Window:            domain.QuotaWindowDaily,
			RemainingFraction: 0.0,
			ResetTime:         time.Now().Add(-1 * time.Hour),
		},
		{
			AccountID:         multiAccID,
			BucketID:          "bucket-weekly",
			Window:            domain.QuotaWindowWeekly,
			RemainingFraction: 0.0,
			ResetTime:         time.Now().Add(10 * time.Hour),
		},
	})

	_ = pFail.PollOnce(ctx)

	accCheck, _ := accRepo.GetByID(ctx, multiAccID)
	if accCheck.Status != domain.AccountStatusExhausted {
		t.Errorf("expected multi-bucket account to remain exhausted because bucket-weekly is unreset, got %s", accCheck.Status)
	}

	// Now advance bucket-weekly reset_time to the past
	_ = quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
		{
			AccountID:         multiAccID,
			BucketID:          "bucket-weekly",
			Window:            domain.QuotaWindowWeekly,
			RemainingFraction: 0.0,
			ResetTime:         time.Now().Add(-1 * time.Minute),
		},
	})

	_ = pFail.PollOnce(ctx)
	accCheck, _ = accRepo.GetByID(ctx, multiAccID)
	if accCheck.Status != domain.AccountStatusActive {
		t.Errorf("expected multi-bucket account to be restored once all buckets passed reset, got %s", accCheck.Status)
	}

	// 4. Invariant checks: disabled and error accounts must never auto-restore
	disabledAcc := &domain.Account{
		ID:          "acc-disabled-check",
		Email:       "disabled@example.com",
		AccessToken: "token-dis",
		TokenExpiry: time.Now().Add(1 * time.Hour),
		Status:      domain.AccountStatusDisabled,
	}
	_ = accRepo.Create(ctx, disabledAcc)
	_ = quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
		{AccountID: disabledAcc.ID, BucketID: "b1", RemainingFraction: 1.0, ResetTime: past},
	})

	errorAcc := &domain.Account{
		ID:          "acc-error-check",
		Email:       "error@example.com",
		AccessToken: "token-err",
		TokenExpiry: time.Now().Add(1 * time.Hour),
		Status:      domain.AccountStatusError,
	}
	_ = accRepo.Create(ctx, errorAcc)
	_ = quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
		{AccountID: errorAcc.ID, BucketID: "b1", RemainingFraction: 1.0, ResetTime: past},
	})

	_ = poller.PollOnce(ctx)
	_ = pFail.PollOnce(ctx)

	disCheck, _ := accRepo.GetByID(ctx, disabledAcc.ID)
	if disCheck.Status != domain.AccountStatusDisabled {
		t.Errorf("disabled account was improperly restored: %s", disCheck.Status)
	}
	errCheck, _ := accRepo.GetByID(ctx, errorAcc.ID)
	if errCheck.Status != domain.AccountStatusError {
		t.Errorf("error account was improperly restored: %s", errCheck.Status)
	}
}

// TestChallenger_Poller_TokenRefresh_And_401Retry stresses:
// 1. Automatic refresh for expired tokens.
// 2. 401 retry on unexpected token invalidation.
// 3. 401 retry failure transitions to AccountStatusError.
// 4. Concurrent execution under race detector.
func TestChallenger_Poller_TokenRefresh_And_401Retry(t *testing.T) {
	_, accRepo, quotaRepo := setupTestStore(t)
	ctx := context.Background()

	var reqCount int64
	var authTokens []string
	var authMu sync.Mutex

	// Mock server that returns 401 on "bad-token" and 200 on "fresh-token"
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&reqCount, 1)
		auth := r.Header.Get("Authorization")
		authMu.Lock()
		authTokens = append(authTokens, auth)
		authMu.Unlock()

		if auth == "Bearer bad-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if auth == "Bearer fresh-token" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"groups":[{"displayName":"pro","buckets":[{"bucketId":"pro","window":"DAILY","remainingFraction":0.9}]}]}`))
			return
		}
		if auth == "Bearer permanent-401" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer authServer.Close()

	// 1. Proactive expired token refresh
	now := time.Now().UTC()
	accProactive := &domain.Account{
		ID:           "acc-proactive",
		Email:        "proactive@example.com",
		AccessToken:  "old-token",
		RefreshToken: "rt-proactive",
		TokenExpiry:  now.Add(-10 * time.Minute), // Expired
		Status:       domain.AccountStatusActive,
	}
	_ = accRepo.Create(ctx, accProactive)

	proactiveRefreshed := false
	refresher := TokenRefresherFunc(func(ctx context.Context, rt string) (string, time.Time, error) {
		if rt == "rt-proactive" {
			proactiveRefreshed = true
			return "fresh-token", now.Add(1 * time.Hour), nil
		}
		if rt == "rt-401" {
			return "fresh-token", now.Add(1 * time.Hour), nil
		}
		if rt == "rt-revoked" {
			return "", time.Time{}, errors.New("invalid_grant: token has been revoked")
		}
		if rt == "rt-perm-401" {
			return "permanent-401", now.Add(1 * time.Hour), nil
		}
		return "", time.Time{}, errors.New("unknown refresh token")
	})

	poller, err := NewPoller(accRepo, quotaRepo,
		WithBaseURL(authServer.URL),
		WithTokenRefresher(refresher),
	)
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce proactive failed: %v", err)
	}

	if !proactiveRefreshed {
		t.Fatal("expected proactive refresh to occur for expired token")
	}

	accCheck, _ := accRepo.GetByID(ctx, accProactive.ID)
	if accCheck.AccessToken != "fresh-token" {
		t.Errorf("expected AccessToken 'fresh-token', got %s", accCheck.AccessToken)
	}

	// 2. 401 Unexpected Expiration -> Force Refresh -> Retry 200 OK
	acc401 := &domain.Account{
		ID:           "acc-401-retry",
		Email:        "retry401@example.com",
		AccessToken:  "bad-token", // Not expired locally, but rejected with 401
		RefreshToken: "rt-401",
		TokenExpiry:  now.Add(1 * time.Hour),
		Status:       domain.AccountStatusActive,
	}
	_ = accRepo.Create(ctx, acc401)

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce with 401 retry failed: %v", err)
	}

	accCheck, _ = accRepo.GetByID(ctx, acc401.ID)
	if accCheck.AccessToken != "fresh-token" {
		t.Errorf("expected 401 retry to update token to fresh-token, got %s", accCheck.AccessToken)
	}
	if accCheck.Status != domain.AccountStatusActive {
		t.Errorf("expected account to stay active after successful 401 retry, got %s", accCheck.Status)
	}

	// 3. 401 when refresher fails -> transitions to AccountStatusError
	accRevoked := &domain.Account{
		ID:           "acc-revoked",
		Email:        "revoked@example.com",
		AccessToken:  "bad-token",
		RefreshToken: "rt-revoked",
		TokenExpiry:  now.Add(1 * time.Hour),
		Status:       domain.AccountStatusActive,
	}
	_ = accRepo.Create(ctx, accRevoked)

	_ = poller.PollOnce(ctx)

	accCheck, _ = accRepo.GetByID(ctx, accRevoked.ID)
	if accCheck.Status != domain.AccountStatusError {
		t.Errorf("expected account with failed refresher to transition to error, got %s", accCheck.Status)
	}

	// 4. 401 when retried request also returns 401 -> transitions to AccountStatusError
	accPerm401 := &domain.Account{
		ID:           "acc-perm-401",
		Email:        "perm401@example.com",
		AccessToken:  "bad-token",
		RefreshToken: "rt-perm-401",
		TokenExpiry:  now.Add(1 * time.Hour),
		Status:       domain.AccountStatusActive,
	}
	_ = accRepo.Create(ctx, accPerm401)

	_ = poller.PollOnce(ctx)

	accCheck, _ = accRepo.GetByID(ctx, accPerm401.ID)
	if accCheck.Status != domain.AccountStatusError {
		t.Errorf("expected account with repeated 401 to transition to error, got %s", accCheck.Status)
	}

	// 5. Concurrent PollAccount and PollOnce stress under -race
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = poller.PollOnce(ctx)
		}()
		go func() {
			defer wg.Done()
			_, _ = poller.PollAccount(ctx, acc401.ID)
		}()
	}
	wg.Wait()
}

// TestChallenger_Poller_Lifecycle_ExternalContextCancellation verifies whether
// the poller accurately updates its running status when the parent context is cancelled.
func TestChallenger_Poller_Lifecycle_ExternalContextCancellation(t *testing.T) {
	_, accRepo, quotaRepo := setupTestStore(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"groups":[]}`))
	}))
	defer server.Close()

	poller, err := NewPoller(accRepo, quotaRepo,
		WithBaseURL(server.URL),
		WithPollInterval(20*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewPoller failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := poller.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if !poller.IsRunning() {
		t.Fatal("expected poller to be running")
	}

	// Cancel external context
	cancel()
	time.Sleep(50 * time.Millisecond) // Allow loop to exit

	// If the loop exited, IsRunning should report false, and Start should be allowed
	if poller.IsRunning() {
		t.Errorf("poller.IsRunning() returned true after context was cancelled and loop terminated")
	}

	// Attempt to start again with fresh context
	if err := poller.Start(context.Background()); err != nil {
		t.Errorf("poller.Start() failed after context cancellation: %v", err)
	}
	_ = poller.Stop()
}
