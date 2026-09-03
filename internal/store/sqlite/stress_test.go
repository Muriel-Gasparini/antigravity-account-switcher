package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
)

// TestStress_100Goroutines_SingleDB executes 100 concurrent goroutines against a single
// SQLite WAL store, performing simultaneous account switches, quota upserts, metrics records,
// event records, and queries. It verifies:
// 1. Zero database lock errors (SQLITE_BUSY, SQLITE_LOCKED)
// 2. Zero deadlocks
// 3. Race-free execution under go test -race
// 4. Exact metrics and quota consistency after completion
func TestStress_100Goroutines_SingleDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "stress_single.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	defer db.Close()

	accRepo := sqlite.NewAccountRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)
	metricsRepo := sqlite.NewMetricsRepository(db)
	eventRepo := sqlite.NewEventRepository(db)

	ctx := context.Background()

	// Seed 10 accounts
	const numAccounts = 10
	accountIDs := make([]string, numAccounts)
	for i := 0; i < numAccounts; i++ {
		id := fmt.Sprintf("stress-acc-%02d", i)
		accountIDs[i] = id
		err := accRepo.Create(ctx, &domain.Account{
			ID:           id,
			Email:        fmt.Sprintf("user%02d@example.com", i),
			RefreshToken: fmt.Sprintf("rt-%02d", i),
			AccessToken:  fmt.Sprintf("at-%02d", i),
			TokenExpiry:  time.Now().Add(1 * time.Hour),
			Status:       domain.AccountStatusActive,
		})
		if err != nil {
			t.Fatalf("seed account %s failed: %v", id, err)
		}
	}

	// Activate account 0
	if err := accRepo.SetActive(ctx, accountIDs[0]); err != nil {
		t.Fatalf("initial SetActive failed: %v", err)
	}

	const concurrency = 100
	const iterationsPerGoroutine = 10

	var (
		wg             sync.WaitGroup
		errCount       atomic.Int64
		metricCount    atomic.Int64
		switchCount    atomic.Int64
		quotaCount     atomic.Int64
		readCount      atomic.Int64
		firstErrorOnce sync.Once
		firstError     error
	)

	recordErr := func(workerID, iter int, op string, err error) {
		if err != nil {
			errCount.Add(1)
			firstErrorOnce.Do(func() {
				firstError = fmt.Errorf("worker %d iter %d op %s failed: %w", workerID, iter, op, err)
			})
		}
	}

	startBarrier := make(chan struct{})
	t0 := time.Now()

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			<-startBarrier

			r := rand.New(rand.NewSource(int64(workerID * 1000)))

			for i := 0; i < iterationsPerGoroutine; i++ {
				targetAcc := accountIDs[r.Intn(numAccounts)]

				// Op 1: Record Token Metric
				err := metricsRepo.Record(ctx, &domain.TokenMetric{
					AccountID:           targetAcc,
					RequestPath:         "/v1internal:streamGenerateContent?alt=sse",
					PromptTokens:        int64(10 + workerID),
					CandidatesTokens:    int64(20 + i),
					CachedContentTokens: int64(workerID % 5),
					ThoughtsTokens:      int64(i % 3),
				})
				if err != nil {
					recordErr(workerID, i, "metrics.Record", err)
					return
				}
				metricCount.Add(1)

				// Op 2: Upsert Quota Bucket
				err = quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
					{
						AccountID:         targetAcc,
						BucketID:          "daily-chat",
						DisplayName:       "Daily Chat",
						Window:            domain.QuotaWindowDaily,
						RemainingFraction: float64(100-i) / 100.0,
						RemainingAmount:   int64(1000 - i*10),
						ResetTime:         time.Now().Add(12 * time.Hour),
					},
					{
						AccountID:         targetAcc,
						BucketID:          "weekly-chat",
						DisplayName:       "Weekly Chat",
						Window:            domain.QuotaWindowWeekly,
						RemainingFraction: float64(500-i) / 500.0,
						RemainingAmount:   int64(5000 - i*10),
						ResetTime:         time.Now().Add(72 * time.Hour),
					},
				})
				if err != nil {
					recordErr(workerID, i, "quota.UpsertBuckets", err)
					return
				}
				quotaCount.Add(1)

				// Op 3: Record Event
				err = eventRepo.Record(ctx, &domain.ProxyEvent{
					Type:      domain.EventTypeRequestSuccess,
					AccountID: targetAcc,
					Message:   fmt.Sprintf("request handled worker=%d iter=%d", workerID, i),
				})
				if err != nil {
					recordErr(workerID, i, "event.Record", err)
					return
				}

				// Op 4: Account Switch (every 3rd iteration)
				if i%3 == 0 {
					switchAcc := accountIDs[(workerID+i)%numAccounts]
					if err := accRepo.SetActive(ctx, switchAcc); err != nil {
						recordErr(workerID, i, "account.SetActive", err)
						return
					}
					switchCount.Add(1)
				}

				// Op 5: Concurrent Reads (GetActive, GetNextAvailable, GetSummary)
				active, err := accRepo.GetActive(ctx)
				if err != nil && !errors.Is(err, domain.ErrNoActiveAccount) {
					recordErr(workerID, i, "account.GetActive", err)
					return
				}
				if active == nil || !active.IsActive {
					recordErr(workerID, i, "account.GetActive.Invariant", errors.New("expected active account to have IsActive=true"))
					return
				}

				next, err := accRepo.GetNextAvailable(ctx, active.ID)
				if err != nil && !errors.Is(err, domain.ErrNoAvailableAccount) {
					recordErr(workerID, i, "account.GetNextAvailable", err)
					return
				}
				_ = next

				summary, err := metricsRepo.GetSummary(ctx, targetAcc, "lifetime")
				if err != nil {
					recordErr(workerID, i, "metrics.GetSummary", err)
					return
				}
				if summary.TotalRequests == 0 {
					recordErr(workerID, i, "metrics.GetSummary.Check", errors.New("expected at least 1 request in summary"))
					return
				}
				readCount.Add(1)
			}
		}(w)
	}

	// Release all 100 workers at once
	close(startBarrier)
	wg.Wait()
	elapsed := time.Since(t0)

	t.Logf("Stress test completed in %v: %d metrics, %d quota upserts, %d switches, %d read cycles, %d errors",
		elapsed, metricCount.Load(), quotaCount.Load(), switchCount.Load(), readCount.Load(), errCount.Load())

	if errCount.Load() > 0 {
		t.Fatalf("Encountered %d errors during high concurrency stress. First error: %v", errCount.Load(), firstError)
	}

	// Post-concurrency Invariant Checks:
	// 1. Exactly 1 active account in database
	active, err := accRepo.GetActive(ctx)
	if err != nil {
		t.Fatalf("post-stress GetActive failed: %v", err)
	}
	if !active.IsActive {
		t.Errorf("post-stress active account has IsActive=false: %+v", active)
	}

	var activeCount int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE is_active = 1").Scan(&activeCount)
	if err != nil {
		t.Fatalf("query active count: %v", err)
	}
	if activeCount != 1 {
		t.Errorf("expected exactly 1 active account, got %d", activeCount)
	}

	// 2. Exact metric count matches recorded count
	expectedMetrics := int64(concurrency * iterationsPerGoroutine)
	if metricCount.Load() != expectedMetrics {
		t.Errorf("expected %d metrics recorded, got %d", expectedMetrics, metricCount.Load())
	}

	summary, err := metricsRepo.GetSummary(ctx, "", "lifetime")
	if err != nil {
		t.Fatalf("post-stress global GetSummary failed: %v", err)
	}
	if summary.TotalRequests != expectedMetrics {
		t.Errorf("expected %d total requests in summary, got %d", expectedMetrics, summary.TotalRequests)
	}

	// 3. All 10 accounts have valid quota buckets
	allBuckets, err := quotaRepo.ListAll(ctx)
	if err != nil {
		t.Fatalf("post-stress ListAll quota failed: %v", err)
	}
	if len(allBuckets) != numAccounts {
		t.Errorf("expected quota buckets for %d accounts, got %d", numAccounts, len(allBuckets))
	}
}

// TestStress_MultiHandle_CrossConnectionContention simulates multiple processes or
// independent connections (e.g. CLI 'wrap' + 'add-account' + 'status') accessing the same SQLite WAL file.
// It verifies that modernc.org/sqlite with WAL mode, busy_timeout(5000), and immediate locking
// handles cross-connection write contention without SQLITE_BUSY.
func TestStress_MultiHandle_CrossConnectionContention(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "stress_multihandle.db")

	// Primary handle initializes schema
	primaryDB, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("initial sqlite.Open: %v", err)
	}
	defer primaryDB.Close()

	ctx := context.Background()
	accRepo := sqlite.NewAccountRepository(primaryDB)

	// Seed 5 accounts
	const numAccounts = 5
	accountIDs := make([]string, numAccounts)
	for i := 0; i < numAccounts; i++ {
		id := fmt.Sprintf("multi-acc-%d", i)
		accountIDs[i] = id
		if err := accRepo.Create(ctx, &domain.Account{
			ID:           id,
			Email:        fmt.Sprintf("multi%d@example.com", i),
			RefreshToken: fmt.Sprintf("rt-%d", i),
			Status:       domain.AccountStatusActive,
		}); err != nil {
			t.Fatalf("create account: %v", err)
		}
	}
	_ = accRepo.SetActive(ctx, accountIDs[0])

	// Open 5 independent DB handles to simulate 5 concurrent processes
	const numHandles = 5
	handles := make([]*sqlite.DB, numHandles)
	for i := 0; i < numHandles; i++ {
		h, err := sqlite.Open(dbPath)
		if err != nil {
			t.Fatalf("open handle %d failed: %v", i, err)
		}
		defer h.Close()
		handles[i] = h
	}

	const workersPerHandle = 20
	const iterations = 10
	var (
		wg             sync.WaitGroup
		errCount       atomic.Int64
		firstErrorOnce sync.Once
		firstError     error
	)

	startBarrier := make(chan struct{})

	for hIdx := 0; hIdx < numHandles; hIdx++ {
		h := handles[hIdx]
		hAccRepo := sqlite.NewAccountRepository(h)
		hQuotaRepo := sqlite.NewQuotaRepository(h)
		hMetricsRepo := sqlite.NewMetricsRepository(h)

		for w := 0; w < workersPerHandle; w++ {
			wg.Add(1)
			go func(handleID, workerID int) {
				defer wg.Done()
				<-startBarrier

				r := rand.New(rand.NewSource(int64(handleID*100 + workerID)))

				for i := 0; i < iterations; i++ {
					accID := accountIDs[r.Intn(numAccounts)]

					// Simultaneous write 1: Quota upsert
					err := hQuotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{
						{
							AccountID:         accID,
							BucketID:          "concurrent-bucket",
							Window:            domain.QuotaWindowDaily,
							RemainingFraction: 0.85,
							RemainingAmount:   850,
							ResetTime:         time.Now().Add(6 * time.Hour),
						},
					})
					if err != nil {
						errCount.Add(1)
						firstErrorOnce.Do(func() {
							firstError = fmt.Errorf("handle %d worker %d quota failed: %w", handleID, workerID, err)
						})
						return
					}

					// Simultaneous write 2: Metrics record
					err = hMetricsRepo.Record(ctx, &domain.TokenMetric{
						AccountID:        accID,
						RequestPath:      "/v1internal:generate",
						PromptTokens:     int64(10 + workerID),
						CandidatesTokens: int64(20 + i),
					})
					if err != nil {
						errCount.Add(1)
						firstErrorOnce.Do(func() {
							firstError = fmt.Errorf("handle %d worker %d metric failed: %w", handleID, workerID, err)
						})
						return
					}

					// Simultaneous write 3: Switch active account
					if i%2 == 0 {
						target := accountIDs[(handleID+workerID+i)%numAccounts]
						if err := hAccRepo.SetActive(ctx, target); err != nil {
							errCount.Add(1)
							firstErrorOnce.Do(func() {
								firstError = fmt.Errorf("handle %d worker %d set active failed: %w", handleID, workerID, err)
							})
							return
						}
					}

					// Read: Check active account
					active, err := hAccRepo.GetActive(ctx)
					if err != nil {
						errCount.Add(1)
						firstErrorOnce.Do(func() {
							firstError = fmt.Errorf("handle %d worker %d get active failed: %w", handleID, workerID, err)
						})
						return
					}
					if !active.IsActive {
						errCount.Add(1)
						firstErrorOnce.Do(func() {
							firstError = fmt.Errorf("handle %d worker %d got inactive active account: %+v", handleID, workerID, active)
						})
						return
					}
				}
			}(hIdx, w)
		}
	}

	close(startBarrier)
	wg.Wait()

	if errCount.Load() > 0 {
		t.Fatalf("Encountered %d errors during multi-handle cross-connection stress. First error: %v", errCount.Load(), firstError)
	}

	// Verify exact single-active constraint on disk
	var activeCount int
	if err := primaryDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE is_active = 1").Scan(&activeCount); err != nil {
		t.Fatalf("query active count: %v", err)
	}
	if activeCount != 1 {
		t.Errorf("expected exactly 1 active account across handles, got %d", activeCount)
	}
}

// TestStress_SingleActiveInvariant_Contention focuses specifically on racing SetActive vs GetActive
// across 100 concurrent workers to guarantee zero transient states where no account is active.
func TestStress_SingleActiveInvariant_Contention(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "stress_invariant.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	accRepo := sqlite.NewAccountRepository(db)
	ctx := context.Background()

	const numAccounts = 8
	accountIDs := make([]string, numAccounts)
	for i := 0; i < numAccounts; i++ {
		id := fmt.Sprintf("inv-acc-%d", i)
		accountIDs[i] = id
		if err := accRepo.Create(ctx, &domain.Account{
			ID:           id,
			Email:        fmt.Sprintf("inv%d@example.com", i),
			RefreshToken: fmt.Sprintf("rt-%d", i),
			Status:       domain.AccountStatusActive,
		}); err != nil {
			t.Fatalf("create account: %v", err)
		}
	}
	_ = accRepo.SetActive(ctx, accountIDs[0])

	var (
		wg             sync.WaitGroup
		stopCh         = make(chan struct{})
		noActiveFound  atomic.Int64
		readOps        atomic.Int64
		switchOps      atomic.Int64
		firstErrorOnce sync.Once
		firstError     error
	)

	// 50 writers switching accounts furiously
	const writers = 50
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(workerID int) {
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
						firstErrorOnce.Do(func() { firstError = fmt.Errorf("SetActive failed: %w", err) })
						return
					}
					switchOps.Add(1)
				}
			}
		}(w)
	}

	// 50 readers sampling GetActive continuously
	const readers = 50
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(readerID int) {
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
							noActiveFound.Add(1)
						} else {
							firstErrorOnce.Do(func() { firstError = fmt.Errorf("GetActive failed: %w", err) })
							return
						}
					} else if acc == nil || !acc.IsActive {
						noActiveFound.Add(1)
					}
				}
			}
		}(r)
	}

	// Let the storm run for 1 second
	time.Sleep(1 * time.Second)
	close(stopCh)
	wg.Wait()

	t.Logf("Invariant stress completed: %d switches, %d reads, %d 'no active' anomalies",
		switchOps.Load(), readOps.Load(), noActiveFound.Load())

	if firstError != nil {
		t.Fatalf("Error during invariant contention: %v", firstError)
	}

	if noActiveFound.Load() > 0 {
		t.Errorf("FAIL: Encountered %d instances where GetActive returned ErrNoActiveAccount during concurrent switches!", noActiveFound.Load())
	}
}

// TestStress_InterleavedRollbacksAndCommits verifies that rolled-back transactions
// (e.g. unique constraint conflicts or non-existent IDs) do not poison SQLite connection state,
// cause lock leaks, or block subsequent concurrent operations.
func TestStress_InterleavedRollbacksAndCommits(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "stress_rollbacks.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	accRepo := sqlite.NewAccountRepository(db)
	ctx := context.Background()

	// Seed 2 valid accounts
	_ = accRepo.Create(ctx, &domain.Account{ID: "valid-1", Email: "valid1@example.com", RefreshToken: "rt1"})
	_ = accRepo.Create(ctx, &domain.Account{ID: "valid-2", Email: "valid2@example.com", RefreshToken: "rt2"})
	_ = accRepo.SetActive(ctx, "valid-1")

	const concurrency = 60
	const iterations = 15
	var (
		wg             sync.WaitGroup
		errCount       atomic.Int64
		rollbackCount  atomic.Int64
		commitCount    atomic.Int64
		firstErrorOnce sync.Once
		firstError     error
	)

	startBarrier := make(chan struct{})

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			<-startBarrier

			for i := 0; i < iterations; i++ {
				if i%2 == 0 {
					// Intentional rollback / error: SetActive on non-existent ID
					err := accRepo.SetActive(ctx, fmt.Sprintf("non-existent-%d-%d", workerID, i))
					if !errors.Is(err, domain.ErrAccountNotFound) {
						errCount.Add(1)
						firstErrorOnce.Do(func() {
							firstError = fmt.Errorf("expected ErrAccountNotFound, got %v", err)
						})
						return
					}
					rollbackCount.Add(1)
				} else {
					// Valid commit: alternate between valid-1 and valid-2
					target := "valid-1"
					if (workerID+i)%2 == 0 {
						target = "valid-2"
					}
					if err := accRepo.SetActive(ctx, target); err != nil {
						errCount.Add(1)
						firstErrorOnce.Do(func() {
							firstError = fmt.Errorf("valid SetActive failed: %w", err)
						})
						return
					}
					commitCount.Add(1)
				}
			}
		}(w)
	}

	close(startBarrier)
	wg.Wait()

	t.Logf("Rollback stress completed: %d rollbacks, %d commits, %d unexpected errors",
		rollbackCount.Load(), commitCount.Load(), errCount.Load())

	if errCount.Load() > 0 {
		t.Fatalf("Unexpected error during rollback stress: %v", firstError)
	}

	// Active account must still be either valid-1 or valid-2
	active, err := accRepo.GetActive(ctx)
	if err != nil {
		t.Fatalf("GetActive failed after rollback stress: %v", err)
	}
	if active.ID != "valid-1" && active.ID != "valid-2" {
		t.Errorf("unexpected active account: %s", active.ID)
	}
}
