package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
)

func TestCategoryIsolation_DatabaseSeedAndCategoryPointers(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cat_test.db")

	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	repo := sqlite.NewAccountRepository(db)

	// 1. Create two accounts; mark acc1 active
	acc1 := &domain.Account{
		ID:       "acc-1",
		Email:    "acc1@example.com",
		IsActive: true,
		Status:   domain.AccountStatusActive,
	}
	acc2 := &domain.Account{
		ID:       "acc-2",
		Email:    "acc2@example.com",
		IsActive: false,
		Status:   domain.AccountStatusActive,
	}

	if err := repo.Create(ctx, acc1); err != nil {
		t.Fatalf("Create acc1: %v", err)
	}
	if err := repo.Create(ctx, acc2); err != nil {
		t.Fatalf("Create acc2: %v", err)
	}

	// 2. Both categories should resolve to acc-1
	claudeAcc, err := repo.GetActiveForCategory(ctx, domain.CategoryClaudeGPT)
	if err != nil {
		t.Fatalf("GetActiveForCategory(Claude): %v", err)
	}
	if claudeAcc.ID != "acc-1" {
		t.Errorf("expected claude active acc-1, got %s", claudeAcc.ID)
	}

	geminiAcc, err := repo.GetActiveForCategory(ctx, domain.CategoryGemini)
	if err != nil {
		t.Fatalf("GetActiveForCategory(Gemini): %v", err)
	}
	if geminiAcc.ID != "acc-1" {
		t.Errorf("expected gemini active acc-1, got %s", geminiAcc.ID)
	}

	// 3. Rotate Claude category only to acc-2
	if err := repo.SetActiveForCategory(ctx, domain.CategoryClaudeGPT, "acc-2"); err != nil {
		t.Fatalf("SetActiveForCategory(Claude, acc-2): %v", err)
	}

	// 4. Verify Claude category is now acc-2, but Gemini category remains acc-1
	claudeAfter, err := repo.GetActiveForCategory(ctx, domain.CategoryClaudeGPT)
	if err != nil {
		t.Fatalf("GetActiveForCategory(Claude) after rotate: %v", err)
	}
	if claudeAfter.ID != "acc-2" {
		t.Errorf("expected claude active acc-2, got %s", claudeAfter.ID)
	}

	geminiAfter, err := repo.GetActiveForCategory(ctx, domain.CategoryGemini)
	if err != nil {
		t.Fatalf("GetActiveForCategory(Gemini) after rotate: %v", err)
	}
	if geminiAfter.ID != "acc-1" {
		t.Errorf("expected gemini active acc-1, got %s", geminiAfter.ID)
	}

	// 5. Verify backward compatibility: legacy GetActive returns a valid active account
	legacyActive, err := repo.GetActive(ctx)
	if err != nil {
		t.Fatalf("GetActive legacy: %v", err)
	}
	if !legacyActive.IsActive {
		t.Errorf("expected legacyActive.IsActive to be true")
	}
}

func TestCategoryIsolation_GetNextAvailableForCategory_WithExhaustedBuckets(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cat_quota_test.db")

	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	accRepo := sqlite.NewAccountRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)

	now := time.Now().UTC()

	// Create 3 accounts:
	// accA: exhausted on Claude only
	// accB: fresh (available for both)
	// accC: exhausted on Gemini only
	accA := &domain.Account{ID: "acc-A", Email: "a@example.com", Status: domain.AccountStatusActive, UpdatedAt: now.Add(-30 * time.Minute)}
	accB := &domain.Account{ID: "acc-B", Email: "b@example.com", Status: domain.AccountStatusActive, UpdatedAt: now.Add(-20 * time.Minute)}
	accC := &domain.Account{ID: "acc-C", Email: "c@example.com", Status: domain.AccountStatusActive, UpdatedAt: now.Add(-10 * time.Minute)}

	if err := accRepo.Create(ctx, accA); err != nil {
		t.Fatalf("Create accA: %v", err)
	}
	if err := accRepo.Create(ctx, accB); err != nil {
		t.Fatalf("Create accB: %v", err)
	}
	if err := accRepo.Create(ctx, accC); err != nil {
		t.Fatalf("Create accC: %v", err)
	}

	// Bucket: accA exhausted on Claude
	if err := quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
		{
			AccountID:         "acc-A",
			BucketID:          "claude-3-5-sonnet",
			DisplayName:       "Claude 3.5 Sonnet",
			Window:            domain.QuotaWindowDaily,
			RemainingFraction: 0.0,
			ResetTime:         now.Add(2 * time.Hour),
		},
	}); err != nil {
		t.Fatalf("UpsertBuckets accA: %v", err)
	}

	// Bucket: accC exhausted on Gemini
	if err := quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
		{
			AccountID:         "acc-C",
			BucketID:          "gemini-2.5-pro",
			DisplayName:       "Gemini 2.5 Pro",
			Window:            domain.QuotaWindowDaily,
			RemainingFraction: 0.0,
			ResetTime:         now.Add(2 * time.Hour),
		},
	}); err != nil {
		t.Fatalf("UpsertBuckets accC: %v", err)
	}

	// For Claude: accA is exhausted, next should be accB
	nextClaude, err := accRepo.GetNextAvailableForCategory(ctx, domain.CategoryClaudeGPT, "")
	if err != nil {
		t.Fatalf("GetNextAvailableForCategory(Claude): %v", err)
	}
	if nextClaude.ID != "acc-B" {
		t.Errorf("expected acc-B for Claude, got %s", nextClaude.ID)
	}

	// For Gemini: accA has NO gemini bucket exhausted, so least recently used is accA!
	nextGemini, err := accRepo.GetNextAvailableForCategory(ctx, domain.CategoryGemini, "")
	if err != nil {
		t.Fatalf("GetNextAvailableForCategory(Gemini): %v", err)
	}
	if nextGemini.ID != "acc-A" {
		t.Errorf("expected acc-A for Gemini (since accA has Gemini quota intact), got %s", nextGemini.ID)
	}

	// For Gemini excluding acc-A: accC is exhausted for Gemini, so next is acc-B!
	nextGeminiNoA, err := accRepo.GetNextAvailableForCategory(ctx, domain.CategoryGemini, "acc-A")
	if err != nil {
		t.Fatalf("GetNextAvailableForCategory(Gemini excluding acc-A): %v", err)
	}
	if nextGeminiNoA.ID != "acc-B" {
		t.Errorf("expected acc-B for Gemini (since accC is exhausted for Gemini), got %s", nextGeminiNoA.ID)
	}
}
