package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
)

// TestChallenger1_ContextCancellation_DeferredRollback tests that when client contexts
// are cancelled mid-transaction or during contention, the deferred tx.Rollback()
// cleans up resources properly without corrupting state or deadlocking the connection pool.
func TestChallenger1_ContextCancellation_DeferredRollback(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "challenger1_ctx_cancel.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	accRepo := sqlite.NewAccountRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)

	ctx := context.Background()

	// Seed 4 accounts
	const numAccounts = 4
	for i := 0; i < numAccounts; i++ {
		id := fmt.Sprintf("acc-ctx-%d", i)
		if err := accRepo.Create(ctx, &domain.Account{
			ID:           id,
			Email:        fmt.Sprintf("ctx%d@example.com", i),
			RefreshToken: fmt.Sprintf("rt-%d", i),
			Status:       domain.AccountStatusActive,
		}); err != nil {
			t.Fatalf("create account: %v", err)
		}
	}
	if err := accRepo.SetActive(ctx, "acc-ctx-0"); err != nil {
		t.Fatalf("set initial active: %v", err)
	}

	const concurrentWorkers = 40
	const iterations = 20

	var (
		wg             sync.WaitGroup
		cancelledCount atomic.Int64
		successCount   atomic.Int64
		errCount       atomic.Int64
		firstErrorOnce sync.Once
		firstError     error
	)

	startBarrier := make(chan struct{})

	for w := 0; w < concurrentWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			<-startBarrier

			r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))

			for i := 0; i < iterations; i++ {
				targetAcc := fmt.Sprintf("acc-ctx-%d", r.Intn(numAccounts))

				if workerID%2 == 0 {
					// Timed-out context (sub-millisecond or few milliseconds deadline to guarantee cancellation mid-flight)
					timeout := time.Duration(r.Intn(3000)+100) * time.Microsecond
					opCtx, cancel := context.WithTimeout(context.Background(), timeout)

					// Randomly test SetActive or UpsertBuckets under canceled ctx
					var opErr error
					if i%2 == 0 {
						opErr = accRepo.SetActive(opCtx, targetAcc)
					} else {
						opErr = quotaRepo.UpsertBuckets(opCtx, []*domain.QuotaBucket{
							{
								AccountID:         targetAcc,
								BucketID:          fmt.Sprintf("b-%d", i),
								DisplayName:       "Test Bucket",
								Window:            domain.QuotaWindowDaily,
								RemainingFraction: 0.5,
								RemainingAmount:   50,
								ResetTime:         time.Now().Add(1 * time.Hour),
							},
						})
					}
					cancel()

					if opErr != nil {
						if errors.Is(opErr, context.Canceled) || errors.Is(opErr, context.DeadlineExceeded) ||
							opCtx.Err() != nil || strings.Contains(opErr.Error(), "interrupted") || errors.Is(opErr, domain.ErrAccountNotFound) {
							cancelledCount.Add(1)
						} else {
							errCount.Add(1)
							firstErrorOnce.Do(func() {
								firstError = fmt.Errorf("unexpected error on canceled worker %d: %w", workerID, opErr)
							})
						}
					} else {
						successCount.Add(1)
					}
				} else {
					// Normal context with ample time
					normalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					var opErr error
					if i%2 == 0 {
						opErr = accRepo.SetActive(normalCtx, targetAcc)
					} else {
						opErr = quotaRepo.UpsertBuckets(normalCtx, []*domain.QuotaBucket{
							{
								AccountID:         targetAcc,
								BucketID:          fmt.Sprintf("b-norm-%d", i),
								DisplayName:       "Normal Bucket",
								Window:            domain.QuotaWindowDaily,
								RemainingFraction: 0.9,
								RemainingAmount:   90,
								ResetTime:         time.Now().Add(2 * time.Hour),
							},
						})
					}
					cancel()

					if opErr != nil {
						errCount.Add(1)
						firstErrorOnce.Do(func() {
							firstError = fmt.Errorf("unexpected error on normal worker %d: %w", workerID, opErr)
						})
					} else {
						successCount.Add(1)
					}
				}
			}
		}(w)
	}

	close(startBarrier)
	wg.Wait()

	t.Logf("Context cancellation stress: %d cancelled, %d successes, %d unexpected errors",
		cancelledCount.Load(), successCount.Load(), errCount.Load())

	if errCount.Load() > 0 {
		t.Fatalf("Contention with context cancellation caused error: %v", firstError)
	}

	// Verify post-storm state: exactly 1 active account
	active, err := accRepo.GetActive(ctx)
	if err != nil {
		t.Fatalf("GetActive failed after context stress: %v", err)
	}
	if !active.IsActive {
		t.Errorf("expected active account to have is_active=true, got %v", active)
	}

	// Verify DB is healthy and can execute new transaction
	if err := accRepo.SetActive(ctx, "acc-ctx-1"); err != nil {
		t.Fatalf("post-stress SetActive failed: %v", err)
	}
}

// TestChallenger1_PartialBatchUpsert_RollbackIsolation tests that when a multi-statement
// transaction (UpsertBuckets) fails midway through a batch, the deferred tx.Rollback()
// guarantees 100% atomicity: ZERO items from the failing batch are committed to disk.
func TestChallenger1_PartialBatchUpsert_RollbackIsolation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "challenger1_batch_isolation.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	accRepo := sqlite.NewAccountRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)
	ctx := context.Background()

	// Seed valid account
	validAccID := "valid-acc-001"
	if err := accRepo.Create(ctx, &domain.Account{
		ID:           validAccID,
		Email:        "valid@example.com",
		RefreshToken: "rt-valid",
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}

	const concurrentGoroutines = 30
	const iterations = 10

	var (
		wg             sync.WaitGroup
		rollbackCount  atomic.Int64
		committedCount atomic.Int64
		errCount       atomic.Int64
		firstErrorOnce sync.Once
		firstError     error
	)

	startBarrier := make(chan struct{})

	for w := 0; w < concurrentGoroutines; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			<-startBarrier

			for i := 0; i < iterations; i++ {
				if workerID%2 == 0 {
					// Failing batch: 5 buckets for valid account, then 1 bucket with non-existent foreign key!
					batch := make([]*domain.QuotaBucket, 6)
					for b := 0; b < 5; b++ {
						batch[b] = &domain.QuotaBucket{
							AccountID:         validAccID,
							BucketID:          fmt.Sprintf("fail-w%d-i%d-b%d", workerID, i, b),
							DisplayName:       "Should Rollback",
							Window:            domain.QuotaWindowDaily,
							RemainingFraction: 0.1,
							RemainingAmount:   10,
							ResetTime:         time.Now().Add(1 * time.Hour),
						}
					}
					// 6th bucket has invalid account ID (violates foreign key)
					batch[5] = &domain.QuotaBucket{
						AccountID:         "non-existent-fk-account",
						BucketID:          fmt.Sprintf("fail-w%d-i%d-b5", workerID, i),
						DisplayName:       "FK Violator",
						Window:            domain.QuotaWindowDaily,
						RemainingFraction: 0.0,
						RemainingAmount:   0,
						ResetTime:         time.Now().Add(1 * time.Hour),
					}

					err := quotaRepo.UpsertBuckets(ctx, batch)
					if err == nil {
						errCount.Add(1)
						firstErrorOnce.Do(func() {
							firstError = fmt.Errorf("expected foreign key violation error in worker %d, got nil", workerID)
						})
						return
					}
					rollbackCount.Add(1)
				} else {
					// Valid batch: 3 buckets for valid account
					batch := make([]*domain.QuotaBucket, 3)
					for b := 0; b < 3; b++ {
						batch[b] = &domain.QuotaBucket{
							AccountID:         validAccID,
							BucketID:          fmt.Sprintf("ok-w%d-i%d-b%d", workerID, i, b),
							DisplayName:       "Should Commit",
							Window:            domain.QuotaWindowDaily,
							RemainingFraction: 0.8,
							RemainingAmount:   80,
							ResetTime:         time.Now().Add(2 * time.Hour),
						}
					}

					if err := quotaRepo.UpsertBuckets(ctx, batch); err != nil {
						errCount.Add(1)
						firstErrorOnce.Do(func() {
							firstError = fmt.Errorf("valid batch failed in worker %d: %w", workerID, err)
						})
						return
					}
					committedCount.Add(1)
				}
			}
		}(w)
	}

	close(startBarrier)
	wg.Wait()

	t.Logf("Batch isolation stress: %d rollbacks, %d commits, %d unexpected errors",
		rollbackCount.Load(), committedCount.Load(), errCount.Load())

	if errCount.Load() > 0 {
		t.Fatalf("Unexpected error: %v", firstError)
	}

	// ORACLE VERIFICATION:
	// Verify that absolutely ZERO buckets starting with "fail-" exist in SQLite!
	buckets, err := quotaRepo.GetByAccountID(ctx, validAccID)
	if err != nil {
		t.Fatalf("GetByAccountID failed: %v", err)
	}

	var leakedRollbacks int
	for _, b := range buckets {
		if len(b.BucketID) >= 5 && b.BucketID[:5] == "fail-" {
			leakedRollbacks++
		}
	}

	if leakedRollbacks > 0 {
		t.Fatalf("ISOLATION LEAK: Found %d buckets from rolled-back transactions in quota_buckets table!", leakedRollbacks)
	}

	// Verify that all committed buckets exist
	expectedCommitted := int(committedCount.Load()) * 3
	if len(buckets) != expectedCommitted {
		t.Errorf("expected exactly %d committed buckets, found %d", expectedCommitted, len(buckets))
	}
}

// TestChallenger1_SetActive_RollbackAtomicity_UnderExtremeContention tests SetActive rollback
// atomicity under extreme concurrency: when SetActive targets a non-existent ID, it executes
// Step 1 (deactivate current), fails on Step 2, and triggers deferred tx.Rollback().
// Under heavy concurrent readers and writers, no reader must ever observe a state where
// 0 accounts are active.
func TestChallenger1_SetActive_RollbackAtomicity_UnderExtremeContention(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "challenger1_setactive_atomicity.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	accRepo := sqlite.NewAccountRepository(db)
	ctx := context.Background()

	const numAccounts = 5
	accountIDs := make([]string, numAccounts)
	for i := 0; i < numAccounts; i++ {
		id := fmt.Sprintf("atom-acc-%d", i)
		accountIDs[i] = id
		if err := accRepo.Create(ctx, &domain.Account{
			ID:           id,
			Email:        fmt.Sprintf("atom%d@example.com", i),
			RefreshToken: fmt.Sprintf("rt-%d", i),
			Status:       domain.AccountStatusActive,
		}); err != nil {
			t.Fatalf("create account: %v", err)
		}
	}
	if err := accRepo.SetActive(ctx, accountIDs[0]); err != nil {
		t.Fatalf("initial SetActive: %v", err)
	}

	const duration = 2 * time.Second
	stopCh := make(chan struct{})

	var (
		wg             sync.WaitGroup
		rollbackOps    atomic.Int64
		commitOps      atomic.Int64
		readOps        atomic.Int64
		noActiveErrors atomic.Int64
		firstErrorOnce sync.Once
		firstError     error
	)

	// 25 workers triggering failing SetActive (rollbacks)
	for w := 0; w < 25; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			idx := 0
			for {
				select {
				case <-stopCh:
					return
				default:
					idx++
					invalidID := fmt.Sprintf("does-not-exist-%d-%d", id, idx)
					err := accRepo.SetActive(ctx, invalidID)
					if !errors.Is(err, domain.ErrAccountNotFound) {
						firstErrorOnce.Do(func() {
							firstError = fmt.Errorf("worker %d expected ErrAccountNotFound, got %v", id, err)
						})
						return
					}
					rollbackOps.Add(1)
				}
			}
		}(w)
	}

	// 25 workers triggering valid SetActive (commits)
	for w := 0; w < 25; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			idx := 0
			for {
				select {
				case <-stopCh:
					return
				default:
					target := accountIDs[idx%numAccounts]
					idx++
					if err := accRepo.SetActive(ctx, target); err != nil {
						firstErrorOnce.Do(func() {
							firstError = fmt.Errorf("valid SetActive failed in worker %d: %w", id, err)
						})
						return
					}
					commitOps.Add(1)
				}
			}
		}(w)
	}

	// 50 continuous readers sampling GetActive
	for r := 0; r < 50; r++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					acc, err := accRepo.GetActive(ctx)
					readOps.Add(1)
					if err != nil {
						if errors.Is(err, domain.ErrNoActiveAccount) {
							noActiveErrors.Add(1)
						} else {
							firstErrorOnce.Do(func() {
								firstError = fmt.Errorf("reader %d failed: %w", id, err)
							})
							return
						}
					} else if acc == nil || !acc.IsActive {
						noActiveErrors.Add(1)
					}
				}
			}
		}(r)
	}

	time.Sleep(duration)
	close(stopCh)
	wg.Wait()

	t.Logf("SetActive rollback atomicity stress: %d rollbacks, %d commits, %d reads, %d no-active anomalies",
		rollbackOps.Load(), commitOps.Load(), readOps.Load(), noActiveErrors.Load())

	if firstError != nil {
		t.Fatalf("Contention error: %v", firstError)
	}

	if noActiveErrors.Load() > 0 {
		t.Fatalf("INVARIANT VIOLATION: Observed %d instances where GetActive returned no active account during rollbacks!",
			noActiveErrors.Load())
	}

	// Final verification of DB state
	var activeCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE is_active = 1").Scan(&activeCount); err != nil {
		t.Fatalf("query active count: %v", err)
	}
	if activeCount != 1 {
		t.Errorf("expected exactly 1 active account in database, found %d", activeCount)
	}
}

// TestChallenger1_PanicRecovery_DeferredRollbackCleanliness verifies that if a panic
// occurs during transaction processing (which is recovered by caller), the deferred
// rollback ensures that the SQLite single connection pool is returned cleanly
// and subsequent transactions proceed without error or deadlock.
func TestChallenger1_PanicRecovery_DeferredRollbackCleanliness(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "challenger1_panic_recovery.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	const concurrentWorkers = 20
	const iterations = 10

	var (
		wg             sync.WaitGroup
		recoveredCount atomic.Int64
		successCount   atomic.Int64
		errCount       atomic.Int64
	)

	for w := 0; w < concurrentWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			for i := 0; i < iterations; i++ {
				// 1. Trigger simulated panic within a transaction holding deferred rollback
				func() {
					defer func() {
						if r := recover(); r != nil {
							recoveredCount.Add(1)
						}
					}()

					tx, err := db.BeginTx(ctx, nil)
					if err != nil {
						errCount.Add(1)
						return
					}
					defer func() { _ = tx.Rollback() }()

					// Modify something in tx
					_, _ = tx.ExecContext(ctx, "INSERT INTO accounts (id, email, refresh_token) VALUES (?, ?, ?)",
						fmt.Sprintf("panic-acc-%d-%d", workerID, i),
						fmt.Sprintf("panic-%d-%d@example.com", workerID, i),
						"rt",
					)

					// Simulate panic before commit
					panic("simulated worker failure")
				}()

				// 2. Immediately execute valid transaction on same pool
				func() {
					tx, err := db.BeginTx(ctx, nil)
					if err != nil {
						errCount.Add(1)
						return
					}
					defer func() { _ = tx.Rollback() }()

					_, err = tx.ExecContext(ctx, "INSERT INTO accounts (id, email, refresh_token) VALUES (?, ?, ?)",
						fmt.Sprintf("valid-acc-%d-%d", workerID, i),
						fmt.Sprintf("valid-%d-%d@example.com", workerID, i),
						"rt",
					)
					if err != nil {
						errCount.Add(1)
						return
					}

					if err := tx.Commit(); err != nil {
						errCount.Add(1)
						return
					}
					successCount.Add(1)
				}()
			}
		}(w)
	}

	wg.Wait()

	t.Logf("Panic recovery stress: %d panics recovered, %d successful commits, %d errors",
		recoveredCount.Load(), successCount.Load(), errCount.Load())

	if errCount.Load() > 0 {
		t.Fatalf("Encountered %d errors after panic recovery rollbacks", errCount.Load())
	}

	// Verify that NONE of the panicked inserts made it to the DB
	var panicRows int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE id LIKE 'panic-%'").Scan(&panicRows); err != nil {
		t.Fatalf("query panic rows: %v", err)
	}
	if panicRows != 0 {
		t.Errorf("expected 0 panic rows committed, found %d", panicRows)
	}

	var validRows int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE id LIKE 'valid-%'").Scan(&validRows); err != nil {
		t.Fatalf("query valid rows: %v", err)
	}
	if validRows != int(successCount.Load()) {
		t.Errorf("expected %d valid rows, found %d", successCount.Load(), validRows)
	}
}

// TestChallenger1_MultiHandle_ExtremeRollbackStorm creates multiple distinct SQLite database
// handles pointing to the same file (simulating concurrent processes or reload handles) and
// launches a mixed storm of high-frequency rollbacks and commits across all handles.
func TestChallenger1_MultiHandle_ExtremeRollbackStorm(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "challenger1_multihandle_storm.db")
	primaryDB, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open primary: %v", err)
	}
	defer primaryDB.Close()

	primaryAccRepo := sqlite.NewAccountRepository(primaryDB)
	ctx := context.Background()

	const numAccounts = 6
	accountIDs := make([]string, numAccounts)
	for i := 0; i < numAccounts; i++ {
		id := fmt.Sprintf("multi-acc-%d", i)
		accountIDs[i] = id
		if err := primaryAccRepo.Create(ctx, &domain.Account{
			ID:           id,
			Email:        fmt.Sprintf("multi%d@example.com", i),
			RefreshToken: fmt.Sprintf("rt-%d", i),
			Status:       domain.AccountStatusActive,
		}); err != nil {
			t.Fatalf("create account: %v", err)
		}
	}
	if err := primaryAccRepo.SetActive(ctx, accountIDs[0]); err != nil {
		t.Fatalf("initial SetActive: %v", err)
	}

	// Create 4 additional handles
	const extraHandles = 4
	handles := make([]*sqlite.DB, extraHandles+1)
	handles[0] = primaryDB
	for h := 1; h <= extraHandles; h++ {
		hDB, err := sqlite.Open(dbPath)
		if err != nil {
			t.Fatalf("sqlite.Open handle %d: %v", h, err)
		}
		defer hDB.Close()
		handles[h] = hDB
	}

	const workersPerHandle = 8
	const totalIterations = 15

	var (
		wg             sync.WaitGroup
		rollbackCount  atomic.Int64
		commitCount    atomic.Int64
		errCount       atomic.Int64
		firstErrorOnce sync.Once
		firstError     error
	)

	startBarrier := make(chan struct{})

	for hIdx, hDB := range handles {
		hAccRepo := sqlite.NewAccountRepository(hDB)
		hQuotaRepo := sqlite.NewQuotaRepository(hDB)

		for w := 0; w < workersPerHandle; w++ {
			wg.Add(1)
			go func(handleID, workerID int, ar *sqlite.AccountRepository, qr *sqlite.QuotaRepository) {
				defer wg.Done()
				<-startBarrier

				r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(handleID*100+workerID)))

				for i := 0; i < totalIterations; i++ {
					mode := r.Intn(4)
					switch mode {
					case 0:
						// Intentional rollback: SetActive non-existent
						err := ar.SetActive(ctx, fmt.Sprintf("non-existent-%d-%d-%d", handleID, workerID, i))
						if errors.Is(err, domain.ErrAccountNotFound) {
							rollbackCount.Add(1)
						} else {
							errCount.Add(1)
							firstErrorOnce.Do(func() {
								firstError = fmt.Errorf("h%d w%d expected ErrAccountNotFound, got %v", handleID, workerID, err)
							})
							return
						}
					case 1:
						// Intentional rollback: UpsertBuckets foreign key violation
						err := qr.UpsertBuckets(ctx, []*domain.QuotaBucket{
							{
								AccountID: fmt.Sprintf("invalid-fk-%d-%d", handleID, workerID),
								BucketID:  "b-invalid",
								Window:    domain.QuotaWindowDaily,
							},
						})
						if err != nil {
							rollbackCount.Add(1)
						} else {
							errCount.Add(1)
							firstErrorOnce.Do(func() {
								firstError = fmt.Errorf("h%d w%d expected FK error, got nil", handleID, workerID)
							})
							return
						}
					case 2:
						// Valid commit: SetActive
						target := accountIDs[r.Intn(numAccounts)]
						if err := ar.SetActive(ctx, target); err != nil {
							errCount.Add(1)
							firstErrorOnce.Do(func() {
								firstError = fmt.Errorf("h%d w%d valid SetActive failed: %w", handleID, workerID, err)
							})
							return
						}
						commitCount.Add(1)
					case 3:
						// Valid commit: UpsertBuckets
						target := accountIDs[r.Intn(numAccounts)]
						err := qr.UpsertBuckets(ctx, []*domain.QuotaBucket{
							{
								AccountID:         target,
								BucketID:          fmt.Sprintf("quota-h%d-w%d", handleID, workerID),
								DisplayName:       "Storm Quota",
								Window:            domain.QuotaWindowDaily,
								RemainingFraction: r.Float64(),
								RemainingAmount:   int64(r.Intn(100)),
								ResetTime:         time.Now().Add(24 * time.Hour),
							},
						})
						if err != nil {
							errCount.Add(1)
							firstErrorOnce.Do(func() {
								firstError = fmt.Errorf("h%d w%d valid UpsertBuckets failed: %w", handleID, workerID, err)
							})
							return
						}
						commitCount.Add(1)
					}
				}
			}(hIdx, w, hAccRepo, hQuotaRepo)
		}
	}

	close(startBarrier)
	wg.Wait()

	t.Logf("Multi-handle storm finished: %d rollbacks, %d commits, %d errors across 5 handles",
		rollbackCount.Load(), commitCount.Load(), errCount.Load())

	if errCount.Load() > 0 {
		t.Fatalf("Multi-handle rollback storm encountered errors: %v", firstError)
	}

	// Invariant check: exactly 1 active account
	var activeCount int
	if err := primaryDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE is_active = 1").Scan(&activeCount); err != nil {
		t.Fatalf("query active count: %v", err)
	}
	if activeCount != 1 {
		t.Errorf("expected 1 active account, got %d", activeCount)
	}
}
