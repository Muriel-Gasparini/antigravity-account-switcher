package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
)

func assertActiveCount(t *testing.T, db *sqlite.DB, expected int) {
	t.Helper()
	var count int
	row := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM accounts WHERE is_active = 1")
	if err := row.Scan(&count); err != nil {
		t.Fatalf("failed to query active account count: %v", err)
	}
	if count != expected {
		t.Fatalf("expected %d active accounts, found %d", expected, count)
	}
}

// TestChallenge_ConcurrentSetActive_SingleActiveInvariant stress-tests the single-active
// constraint under high concurrency with mixed readers and writers.
func TestChallenge_ConcurrentSetActive_SingleActiveInvariant(t *testing.T) {
	db, accRepo, _, _, _ := setupTestStore(t)
	ctx := context.Background()

	const numAccounts = 10
	for i := 0; i < numAccounts; i++ {
		acc := &domain.Account{
			ID:           fmt.Sprintf("inv-acc-%d", i),
			Email:        fmt.Sprintf("inv-user-%d@example.com", i),
			RefreshToken: fmt.Sprintf("rt-%d", i),
			Status:       domain.AccountStatusActive,
		}
		if err := accRepo.Create(ctx, acc); err != nil {
			t.Fatalf("Create account %d failed: %v", i, err)
		}
	}

	// Initially set acc-0 active
	if err := accRepo.SetActive(ctx, "inv-acc-0"); err != nil {
		t.Fatalf("Initial SetActive failed: %v", err)
	}

	// Verify exactly 1 active account initially
	assertActiveCount(t, db, 1)

	var (
		writersWg      sync.WaitGroup
		readersWg      sync.WaitGroup
		stopCh         = make(chan struct{})
		activeCountsOK int64
		readerErrors   int64
		writerErrors   int64
	)

	const numWriters = 20
	const opsPerWriter = 25
	const numReaders = 10

	// Launch continuous readers
	for r := 0; r < numReaders; r++ {
		readersWg.Add(1)
		go func(readerID int) {
			defer readersWg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					// Check via domain repository
					active, err := accRepo.GetActive(ctx)
					if err != nil {
						atomic.AddInt64(&readerErrors, 1)
						t.Errorf("reader %d: GetActive failed: %v", readerID, err)
						return
					}
					if !active.IsActive {
						atomic.AddInt64(&readerErrors, 1)
						t.Errorf("reader %d: GetActive returned inactive account %+v", readerID, active)
						return
					}

					// Verify raw DB invariant: exactly 1 account with is_active = 1
					var rawCount int
					row := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE is_active = 1")
					if err := row.Scan(&rawCount); err != nil {
						atomic.AddInt64(&readerErrors, 1)
						t.Errorf("reader %d: count query failed: %v", readerID, err)
						return
					}
					if rawCount != 1 {
						atomic.AddInt64(&readerErrors, 1)
						t.Errorf("VIOLATION: reader %d observed %d active accounts (expected 1)", readerID, rawCount)
						return
					}
					atomic.AddInt64(&activeCountsOK, 1)
					time.Sleep(1 * time.Millisecond)
				}
			}
		}(r)
	}

	// Launch writers doing concurrent SetActive (including some invalid IDs)
	for w := 0; w < numWriters; w++ {
		writersWg.Add(1)
		go func(writerID int) {
			defer writersWg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(writerID)))

			for op := 0; op < opsPerWriter; op++ {
				var targetID string
				if op%7 == 0 {
					// Intentionally test non-existent account
					targetID = "non-existent-acc-id"
				} else {
					targetID = fmt.Sprintf("inv-acc-%d", rng.Intn(numAccounts))
				}

				err := accRepo.SetActive(ctx, targetID)
				if targetID == "non-existent-acc-id" {
					if !errors.Is(err, domain.ErrAccountNotFound) {
						atomic.AddInt64(&writerErrors, 1)
						t.Errorf("writer %d: expected ErrAccountNotFound for non-existent ID, got %v", writerID, err)
					}
				} else {
					if err != nil {
						atomic.AddInt64(&writerErrors, 1)
						t.Errorf("writer %d: SetActive(%s) failed: %v", writerID, targetID, err)
					}
				}

				time.Sleep(time.Duration(rng.Intn(3)) * time.Millisecond)
			}
		}(w)
	}

	writersWg.Wait()
	close(stopCh)
	readersWg.Wait()

	if readerErrors > 0 || writerErrors > 0 {
		t.Fatalf("Encountered %d reader errors and %d writer errors during concurrent SetActive", readerErrors, writerErrors)
	}

	// Final check: invariant must hold
	assertActiveCount(t, db, 1)

	active, err := accRepo.GetActive(ctx)
	if err != nil {
		t.Fatalf("final GetActive failed: %v", err)
	}
	if !active.IsActive {
		t.Fatalf("final active account %s has IsActive=false", active.ID)
	}

	t.Logf("PASS: Invariant held across %d successful reader checks and %d writer operations", activeCountsOK, numWriters*opsPerWriter)
}

// TestChallenge_PartialUniqueIndex_DirectBypassConstraint tests that the database engine
// rejects any attempt to violate the single-active constraint through raw SQL operations.
func TestChallenge_PartialUniqueIndex_DirectBypassConstraint(t *testing.T) {
	db, accRepo, _, _, _ := setupTestStore(t)
	ctx := context.Background()

	acc1 := &domain.Account{ID: "acc-c1", Email: "c1@example.com", RefreshToken: "rt1"}
	acc2 := &domain.Account{ID: "acc-c2", Email: "c2@example.com", RefreshToken: "rt2"}
	_ = accRepo.Create(ctx, acc1)
	_ = accRepo.Create(ctx, acc2)

	// Set acc-c1 active
	if err := accRepo.SetActive(ctx, "acc-c1"); err != nil {
		t.Fatalf("SetActive acc-c1: %v", err)
	}
	assertActiveCount(t, db, 1)

	// Attempt 1: Direct INSERT of another active account
	_, err := db.ExecContext(ctx, `
		INSERT INTO accounts (id, email, refresh_token, is_active, status, created_at, updated_at)
		VALUES ('acc-c3', 'c3@example.com', 'rt3', 1, 'active', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`)
	if err == nil {
		t.Fatal("expected UNIQUE constraint violation when inserting second active account via raw SQL, got nil")
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("expected UNIQUE constraint error, got %v", err)
	}
	assertActiveCount(t, db, 1)

	// Attempt 2: Direct UPDATE of second account to is_active = 1
	_, err = db.ExecContext(ctx, "UPDATE accounts SET is_active = 1 WHERE id = 'acc-c2'")
	if err == nil {
		t.Fatal("expected UNIQUE constraint violation when updating second account to is_active = 1, got nil")
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("expected UNIQUE constraint error, got %v", err)
	}
	assertActiveCount(t, db, 1)

	// Attempt 3: SetActive with non-existent account must rollback and keep acc-c1 active
	err = accRepo.SetActive(ctx, "non-existent-acc")
	if !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("expected ErrAccountNotFound, got %v", err)
	}
	assertActiveCount(t, db, 1)

	active, err := accRepo.GetActive(ctx)
	if err != nil {
		t.Fatalf("GetActive after failed SetActive: %v", err)
	}
	if active.ID != "acc-c1" {
		t.Fatalf("expected acc-c1 to remain active after failed SetActive, got %s", active.ID)
	}

	// Attempt 4: SetActive on already active account should succeed idempotently
	if err := accRepo.SetActive(ctx, "acc-c1"); err != nil {
		t.Fatalf("SetActive on already active account failed: %v", err)
	}
	assertActiveCount(t, db, 1)
	active, _ = accRepo.GetActive(ctx)
	if active.ID != "acc-c1" {
		t.Fatalf("expected acc-c1 to still be active, got %s", active.ID)
	}
}

// TestChallenge_GetNextAvailable_RoundRobinLRU_AndExclusions thoroughly checks:
// - Round-robin LRU cycling across multiple active accounts
// - Exclusion of the current active account
// - Exclusion of exhausted accounts
// - Exclusion of accounts with status error or disabled
// - Handling when all alternative accounts are exhausted
// - Dynamic reintegration of accounts once their status resets to active
func TestChallenge_GetNextAvailable_RoundRobinLRU_AndExclusions(t *testing.T) {
	_, accRepo, _, _, _ := setupTestStore(t)
	ctx := context.Background()

	// 1. Empty database
	if _, err := accRepo.GetNextAvailable(ctx, ""); !errors.Is(err, domain.ErrNoAvailableAccount) {
		t.Fatalf("expected ErrNoAvailableAccount on empty DB, got %v", err)
	}
	if _, err := accRepo.GetNextAvailable(ctx, "some-id"); !errors.Is(err, domain.ErrNoAvailableAccount) {
		t.Fatalf("expected ErrNoAvailableAccount on empty DB with excludeID, got %v", err)
	}

	// 2. Setup 5 accounts with distinct updated_at timestamps
	baseTime := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	accountIDs := []string{"acc-A", "acc-B", "acc-C", "acc-D", "acc-E"}
	for i, id := range accountIDs {
		acc := &domain.Account{
			ID:           id,
			Email:        fmt.Sprintf("%s@example.com", strings.ToLower(id)),
			RefreshToken: fmt.Sprintf("rt-%s", id),
			Status:       domain.AccountStatusActive,
			UpdatedAt:    baseTime.Add(time.Duration(i) * time.Minute), // A oldest, E newest
		}
		if err := accRepo.Create(ctx, acc); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}

	// Set A as active
	if err := accRepo.SetActive(ctx, "acc-A"); err != nil {
		t.Fatalf("SetActive acc-A: %v", err)
	}

	// 3. Sequential failover round-robin:
	// A is active. GetNextAvailable(exclude A) should return B (the oldest among B,C,D,E).
	next, err := accRepo.GetNextAvailable(ctx, "acc-A")
	if err != nil {
		t.Fatalf("GetNextAvailable(exclude A): %v", err)
	}
	if next.ID != "acc-B" {
		t.Fatalf("expected acc-B, got %s", next.ID)
	}

	// Simulate rotation: SetActive(acc-B).
	// This bumps B's updated_at to now.
	time.Sleep(10 * time.Millisecond)
	if err := accRepo.SetActive(ctx, "acc-B"); err != nil {
		t.Fatalf("SetActive acc-B: %v", err)
	}

	// Now B is active. Next available excluding B should be C.
	next, err = accRepo.GetNextAvailable(ctx, "acc-B")
	if err != nil {
		t.Fatalf("GetNextAvailable(exclude B): %v", err)
	}
	if next.ID != "acc-C" {
		t.Fatalf("expected acc-C, got %s", next.ID)
	}

	// Rotate to C
	time.Sleep(10 * time.Millisecond)
	_ = accRepo.SetActive(ctx, "acc-C")

	// Next available excluding C should be D
	next, err = accRepo.GetNextAvailable(ctx, "acc-C")
	if err != nil {
		t.Fatalf("GetNextAvailable(exclude C): %v", err)
	}
	if next.ID != "acc-D" {
		t.Fatalf("expected acc-D, got %s", next.ID)
	}

	// Rotate to D
	time.Sleep(10 * time.Millisecond)
	_ = accRepo.SetActive(ctx, "acc-D")

	// Next available excluding D should be E
	next, err = accRepo.GetNextAvailable(ctx, "acc-D")
	if err != nil {
		t.Fatalf("GetNextAvailable(exclude D): %v", err)
	}
	if next.ID != "acc-E" {
		t.Fatalf("expected acc-E, got %s", next.ID)
	}

	// Rotate to E
	time.Sleep(10 * time.Millisecond)
	_ = accRepo.SetActive(ctx, "acc-E")

	// Next available excluding E should cycle back to A (since A was updated longest ago)!
	next, err = accRepo.GetNextAvailable(ctx, "acc-E")
	if err != nil {
		t.Fatalf("GetNextAvailable(exclude E): %v", err)
	}
	if next.ID != "acc-A" {
		t.Fatalf("expected round-robin cycle back to acc-A, got %s", next.ID)
	}

	// Rotate back to A
	time.Sleep(10 * time.Millisecond)
	_ = accRepo.SetActive(ctx, "acc-A")

	// Next available excluding A should now be B!
	next, err = accRepo.GetNextAvailable(ctx, "acc-A")
	if err != nil {
		t.Fatalf("GetNextAvailable(exclude A): %v", err)
	}
	if next.ID != "acc-B" {
		t.Fatalf("expected round-robin second cycle to acc-B, got %s", next.ID)
	}

	// 4. Test Exclusion of non-active statuses:
	// A is active. Mark B as exhausted.
	if err := accRepo.UpdateStatus(ctx, "acc-B", domain.AccountStatusExhausted); err != nil {
		t.Fatalf("UpdateStatus B exhausted: %v", err)
	}

	// Next available excluding A must SKIP B and return C!
	next, err = accRepo.GetNextAvailable(ctx, "acc-A")
	if err != nil {
		t.Fatalf("GetNextAvailable: %v", err)
	}
	if next.ID != "acc-C" {
		t.Fatalf("expected acc-C (skipping exhausted B), got %s", next.ID)
	}

	// Mark C as error, mark D as disabled
	_ = accRepo.UpdateStatus(ctx, "acc-C", domain.AccountStatusError)
	_ = accRepo.UpdateStatus(ctx, "acc-D", domain.AccountStatusDisabled)

	// Next available excluding A must SKIP B, C, D and return E!
	next, err = accRepo.GetNextAvailable(ctx, "acc-A")
	if err != nil {
		t.Fatalf("GetNextAvailable: %v", err)
	}
	if next.ID != "acc-E" {
		t.Fatalf("expected acc-E (skipping B, C, D), got %s", next.ID)
	}

	// Mark E as exhausted too
	_ = accRepo.UpdateStatus(ctx, "acc-E", domain.AccountStatusExhausted)

	// Now all non-A accounts are exhausted/error/disabled.
	// GetNextAvailable excluding A must return ErrNoAvailableAccount!
	_, err = accRepo.GetNextAvailable(ctx, "acc-A")
	if !errors.Is(err, domain.ErrNoAvailableAccount) {
		t.Fatalf("expected ErrNoAvailableAccount when all others exhausted, got %v", err)
	}

	// 5. Test recovery of exhausted account:
	// B's quota resets -> status restored to active
	if err := accRepo.UpdateStatus(ctx, "acc-B", domain.AccountStatusActive); err != nil {
		t.Fatalf("UpdateStatus B active: %v", err)
	}

	// Now next available excluding A must immediately return recovered account B!
	next, err = accRepo.GetNextAvailable(ctx, "acc-A")
	if err != nil {
		t.Fatalf("GetNextAvailable after recovery: %v", err)
	}
	if next.ID != "acc-B" {
		t.Fatalf("expected recovered acc-B, got %s", next.ID)
	}

	// 6. Test Single Account Pool:
	// Delete B, C, D, E, leaving only A
	for _, id := range []string{"acc-B", "acc-C", "acc-D", "acc-E"} {
		_ = accRepo.Delete(ctx, id)
	}
	// Calling GetNextAvailable excluding A must return ErrNoAvailableAccount
	_, err = accRepo.GetNextAvailable(ctx, "acc-A")
	if !errors.Is(err, domain.ErrNoAvailableAccount) {
		t.Fatalf("expected ErrNoAvailableAccount on single-account pool with exclusion, got %v", err)
	}
}

// TestChallenge_MetricsAggregations_EmptyTables tests boundary behavior on an empty database:
// zero counts, no nil pointers, no scan errors.
func TestChallenge_MetricsAggregations_EmptyTables(t *testing.T) {
	_, _, _, metricsRepo, _ := setupTestStore(t)
	ctx := context.Background()

	periods := []string{
		"day", "today", "daily",
		"week", "weekly",
		"month", "monthly",
		"lifetime", "total", "all", "",
		"  DAY  ", "  Lifetime  ", // case and whitespace insensitivity
	}

	for _, p := range periods {
		// Global summary on empty DB
		summary, err := metricsRepo.GetSummary(ctx, "", p)
		if err != nil {
			t.Errorf("GetSummary('', %q) on empty DB failed: %v", p, err)
			continue
		}
		if summary == nil {
			t.Errorf("GetSummary('', %q) returned nil summary", p)
			continue
		}
		if summary.TotalPromptTokens != 0 ||
			summary.TotalCandidatesTokens != 0 ||
			summary.TotalTokens != 0 ||
			summary.TotalCachedContentTokens != 0 ||
			summary.TotalThoughtsTokens != 0 ||
			summary.TotalRequests != 0 {
			t.Errorf("expected all zeros for period %q, got %+v", p, summary)
		}

		// Account-specific summary on empty DB
		accSummary, err := metricsRepo.GetSummary(ctx, "non-existent-acc", p)
		if err != nil {
			t.Errorf("GetSummary('non-existent', %q) on empty DB failed: %v", p, err)
			continue
		}
		if accSummary.TotalRequests != 0 || accSummary.TotalTokens != 0 {
			t.Errorf("expected 0 for non-existent acc in period %q, got %+v", p, accSummary)
		}
	}

	// Invalid period names must return an error
	invalidPeriods := []string{"year", "hourly", "decade", "yesterday", "unknown"}
	for _, ip := range invalidPeriods {
		if _, err := metricsRepo.GetSummary(ctx, "", ip); err == nil {
			t.Errorf("expected error for invalid period %q, got nil", ip)
		}
	}

	// GetDailyHistory on empty DB
	daysToTest := []int{0, 1, 7, 14, 30, 365, -5}
	for _, d := range daysToTest {
		history, err := metricsRepo.GetDailyHistory(ctx, "", d)
		if err != nil {
			t.Errorf("GetDailyHistory('', %d) on empty DB failed: %v", d, err)
		}
		if len(history) != 0 {
			t.Errorf("expected 0 history items on empty DB for days=%d, got %d", d, len(history))
		}
	}
}

// TestChallenge_MetricsAggregations_LeapYearAndEdgeDates tests aggregation behavior across:
// - Leap year day (2024-02-29)
// - Year boundary (2025-12-31 to 2026-01-01)
// - Timestamp grouping by day via SQLite's strftime
func TestChallenge_MetricsAggregations_LeapYearAndEdgeDates(t *testing.T) {
	db, accRepo, _, metricsRepo, _ := setupTestStore(t)
	ctx := context.Background()

	acc := &domain.Account{ID: "acc-leap", Email: "leap@example.com", RefreshToken: "rt"}
	if err := accRepo.Create(ctx, acc); err != nil {
		t.Fatalf("Create acc: %v", err)
	}

	// 1. Leap Year: Feb 28, Feb 29 (leap day), March 1 of 2024
	testMetrics := []*domain.TokenMetric{
		// Feb 28
		{
			AccountID:        "acc-leap",
			PromptTokens:     100,
			CandidatesTokens: 50,
			TotalTokens:      150,
			Timestamp:        time.Date(2024, 2, 28, 23, 50, 0, 0, time.UTC),
		},
		// Feb 29 early morning
		{
			AccountID:        "acc-leap",
			PromptTokens:     200,
			CandidatesTokens: 100,
			TotalTokens:      300,
			Timestamp:        time.Date(2024, 2, 29, 0, 5, 0, 0, time.UTC),
		},
		// Feb 29 noon
		{
			AccountID:        "acc-leap",
			PromptTokens:     300,
			CandidatesTokens: 150,
			TotalTokens:      450,
			Timestamp:        time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC),
		},
		// Feb 29 late night
		{
			AccountID:        "acc-leap",
			PromptTokens:     400,
			CandidatesTokens: 200,
			TotalTokens:      600,
			Timestamp:        time.Date(2024, 2, 29, 23, 59, 59, 0, time.UTC),
		},
		// March 1 early morning
		{
			AccountID:        "acc-leap",
			PromptTokens:     500,
			CandidatesTokens: 250,
			TotalTokens:      750,
			Timestamp:        time.Date(2024, 3, 1, 0, 0, 1, 0, time.UTC),
		},
		// Year boundary: 2025-12-31 23:59:59
		{
			AccountID:        "acc-leap",
			PromptTokens:     1000,
			CandidatesTokens: 500,
			TotalTokens:      1500,
			Timestamp:        time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC),
		},
		// Year boundary: 2026-01-01 00:00:00
		{
			AccountID:        "acc-leap",
			PromptTokens:     2000,
			CandidatesTokens: 1000,
			TotalTokens:      3000,
			Timestamp:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	for i, m := range testMetrics {
		if err := metricsRepo.Record(ctx, m); err != nil {
			t.Fatalf("Record metric %d failed: %v", i, err)
		}
	}

	// Check lifetime summary: sums all 7 records
	// PromptTokens: 100 + 200 + 300 + 400 + 500 + 1000 + 2000 = 4500
	// TotalTokens: 150 + 300 + 450 + 600 + 750 + 1500 + 3000 = 6750
	summary, err := metricsRepo.GetSummary(ctx, "acc-leap", "lifetime")
	if err != nil {
		t.Fatalf("GetSummary lifetime: %v", err)
	}
	if summary.TotalRequests != 7 {
		t.Errorf("expected 7 requests, got %d", summary.TotalRequests)
	}
	if summary.TotalPromptTokens != 4500 {
		t.Errorf("expected 4500 prompt tokens, got %d", summary.TotalPromptTokens)
	}
	if summary.TotalTokens != 6750 {
		t.Errorf("expected 6750 total tokens, got %d", summary.TotalTokens)
	}

	// Query daily history spanning back far enough to include 2024 (e.g. 2000 days)
	history, err := metricsRepo.GetDailyHistory(ctx, "acc-leap", 2000)
	if err != nil {
		t.Fatalf("GetDailyHistory: %v", err)
	}

	// Convert history into map by day
	byDay := make(map[string]*domain.DailyTokenUsage)
	for _, h := range history {
		byDay[h.Date] = h
	}

	// Verify Leap Day (2024-02-29): exactly 3 records, PromptTokens=900, TotalTokens=1350
	feb29, exists := byDay["2024-02-29"]
	if !exists {
		t.Fatal("expected history entry for leap day 2024-02-29, but not found")
	}
	if feb29.RequestCount != 3 {
		t.Errorf("expected 3 requests on 2024-02-29, got %d", feb29.RequestCount)
	}
	if feb29.PromptTokens != 900 {
		t.Errorf("expected 900 prompt tokens on 2024-02-29, got %d", feb29.PromptTokens)
	}
	if feb29.TotalTokens != 1350 {
		t.Errorf("expected 1350 total tokens on 2024-02-29, got %d", feb29.TotalTokens)
	}

	// Verify Feb 28: 1 record, PromptTokens=100, TotalTokens=150
	feb28, exists := byDay["2024-02-28"]
	if !exists {
		t.Fatal("expected history entry for 2024-02-28, but not found")
	}
	if feb28.RequestCount != 1 || feb28.TotalTokens != 150 {
		t.Errorf("unexpected feb 28 stats: %+v", feb28)
	}

	// Verify March 1: 1 record, PromptTokens=500, TotalTokens=750
	mar01, exists := byDay["2024-03-01"]
	if !exists {
		t.Fatal("expected history entry for 2024-03-01, but not found")
	}
	if mar01.RequestCount != 1 || mar01.TotalTokens != 750 {
		t.Errorf("unexpected mar 01 stats: %+v", mar01)
	}

	// Verify Year Turnover: 2025-12-31 and 2026-01-01 partitioned correctly
	dec31, exists := byDay["2025-12-31"]
	if !exists {
		t.Fatal("expected history entry for 2025-12-31, but not found")
	}
	if dec31.RequestCount != 1 || dec31.TotalTokens != 1500 {
		t.Errorf("unexpected 2025-12-31 stats: %+v", dec31)
	}

	jan01, exists := byDay["2026-01-01"]
	if !exists {
		t.Fatal("expected history entry for 2026-01-01, but not found")
	}
	if jan01.RequestCount != 1 || jan01.TotalTokens != 3000 {
		t.Errorf("unexpected 2026-01-01 stats: %+v", jan01)
	}

	// Verify strftime directly with SQLite query
	var formattedDate string
	err = db.QueryRowContext(ctx, "SELECT strftime('%Y-%m-%d', '2024-02-29 12:00:00')").Scan(&formattedDate)
	if err != nil {
		t.Fatalf("strftime query: %v", err)
	}
	if formattedDate != "2024-02-29" {
		t.Errorf("expected '2024-02-29', got %s", formattedDate)
	}
}

// TestChallenge_MetricsAggregations_LargeNumbersAndBounds stress-tests:
// - Values larger than 32-bit integer (> 2^31 - 1 = 2,147,483,647)
// - High volume of large records (sums in trillions of tokens)
// - Multi-quintillion values near int64 limit (up to 8 * 10^18)
// - Zero/default value behavior and auto-calculation of TotalTokens
func TestChallenge_MetricsAggregations_LargeNumbersAndBounds(t *testing.T) {
	_, accRepo, _, metricsRepo, _ := setupTestStore(t)
	ctx := context.Background()

	acc := &domain.Account{ID: "acc-large", Email: "large@example.com", RefreshToken: "rt"}
	if err := accRepo.Create(ctx, acc); err != nil {
		t.Fatalf("Create acc: %v", err)
	}

	// 1. Single metric exceeding 32-bit integer limits:
	// PromptTokens = 5,000,000,000 (5 billion)
	// CandidatesTokens = 3,000,000,000 (3 billion)
	// TotalTokens = 8,000,000,000 (8 billion)
	m1 := &domain.TokenMetric{
		AccountID:           "acc-large",
		RequestPath:         "/v1internal:streamGenerateContent",
		PromptTokens:        5_000_000_000,
		CandidatesTokens:    3_000_000_000,
		CachedContentTokens: 1_000_000_000,
		ThoughtsTokens:      500_000_000,
		Timestamp:           time.Now().UTC(),
	}
	// Note: TotalTokens is 0, so Record should auto-calculate 8_000_000_000
	if err := metricsRepo.Record(ctx, m1); err != nil {
		t.Fatalf("Record large metric: %v", err)
	}
	if m1.TotalTokens != 8_000_000_000 {
		t.Errorf("expected auto-calculated TotalTokens 8_000_000_000, got %d", m1.TotalTokens)
	}

	summary1, err := metricsRepo.GetSummary(ctx, "acc-large", "lifetime")
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if summary1.TotalPromptTokens != 5_000_000_000 {
		t.Errorf("expected 5B prompt tokens, got %d", summary1.TotalPromptTokens)
	}
	if summary1.TotalCandidatesTokens != 3_000_000_000 {
		t.Errorf("expected 3B candidates tokens, got %d", summary1.TotalCandidatesTokens)
	}
	if summary1.TotalTokens != 8_000_000_000 {
		t.Errorf("expected 8B total tokens, got %d", summary1.TotalTokens)
	}

	// 2. High accumulation test:
	// Insert 200 records, each with 100,000,000 tokens (100 million).
	// Total accumulated tokens will be 200 * 100M = 20,000,000,000 (20 billion) + 8 billion from m1 = 28 billion.
	const numRecords = 200
	const tokensPerRecord = 100_000_000
	for i := 0; i < numRecords; i++ {
		err := metricsRepo.Record(ctx, &domain.TokenMetric{
			AccountID:        "acc-large",
			PromptTokens:     tokensPerRecord / 2,
			CandidatesTokens: tokensPerRecord / 2,
			TotalTokens:      tokensPerRecord,
			Timestamp:        time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("Record batch %d: %v", i, err)
		}
	}

	summary2, err := metricsRepo.GetSummary(ctx, "acc-large", "lifetime")
	if err != nil {
		t.Fatalf("GetSummary after batch: %v", err)
	}
	expectedTotal := int64(8_000_000_000) + int64(numRecords*tokensPerRecord)
	if summary2.TotalTokens != expectedTotal {
		t.Errorf("expected accumulated total %d, got %d", expectedTotal, summary2.TotalTokens)
	}
	if summary2.TotalRequests != int64(numRecords+1) {
		t.Errorf("expected %d requests, got %d", numRecords+1, summary2.TotalRequests)
	}

	// 3. Multi-quintillion values near int64 maximum:
	// math.MaxInt64 is ~9.22 * 10^18.
	// Let's test a metric with 3 * 10^18 tokens.
	accHuge := &domain.Account{ID: "acc-huge", Email: "huge@example.com", RefreshToken: "rt"}
	_ = accRepo.Create(ctx, accHuge)

	const hugeVal1 int64 = 3_000_000_000_000_000_000 // 3 * 10^18
	const hugeVal2 int64 = 4_000_000_000_000_000_000 // 4 * 10^18
	err = metricsRepo.Record(ctx, &domain.TokenMetric{
		AccountID:        "acc-huge",
		PromptTokens:     hugeVal1,
		CandidatesTokens: 0,
		TotalTokens:      hugeVal1,
		Timestamp:        time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Record hugeVal1: %v", err)
	}

	err = metricsRepo.Record(ctx, &domain.TokenMetric{
		AccountID:        "acc-huge",
		PromptTokens:     hugeVal2,
		CandidatesTokens: 0,
		TotalTokens:      hugeVal2,
		Timestamp:        time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Record hugeVal2: %v", err)
	}

	summaryHuge, err := metricsRepo.GetSummary(ctx, "acc-huge", "lifetime")
	if err != nil {
		t.Fatalf("GetSummary huge: %v", err)
	}
	const expectedHuge int64 = 7_000_000_000_000_000_000 // 7 * 10^18
	if summaryHuge.TotalTokens != expectedHuge {
		t.Errorf("expected %d tokens, got %d", expectedHuge, summaryHuge.TotalTokens)
	}
	if summaryHuge.TotalPromptTokens != expectedHuge {
		t.Errorf("expected %d prompt tokens, got %d", expectedHuge, summaryHuge.TotalPromptTokens)
	}

	// 4. Test behavior when sum overflows int64 (> 9.22 * 10^18):
	// SQLite's SUM() returns float64 on integer overflow.
	// Let's verify what database/sql Scan does when float64 is returned into int64.
	accOverflow := &domain.Account{ID: "acc-overflow", Email: "overflow@example.com", RefreshToken: "rt"}
	_ = accRepo.Create(ctx, accOverflow)

	// Two records each of math.MaxInt64 - 100
	_ = metricsRepo.Record(ctx, &domain.TokenMetric{
		AccountID:   "acc-overflow",
		TotalTokens: math.MaxInt64 - 100,
		Timestamp:   time.Now().UTC(),
	})
	_ = metricsRepo.Record(ctx, &domain.TokenMetric{
		AccountID:   "acc-overflow",
		TotalTokens: math.MaxInt64 - 100,
		Timestamp:   time.Now().UTC(),
	})

	// Does GetSummary handle or return error on int64 overflow?
	summaryOverflow, err := metricsRepo.GetSummary(ctx, "acc-overflow", "lifetime")
	if err != nil {
		t.Logf("Observed behavior on SQLite SUM() int64 overflow: error = %v", err)
	} else {
		t.Logf("Observed behavior on SQLite SUM() int64 overflow: TotalTokens = %d", summaryOverflow.TotalTokens)
	}
}

// TestChallenge_TimezoneHandling_InMetrics tests how metrics recording and aggregation
// handle non-UTC timestamps (e.g. timestamps produced by local machines with non-UTC zones).
func TestChallenge_TimezoneHandling_InMetrics(t *testing.T) {
	_, accRepo, _, metricsRepo, _ := setupTestStore(t)
	ctx := context.Background()

	acc := &domain.Account{ID: "acc-tz", Email: "tz@example.com", RefreshToken: "rt"}
	if err := accRepo.Create(ctx, acc); err != nil {
		t.Fatalf("Create acc: %v", err)
	}

	nowUTC := time.Now().UTC()

	// 1. Record metric with timestamp created in a UTC-3 timezone
	locBRT := time.FixedZone("BRT", -3*3600)
	timeInBRT := nowUTC.In(locBRT).Add(-1 * time.Hour) // 1 hour ago

	mBRT := &domain.TokenMetric{
		AccountID:        "acc-tz",
		PromptTokens:     100,
		CandidatesTokens: 50,
		TotalTokens:      150,
		Timestamp:        timeInBRT,
	}
	if err := metricsRepo.Record(ctx, mBRT); err != nil {
		t.Fatalf("Record mBRT: %v", err)
	}

	// 2. Record metric with timestamp created in a UTC+9 timezone
	locJST := time.FixedZone("JST", 9*3600)
	timeInJST := nowUTC.In(locJST).Add(-2 * time.Hour) // 2 hours ago

	mJST := &domain.TokenMetric{
		AccountID:        "acc-tz",
		PromptTokens:     200,
		CandidatesTokens: 100,
		TotalTokens:      300,
		Timestamp:        timeInJST,
	}
	if err := metricsRepo.Record(ctx, mJST); err != nil {
		t.Fatalf("Record mJST: %v", err)
	}

	// 3. Query GetSummary with "day":
	// Both mBRT (1h ago) and mJST (2h ago) fall within the past 24 hours.
	summary, err := metricsRepo.GetSummary(ctx, "acc-tz", "day")
	if err != nil {
		t.Fatalf("GetSummary day: %v", err)
	}

	t.Logf("Timezone test: day summary captured %d requests and %d tokens", summary.TotalRequests, summary.TotalTokens)
	if summary.TotalRequests != 2 {
		t.Errorf("expected 2 requests captured within day period despite timezone offsets, got %d (tokens: %d)",
			summary.TotalRequests, summary.TotalTokens)
	}

	// 4. Query GetDailyHistory:
	history, err := metricsRepo.GetDailyHistory(ctx, "acc-tz", 7)
	if err != nil {
		t.Fatalf("GetDailyHistory: %v", err)
	}
	t.Logf("GetDailyHistory returned %d entries", len(history))
	for _, h := range history {
		t.Logf("  Day: %s, TotalTokens: %d, RequestCount: %d", h.Date, h.TotalTokens, h.RequestCount)
	}
}

// TestChallenge_ConcurrentMixedWorkload_RaceDetector runs a sustained, high-intensity
// concurrent workload across accounts, quota buckets, metrics, and events while being
// checked by Go's ThreadSanitizer (-race).
func TestChallenge_ConcurrentMixedWorkload_RaceDetector(t *testing.T) {
	_, accRepo, quotaRepo, metricsRepo, eventRepo := setupTestStore(t)
	ctx := context.Background()

	const numAccounts = 8
	for i := 0; i < numAccounts; i++ {
		acc := &domain.Account{
			ID:           fmt.Sprintf("mix-acc-%d", i),
			Email:        fmt.Sprintf("mix-%d@example.com", i),
			RefreshToken: fmt.Sprintf("rt-%d", i),
			Status:       domain.AccountStatusActive,
		}
		if err := accRepo.Create(ctx, acc); err != nil {
			t.Fatalf("Create acc %d: %v", i, err)
		}
	}
	_ = accRepo.SetActive(ctx, "mix-acc-0")

	var (
		wg      sync.WaitGroup
		stopCh  = make(chan struct{})
		errChan = make(chan error, 100)
	)

	const numWorkers = 40
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))

			for {
				select {
				case <-stopCh:
					return
				default:
					accID := fmt.Sprintf("mix-acc-%d", rng.Intn(numAccounts))

					switch rng.Intn(6) {
					case 0:
						// SetActive
						if err := accRepo.SetActive(ctx, accID); err != nil {
							select {
							case errChan <- fmt.Errorf("worker %d SetActive(%s): %w", workerID, accID, err):
							default:
							}
						}
					case 1:
						// GetActive
						active, err := accRepo.GetActive(ctx)
						if err != nil {
							select {
							case errChan <- fmt.Errorf("worker %d GetActive: %w", workerID, err):
							default:
							}
						} else if !active.IsActive {
							select {
							case errChan <- fmt.Errorf("worker %d GetActive returned inactive: %s", workerID, active.ID):
							default:
							}
						}
					case 2:
						// GetNextAvailable
						_, err := accRepo.GetNextAvailable(ctx, accID)
						if err != nil && !errors.Is(err, domain.ErrNoAvailableAccount) {
							select {
							case errChan <- fmt.Errorf("worker %d GetNextAvailable: %w", workerID, err):
							default:
							}
						}
					case 3:
						// Record TokenMetric
						err := metricsRepo.Record(ctx, &domain.TokenMetric{
							AccountID:        accID,
							RequestPath:      "/v1internal:streamGenerateContent",
							PromptTokens:     int64(rng.Intn(5000) + 1),
							CandidatesTokens: int64(rng.Intn(2000) + 1),
							Timestamp:        time.Now().UTC(),
						})
						if err != nil {
							select {
							case errChan <- fmt.Errorf("worker %d Record: %w", workerID, err):
							default:
							}
						}
					case 4:
						// GetSummary
						periods := []string{"day", "week", "month", "lifetime"}
						p := periods[rng.Intn(len(periods))]
						_, err := metricsRepo.GetSummary(ctx, accID, p)
						if err != nil {
							select {
							case errChan <- fmt.Errorf("worker %d GetSummary: %w", workerID, err):
							default:
							}
						}
					case 5:
						// Upsert QuotaBucket
						err := quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
							{
								AccountID:         accID,
								BucketID:          "daily-chat",
								Window:            domain.QuotaWindowDaily,
								RemainingFraction: rng.Float64(),
								RemainingAmount:   int64(rng.Intn(1000)),
								ResetTime:         time.Now().UTC().Add(time.Duration(rng.Intn(24)) * time.Hour),
							},
						})
						if err != nil {
							select {
							case errChan <- fmt.Errorf("worker %d UpsertBuckets: %w", workerID, err):
							default:
							}
						}
						// Record event
						_ = eventRepo.Record(ctx, &domain.ProxyEvent{
							Type:      domain.EventTypeRequestSuccess,
							AccountID: accID,
							Message:   "quota synced",
						})
					}

					time.Sleep(time.Duration(rng.Intn(2)) * time.Millisecond)
				}
			}
		}(w)
	}

	// Let the workers run concurrently for 2 seconds
	time.Sleep(2 * time.Second)
	close(stopCh)
	wg.Wait()
	close(errChan)

	var errCount int
	for err := range errChan {
		t.Errorf("concurrent worker error: %v", err)
		errCount++
	}
	if errCount > 0 {
		t.Fatalf("Encountered %d errors in concurrent mixed workload", errCount)
	}
}
