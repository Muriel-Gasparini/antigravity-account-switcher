package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

func weeklyBucket(accountID, bucketID, displayName string, frac float64, reset time.Time) *domain.QuotaBucket {
	return &domain.QuotaBucket{
		AccountID:         accountID,
		BucketID:          bucketID,
		DisplayName:       displayName,
		Window:            domain.QuotaWindowWeekly,
		RemainingFraction: frac,
		ResetTime:         reset,
	}
}

func TestGetNextAvailableForCategory_SelectsExhaustedStatusAccountWithHealthyCategoryBucket(t *testing.T) {
	_, accRepo, quotaRepo, _, _ := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	accs := []*domain.Account{
		{ID: "acc-dead-3p", Email: "dead3p@example.com", RefreshToken: "rt", Status: domain.AccountStatusExhausted},
		{ID: "acc-dead-gemini", Email: "deadgem@example.com", RefreshToken: "rt", Status: domain.AccountStatusActive},
		{ID: "acc-exh-no-evidence", Email: "noquota@example.com", RefreshToken: "rt", Status: domain.AccountStatusExhausted},
		{ID: "acc-disabled", Email: "disabled@example.com", RefreshToken: "rt", Status: domain.AccountStatusDisabled},
		{ID: "acc-error", Email: "error@example.com", RefreshToken: "rt", Status: domain.AccountStatusError},
	}
	for _, a := range accs {
		if err := accRepo.Create(ctx, a); err != nil {
			t.Fatalf("Create %s: %v", a.ID, err)
		}
	}
	if err := quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
		weeklyBucket("acc-dead-3p", "3p-weekly", "Claude and GPT models (weekly)", 0.0, now.Add(48*time.Hour)),
		weeklyBucket("acc-dead-3p", "gemini-weekly", "Gemini Models (weekly)", 1.0, now.Add(96*time.Hour)),
		weeklyBucket("acc-dead-gemini", "gemini-weekly", "Gemini Models (weekly)", 0.0, now.Add(48*time.Hour)),
	}); err != nil {
		t.Fatalf("UpsertBuckets: %v", err)
	}

	// Gemini: only acc-dead-3p is eligible. Its 'exhausted' status came from a
	// Claude/GPT 429 and must not lock it out of Gemini, for which quota rows
	// positively show a healthy bucket. acc-exh-no-evidence has no quota rows to
	// prove recovery, and 'disabled'/'error' accounts are never eligible.
	next, err := accRepo.GetNextAvailableForCategory(ctx, domain.CategoryGemini, "")
	if err != nil {
		t.Fatalf("GetNextAvailableForCategory(Gemini): %v", err)
	}
	if next.ID != "acc-dead-3p" {
		t.Errorf("expected acc-dead-3p for Gemini, got %s", next.ID)
	}

	// Claude/GPT: acc-dead-3p's category bucket is dead; acc-dead-gemini is eligible.
	nextClaude, err := accRepo.GetNextAvailableForCategory(ctx, domain.CategoryClaudeGPT, "")
	if err != nil {
		t.Fatalf("GetNextAvailableForCategory(ClaudeGPT): %v", err)
	}
	if nextClaude.ID != "acc-dead-gemini" {
		t.Errorf("expected acc-dead-gemini for ClaudeGPT, got %s", nextClaude.ID)
	}
}

func TestGetActiveForCategory_AdvancesOffPinWithExhaustedCategoryBucket(t *testing.T) {
	_, accRepo, quotaRepo, _, _ := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, a := range []*domain.Account{
		{ID: "acc-pinned", Email: "pinned@example.com", RefreshToken: "rt", Status: domain.AccountStatusExhausted},
		{ID: "acc-healthy", Email: "healthy@example.com", RefreshToken: "rt", Status: domain.AccountStatusActive},
	} {
		if err := accRepo.Create(ctx, a); err != nil {
			t.Fatalf("Create %s: %v", a.ID, err)
		}
	}
	if err := quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
		weeklyBucket("acc-pinned", "gemini-weekly", "Gemini Models (weekly)", 0.0, now.Add(48*time.Hour)),
		weeklyBucket("acc-healthy", "gemini-weekly", "Gemini Models (weekly)", 0.9, now.Add(48*time.Hour)),
	}); err != nil {
		t.Fatalf("UpsertBuckets: %v", err)
	}
	if err := accRepo.SetActiveForCategory(ctx, domain.CategoryGemini, "acc-pinned"); err != nil {
		t.Fatalf("SetActiveForCategory: %v", err)
	}

	got, err := accRepo.GetActiveForCategory(ctx, domain.CategoryGemini)
	if err != nil {
		t.Fatalf("GetActiveForCategory: %v", err)
	}
	if got.ID != "acc-healthy" {
		t.Errorf("expected pin to advance to acc-healthy, got %s", got.ID)
	}

	// The pin must stay advanced on repeat calls (no thrash).
	got, err = accRepo.GetActiveForCategory(ctx, domain.CategoryGemini)
	if err != nil {
		t.Fatalf("GetActiveForCategory repeat: %v", err)
	}
	if got.ID != "acc-healthy" {
		t.Errorf("expected pin to remain acc-healthy, got %s", got.ID)
	}
}

func TestGetActiveForCategory_KeepsPinWhenOtherCategoryBucketExhausted(t *testing.T) {
	_, accRepo, quotaRepo, _, _ := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, a := range []*domain.Account{
		{ID: "acc-pinned", Email: "pinned@example.com", RefreshToken: "rt", Status: domain.AccountStatusExhausted},
		{ID: "acc-other", Email: "other@example.com", RefreshToken: "rt", Status: domain.AccountStatusActive},
	} {
		if err := accRepo.Create(ctx, a); err != nil {
			t.Fatalf("Create %s: %v", a.ID, err)
		}
	}
	if err := quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
		weeklyBucket("acc-pinned", "3p-weekly", "Claude and GPT models (weekly)", 0.0, now.Add(48*time.Hour)),
		weeklyBucket("acc-other", "gemini-weekly", "Gemini Models (weekly)", 0.9, now.Add(48*time.Hour)),
	}); err != nil {
		t.Fatalf("UpsertBuckets: %v", err)
	}
	if err := accRepo.SetActiveForCategory(ctx, domain.CategoryGemini, "acc-pinned"); err != nil {
		t.Fatalf("SetActiveForCategory: %v", err)
	}

	// acc-pinned is dead for Claude/GPT only; its Gemini bucket is intact,
	// so the Gemini pin must not move.
	got, err := accRepo.GetActiveForCategory(ctx, domain.CategoryGemini)
	if err != nil {
		t.Fatalf("GetActiveForCategory: %v", err)
	}
	if got.ID != "acc-pinned" {
		t.Errorf("expected pin to remain acc-pinned, got %s", got.ID)
	}
}

func TestGetActiveForCategory_KeepsDeadPinWhenNoEligibleCandidate(t *testing.T) {
	_, accRepo, quotaRepo, _, _ := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	pinned := &domain.Account{ID: "acc-pinned", Email: "pinned@example.com", RefreshToken: "rt", Status: domain.AccountStatusExhausted}
	if err := accRepo.Create(ctx, pinned); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
		weeklyBucket("acc-pinned", "gemini-weekly", "Gemini Models (weekly)", 0.0, now.Add(48*time.Hour)),
	}); err != nil {
		t.Fatalf("UpsertBuckets: %v", err)
	}
	if err := accRepo.SetActiveForCategory(ctx, domain.CategoryGemini, "acc-pinned"); err != nil {
		t.Fatalf("SetActiveForCategory: %v", err)
	}

	got, err := accRepo.GetActiveForCategory(ctx, domain.CategoryGemini)
	if err != nil {
		t.Fatalf("GetActiveForCategory: %v", err)
	}
	if got.ID != "acc-pinned" {
		t.Errorf("expected dead pin to be returned when no candidate exists, got %s", got.ID)
	}
}

func TestGetActiveForCategory_KeepsPinWhenExhaustedBucketPastReset(t *testing.T) {
	_, accRepo, quotaRepo, _, _ := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	pinned := &domain.Account{ID: "acc-pinned", Email: "pinned@example.com", RefreshToken: "rt", Status: domain.AccountStatusExhausted}
	if err := accRepo.Create(ctx, pinned); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Bucket is empty but its reset window has already passed: treat as restored.
	if err := quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
		weeklyBucket("acc-pinned", "gemini-weekly", "Gemini Models (weekly)", 0.0, now.Add(-1*time.Hour)),
	}); err != nil {
		t.Fatalf("UpsertBuckets: %v", err)
	}
	if err := accRepo.SetActiveForCategory(ctx, domain.CategoryGemini, "acc-pinned"); err != nil {
		t.Fatalf("SetActiveForCategory: %v", err)
	}

	got, err := accRepo.GetActiveForCategory(ctx, domain.CategoryGemini)
	if err != nil {
		t.Fatalf("GetActiveForCategory: %v", err)
	}
	if got.ID != "acc-pinned" {
		t.Errorf("expected pin to remain acc-pinned (bucket past reset), got %s", got.ID)
	}
}
