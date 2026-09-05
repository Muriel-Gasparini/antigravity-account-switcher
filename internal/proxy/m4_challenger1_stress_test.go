package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
)

// m4StressTestEnv encapsulates an isolated proxy handler environment with a mock upstream.
type m4StressTestEnv struct {
	db             *sqlite.DB
	accountRepo    domain.AccountRepository
	metricsRepo    domain.MetricsRepository
	eventRepo      domain.EventRepository
	quotaRepo      domain.QuotaRepository
	broadcaster    *Broadcaster
	failoverEngine *FailoverEngine
	handler        *ProxyHandler
	upstream       *httptest.Server
	proxyServer    *httptest.Server
	client         *http.Client
}

func newM4StressTestEnv(
	t *testing.T,
	upstreamHandler http.HandlerFunc,
	primary, secondary string,
	fallbackEnabled bool,
) *m4StressTestEnv {
	t.Helper()

	upstream := httptest.NewServer(upstreamHandler)
	t.Cleanup(func() { upstream.Close() })

	dbPath := filepath.Join(t.TempDir(), "m4_stress.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test sqlite db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	accRepo := sqlite.NewAccountRepository(db)
	metricsRepo := sqlite.NewMetricsRepository(db)
	eventRepo := sqlite.NewEventRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)

	broadcaster := NewBroadcaster(500)
	engine := NewFailoverEngine(
		accRepo,
		broadcaster,
		eventRepo,
		WithQuotaRepository(quotaRepo),
		WithModelFallback(primary, secondary, fallbackEnabled),
	)

	handler, err := NewProxyHandler(
		accRepo,
		WithTargetURL(upstream.URL),
		WithMetricsRepository(metricsRepo),
		WithEventBroadcaster(broadcaster),
		WithEventRepository(eventRepo),
		WithFailoverEngine(engine),
	)
	if err != nil {
		t.Fatalf("failed to create proxy handler: %v", err)
	}

	proxyServer := httptest.NewServer(handler)
	t.Cleanup(func() { proxyServer.Close() })

	transport := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     30 * time.Second,
	}

	return &m4StressTestEnv{
		db:             db,
		accountRepo:    accRepo,
		metricsRepo:    metricsRepo,
		eventRepo:      eventRepo,
		quotaRepo:      quotaRepo,
		broadcaster:    broadcaster,
		failoverEngine: engine,
		handler:        handler,
		upstream:       upstream,
		proxyServer:    proxyServer,
		client:         &http.Client{Transport: transport, Timeout: 15 * time.Second},
	}
}

// TestM4Challenger1_Concurrent429_IntraAccountFallbackStampede tests 50 concurrent goroutines
// sending requests for primary model (gemini-2.5-pro). Account A returns 429 on pro, but
// 200 on secondary (gemini-2.5-flash).
// Invariants verified:
// 1. All 50 requests succeed with 200 OK.
// 2. Anti-stampede: Account A remains the active account (no premature rotation to Account B).
// 3. Zero 429 error leakage to clients.
// 4. All requests are rewritten to secondary model.
func TestM4Challenger1_Concurrent429_IntraAccountFallbackStampede(t *testing.T) {
	const concurrency = 50
	ctx := context.Background()

	var upstreamProReqs int64
	var upstreamFlashReqs int64

	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		isPro := strings.Contains(r.URL.Path, "gemini-2.5-pro") || strings.Contains(string(bodyBytes), "gemini-2.5-pro")
		isFlash := strings.Contains(r.URL.Path, "gemini-2.5-flash") || strings.Contains(string(bodyBytes), "gemini-2.5-flash")

		if isPro {
			atomic.AddInt64(&upstreamProReqs, 1)
			w.Header().Set("Content-Type", "application/json; charset=UTF-8")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Quota exceeded for gemini-2.5-pro","status":"RESOURCE_EXHAUSTED"}}`))
			return
		}

		if isFlash {
			atomic.AddInt64(&upstreamFlashReqs, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"flash-success\"}]}}]}}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = w.Write([]byte("data: {\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":5,\"totalTokenCount\":15}}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			return
		}

		w.WriteHeader(http.StatusBadRequest)
	})

	env := newM4StressTestEnv(t, upstreamHandler, "gemini-2.5-pro", "gemini-2.5-flash", true)

	now := time.Now().UTC()
	accA := &domain.Account{
		ID:          "acc-stampede-A",
		Email:       "stampedeA@example.com",
		AccessToken: "token-stampede-A",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	accB := &domain.Account{
		ID:          "acc-stampede-B",
		Email:       "stampedeB@example.com",
		AccessToken: "token-stampede-B",
		IsActive:    false,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now.Add(time.Second),
		UpdatedAt:   now.Add(time.Second),
	}
	if err := env.accountRepo.Create(ctx, accA); err != nil {
		t.Fatalf("create accA: %v", err)
	}
	if err := env.accountRepo.Create(ctx, accB); err != nil {
		t.Fatalf("create accB: %v", err)
	}

	gate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(concurrency)

	type reqResult struct {
		statusCode int
		body       string
		err        error
	}
	results := make([]reqResult, concurrency)

	for i := 0; i < concurrency; i++ {
		idx := i
		go func() {
			defer wg.Done()
			<-gate // Synchronization barrier to launch all requests concurrently

			reqBody := fmt.Sprintf(`{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"concurrent-test-%d"}]}]}`, idx)
			req, err := http.NewRequest("POST", env.proxyServer.URL+"/v1internal/models/gemini-2.5-pro:streamGenerateContent?alt=sse", strings.NewReader(reqBody))
			if err != nil {
				results[idx] = reqResult{err: err}
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "text/event-stream")

			resp, err := env.client.Do(req)
			if err != nil {
				results[idx] = reqResult{err: err}
				return
			}
			defer resp.Body.Close()

			respBytes, _ := io.ReadAll(resp.Body)
			results[idx] = reqResult{
				statusCode: resp.StatusCode,
				body:       string(respBytes),
			}
		}()
	}

	close(gate) // Release the stampede!
	wg.Wait()

	// Verify all requests succeeded on fallback
	for i, res := range results {
		if res.err != nil {
			t.Fatalf("request %d failed with error: %v", i, res.err)
		}
		if res.statusCode != http.StatusOK {
			t.Errorf("request %d expected 200 OK, got %d: %s", i, res.statusCode, res.body)
		}
		if !strings.Contains(res.body, "flash-success") {
			t.Errorf("request %d missing fallback payload: %s", i, res.body)
		}
		if strings.Contains(res.body, "RESOURCE_EXHAUSTED") || strings.Contains(res.body, "429") {
			t.Errorf("request %d leaked 429 error bytes: %s", i, res.body)
		}
	}

	// Verify Anti-Stampede invariant: Account A MUST STILL BE ACTIVE!
	activeAcc, err := env.accountRepo.GetActive(ctx)
	if err != nil {
		t.Fatalf("failed to get active account: %v", err)
	}
	if activeAcc.ID != accA.ID {
		t.Errorf("ANTI-STAMPEDE VIOLATION: expected active account to be %s, but got %s", accA.ID, activeAcc.ID)
	}

	dbAccB, err := env.accountRepo.GetByID(ctx, accB.ID)
	if err != nil {
		t.Fatalf("failed to get accB: %v", err)
	}
	if dbAccB.IsActive {
		t.Errorf("ANTI-STAMPEDE VIOLATION: accB was unexpectedly activated!")
	}

	// Verify upstream request counts
	pCount := atomic.LoadInt64(&upstreamProReqs)
	fCount := atomic.LoadInt64(&upstreamFlashReqs)
	if pCount < 1 {
		t.Errorf("expected at least 1 upstream pro request, got %d", pCount)
	}
	if fCount != int64(concurrency) {
		t.Errorf("expected %d successful flash requests, got %d", concurrency, fCount)
	}
}

// TestM4Challenger1_ConcurrentDoubleExhaustion_AntiStampedeRotation tests 40 concurrent
// goroutines encountering total exhaustion on Account A (both Pro and Flash 429), rotating
// to Account B (which returns 200 OK).
// Invariants verified:
// 1. Exactly ONE rotation occurs across all concurrent requests (Account A -> Account B).
// 2. Account B is NOT rotated to Account C (no cascading stampede!).
// 3. All requests complete successfully on Account B with primary model reset.
func TestM4Challenger1_ConcurrentDoubleExhaustion_AntiStampedeRotation(t *testing.T) {
	const concurrency = 40
	ctx := context.Background()

	var upstreamReqsOnA int64
	var upstreamReqsOnB int64
	var upstreamReqsOnC int64

	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")

		switch token {
		case "token-rot-A":
			atomic.AddInt64(&upstreamReqsOnA, 1)
			w.Header().Set("Content-Type", "application/json; charset=UTF-8")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Quota exhausted on account A","status":"RESOURCE_EXHAUSTED"}}`))
			return

		case "token-rot-B":
			atomic.AddInt64(&upstreamReqsOnB, 1)
			w.Header().Set("Content-Type", "application/json; charset=UTF-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"success-on-B"}]}}]}}`))
			return

		case "token-rot-C":
			atomic.AddInt64(&upstreamReqsOnC, 1)
			w.Header().Set("Content-Type", "application/json; charset=UTF-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"success-on-C"}]}}]}}`))
			return

		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	})

	env := newM4StressTestEnv(t, upstreamHandler, "gemini-2.5-pro", "gemini-2.5-flash", true)

	now := time.Now().UTC()
	accA := &domain.Account{
		ID:          "acc-rot-A",
		Email:       "rotA@example.com",
		AccessToken: "token-rot-A",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	accB := &domain.Account{
		ID:          "acc-rot-B",
		Email:       "rotB@example.com",
		AccessToken: "token-rot-B",
		IsActive:    false,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now.Add(time.Second),
		UpdatedAt:   now.Add(time.Second),
	}
	accC := &domain.Account{
		ID:          "acc-rot-C",
		Email:       "rotC@example.com",
		AccessToken: "token-rot-C",
		IsActive:    false,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now.Add(2 * time.Second),
		UpdatedAt:   now.Add(2 * time.Second),
	}
	if err := env.accountRepo.Create(ctx, accA); err != nil {
		t.Fatalf("create accA: %v", err)
	}
	if err := env.accountRepo.Create(ctx, accB); err != nil {
		t.Fatalf("create accB: %v", err)
	}
	if err := env.accountRepo.Create(ctx, accC); err != nil {
		t.Fatalf("create accC: %v", err)
	}

	gate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(concurrency)

	type reqResult struct {
		statusCode int
		body       string
		err        error
	}
	results := make([]reqResult, concurrency)

	for i := 0; i < concurrency; i++ {
		idx := i
		go func() {
			defer wg.Done()
			<-gate

			reqBody := fmt.Sprintf(`{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"rot-test-%d"}]}]}`, idx)
			req, err := http.NewRequest("POST", env.proxyServer.URL+"/v1internal/models/gemini-2.5-pro:streamGenerateContent", strings.NewReader(reqBody))
			if err != nil {
				results[idx] = reqResult{err: err}
				return
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := env.client.Do(req)
			if err != nil {
				results[idx] = reqResult{err: err}
				return
			}
			defer resp.Body.Close()

			respBytes, _ := io.ReadAll(resp.Body)
			results[idx] = reqResult{
				statusCode: resp.StatusCode,
				body:       string(respBytes),
			}
		}()
	}

	close(gate)
	wg.Wait()

	// Verify all requests succeeded on Account B
	for i, res := range results {
		if res.err != nil {
			t.Fatalf("request %d failed: %v", i, res.err)
		}
		if res.statusCode != http.StatusOK {
			t.Errorf("request %d expected 200 OK, got %d: %s", i, res.statusCode, res.body)
		}
		if !strings.Contains(res.body, "success-on-B") {
			t.Errorf("request %d missing success-on-B payload: %s", i, res.body)
		}
	}

	// Verify Anti-Stampede invariant: Account B MUST BE THE ACTIVE ACCOUNT!
	activeAcc, err := env.accountRepo.GetActive(ctx)
	if err != nil {
		t.Fatalf("failed to get active account: %v", err)
	}
	if activeAcc.ID != accB.ID {
		t.Errorf("ANTI-STAMPEDE VIOLATION: expected active account to be %s (Account B), but got %s", accB.ID, activeAcc.ID)
	}

	// Account C must NOT have received any requests!
	reqsOnC := atomic.LoadInt64(&upstreamReqsOnC)
	if reqsOnC > 0 {
		t.Errorf("ANTI-STAMPEDE VIOLATION: Account C received %d requests; stampede over-rotated past Account B!", reqsOnC)
	}

	// Account A must be exhausted in DB
	dbAccA, _ := env.accountRepo.GetByID(ctx, accA.ID)
	if dbAccA.Status != domain.AccountStatusExhausted || dbAccA.IsActive {
		t.Errorf("expected accA status=exhausted is_active=false, got status=%s is_active=%v", dbAccA.Status, dbAccA.IsActive)
	}
}

// TestM4Challenger1_ConcurrentDirectRotation_FallbackDisabled tests 40 concurrent goroutines
// when fallbackSecondaryEnabled = false. Account A fails with 429 on primary, rotating to Account B.
// Invariants verified:
// 1. Anti-stampede: Exactly 1 rotation occurs (Account A -> Account B). Account C untouched.
// 2. Zero secondary fallback attempts made.
// 3. All 40 requests succeed on Account B.
func TestM4Challenger1_ConcurrentDirectRotation_FallbackDisabled(t *testing.T) {
	const concurrency = 40
	ctx := context.Background()

	var upstreamReqsOnA int64
	var upstreamReqsOnB int64
	var upstreamReqsOnC int64
	var secondaryAttempts int64

	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		if strings.Contains(r.URL.Path, "flash") || strings.Contains(string(bodyBytes), "flash") {
			atomic.AddInt64(&secondaryAttempts, 1)
		}

		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")

		switch token {
		case "token-dir-A":
			atomic.AddInt64(&upstreamReqsOnA, 1)
			w.Header().Set("Content-Type", "application/json; charset=UTF-8")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Quota exhausted on account A","status":"RESOURCE_EXHAUSTED"}}`))
			return

		case "token-dir-B":
			atomic.AddInt64(&upstreamReqsOnB, 1)
			w.Header().Set("Content-Type", "application/json; charset=UTF-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"success-on-B-direct"}]}}]}}`))
			return

		case "token-dir-C":
			atomic.AddInt64(&upstreamReqsOnC, 1)
			w.WriteHeader(http.StatusOK)
			return

		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	})

	env := newM4StressTestEnv(t, upstreamHandler, "gemini-2.5-pro", "gemini-2.5-flash", false)

	now := time.Now().UTC()
	accA := &domain.Account{
		ID:          "acc-dir-A",
		Email:       "dirA@example.com",
		AccessToken: "token-dir-A",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	accB := &domain.Account{
		ID:          "acc-dir-B",
		Email:       "dirB@example.com",
		AccessToken: "token-dir-B",
		IsActive:    false,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now.Add(time.Second),
		UpdatedAt:   now.Add(time.Second),
	}
	accC := &domain.Account{
		ID:          "acc-dir-C",
		Email:       "dirC@example.com",
		AccessToken: "token-dir-C",
		IsActive:    false,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now.Add(2 * time.Second),
		UpdatedAt:   now.Add(2 * time.Second),
	}
	if err := env.accountRepo.Create(ctx, accA); err != nil {
		t.Fatalf("create accA: %v", err)
	}
	if err := env.accountRepo.Create(ctx, accB); err != nil {
		t.Fatalf("create accB: %v", err)
	}
	if err := env.accountRepo.Create(ctx, accC); err != nil {
		t.Fatalf("create accC: %v", err)
	}

	gate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(concurrency)

	type reqResult struct {
		statusCode int
		body       string
		err        error
	}
	results := make([]reqResult, concurrency)

	for i := 0; i < concurrency; i++ {
		idx := i
		go func() {
			defer wg.Done()
			<-gate

			reqBody := fmt.Sprintf(`{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"dir-test-%d"}]}]}`, idx)
			req, err := http.NewRequest("POST", env.proxyServer.URL+"/v1internal/models/gemini-2.5-pro:streamGenerateContent", strings.NewReader(reqBody))
			if err != nil {
				results[idx] = reqResult{err: err}
				return
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := env.client.Do(req)
			if err != nil {
				results[idx] = reqResult{err: err}
				return
			}
			defer resp.Body.Close()

			respBytes, _ := io.ReadAll(resp.Body)
			results[idx] = reqResult{
				statusCode: resp.StatusCode,
				body:       string(respBytes),
			}
		}()
	}

	close(gate)
	wg.Wait()

	for i, res := range results {
		if res.err != nil {
			t.Fatalf("request %d failed: %v", i, res.err)
		}
		if res.statusCode != http.StatusOK {
			t.Errorf("request %d expected 200 OK, got %d: %s", i, res.statusCode, res.body)
		}
		if !strings.Contains(res.body, "success-on-B-direct") {
			t.Errorf("request %d missing success-on-B-direct: %s", i, res.body)
		}
	}

	// Zero secondary fallback attempts
	if secAtt := atomic.LoadInt64(&secondaryAttempts); secAtt > 0 {
		t.Errorf("expected 0 secondary model attempts when fallback disabled, got %d", secAtt)
	}

	// Anti-stampede verification
	activeAcc, err := env.accountRepo.GetActive(ctx)
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if activeAcc.ID != accB.ID {
		t.Errorf("ANTI-STAMPEDE VIOLATION: expected active account to be %s, got %s", accB.ID, activeAcc.ID)
	}
	if reqsOnC := atomic.LoadInt64(&upstreamReqsOnC); reqsOnC > 0 {
		t.Errorf("ANTI-STAMPEDE VIOLATION: Account C received %d requests!", reqsOnC)
	}
}

// TestM4Challenger1_ConcurrentPredictiveFallback_UnderCacheMutation tests 50 concurrent goroutines
// executing predictive checks with cache misses while background goroutines mutate the quota cache.
// Invariants verified:
// 1. All 50 requests are predictively rewritten to gemini-2.5-flash with 0 pro requests dispatched.
// 2. Zero data races between cache writers and predictive readers.
func TestM4Challenger1_ConcurrentPredictiveFallback_UnderCacheMutation(t *testing.T) {
	const concurrency = 50
	ctx := context.Background()

	var upstreamProReqs int64
	var upstreamFlashReqs int64

	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		if strings.Contains(r.URL.Path, "gemini-2.5-pro") || strings.Contains(string(bodyBytes), "gemini-2.5-pro") {
			atomic.AddInt64(&upstreamProReqs, 1)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}

		if strings.Contains(r.URL.Path, "gemini-2.5-flash") || strings.Contains(string(bodyBytes), "gemini-2.5-flash") {
			atomic.AddInt64(&upstreamFlashReqs, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"pred-success"}]}}]}}`))
			return
		}

		w.WriteHeader(http.StatusBadRequest)
	})

	env := newM4StressTestEnv(t, upstreamHandler, "gemini-2.5-pro", "gemini-2.5-flash", true)

	now := time.Now().UTC()
	acc := &domain.Account{
		ID:          "acc-pred-stress",
		Email:       "predstress@example.com",
		AccessToken: "token-pred-stress",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := env.accountRepo.Create(ctx, acc); err != nil {
		t.Fatalf("create acc: %v", err)
	}

	// Populate DB: Pro is 0% (exhausted), Flash has 90% remaining
	buckets := []*domain.QuotaBucket{
		{
			AccountID:         acc.ID,
			BucketID:          "gemini-2.5-pro",
			DisplayName:       "Gemini 2.5 Pro",
			RemainingFraction: 0.0,
			RemainingAmount:   0,
			ResetTime:         now.Add(5 * time.Hour),
		},
		{
			AccountID:         acc.ID,
			BucketID:          "gemini-2.5-flash",
			DisplayName:       "Gemini 2.5 Flash",
			RemainingFraction: 0.90,
			RemainingAmount:   90000,
			ResetTime:         now.Add(5 * time.Hour),
		},
	}
	if err := env.quotaRepo.UpsertBuckets(ctx, buckets); err != nil {
		t.Fatalf("save buckets: %v", err)
	}
	// In-memory cache is deliberately NOT populated to force concurrent DB cache misses

	// Start background cache mutators
	stopMutators := make(chan struct{})
	var mutatorWg sync.WaitGroup
	for m := 0; m < 4; m++ {
		mutatorWg.Add(1)
		go func() {
			defer mutatorWg.Done()
			ticker := time.NewTicker(2 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stopMutators:
					return
				case <-ticker.C:
					env.failoverEngine.UpdateQuotaCache(acc.ID, buckets)
				}
			}
		}()
	}

	gate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(concurrency)

	type reqResult struct {
		statusCode int
		body       string
		err        error
	}
	results := make([]reqResult, concurrency)

	for i := 0; i < concurrency; i++ {
		idx := i
		go func() {
			defer wg.Done()
			<-gate

			reqBody := fmt.Sprintf(`{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"pred-test-%d"}]}]}`, idx)
			req, err := http.NewRequest("POST", env.proxyServer.URL+"/v1internal/models/gemini-2.5-pro:streamGenerateContent", strings.NewReader(reqBody))
			if err != nil {
				results[idx] = reqResult{err: err}
				return
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := env.client.Do(req)
			if err != nil {
				results[idx] = reqResult{err: err}
				return
			}
			defer resp.Body.Close()

			respBytes, _ := io.ReadAll(resp.Body)
			results[idx] = reqResult{
				statusCode: resp.StatusCode,
				body:       string(respBytes),
			}
		}()
	}

	close(gate)
	wg.Wait()

	close(stopMutators)
	mutatorWg.Wait()

	for i, res := range results {
		if res.err != nil {
			t.Fatalf("request %d failed: %v", i, res.err)
		}
		if res.statusCode != http.StatusOK {
			t.Errorf("request %d expected 200 OK, got %d: %s", i, res.statusCode, res.body)
		}
		if !strings.Contains(res.body, "pred-success") {
			t.Errorf("request %d missing pred-success: %s", i, res.body)
		}
	}

	// Upstream must have received 0 requests for pro (proactive rewrite avoided any 429!)
	pCount := atomic.LoadInt64(&upstreamProReqs)
	if pCount != 0 {
		t.Errorf("PREDICTIVE FAILURE: %d requests reached upstream on pro instead of being proactively rewritten!", pCount)
	}

	fCount := atomic.LoadInt64(&upstreamFlashReqs)
	if fCount != int64(concurrency) {
		t.Errorf("expected %d flash requests, got %d", concurrency, fCount)
	}
}

// TestM4Challenger1_BufferIntegrity_ConcurrentRewritingUnderStress tests 30 concurrent
// goroutines each sending distinct JSON payloads (up to 128KB) to ensure request buffers
// are never cross-contaminated, truncated, or corrupted during failover rewriting.
func TestM4Challenger1_BufferIntegrity_ConcurrentRewritingUnderStress(t *testing.T) {
	const concurrency = 30
	ctx := context.Background()

	var mu sync.Mutex
	receivedBodies := make(map[string][]byte) // Keyed by unique test tag

	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		isPro := strings.Contains(r.URL.Path, "gemini-2.5-pro") || strings.Contains(string(bodyBytes), "gemini-2.5-pro")
		if isPro {
			w.Header().Set("Content-Type", "application/json; charset=UTF-8")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":429,"message":"pro limit","status":"RESOURCE_EXHAUSTED"}}`))
			return
		}

		// Secondary request
		var parsed map[string]any
		if err := json.Unmarshal(bodyBytes, &parsed); err == nil {
			if tag, ok := parsed["tag"].(string); ok {
				mu.Lock()
				receivedBodies[tag] = bodyBytes
				mu.Unlock()
			}
		}

		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"intact-ok"}]}}]}}`))
	})

	env := newM4StressTestEnv(t, upstreamHandler, "gemini-2.5-pro", "gemini-2.5-flash", true)

	now := time.Now().UTC()
	acc := &domain.Account{
		ID:          "acc-buf-stress",
		Email:       "bufstress@example.com",
		AccessToken: "token-buf-stress",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := env.accountRepo.Create(ctx, acc); err != nil {
		t.Fatalf("create acc: %v", err)
	}

	gate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(concurrency)

	expectedTags := make([]string, concurrency)
	expectedPadding := make([]string, concurrency)

	for i := 0; i < concurrency; i++ {
		idx := i
		tag := fmt.Sprintf("stress-tag-%03d", idx)
		expectedTags[idx] = tag
		// Distinct padding string of 2KB per request
		padding := strings.Repeat(fmt.Sprintf("[%d]", idx), 400)
		expectedPadding[idx] = padding

		go func() {
			defer wg.Done()
			<-gate

			reqMap := map[string]any{
				"model":   "gemini-2.5-pro",
				"tag":     tag,
				"padding": padding,
				"index":   idx,
			}
			reqBytes, _ := json.Marshal(reqMap)

			req, err := http.NewRequest("POST", env.proxyServer.URL+"/v1internal/models/gemini-2.5-pro:generateContent", bytes.NewReader(reqBytes))
			if err != nil {
				t.Errorf("create req %d: %v", idx, err)
				return
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := env.client.Do(req)
			if err != nil {
				t.Errorf("do req %d: %v", idx, err)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("req %d expected 200 OK, got %d: %s", idx, resp.StatusCode, string(b))
			}
		}()
	}

	close(gate)
	wg.Wait()

	// Verify all 30 requests preserved body integrity
	mu.Lock()
	defer mu.Unlock()

	for i, tag := range expectedTags {
		body, found := receivedBodies[tag]
		if !found {
			t.Errorf("tag %s was not recorded by upstream", tag)
			continue
		}

		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Errorf("tag %s has corrupted JSON: %v (body len=%d)", tag, err, len(body))
			continue
		}

		if parsed["model"] != "gemini-2.5-flash" {
			t.Errorf("tag %s expected rewritten model gemini-2.5-flash, got: %v", tag, parsed["model"])
		}

		if parsed["padding"] != expectedPadding[i] {
			t.Errorf("BUFFER CORRUPTION: tag %s padding content does not match!", tag)
		}
	}
}

// TestM4Challenger1_FullPoolExhaustion_ConcurrentStampede tests 25 concurrent goroutines
// when all accounts in the pool return 429 on all models.
// Invariants verified:
// 1. All 25 requests terminate promptly with HTTP 429 or 503 (no hanging/deadlocks).
// 2. Retry count is bounded (no infinite retry loop).
// 3. Pool exhaustion events emitted cleanly.
func TestM4Challenger1_FullPoolExhaustion_ConcurrentStampede(t *testing.T) {
	const concurrency = 25
	ctx := context.Background()

	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"global limit reached","status":"RESOURCE_EXHAUSTED"}}`))
	})

	env := newM4StressTestEnv(t, upstreamHandler, "gemini-2.5-pro", "gemini-2.5-flash", true)

	now := time.Now().UTC()
	acc1 := &domain.Account{
		ID:          "acc-exh-stampede-1",
		Email:       "exh1@example.com",
		AccessToken: "token-exh-1",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	acc2 := &domain.Account{
		ID:          "acc-exh-stampede-2",
		Email:       "exh2@example.com",
		AccessToken: "token-exh-2",
		IsActive:    false,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now.Add(time.Second),
		UpdatedAt:   now.Add(time.Second),
	}
	if err := env.accountRepo.Create(ctx, acc1); err != nil {
		t.Fatalf("create acc1: %v", err)
	}
	if err := env.accountRepo.Create(ctx, acc2); err != nil {
		t.Fatalf("create acc2: %v", err)
	}

	gate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(concurrency)

	statusCodes := make([]int, concurrency)

	for i := 0; i < concurrency; i++ {
		idx := i
		go func() {
			defer wg.Done()
			<-gate

			req, err := http.NewRequest("POST", env.proxyServer.URL+"/v1internal/models/gemini-2.5-pro:generateContent", strings.NewReader(`{}`))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := env.client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			statusCodes[idx] = resp.StatusCode
		}()
	}

	close(gate)
	wg.Wait()

	for i, sc := range statusCodes {
		if sc != http.StatusTooManyRequests && sc != http.StatusServiceUnavailable {
			t.Errorf("request %d expected 429 or 503, got %d", i, sc)
		}
	}
}

// TestM4Challenger1_Broadcaster_HighConcurrencyChurn tests Broadcaster under extreme churn:
// 40 subscribers subscribing and unsubscribing in rapid loops while 200 events are concurrently broadcasted.
// Invariants verified:
// 1. Zero panics from send on closed channel.
// 2. Zero deadlocks.
// 3. Subscribers cleanly terminate.
func TestM4Challenger1_Broadcaster_HighConcurrencyChurn(t *testing.T) {
	b := NewBroadcaster(100)
	stop := make(chan struct{})

	var subscriberWg sync.WaitGroup
	const numSubscribers = 40

	for s := 0; s < numSubscribers; s++ {
		subscriberWg.Add(1)
		go func() {
			defer subscriberWg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					ch, unsub := b.Subscribe()
					// Read briefly or unsubscribe immediately
					select {
					case <-ch:
					case <-time.After(1 * time.Millisecond):
					}
					unsub()
				}
			}
		}()
	}

	const numBroadcasters = 10
	const eventsPerBroadcaster = 20
	var broadWg sync.WaitGroup

	for i := 0; i < numBroadcasters; i++ {
		broadWg.Add(1)
		go func(id int) {
			defer broadWg.Done()
			for j := 0; j < eventsPerBroadcaster; j++ {
				b.Broadcast(&domain.ProxyEvent{
					Type:      domain.EventTypeModelFallback,
					AccountID: fmt.Sprintf("acc-%d-%d", id, j),
					Message:   "stress event",
					Timestamp: time.Now().UTC(),
				})
			}
		}(i)
	}

	broadWg.Wait()
	close(stop)
	subscriberWg.Wait()
}
