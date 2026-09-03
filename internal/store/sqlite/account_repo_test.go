package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
)

func setupTestStore(t *testing.T) (*sqlite.DB, *sqlite.AccountRepository, *sqlite.QuotaRepository, *sqlite.MetricsRepository, *sqlite.EventRepository) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	accRepo := sqlite.NewAccountRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)
	metricsRepo := sqlite.NewMetricsRepository(db)
	eventRepo := sqlite.NewEventRepository(db)

	return db, accRepo, quotaRepo, metricsRepo, eventRepo
}

func TestAccountRepository_CRUD(t *testing.T) {
	_, repo, _, _, _ := setupTestStore(t)
	ctx := context.Background()

	// 1. Create account
	acc := &domain.Account{
		ID:           "acc-1",
		Email:        "user1@gmail.com",
		RefreshToken: "rt-12345",
		AccessToken:  "at-12345",
		TokenExpiry:  time.Now().Add(1 * time.Hour).Truncate(time.Second),
		IsActive:     false,
		Status:       domain.AccountStatusActive,
	}
	if err := repo.Create(ctx, acc); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// 2. Duplicate email should return domain.ErrAccountEmailExists
	dupAcc := &domain.Account{
		ID:           "acc-dup",
		Email:        "user1@gmail.com",
		RefreshToken: "rt-other",
	}
	if err := repo.Create(ctx, dupAcc); !errors.Is(err, domain.ErrAccountEmailExists) {
		t.Fatalf("expected ErrAccountEmailExists, got %v", err)
	}

	// 3. GetByID
	fetched, err := repo.GetByID(ctx, "acc-1")
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}
	if fetched.Email != "user1@gmail.com" {
		t.Errorf("expected email user1@gmail.com, got %s", fetched.Email)
	}
	if fetched.RefreshToken != "rt-12345" {
		t.Errorf("expected refresh token rt-12345, got %s", fetched.RefreshToken)
	}
	if fetched.Status != domain.AccountStatusActive {
		t.Errorf("expected status active, got %s", fetched.Status)
	}
	if fetched.IsActive {
		t.Errorf("expected is_active false, got true")
	}

	// 4. GetByEmail
	byEmail, err := repo.GetByEmail(ctx, "user1@gmail.com")
	if err != nil {
		t.Fatalf("GetByEmail failed: %v", err)
	}
	if byEmail.ID != "acc-1" {
		t.Errorf("expected id acc-1, got %s", byEmail.ID)
	}

	// 5. Non-existent account returns ErrAccountNotFound
	if _, err := repo.GetByID(ctx, "non-existent"); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("expected ErrAccountNotFound, got %v", err)
	}
	if _, err := repo.GetByEmail(ctx, "nonexistent@gmail.com"); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("expected ErrAccountNotFound, got %v", err)
	}

	// 6. List accounts
	acc2 := &domain.Account{
		ID:           "acc-2",
		Email:        "user2@gmail.com",
		RefreshToken: "rt-222",
	}
	if err := repo.Create(ctx, acc2); err != nil {
		t.Fatalf("Create acc-2 failed: %v", err)
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(list))
	}
	if list[0].ID != "acc-1" || list[1].ID != "acc-2" {
		t.Errorf("expected accounts in creation order [acc-1, acc-2], got [%s, %s]", list[0].ID, list[1].ID)
	}

	// 7. UpdateStatus
	if err := repo.UpdateStatus(ctx, "acc-1", domain.AccountStatusExhausted); err != nil {
		t.Fatalf("UpdateStatus failed: %v", err)
	}
	updatedAcc, _ := repo.GetByID(ctx, "acc-1")
	if updatedAcc.Status != domain.AccountStatusExhausted {
		t.Errorf("expected status exhausted, got %s", updatedAcc.Status)
	}

	// 8. UpdateToken
	newExpiry := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	if err := repo.UpdateToken(ctx, "acc-1", "at-updated", newExpiry); err != nil {
		t.Fatalf("UpdateToken failed: %v", err)
	}
	tokenAcc, _ := repo.GetByID(ctx, "acc-1")
	if tokenAcc.AccessToken != "at-updated" {
		t.Errorf("expected token at-updated, got %s", tokenAcc.AccessToken)
	}

	// 9. UpdateRefreshToken
	if err := repo.UpdateRefreshToken(ctx, "acc-1", "rt-updated"); err != nil {
		t.Fatalf("UpdateRefreshToken failed: %v", err)
	}
	rtAcc, _ := repo.GetByID(ctx, "acc-1")
	if rtAcc.RefreshToken != "rt-updated" {
		t.Errorf("expected refresh token rt-updated, got %s", rtAcc.RefreshToken)
	}

	// 10. Delete account
	if err := repo.Delete(ctx, "acc-1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, err := repo.GetByID(ctx, "acc-1"); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("expected ErrAccountNotFound after delete, got %v", err)
	}
}

func TestAccountRepository_ActiveAndSingleActiveConstraint(t *testing.T) {
	db, repo, _, _, _ := setupTestStore(t)
	ctx := context.Background()

	// Initial check: no active account
	if _, err := repo.GetActive(ctx); !errors.Is(err, domain.ErrNoActiveAccount) {
		t.Fatalf("expected ErrNoActiveAccount initially, got %v", err)
	}

	// Create 2 accounts
	acc1 := &domain.Account{ID: "acc-1", Email: "u1@example.com", RefreshToken: "rt1"}
	acc2 := &domain.Account{ID: "acc-2", Email: "u2@example.com", RefreshToken: "rt2"}
	_ = repo.Create(ctx, acc1)
	_ = repo.Create(ctx, acc2)

	// Set acc-1 active
	if err := repo.SetActive(ctx, "acc-1"); err != nil {
		t.Fatalf("SetActive(acc-1) failed: %v", err)
	}
	active, err := repo.GetActive(ctx)
	if err != nil {
		t.Fatalf("GetActive failed: %v", err)
	}
	if active.ID != "acc-1" || !active.IsActive {
		t.Errorf("expected active account acc-1, got %+v", active)
	}

	// Switch to acc-2
	if err := repo.SetActive(ctx, "acc-2"); err != nil {
		t.Fatalf("SetActive(acc-2) failed: %v", err)
	}
	active, err = repo.GetActive(ctx)
	if err != nil {
		t.Fatalf("GetActive failed: %v", err)
	}
	if active.ID != "acc-2" || !active.IsActive {
		t.Errorf("expected active account acc-2, got %+v", active)
	}

	// Verify acc-1 is now inactive
	oldActive, err := repo.GetByID(ctx, "acc-1")
	if err != nil {
		t.Fatalf("GetByID(acc-1) failed: %v", err)
	}
	if oldActive.IsActive {
		t.Errorf("expected acc-1 to be inactive, got true")
	}

	// Verify that raw SQL cannot create a second active account (partial unique index constraint)
	_, err = db.ExecContext(ctx, "UPDATE accounts SET is_active = 1 WHERE id = 'acc-1'")
	if err == nil {
		t.Fatal("expected constraint error when trying to force two active accounts, got nil")
	}

	// SetActive on non-existent account returns ErrAccountNotFound
	if err := repo.SetActive(ctx, "non-existent"); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("expected ErrAccountNotFound, got %v", err)
	}
}

func TestAccountRepository_GetNextAvailable_LRU(t *testing.T) {
	_, repo, _, _, _ := setupTestStore(t)
	ctx := context.Background()

	// When pool is empty
	if _, err := repo.GetNextAvailable(ctx, ""); !errors.Is(err, domain.ErrNoAvailableAccount) {
		t.Fatalf("expected ErrNoAvailableAccount on empty pool, got %v", err)
	}

	// Populate 3 accounts with staggered updated_at timestamps
	now := time.Now().UTC()
	accs := []*domain.Account{
		{ID: "acc-1", Email: "u1@example.com", RefreshToken: "rt1", Status: domain.AccountStatusActive, UpdatedAt: now.Add(-30 * time.Minute)},
		{ID: "acc-2", Email: "u2@example.com", RefreshToken: "rt2", Status: domain.AccountStatusActive, UpdatedAt: now.Add(-20 * time.Minute)},
		{ID: "acc-3", Email: "u3@example.com", RefreshToken: "rt3", Status: domain.AccountStatusExhausted, UpdatedAt: now.Add(-40 * time.Minute)}, // Exhausted should be ignored
	}
	for _, acc := range accs {
		if err := repo.Create(ctx, acc); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	// GetNextAvailable excluding "" -> least recently updated active is acc-1
	next, err := repo.GetNextAvailable(ctx, "")
	if err != nil {
		t.Fatalf("GetNextAvailable failed: %v", err)
	}
	if next.ID != "acc-1" {
		t.Errorf("expected acc-1 (oldest updated active), got %s", next.ID)
	}

	// GetNextAvailable excluding "acc-1" -> next active is acc-2
	next, err = repo.GetNextAvailable(ctx, "acc-1")
	if err != nil {
		t.Fatalf("GetNextAvailable(exclude acc-1) failed: %v", err)
	}
	if next.ID != "acc-2" {
		t.Errorf("expected acc-2, got %s", next.ID)
	}

	// When all active accounts are excluded or exhausted
	if _, err := repo.GetNextAvailable(ctx, "acc-1"); err == nil {
		// Mark acc-2 exhausted too
		_ = repo.UpdateStatus(ctx, "acc-2", domain.AccountStatusExhausted)
		if _, err := repo.GetNextAvailable(ctx, "acc-1"); !errors.Is(err, domain.ErrNoAvailableAccount) {
			t.Fatalf("expected ErrNoAvailableAccount when all others exhausted, got %v", err)
		}
	}
}

func TestAccountRepository_CascadeDeletes(t *testing.T) {
	_, accRepo, quotaRepo, metricsRepo, _ := setupTestStore(t)
	ctx := context.Background()

	acc := &domain.Account{ID: "acc-cascade", Email: "cascade@example.com", RefreshToken: "rt"}
	if err := accRepo.Create(ctx, acc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Add quota bucket
	err := quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
		{AccountID: "acc-cascade", BucketID: "daily-quota", Window: domain.QuotaWindowDaily, ResetTime: time.Now().Add(time.Hour)},
	})
	if err != nil {
		t.Fatalf("UpsertBuckets: %v", err)
	}

	// Add token metric
	err = metricsRepo.Record(ctx, &domain.TokenMetric{
		AccountID:    "acc-cascade",
		RequestPath:  "/test",
		PromptTokens: 10,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Verify child rows exist
	buckets, _ := quotaRepo.GetByAccountID(ctx, "acc-cascade")
	if len(buckets) != 1 {
		t.Fatalf("expected 1 bucket before delete, got %d", len(buckets))
	}
	summary, _ := metricsRepo.GetSummary(ctx, "acc-cascade", "lifetime")
	if summary.TotalPromptTokens != 10 {
		t.Fatalf("expected 10 prompt tokens before delete, got %d", summary.TotalPromptTokens)
	}

	// Delete parent account
	if err := accRepo.Delete(ctx, "acc-cascade"); err != nil {
		t.Fatalf("Delete account: %v", err)
	}

	// Verify child rows were automatically deleted by SQLite ON DELETE CASCADE
	bucketsAfter, _ := quotaRepo.GetByAccountID(ctx, "acc-cascade")
	if len(bucketsAfter) != 0 {
		t.Errorf("expected 0 buckets after cascading delete, got %d", len(bucketsAfter))
	}
	summaryAfter, _ := metricsRepo.GetSummary(ctx, "acc-cascade", "lifetime")
	if summaryAfter.TotalPromptTokens != 0 {
		t.Errorf("expected 0 prompt tokens after cascading delete, got %d", summaryAfter.TotalPromptTokens)
	}
}

func TestConcurrent_MultiGoroutineOperations(t *testing.T) {
	_, accRepo, quotaRepo, metricsRepo, eventRepo := setupTestStore(t)
	ctx := context.Background()

	numAccounts := 5
	for i := 1; i <= numAccounts; i++ {
		err := accRepo.Create(ctx, &domain.Account{
			ID:           fmt.Sprintf("worker-acc-%d", i),
			Email:        fmt.Sprintf("worker%d@example.com", i),
			RefreshToken: fmt.Sprintf("rt-%d", i),
			Status:       domain.AccountStatusActive,
		})
		if err != nil {
			t.Fatalf("Create account: %v", err)
		}
	}
	_ = accRepo.SetActive(ctx, "worker-acc-1")

	var wg sync.WaitGroup
	concurrency := 30

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			accID := fmt.Sprintf("worker-acc-%d", (workerID%numAccounts)+1)

			// Record metric
			err := metricsRepo.Record(ctx, &domain.TokenMetric{
				AccountID:        accID,
				RequestPath:      "/v1internal:streamGenerateContent",
				PromptTokens:     int64(10 + workerID),
				CandidatesTokens: int64(5 + workerID),
			})
			if err != nil {
				t.Errorf("worker %d record metric failed: %v", workerID, err)
			}

			// Record event
			err = eventRepo.Record(ctx, &domain.ProxyEvent{
				Type:      domain.EventTypeRequestSuccess,
				AccountID: accID,
				Message:   fmt.Sprintf("request handled by worker %d", workerID),
			})
			if err != nil {
				t.Errorf("worker %d record event failed: %v", workerID, err)
			}

			// Upsert quota bucket
			err = quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
				{
					AccountID:         accID,
					BucketID:          "daily-chat",
					Window:            domain.QuotaWindowDaily,
					RemainingFraction: 0.95,
					RemainingAmount:   950,
					ResetTime:         time.Now().Add(24 * time.Hour),
				},
			})
			if err != nil {
				t.Errorf("worker %d upsert bucket failed: %v", workerID, err)
			}

			// Read active account
			active, err := accRepo.GetActive(ctx)
			if err != nil && !errors.Is(err, domain.ErrNoActiveAccount) {
				t.Errorf("worker %d get active failed: %v", workerID, err)
			}
			_ = active

			// Every 5th worker rotates the active account
			if workerID%5 == 0 {
				if err := accRepo.SetActive(ctx, accID); err != nil {
					t.Errorf("worker %d set active failed: %v", workerID, err)
				}
			}
		}(i)
	}

	wg.Wait()

	// Verify all metrics and events were persisted without locks or data loss
	summary, err := metricsRepo.GetSummary(ctx, "", "lifetime")
	if err != nil {
		t.Fatalf("GetSummary failed: %v", err)
	}
	if summary.TotalRequests != int64(concurrency) {
		t.Errorf("expected %d recorded requests, got %d", concurrency, summary.TotalRequests)
	}

	events, err := eventRepo.ListRecent(ctx, concurrency*2)
	if err != nil {
		t.Fatalf("ListRecent failed: %v", err)
	}
	if len(events) != concurrency {
		t.Errorf("expected %d events, got %d", concurrency, len(events))
	}
}
