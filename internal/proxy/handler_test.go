package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/test/mocks"
)

func setupTestDB(t *testing.T) (*sqlite.DB, domain.AccountRepository, domain.MetricsRepository, domain.EventRepository) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to create sqlite db: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})

	accountRepo := sqlite.NewAccountRepository(db)
	metricsRepo := sqlite.NewMetricsRepository(db)
	eventRepo := sqlite.NewEventRepository(db)
	return db, accountRepo, metricsRepo, eventRepo
}

func TestProxyHandler_BasicForwarding(t *testing.T) {
	mockGoogle := mocks.NewMockGoogleServer()
	defer mockGoogle.Close()

	_, accountRepo, metricsRepo, _ := setupTestDB(t)

	now := time.Now().UTC()
	acc := &domain.Account{
		ID:          "acc-1",
		Email:       "user1@gmail.com",
		AccessToken: "token-1",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := accountRepo.Create(context.Background(), acc); err != nil {
		t.Fatalf("failed to create test account: %v", err)
	}

	broadcaster := NewBroadcaster(10)
	handler, err := NewProxyHandler(
		accountRepo,
		WithTargetURL(mockGoogle.URL),
		WithMetricsRepository(metricsRepo),
		WithEventBroadcaster(broadcaster),
	)
	if err != nil {
		t.Fatalf("failed to create proxy handler: %v", err)
	}

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	client := &http.Client{}
	reqBody := `{"prompt":{"messages":[{"content":"Hello World"}]}}`
	req, err := http.NewRequest("POST", proxyServer.URL+"/v1internal:streamGenerateContent?alt=sse", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("failed to create client request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "antigravity-ide/1.0")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}

	// Verify upstream received request correctly
	reqs := mockGoogle.GetRecordedRequests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 recorded upstream request, got %d", len(reqs))
	}

	recorded := reqs[0]
	if recorded.Path != "/v1internal:streamGenerateContent" {
		t.Errorf("expected path /v1internal:streamGenerateContent, got %s", recorded.Path)
	}
	if recorded.RawQuery != "alt=sse" {
		t.Errorf("expected raw query alt=sse, got %s", recorded.RawQuery)
	}
	if recorded.AuthBearer != "token-1" {
		t.Errorf("expected AuthBearer token-1, got %s", recorded.AuthBearer)
	}
	if string(recorded.Body) != reqBody {
		t.Errorf("body mismatch: got %s, want %s", string(recorded.Body), reqBody)
	}

	// Verify Host header was retargeted to mockGoogle host
	parsedMockURL, _ := url.Parse(mockGoogle.URL)
	if recorded.Header.Get("Host") != parsedMockURL.Host && recorded.Header.Get("X-Forwarded-Host") == "" {
		t.Errorf("expected host header or forwarded host to match upstream")
	}

	// Verify X-Forwarded-For is present
	if recorded.Header.Get("X-Forwarded-For") == "" {
		t.Errorf("expected X-Forwarded-For header to be set")
	}
}

func TestProxyHandler_HopByHopStripping(t *testing.T) {
	mockGoogle := mocks.NewMockGoogleServer()
	defer mockGoogle.Close()

	_, accountRepo, _, _ := setupTestDB(t)

	now := time.Now().UTC()
	acc := &domain.Account{
		ID:          "acc-hop",
		Email:       "hop@gmail.com",
		AccessToken: "token-hop",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_ = accountRepo.Create(context.Background(), acc)

	handler, err := NewProxyHandler(accountRepo, WithTargetURL(mockGoogle.URL))
	if err != nil {
		t.Fatalf("failed to create proxy handler: %v", err)
	}

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	client := &http.Client{}
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1internal:streamGenerateContent", strings.NewReader("{}"))
	req.Header.Set("Connection", "close, X-Custom-Hop")
	req.Header.Set("X-Custom-Hop", "strip-me")
	req.Header.Set("Proxy-Authorization", "Basic c2VjcmV0")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("Upgrade", "websocket")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	reqs := mockGoogle.GetRecordedRequests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 upstream request, got %d", len(reqs))
	}

	upHeader := reqs[0].Header
	if upHeader.Get("Proxy-Authorization") != "" {
		t.Errorf("Proxy-Authorization was not stripped")
	}
	if upHeader.Get("Keep-Alive") != "" {
		t.Errorf("Keep-Alive was not stripped")
	}
	if upHeader.Get("Upgrade") != "" {
		t.Errorf("Upgrade was not stripped")
	}
	if upHeader.Get("X-Custom-Hop") != "" {
		t.Errorf("custom hop header listed in Connection was not stripped")
	}
}

func TestProxyHandler_TransparentFailover429(t *testing.T) {
	mockGoogle := mocks.NewMockGoogleServer()
	defer mockGoogle.Close()

	_, accountRepo, _, _ := setupTestDB(t)

	now := time.Now().UTC()
	accA := &domain.Account{
		ID:          "acc-A",
		Email:       "accountA@gmail.com",
		AccessToken: "token-A",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now.Add(-10 * time.Minute),
		UpdatedAt:   now.Add(-10 * time.Minute),
	}
	accB := &domain.Account{
		ID:          "acc-B",
		Email:       "accountB@gmail.com",
		AccessToken: "token-B",
		IsActive:    false,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now.Add(-5 * time.Minute),
		UpdatedAt:   now.Add(-5 * time.Minute),
	}
	if err := accountRepo.Create(context.Background(), accA); err != nil {
		t.Fatalf("failed to create accA: %v", err)
	}
	if err := accountRepo.Create(context.Background(), accB); err != nil {
		t.Fatalf("failed to create accB: %v", err)
	}

	// Configure mock Google server to fail Account A with 429 on first attempt
	mockGoogle.SetFailoverTrigger("token-A", 1)

	broadcaster := NewBroadcaster(10)
	eventsCh, unsubscribe := broadcaster.Subscribe()
	defer unsubscribe()

	handler, err := NewProxyHandler(
		accountRepo,
		WithTargetURL(mockGoogle.URL),
		WithEventBroadcaster(broadcaster),
	)
	if err != nil {
		t.Fatalf("failed to create proxy handler: %v", err)
	}

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	client := &http.Client{}
	reqPayload := `{"prompt":"write a sorting algorithm in Go"}`
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1internal:streamGenerateContent?alt=sse", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// Client must receive transparent 200 OK!
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK transparent failover, got %d: %s", resp.StatusCode, string(b))
	}

	// Verify Account statuses in SQLite
	updatedA, _ := accountRepo.GetByID(context.Background(), "acc-A")
	if updatedA.Status != domain.AccountStatusExhausted {
		t.Errorf("expected acc-A status to be exhausted, got %s", updatedA.Status)
	}
	if updatedA.IsActive {
		t.Errorf("expected acc-A is_active to be false")
	}

	updatedB, _ := accountRepo.GetByID(context.Background(), "acc-B")
	if updatedB.Status != domain.AccountStatusActive {
		t.Errorf("expected acc-B status to be active, got %s", updatedB.Status)
	}
	if !updatedB.IsActive {
		t.Errorf("expected acc-B is_active to be true")
	}

	// Verify upstream requests: 2 attempts, both with identical payload
	reqs := mockGoogle.GetRecordedRequests()
	if len(reqs) != 2 {
		t.Fatalf("expected exactly 2 upstream requests (1 failed, 1 succeeded), got %d", len(reqs))
	}
	if reqs[0].AuthBearer != "token-A" {
		t.Errorf("expected 1st attempt with token-A, got %s", reqs[0].AuthBearer)
	}
	if reqs[1].AuthBearer != "token-B" {
		t.Errorf("expected 2nd attempt with token-B, got %s", reqs[1].AuthBearer)
	}
	if string(reqs[0].Body) != reqPayload || string(reqs[1].Body) != reqPayload {
		t.Errorf("body rewind failed: payloads not identical across retries")
	}

	// Verify event broadcasting received 429 failover event
	var sawFailover429 bool
	for i := 0; i < 3; i++ {
		select {
		case ev := <-eventsCh:
			if ev.Type == domain.EventTypeFailover429 {
				sawFailover429 = true
			}
		case <-time.After(500 * time.Millisecond):
		}
	}
	if !sawFailover429 {
		t.Errorf("expected EventTypeFailover429 to be broadcast")
	}
}

func TestProxyHandler_PoolExhaustion(t *testing.T) {
	mockGoogle := mocks.NewMockGoogleServer()
	defer mockGoogle.Close()

	_, accountRepo, _, _ := setupTestDB(t)

	now := time.Now().UTC()
	accA := &domain.Account{
		ID:          "acc-A",
		Email:       "a@example.com",
		AccessToken: "token-A",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	accB := &domain.Account{
		ID:          "acc-B",
		Email:       "b@example.com",
		AccessToken: "token-B",
		IsActive:    false,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_ = accountRepo.Create(context.Background(), accA)
	_ = accountRepo.Create(context.Background(), accB)

	// Both accounts hit 429
	mockGoogle.SetFailoverTrigger("token-A", 5)
	mockGoogle.SetFailoverTrigger("token-B", 5)

	handler, _ := NewProxyHandler(accountRepo, WithTargetURL(mockGoogle.URL))
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	client := &http.Client{}
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1internal:streamGenerateContent", strings.NewReader(`{}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// Client must receive HTTP 429 verbatim
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected HTTP 429 on pool exhaustion, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "RESOURCE_EXHAUSTED") {
		t.Errorf("expected upstream error body with RESOURCE_EXHAUSTED, got: %s", string(body))
	}

	// Both accounts must be marked exhausted
	a, _ := accountRepo.GetByID(context.Background(), "acc-A")
	b, _ := accountRepo.GetByID(context.Background(), "acc-B")
	if a.Status != domain.AccountStatusExhausted || b.Status != domain.AccountStatusExhausted {
		t.Errorf("expected both accounts exhausted, got A:%s B:%s", a.Status, b.Status)
	}
}

func TestProxyHandler_SSEStreamAndTokenCapture(t *testing.T) {
	mockGoogle := mocks.NewMockGoogleServer()
	defer mockGoogle.Close()

	_, accountRepo, metricsRepo, _ := setupTestDB(t)

	now := time.Now().UTC()
	acc := &domain.Account{
		ID:          "acc-sse",
		Email:       "sse@example.com",
		AccessToken: "token-sse",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_ = accountRepo.Create(context.Background(), acc)

	broadcaster := NewBroadcaster(10)
	eventsCh, unsubscribe := broadcaster.Subscribe()
	defer unsubscribe()

	handler, _ := NewProxyHandler(
		accountRepo,
		WithTargetURL(mockGoogle.URL),
		WithMetricsRepository(metricsRepo),
		WithEventBroadcaster(broadcaster),
	)

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	client := &http.Client{}
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1internal:streamGenerateContent?alt=sse", strings.NewReader("{}"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	// Read full SSE body
	sseBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(sseBody), "data: ") {
		t.Fatalf("expected SSE stream with data: chunks, got: %s", string(sseBody))
	}

	// Wait for background persistence in defer to complete
	var summary *domain.AggregatedMetrics
	for i := 0; i < 50; i++ {
		summary, _ = metricsRepo.GetSummary(context.Background(), "acc-sse", string(domain.PeriodLifetime))
		if summary != nil && summary.TotalRequests > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if summary == nil || summary.TotalRequests == 0 {
		t.Fatalf("metrics not recorded in SQLite")
	}

	// Default MockGoogleServer usageMetadata: prompt 125, candidates 42, total 167
	if summary.TotalPromptTokens != 125 {
		t.Errorf("expected 125 prompt tokens, got %d", summary.TotalPromptTokens)
	}
	if summary.TotalCandidatesTokens != 42 {
		t.Errorf("expected 42 candidates tokens, got %d", summary.TotalCandidatesTokens)
	}
	if summary.TotalTokens != 167 {
		t.Errorf("expected 167 total tokens, got %d", summary.TotalTokens)
	}

	// Check broadcast event
	select {
	case ev := <-eventsCh:
		if ev.Type != domain.EventTypeTokensCaptured {
			t.Errorf("expected EventTypeTokensCaptured, got %s", ev.Type)
		}
	case <-time.After(1 * time.Second):
		t.Error("timed out waiting for tokens captured event")
	}
}

func TestProxyHandler_BufferedRequest_Limits(t *testing.T) {
	mockGoogle := mocks.NewMockGoogleServer()
	defer mockGoogle.Close()

	_, accountRepo, _, _ := setupTestDB(t)

	now := time.Now().UTC()
	acc := &domain.Account{
		ID:          "acc-limit",
		Email:       "limit@example.com",
		AccessToken: "token-limit",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_ = accountRepo.Create(context.Background(), acc)

	// Set max body size to 1024 bytes (1KB)
	handler, _ := NewProxyHandler(accountRepo, WithTargetURL(mockGoogle.URL), WithMaxBodyBytes(1024))
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	client := &http.Client{}

	// 1. Normal payload (500 bytes) should succeed
	smallBody := strings.Repeat("A", 500)
	req1, _ := http.NewRequest("POST", proxyServer.URL+"/v1internal:streamGenerateContent", strings.NewReader(smallBody))
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("small request failed: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK for small body, got %d", resp1.StatusCode)
	}

	// 2. Oversized payload (2000 bytes > 1024 limit) should fail with 413
	largeBody := strings.Repeat("B", 2000)
	req2, _ := http.NewRequest("POST", proxyServer.URL+"/v1internal:streamGenerateContent", strings.NewReader(largeBody))
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("large request failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 Request Entity Too Large, got %d", resp2.StatusCode)
	}
}

func TestProxyHandler_NoAccountsAtAll(t *testing.T) {
	mockGoogle := mocks.NewMockGoogleServer()
	defer mockGoogle.Close()

	_, accountRepo, _, _ := setupTestDB(t)

	handler, _ := NewProxyHandler(accountRepo, WithTargetURL(mockGoogle.URL))
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	client := &http.Client{}
	req, _ := http.NewRequest("POST", proxyServer.URL+"/test", strings.NewReader("{}"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable, got %d", resp.StatusCode)
	}
}

func TestProxyHandler_AutoActivateAvailable(t *testing.T) {
	mockGoogle := mocks.NewMockGoogleServer()
	defer mockGoogle.Close()

	_, accountRepo, _, _ := setupTestDB(t)

	// Single account, not marked active
	now := time.Now().UTC()
	acc := &domain.Account{
		ID:          "acc-auto",
		Email:       "auto@example.com",
		AccessToken: "token-auto",
		IsActive:    false,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_ = accountRepo.Create(context.Background(), acc)

	handler, _ := NewProxyHandler(accountRepo, WithTargetURL(mockGoogle.URL))
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	client := &http.Client{}
	req, _ := http.NewRequest("POST", proxyServer.URL+"/v1internal:streamGenerateContent", strings.NewReader("{}"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	// Verify it was marked active
	active, _ := accountRepo.GetActive(context.Background())
	if active.ID != "acc-auto" {
		t.Errorf("expected acc-auto to be activated")
	}
}

func TestProxyHandler_ConcurrentRequests(t *testing.T) {
	mockGoogle := mocks.NewMockGoogleServer()
	defer mockGoogle.Close()

	_, accountRepo, metricsRepo, _ := setupTestDB(t)

	now := time.Now().UTC()
	acc := &domain.Account{
		ID:          "acc-conc",
		Email:       "conc@example.com",
		AccessToken: "token-conc",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_ = accountRepo.Create(context.Background(), acc)

	handler, _ := NewProxyHandler(
		accountRepo,
		WithTargetURL(mockGoogle.URL),
		WithMetricsRepository(metricsRepo),
	)

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	const concurrency = 20
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			client := &http.Client{}
			body := fmt.Sprintf(`{"req":%d}`, idx)
			req, _ := http.NewRequest("POST", proxyServer.URL+"/v1internal:streamGenerateContent?alt=sse", strings.NewReader(body))
			resp, err := client.Do(req)
			if err != nil {
				t.Errorf("concurrent request %d failed: %v", idx, err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("concurrent request %d got status %d", idx, resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()
}

func TestBuffer_UnitTests(t *testing.T) {
	// Test nil body
	reqNil, _ := http.NewRequest("GET", "http://example.com", nil)
	bufNil, err := NewBufferedRequest(reqNil)
	if err != nil {
		t.Fatalf("expected nil body success, got error: %v", err)
	}
	if bufNil.Size() != 0 || bufNil.Bytes() != nil {
		t.Errorf("expected empty buffer")
	}
	if bufNil.NewReader() != http.NoBody {
		t.Errorf("expected http.NoBody")
	}

	// Test populated body
	payload := []byte("hello buffered request")
	req, _ := http.NewRequest("POST", "http://example.com", bytes.NewReader(payload))
	buf, err := NewBufferedRequest(req)
	if err != nil {
		t.Fatalf("expected buffer success, got: %v", err)
	}
	if buf.Size() != len(payload) {
		t.Errorf("expected size %d, got %d", len(payload), buf.Size())
	}
	if string(buf.Bytes()) != string(payload) {
		t.Errorf("bytes mismatch")
	}

	// Read from NewReader
	r1 := buf.NewReader()
	b1, _ := io.ReadAll(r1)
	r1.Close()
	if string(b1) != string(payload) {
		t.Errorf("NewReader read 1 failed")
	}

	// Read again from NewReader
	r2 := buf.NewReader()
	b2, _ := io.ReadAll(r2)
	r2.Close()
	if string(b2) != string(payload) {
		t.Errorf("NewReader read 2 failed")
	}

	// Test ResetBody
	reqReset, _ := http.NewRequest("POST", "http://example.com", nil)
	buf.ResetBody(reqReset)
	bReset, _ := io.ReadAll(reqReset.Body)
	reqReset.Body.Close()
	if string(bReset) != string(payload) {
		t.Errorf("ResetBody read failed")
	}
}

func TestProxyHandler_ProactiveTokenRefresh(t *testing.T) {
	mockGoogle := mocks.NewMockGoogleServer()
	defer mockGoogle.Close()

	_, accountRepo, metricsRepo, _ := setupTestDB(t)

	// Create account with expired token
	acc := &domain.Account{
		ID:           "acc-expired",
		Email:        "expired@example.com",
		AccessToken:  "old-token",
		RefreshToken: "refresh-123",
		TokenExpiry:  time.Now().UTC().Add(-10 * time.Minute), // expired
		IsActive:     true,
		Status:       domain.AccountStatusActive,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := accountRepo.Create(context.Background(), acc); err != nil {
		t.Fatalf("create account: %v", err)
	}

	refreshCalled := false
	refresher := TokenRefresherFunc(func(ctx context.Context, rt string) (string, time.Time, error) {
		if rt != "refresh-123" {
			t.Errorf("unexpected refresh token: %s", rt)
		}
		refreshCalled = true
		return "fresh-token-proactive", time.Now().UTC().Add(1 * time.Hour), nil
	})

	handler, err := NewProxyHandler(
		accountRepo,
		WithTargetURL(mockGoogle.URL),
		WithMetricsRepository(metricsRepo),
		WithTokenRefresher(refresher),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler failed: %v", err)
	}

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	req, _ := http.NewRequest(http.MethodPost, proxyServer.URL+"/v1internal:streamGenerateContent?alt=sse", strings.NewReader(`{"test":1}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do request: %v", err)
	}
	defer resp.Body.Close()

	if !refreshCalled {
		t.Errorf("expected TokenRefresher to be called proactively for expired token")
	}

	reqs := mockGoogle.GetRecordedRequests()
	if len(reqs) == 0 {
		t.Fatalf("expected recorded request")
	}
	if reqs[0].AuthBearer != "fresh-token-proactive" {
		t.Errorf("expected Bearer fresh-token-proactive, got: %s", reqs[0].AuthBearer)
	}
}

func TestProxyHandler_Reactive401TokenRefresh(t *testing.T) {
	requestCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		auth := r.Header.Get("Authorization")
		if auth == "Bearer expired-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":401,"message":"Token expired"}}`))
			return
		}
		if auth == "Bearer newly-refreshed-token" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result":"success"}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer upstream.Close()

	_, accountRepo, metricsRepo, _ := setupTestDB(t)

	acc := &domain.Account{
		ID:           "acc-react",
		Email:        "react@example.com",
		AccessToken:  "expired-token",
		RefreshToken: "refresh-react",
		TokenExpiry:  time.Now().UTC().Add(10 * time.Minute), // not yet expired locally, but rejected upstream
		IsActive:     true,
		Status:       domain.AccountStatusActive,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := accountRepo.Create(context.Background(), acc); err != nil {
		t.Fatalf("create account: %v", err)
	}

	refreshed := false
	refresher := TokenRefresherFunc(func(ctx context.Context, rt string) (string, time.Time, error) {
		refreshed = true
		return "newly-refreshed-token", time.Now().UTC().Add(1 * time.Hour), nil
	})

	handler, err := NewProxyHandler(
		accountRepo,
		WithTargetURL(upstream.URL),
		WithMetricsRepository(metricsRepo),
		WithTokenRefresher(refresher),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler failed: %v", err)
	}

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	req, _ := http.NewRequest(http.MethodPost, proxyServer.URL+"/v1internal:streamGenerateContent", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected HTTP 200 after 401 retry, got %d: %s", resp.StatusCode, string(body))
	}
	if !refreshed {
		t.Errorf("expected TokenRefresher to be invoked after 401")
	}
	if requestCount != 2 {
		t.Errorf("expected 2 requests to upstream (401 + retry), got %d", requestCount)
	}

	// Verify updated token was saved in repository
	updatedAcc, _ := accountRepo.GetByID(context.Background(), "acc-react")
	if updatedAcc.AccessToken != "newly-refreshed-token" {
		t.Errorf("expected updated token saved to repo, got %s", updatedAcc.AccessToken)
	}
}

func TestProxyHandler_SubpathTargetURL(t *testing.T) {
	receivedPath := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	_, accountRepo, _, _ := setupTestDB(t)
	acc := &domain.Account{
		ID:          "acc-subpath",
		Email:       "subpath@example.com",
		AccessToken: "token-sub",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	_ = accountRepo.Create(context.Background(), acc)

	// Target URL has a subpath prefix
	targetWithPrefix := upstream.URL + "/custom/prefix"
	handler, err := NewProxyHandler(
		accountRepo,
		WithTargetURL(targetWithPrefix),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler failed: %v", err)
	}

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	req, _ := http.NewRequest(http.MethodPost, proxyServer.URL+"/v1internal:generateContent", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	expectedPath := "/custom/prefix/v1internal:generateContent"
	if receivedPath != expectedPath {
		t.Errorf("expected subpath preservation %q, got %q", expectedPath, receivedPath)
	}
}

func TestProxyHandler_ConnectTunneling(t *testing.T) {
	// 1. Start a local TCP echo server (simulates upstream TLS server like speech.googleapis.com)
	echoListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer echoListener.Close()

	go func() {
		for {
			conn, err := echoListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	_, accountRepo, _, _ := setupTestDB(t)
	handler, err := NewProxyHandler(accountRepo)
	if err != nil {
		t.Fatalf("NewProxyHandler: %v", err)
	}

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	// 2. Connect to the proxy via raw TCP and send HTTP CONNECT
	proxyConn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial proxy: %v", err)
	}
	defer proxyConn.Close()

	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoListener.Addr().String(), echoListener.Addr().String())
	if _, err := proxyConn.Write([]byte(connectReq)); err != nil {
		t.Fatalf("Write CONNECT: %v", err)
	}

	// 3. Read proxy response - MUST be "HTTP/1.1 200 Connection Established\r\n\r\n"
	buf := make([]byte, 1024)
	n, err := proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("Read CONNECT response: %v", err)
	}

	respStr := string(buf[:n])
	if !strings.HasPrefix(respStr, "HTTP/1.1 200 Connection Established\r\n\r\n") {
		t.Fatalf("expected pure RFC 7231 '200 Connection Established', got: %q", respStr)
	}

	// 4. Test bidirectional data transfer through the established tunnel
	testPayload := []byte("antigravity-voice-audio-stream-chunk-data")
	if _, err := proxyConn.Write(testPayload); err != nil {
		t.Fatalf("Write payload through tunnel: %v", err)
	}

	replyBuf := make([]byte, len(testPayload))
	if _, err := io.ReadFull(proxyConn, replyBuf); err != nil {
		t.Fatalf("ReadFull through tunnel: %v", err)
	}

	if !bytes.Equal(replyBuf, testPayload) {
		t.Fatalf("expected payload %q, got %q", string(testPayload), string(replyBuf))
	}
}
