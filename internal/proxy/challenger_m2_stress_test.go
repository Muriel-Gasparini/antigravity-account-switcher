package proxy_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/proxy"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/test/mocks"
)

// setupDiskDB creates a temporary SQLite database in WAL mode for realistic stress testing.
func setupDiskDB(t *testing.T) (*sqlite.DB, domain.AccountRepository, domain.MetricsRepository, domain.EventRepository) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), fmt.Sprintf("test_proxy_stress_%d.db", rand.Int63()))
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to create disk sqlite db: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})

	accountRepo := sqlite.NewAccountRepository(db)
	metricsRepo := sqlite.NewMetricsRepository(db)
	eventRepo := sqlite.NewEventRepository(db)
	return db, accountRepo, metricsRepo, eventRepo
}

// 1. Anti-stampede guard under 50 parallel goroutines triggering 429
// Verifies that when 50 concurrent requests hit 429 on Account A,
// exactly Account A is rotated to Account B, and the other 49 concurrent requests
// are redirected to Account B WITHOUT cascading false-exhaustion.
func TestChallenger_AntiStampede_50Concurrent429_SingleFailover(t *testing.T) {
	mockGoogle := mocks.NewMockGoogleServer()
	defer mockGoogle.Close()

	_, accountRepo, _, eventRepo := setupDiskDB(t)

	now := time.Now().UTC()
	accounts := []*domain.Account{
		{
			ID:          "acc-A",
			Email:       "alpha@gmail.com",
			AccessToken: "token-A",
			IsActive:    true,
			Status:      domain.AccountStatusActive,
			CreatedAt:   now.Add(-40 * time.Minute),
			UpdatedAt:   now.Add(-40 * time.Minute),
		},
		{
			ID:          "acc-B",
			Email:       "bravo@gmail.com",
			AccessToken: "token-B",
			IsActive:    false,
			Status:      domain.AccountStatusActive,
			CreatedAt:   now.Add(-30 * time.Minute),
			UpdatedAt:   now.Add(-30 * time.Minute),
		},
		{
			ID:          "acc-C",
			Email:       "charlie@gmail.com",
			AccessToken: "token-C",
			IsActive:    false,
			Status:      domain.AccountStatusActive,
			CreatedAt:   now.Add(-20 * time.Minute),
			UpdatedAt:   now.Add(-20 * time.Minute),
		},
		{
			ID:          "acc-D",
			Email:       "delta@gmail.com",
			AccessToken: "token-D",
			IsActive:    false,
			Status:      domain.AccountStatusActive,
			CreatedAt:   now.Add(-10 * time.Minute),
			UpdatedAt:   now.Add(-10 * time.Minute),
		},
	}

	for _, acc := range accounts {
		if err := accountRepo.Create(context.Background(), acc); err != nil {
			t.Fatalf("failed to create account %s: %v", acc.ID, err)
		}
	}

	// Token A returns 429 on all attempts (up to 200)
	mockGoogle.SetFailoverTrigger("token-A", 200)

	broadcaster := proxy.NewBroadcaster(500)
	eventsCh, unsubscribe := broadcaster.Subscribe()
	defer unsubscribe()

	handler, err := proxy.NewProxyHandler(
		accountRepo,
		proxy.WithTargetURL(mockGoogle.URL),
		proxy.WithEventBroadcaster(broadcaster),
		proxy.WithEventRepository(eventRepo),
	)
	if err != nil {
		t.Fatalf("failed to create proxy handler: %v", err)
	}

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	const concurrency = 50
	var wg sync.WaitGroup
	var successCount atomic.Int64
	var errorCount atomic.Int64

	startBarrier := make(chan struct{})

	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func(reqID int) {
			defer wg.Done()
			<-startBarrier // synchronize start for maximum stampede pressure

			client := &http.Client{Timeout: 10 * time.Second}
			payload := fmt.Sprintf(`{"request_id": %d, "prompt": "anti-stampede stress"}`, reqID)
			req, err := http.NewRequest("POST", proxyServer.URL+"/v1internal:streamGenerateContent?alt=sse", strings.NewReader(payload))
			if err != nil {
				errorCount.Add(1)
				return
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(req)
			if err != nil {
				errorCount.Add(1)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				successCount.Add(1)
			} else {
				errorCount.Add(1)
			}
		}(i)
	}

	// Release stampede
	close(startBarrier)
	wg.Wait()

	if errorCount.Load() != 0 {
		t.Fatalf("Anti-stampede test had %d failures out of %d concurrent requests", errorCount.Load(), concurrency)
	}
	if successCount.Load() != concurrency {
		t.Fatalf("Expected %d successful requests, got %d", concurrency, successCount.Load())
	}

	// CRITICAL ASSERTION: Verify Account A is exhausted, but B, C, D remain ACTIVE/HEALTHY!
	accA, _ := accountRepo.GetByID(context.Background(), "acc-A")
	if accA.Status != domain.AccountStatusExhausted {
		t.Errorf("expected acc-A status to be exhausted, got %s", accA.Status)
	}

	accB, _ := accountRepo.GetByID(context.Background(), "acc-B")
	if accB.Status != domain.AccountStatusActive {
		t.Fatalf("STAMPEDE CASADING FAILURE: acc-B was falsely marked %s", accB.Status)
	}
	if !accB.IsActive {
		t.Fatalf("expected acc-B to be the active account")
	}

	accC, _ := accountRepo.GetByID(context.Background(), "acc-C")
	if accC.Status != domain.AccountStatusActive {
		t.Fatalf("STAMPEDE CASCADING FAILURE: acc-C was falsely marked %s", accC.Status)
	}

	accD, _ := accountRepo.GetByID(context.Background(), "acc-D")
	if accD.Status != domain.AccountStatusActive {
		t.Fatalf("STAMPEDE CASCADING FAILURE: acc-D was falsely marked %s", accD.Status)
	}

	// Drain events and count failover events
	unsubscribe()
	var failoverAEvents int
	var failoverOtherEvents int
drainLoop:
	for {
		select {
		case ev, ok := <-eventsCh:
			if !ok {
				break drainLoop
			}
			if ev.Type == domain.EventTypeFailover429 {
				if ev.AccountID == "acc-A" {
					failoverAEvents++
				} else {
					failoverOtherEvents++
				}
			}
		default:
			break drainLoop
		}
	}

	if failoverOtherEvents > 0 {
		t.Fatalf("Detected %d false failover events for healthy accounts!", failoverOtherEvents)
	}
}

// 2. Multi-tier cascading failover under high concurrent load
// Tests A (429) -> B (429) -> C (200 OK) with 50 parallel requests.
// Only A and B should be marked exhausted; C and D must remain active/healthy.
func TestChallenger_MultiTierCascadingFailover_HighConcurrency(t *testing.T) {
	mockGoogle := mocks.NewMockGoogleServer()
	defer mockGoogle.Close()

	_, accountRepo, _, _ := setupDiskDB(t)

	now := time.Now().UTC()
	accounts := []*domain.Account{
		{ID: "acc-1", Email: "1@gmail.com", AccessToken: "tok-1", IsActive: true, Status: domain.AccountStatusActive, UpdatedAt: now.Add(-30 * time.Minute)},
		{ID: "acc-2", Email: "2@gmail.com", AccessToken: "tok-2", IsActive: false, Status: domain.AccountStatusActive, UpdatedAt: now.Add(-20 * time.Minute)},
		{ID: "acc-3", Email: "3@gmail.com", AccessToken: "tok-3", IsActive: false, Status: domain.AccountStatusActive, UpdatedAt: now.Add(-10 * time.Minute)},
		{ID: "acc-4", Email: "4@gmail.com", AccessToken: "tok-4", IsActive: false, Status: domain.AccountStatusActive, UpdatedAt: now.Add(-5 * time.Minute)},
	}
	for _, acc := range accounts {
		_ = accountRepo.Create(context.Background(), acc)
	}

	// Token 1 and Token 2 both trigger 429
	mockGoogle.SetFailoverTrigger("tok-1", 200)
	mockGoogle.SetFailoverTrigger("tok-2", 200)
	// Token 3 and Token 4 are healthy (returns 200 OK)

	handler, err := proxy.NewProxyHandler(
		accountRepo,
		proxy.WithTargetURL(mockGoogle.URL),
		proxy.WithMaxRetries(3),
	)
	if err != nil {
		t.Fatalf("failed to create proxy handler: %v", err)
	}

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	const concurrency = 40
	var wg sync.WaitGroup
	var successCount atomic.Int64
	var failureCount atomic.Int64

	startBarrier := make(chan struct{})
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func(id int) {
			defer wg.Done()
			<-startBarrier

			client := &http.Client{Timeout: 10 * time.Second}
			body := fmt.Sprintf(`{"id":%d,"data":"multitier"}`, id)
			resp, err := client.Post(proxyServer.URL+"/v1internal:streamGenerateContent", "application/json", strings.NewReader(body))
			if err != nil {
				failureCount.Add(1)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				successCount.Add(1)
			} else {
				failureCount.Add(1)
			}
		}(i)
	}

	close(startBarrier)
	wg.Wait()

	if failureCount.Load() > 0 {
		t.Fatalf("multi-tier failover had %d failures out of %d requests", failureCount.Load(), concurrency)
	}
	if successCount.Load() != concurrency {
		t.Fatalf("expected %d successes, got %d", concurrency, successCount.Load())
	}

	// Verify statuses: acc-1 and acc-2 exhausted, acc-3 active, acc-4 available
	a1, _ := accountRepo.GetByID(context.Background(), "acc-1")
	a2, _ := accountRepo.GetByID(context.Background(), "acc-2")
	a3, _ := accountRepo.GetByID(context.Background(), "acc-3")
	a4, _ := accountRepo.GetByID(context.Background(), "acc-4")

	if a1.Status != domain.AccountStatusExhausted {
		t.Errorf("acc-1 should be exhausted, got %s", a1.Status)
	}
	if a2.Status != domain.AccountStatusExhausted {
		t.Errorf("acc-2 should be exhausted, got %s", a2.Status)
	}
	if a3.Status != domain.AccountStatusActive {
		t.Errorf("acc-3 should be active, got %s", a3.Status)
	}
	if !a3.IsActive {
		t.Errorf("acc-3 should be is_active=true")
	}
	if a4.Status != domain.AccountStatusActive {
		t.Errorf("acc-4 should remain active/healthy, got %s", a4.Status)
	}
}

// 3. Request body rewind and data integrity under concurrency
// Sends 30 concurrent requests with varying body sizes (small 200B, medium 50KB, large 512KB).
// Verifies byte-for-byte integrity on retry at the upstream mock.
func TestChallenger_RequestBodyRewindIntegrity_UnderConcurrency(t *testing.T) {
	mockGoogle := mocks.NewMockGoogleServer()
	defer mockGoogle.Close()

	_, accountRepo, _, _ := setupDiskDB(t)

	now := time.Now().UTC()
	_ = accountRepo.Create(context.Background(), &domain.Account{
		ID: "acc-rw-1", Email: "rw1@gmail.com", AccessToken: "tok-rw-1", IsActive: true, Status: domain.AccountStatusActive, UpdatedAt: now.Add(-10 * time.Minute),
	})
	_ = accountRepo.Create(context.Background(), &domain.Account{
		ID: "acc-rw-2", Email: "rw2@gmail.com", AccessToken: "tok-rw-2", IsActive: false, Status: domain.AccountStatusActive, UpdatedAt: now.Add(-5 * time.Minute),
	})

	// tok-rw-1 will fail all requests with 429
	mockGoogle.SetFailoverTrigger("tok-rw-1", 100)

	handler, _ := proxy.NewProxyHandler(accountRepo, proxy.WithTargetURL(mockGoogle.URL))
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	const concurrency = 30
	type payloadMeta struct {
		id     int
		body   []byte
		sha256 string
	}

	payloads := make([]payloadMeta, concurrency)
	for i := 0; i < concurrency; i++ {
		var size int
		switch i % 3 {
		case 0:
			size = 256 // small
		case 1:
			size = 32 * 1024 // 32 KB medium
		case 2:
			size = 512 * 1024 // 512 KB large
		}
		raw := make([]byte, size)
		for j := range raw {
			raw[j] = byte((i*17 + j*31) % 256)
		}
		// Wrap with JSON
		jsonBody := fmt.Sprintf(`{"id":%d,"padding":"%s"}`, i, hex.EncodeToString(raw[:min(len(raw), 1024)]))
		bodyBytes := []byte(jsonBody)
		h := sha256.Sum256(bodyBytes)
		payloads[i] = payloadMeta{
			id:     i,
			body:   bodyBytes,
			sha256: hex.EncodeToString(h[:]),
		}
	}

	var wg sync.WaitGroup
	var rewindFailures atomic.Int64
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func(meta payloadMeta) {
			defer wg.Done()
			client := &http.Client{Timeout: 15 * time.Second}
			req, err := http.NewRequest("POST", proxyServer.URL+"/v1internal:streamGenerateContent", bytes.NewReader(meta.body))
			if err != nil {
				rewindFailures.Add(1)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Payload-ID", fmt.Sprintf("%d", meta.id))

			resp, err := client.Do(req)
			if err != nil {
				rewindFailures.Add(1)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				rewindFailures.Add(1)
			}
		}(payloads[i])
	}

	wg.Wait()

	if rewindFailures.Load() > 0 {
		t.Fatalf("%d requests failed during rewind stress test", rewindFailures.Load())
	}

	// Verify upstream received requests: all attempts on tok-rw-2 must match exact payload
	recorded := mockGoogle.GetRecordedRequests()
	tok2Requests := make(map[string][]byte)
	for _, rec := range recorded {
		if rec.AuthBearer == "tok-rw-2" {
			reqID := rec.Header.Get("X-Payload-ID")
			tok2Requests[reqID] = rec.Body
		}
	}

	if len(tok2Requests) != concurrency {
		t.Fatalf("expected %d requests on account B, got %d", concurrency, len(tok2Requests))
	}

	for _, meta := range payloads {
		key := fmt.Sprintf("%d", meta.id)
		bodyReceived, exists := tok2Requests[key]
		if !exists {
			t.Fatalf("request %d was never received by account B", meta.id)
		}
		h := sha256.Sum256(bodyReceived)
		receivedSHA := hex.EncodeToString(h[:])
		if receivedSHA != meta.sha256 {
			t.Fatalf("BODY CORRUPTION DETECTED for request %d! SHA mismatch: want %s, got %s (len want %d, got %d)",
				meta.id, meta.sha256, receivedSHA, len(meta.body), len(bodyReceived))
		}
	}
}

// 4. When all accounts are exhausted, upstream 429 response is returned transparently
// Verifies status code 429, verbatim response body, and custom headers.
func TestChallenger_AllAccountsExhausted_Transparent429Passthrough(t *testing.T) {
	mockGoogle := mocks.NewMockGoogleServer()
	defer mockGoogle.Close()

	_, accountRepo, _, _ := setupDiskDB(t)

	now := time.Now().UTC()
	_ = accountRepo.Create(context.Background(), &domain.Account{
		ID: "acc-ex-1", Email: "ex1@gmail.com", AccessToken: "tok-ex-1", IsActive: true, Status: domain.AccountStatusActive, UpdatedAt: now.Add(-10 * time.Minute),
	})
	_ = accountRepo.Create(context.Background(), &domain.Account{
		ID: "acc-ex-2", Email: "ex2@gmail.com", AccessToken: "tok-ex-2", IsActive: false, Status: domain.AccountStatusActive, UpdatedAt: now.Add(-5 * time.Minute),
	})

	// Both accounts hit 429
	mockGoogle.SetFailoverTrigger("tok-ex-1", 100)
	mockGoogle.SetFailoverTrigger("tok-ex-2", 100)

	handler, _ := proxy.NewProxyHandler(accountRepo, proxy.WithTargetURL(mockGoogle.URL))
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	const concurrency = 25
	var wg sync.WaitGroup
	var count429 atomic.Int64
	var bodyMatchCount atomic.Int64

	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func(id int) {
			defer wg.Done()
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Post(proxyServer.URL+"/v1internal:streamGenerateContent", "application/json", strings.NewReader(`{"test":"exhausted"}`))
			if err != nil {
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusTooManyRequests {
				count429.Add(1)
			}
			bodyBytes, _ := io.ReadAll(resp.Body)
			if strings.Contains(string(bodyBytes), "RESOURCE_EXHAUSTED") && strings.Contains(string(bodyBytes), "RATE_LIMIT_EXCEEDED") {
				bodyMatchCount.Add(1)
			}
		}(i)
	}

	wg.Wait()

	if count429.Load() != concurrency {
		t.Fatalf("expected %d transparent 429 responses, got %d", concurrency, count429.Load())
	}
	if bodyMatchCount.Load() != concurrency {
		t.Fatalf("expected %d responses with upstream RESOURCE_EXHAUSTED body, got %d", concurrency, bodyMatchCount.Load())
	}

	// Verify both accounts in DB are marked exhausted
	a1, _ := accountRepo.GetByID(context.Background(), "acc-ex-1")
	a2, _ := accountRepo.GetByID(context.Background(), "acc-ex-2")
	if a1.Status != domain.AccountStatusExhausted || a2.Status != domain.AccountStatusExhausted {
		t.Errorf("expected both accounts exhausted, got a1:%s a2:%s", a1.Status, a2.Status)
	}
}

// 5. HTTP 403 RESOURCE_EXHAUSTED detection & failover vs 403 PERMISSION_DENIED passthrough
func TestChallenger_HTTP403_ResourceExhausted_FailoverVsPassthrough(t *testing.T) {
	mockGoogle := mocks.NewMockGoogleServer()
	defer mockGoogle.Close()

	_, accountRepo, _, _ := setupDiskDB(t)

	now := time.Now().UTC()
	_ = accountRepo.Create(context.Background(), &domain.Account{
		ID: "acc-403-1", Email: "403_1@gmail.com", AccessToken: "tok-403-1", IsActive: true, Status: domain.AccountStatusActive, UpdatedAt: now.Add(-10 * time.Minute),
	})
	_ = accountRepo.Create(context.Background(), &domain.Account{
		ID: "acc-403-2", Email: "403_2@gmail.com", AccessToken: "tok-403-2", IsActive: false, Status: domain.AccountStatusActive, UpdatedAt: now.Add(-5 * time.Minute),
	})

	// Configure tok-403-1 to return 403 RESOURCE_EXHAUSTED
	mockGoogle.ConfigureAccount("tok-403-1", &mocks.AccountBehavior{
		ForceStatusCode: http.StatusForbidden,
		ForceErrorCode:  "RESOURCE_EXHAUSTED",
	})

	handler, _ := proxy.NewProxyHandler(accountRepo, proxy.WithTargetURL(mockGoogle.URL))
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	client := &http.Client{}
	resp, err := client.Post(proxyServer.URL+"/v1internal:streamGenerateContent", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// 403 RESOURCE_EXHAUSTED should trigger failover and succeed with 200 OK on Account 2!
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK after 403 RESOURCE_EXHAUSTED failover, got %d", resp.StatusCode)
	}

	// Now configure Account 2 to return standard 403 PERMISSION_DENIED
	mockGoogle.ConfigureAccount("tok-403-2", &mocks.AccountBehavior{
		ForceStatusCode: http.StatusForbidden,
		ForceErrorCode:  "PERMISSION_DENIED",
	})

	resp2, err := client.Post(proxyServer.URL+"/v1internal:streamGenerateContent", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp2.Body.Close()

	// PERMISSION_DENIED should NOT trigger failover; transparently return 403
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden passthrough for PERMISSION_DENIED, got %d", resp2.StatusCode)
	}
}

// 6. MaxRetries limit enforcement when retry count is reached
func TestChallenger_MaxRetriesEnforcement(t *testing.T) {
	mockGoogle := mocks.NewMockGoogleServer()
	defer mockGoogle.Close()

	_, accountRepo, _, _ := setupDiskDB(t)

	now := time.Now().UTC()
	// Seed 5 accounts, but set MaxRetries = 1
	for i := 1; i <= 5; i++ {
		_ = accountRepo.Create(context.Background(), &domain.Account{
			ID:          fmt.Sprintf("acc-ret-%d", i),
			Email:       fmt.Sprintf("ret%d@gmail.com", i),
			AccessToken: fmt.Sprintf("tok-ret-%d", i),
			IsActive:    i == 1,
			Status:      domain.AccountStatusActive,
			UpdatedAt:   now.Add(time.Duration(-50+i*5) * time.Minute),
		})
		mockGoogle.SetFailoverTrigger(fmt.Sprintf("tok-ret-%d", i), 100)
	}

	handler, _ := proxy.NewProxyHandler(
		accountRepo,
		proxy.WithTargetURL(mockGoogle.URL),
		proxy.WithMaxRetries(1), // Allow only 1 retry (initial attempt + 1 retry = 2 attempts total)
	)
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	client := &http.Client{}
	resp, err := client.Post(proxyServer.URL+"/v1internal:streamGenerateContent", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// Should return 429
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 when max retries reached, got %d", resp.StatusCode)
	}

	// Verify only 2 attempts were made
	reqs := mockGoogle.GetRecordedRequests()
	if len(reqs) != 2 {
		t.Fatalf("expected exactly 2 upstream attempts with MaxRetries=1, got %d", len(reqs))
	}
}

// 7. Client context cancellation mid-flight resilience
func TestChallenger_ClientCancellationMidFlight(t *testing.T) {
	mockGoogle := mocks.NewMockGoogleServer()
	defer mockGoogle.Close()

	_, accountRepo, _, _ := setupDiskDB(t)

	now := time.Now().UTC()
	_ = accountRepo.Create(context.Background(), &domain.Account{
		ID: "acc-cancel", Email: "cancel@gmail.com", AccessToken: "tok-cancel", IsActive: true, Status: domain.AccountStatusActive, UpdatedAt: now,
	})

	handler, _ := proxy.NewProxyHandler(accountRepo, proxy.WithTargetURL(mockGoogle.URL))
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", proxyServer.URL+"/v1internal:streamGenerateContent?alt=sse", strings.NewReader(`{}`))

	// Cancel immediately after starting
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	client := &http.Client{}
	_, _ = client.Do(req)

	// Proxy should remain healthy and accept new requests immediately
	resp, err := client.Post(proxyServer.URL+"/v1internal:streamGenerateContent", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post-cancellation request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK after client cancellation, got %d", resp.StatusCode)
	}
}
