package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

func TestQuotaRepository_UpsertAndQuery(t *testing.T) {
	_, accRepo, quotaRepo, _, _ := setupTestStore(t)
	ctx := context.Background()

	// Create test accounts
	_ = accRepo.Create(ctx, &domain.Account{ID: "acc-1", Email: "u1@example.com", RefreshToken: "rt1"})
	_ = accRepo.Create(ctx, &domain.Account{ID: "acc-2", Email: "u2@example.com", RefreshToken: "rt2"})

	resetTime := time.Now().Add(12 * time.Hour).Truncate(time.Second)

	// 1. Initial Upsert of multiple buckets for acc-1
	initialBuckets := []*domain.QuotaBucket{
		{
			AccountID:         "acc-1",
			BucketID:          "daily-requests",
			DisplayName:       "Daily Request Quota",
			Window:            domain.QuotaWindowDaily,
			RemainingFraction: 0.85,
			RemainingAmount:   850,
			ResetTime:         resetTime,
		},
		{
			AccountID:         "acc-1",
			BucketID:          "weekly-requests",
			DisplayName:       "Weekly Request Quota",
			Window:            domain.QuotaWindowWeekly,
			RemainingFraction: 0.95,
			RemainingAmount:   9500,
			ResetTime:         resetTime.Add(6 * 24 * time.Hour),
		},
	}

	if err := quotaRepo.UpsertBuckets(ctx, initialBuckets); err != nil {
		t.Fatalf("UpsertBuckets failed: %v", err)
	}

	// 2. Query by account ID
	buckets, err := quotaRepo.GetByAccountID(ctx, "acc-1")
	if err != nil {
		t.Fatalf("GetByAccountID failed: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(buckets))
	}
	if buckets[0].BucketID != "daily-requests" || buckets[1].BucketID != "weekly-requests" {
		t.Errorf("unexpected bucket order: %+v", buckets)
	}
	if buckets[0].RemainingFraction != 0.85 {
		t.Errorf("expected 0.85 remaining fraction, got %f", buckets[0].RemainingFraction)
	}

	// 3. Upsert update existing bucket in-place
	updatedBuckets := []*domain.QuotaBucket{
		{
			AccountID:         "acc-1",
			BucketID:          "daily-requests",
			DisplayName:       "Daily Request Quota",
			Window:            domain.QuotaWindowDaily,
			RemainingFraction: 0.10,
			RemainingAmount:   100,
			ResetTime:         resetTime,
		},
	}
	if err := quotaRepo.UpsertBuckets(ctx, updatedBuckets); err != nil {
		t.Fatalf("UpsertBuckets update failed: %v", err)
	}

	bucketsAfterUpdate, _ := quotaRepo.GetByAccountID(ctx, "acc-1")
	if len(bucketsAfterUpdate) != 2 {
		t.Fatalf("expected count still 2 after upsert update, got %d", len(bucketsAfterUpdate))
	}
	if bucketsAfterUpdate[0].RemainingFraction != 0.10 || bucketsAfterUpdate[0].RemainingAmount != 100 {
		t.Errorf("expected updated fraction 0.10 and amount 100, got fraction %f, amount %d",
			bucketsAfterUpdate[0].RemainingFraction, bucketsAfterUpdate[0].RemainingAmount)
	}

	// 4. Add bucket for acc-2 and test ListAll
	_ = quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
		{
			AccountID:         "acc-2",
			BucketID:          "daily-requests",
			DisplayName:       "Daily Acc 2",
			Window:            domain.QuotaWindowDaily,
			RemainingFraction: 1.0,
			RemainingAmount:   1000,
			ResetTime:         resetTime,
		},
	})

	allMap, err := quotaRepo.ListAll(ctx)
	if err != nil {
		t.Fatalf("ListAll failed: %v", err)
	}
	if len(allMap) != 2 {
		t.Fatalf("expected 2 accounts in ListAll map, got %d", len(allMap))
	}
	if len(allMap["acc-1"]) != 2 || len(allMap["acc-2"]) != 1 {
		t.Errorf("unexpected counts in map: acc-1=%d, acc-2=%d", len(allMap["acc-1"]), len(allMap["acc-2"]))
	}

	// 5. DeleteByAccountID
	if err := quotaRepo.DeleteByAccountID(ctx, "acc-1"); err != nil {
		t.Fatalf("DeleteByAccountID failed: %v", err)
	}
	bucketsDeleted, _ := quotaRepo.GetByAccountID(ctx, "acc-1")
	if len(bucketsDeleted) != 0 {
		t.Errorf("expected 0 buckets for acc-1 after delete, got %d", len(bucketsDeleted))
	}
	bucketsAcc2, _ := quotaRepo.GetByAccountID(ctx, "acc-2")
	if len(bucketsAcc2) != 1 {
		t.Errorf("expected acc-2 buckets intact after deleting acc-1, got %d", len(bucketsAcc2))
	}
}

func TestQuotaRepository_GetExhaustedAccountsPastReset(t *testing.T) {
	_, accRepo, quotaRepo, _, _ := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Account 1: Exhausted, reset time passed 1 hour ago -> SHOULD BE RETURNED
	_ = accRepo.Create(ctx, &domain.Account{ID: "acc-reset-1", Email: "r1@example.com", RefreshToken: "rt", Status: domain.AccountStatusExhausted})
	_ = quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
		{
			AccountID:         "acc-reset-1",
			BucketID:          "daily",
			Window:            domain.QuotaWindowDaily,
			RemainingFraction: 0.0,
			RemainingAmount:   0,
			ResetTime:         now.Add(-1 * time.Hour),
		},
	})

	// Account 2: Exhausted, reset time in future 1 hour -> SHOULD NOT BE RETURNED
	_ = accRepo.Create(ctx, &domain.Account{ID: "acc-not-reset-2", Email: "r2@example.com", RefreshToken: "rt", Status: domain.AccountStatusExhausted})
	_ = quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
		{
			AccountID:         "acc-not-reset-2",
			BucketID:          "daily",
			Window:            domain.QuotaWindowDaily,
			RemainingFraction: 0.0,
			RemainingAmount:   0,
			ResetTime:         now.Add(1 * time.Hour),
		},
	})

	// Account 3: Active (not exhausted), reset time passed -> SHOULD NOT BE RETURNED
	_ = accRepo.Create(ctx, &domain.Account{ID: "acc-active-3", Email: "r3@example.com", RefreshToken: "rt", Status: domain.AccountStatusActive})
	_ = quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
		{
			AccountID:         "acc-active-3",
			BucketID:          "daily",
			Window:            domain.QuotaWindowDaily,
			RemainingFraction: 0.5,
			RemainingAmount:   500,
			ResetTime:         now.Add(-2 * time.Hour),
		},
	})

	// Account 4: Exhausted, 2 buckets: 1 passed, 1 in future -> SHOULD NOT BE RETURNED
	_ = accRepo.Create(ctx, &domain.Account{ID: "acc-mixed-4", Email: "r4@example.com", RefreshToken: "rt", Status: domain.AccountStatusExhausted})
	_ = quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
		{
			AccountID:         "acc-mixed-4",
			BucketID:          "daily",
			Window:            domain.QuotaWindowDaily,
			RemainingFraction: 0.0,
			RemainingAmount:   0,
			ResetTime:         now.Add(-2 * time.Hour),
		},
		{
			AccountID:         "acc-mixed-4",
			BucketID:          "weekly",
			Window:            domain.QuotaWindowWeekly,
			RemainingFraction: 0.0,
			RemainingAmount:   0,
			ResetTime:         now.Add(2 * time.Hour),
		},
	})

	resetAccounts, err := quotaRepo.GetExhaustedAccountsPastReset(ctx, now)
	if err != nil {
		t.Fatalf("GetExhaustedAccountsPastReset failed: %v", err)
	}

	if len(resetAccounts) != 1 {
		t.Fatalf("expected exactly 1 reset account, got %d (%v)", len(resetAccounts), resetAccounts)
	}
	if resetAccounts[0] != "acc-reset-1" {
		t.Errorf("expected acc-reset-1, got %s", resetAccounts[0])
	}
}
