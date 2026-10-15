package proxy_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/proxy"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
)

func TestProxy_ModelScopedQuotaIsolation_ClaudeExhaustsAndRotates_GeminiStaysOnInitialAccount(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "proxy_iso_test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	accRepo := sqlite.NewAccountRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)
	broadcaster := proxy.NewBroadcaster(100)

	// Setup two accounts
	accA := &domain.Account{
		ID:          "acc-A",
		Email:       "alice@example.com",
		AccessToken: "token-alice",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		UpdatedAt:   time.Now().UTC().Add(-10 * time.Minute),
	}
	accB := &domain.Account{
		ID:          "acc-B",
		Email:       "bob@example.com",
		AccessToken: "token-bob",
		IsActive:    false,
		Status:      domain.AccountStatusActive,
		UpdatedAt:   time.Now().UTC().Add(-5 * time.Minute),
	}
	if err := accRepo.Create(ctx, accA); err != nil {
		t.Fatalf("Create accA: %v", err)
	}
	if err := accRepo.Create(ctx, accB); err != nil {
		t.Fatalf("Create accB: %v", err)
	}
	if err := accRepo.SetActive(ctx, "acc-A"); err != nil {
		t.Fatalf("SetActive acc-A: %v", err)
	}

	failoverEngine := proxy.NewFailoverEngine(
		accRepo,
		broadcaster,
		nil,
		proxy.WithQuotaRepository(quotaRepo),
		proxy.WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", false),
	)

	// Mock upstream server:
	// If Authorization is token-alice and requested model is claude, return 429
	// If Authorization is token-bob and requested model is claude, return 200
	// If requested model is gemini on token-alice, return 200
	var reqTokensMu sync.Mutex
	var receivedTokens []string

	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqTokensMu.Lock()
		auth := r.Header.Get("Authorization")
		receivedTokens = append(receivedTokens, auth)
		reqTokensMu.Unlock()

		body, _ := io.ReadAll(r.Body)
		model, _, _ := proxy.ExtractModelFromRequest(r, body)

		if model == "claude-3-7-sonnet" {
			if auth == "Bearer token-alice" {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED"}}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"response":"claude ok on bob"}`))
			return
		}

		if model == "gemini-2.5-pro" {
			if auth == "Bearer token-alice" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"response":"gemini ok on alice"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"response":"gemini ok on bob"}`))
			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer mockUpstream.Close()

	handler, err := proxy.NewProxyHandler(
		accRepo,
		proxy.WithTargetURL(mockUpstream.URL),
		proxy.WithFailoverEngine(failoverEngine),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler: %v", err)
	}

	// Request 1: Claude request arrives. Alice encounters 429, rotates to Bob, retry succeeds on Bob.
	claudeReqBody := []byte(`{"model":"claude-3-7-sonnet","prompt":"hello"}`)
	req1 := httptest.NewRequest(http.MethodPost, "/v1internal:generateContent", bytes.NewReader(claudeReqBody))
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()

	handler.ServeHTTP(w1, req1)

	if w1.Code != http.StatusOK {
		t.Fatalf("expected request 1 status 200 OK, got %d: %s", w1.Code, w1.Body.String())
	}

	// Verify Claude pointer rotated to acc-B
	claudeActive, err := accRepo.GetActiveForCategory(ctx, domain.CategoryClaudeGPT)
	if err != nil {
		t.Fatalf("GetActiveForCategory(Claude): %v", err)
	}
	if claudeActive.ID != "acc-B" {
		t.Errorf("expected Claude active account to be acc-B, got %s", claudeActive.ID)
	}

	// CRITICAL CHECK: Gemini pointer MUST REMAIN ON acc-A!
	geminiActive, err := accRepo.GetActiveForCategory(ctx, domain.CategoryGemini)
	if err != nil {
		t.Fatalf("GetActiveForCategory(Gemini): %v", err)
	}
	if geminiActive.ID != "acc-A" {
		t.Fatalf("CRITICAL BUG: Gemini active account was corrupted! Expected acc-A, got %s", geminiActive.ID)
	}

	// Request 2: Gemini request arrives. Must route to Alice (acc-A), NOT Bob!
	reqTokensMu.Lock()
	receivedTokens = nil // Reset tokens
	reqTokensMu.Unlock()

	geminiReqBody := []byte(`{"model":"gemini-2.5-pro","prompt":"hello"}`)
	req2 := httptest.NewRequest(http.MethodPost, "/v1internal:generateContent", bytes.NewReader(geminiReqBody))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()

	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected request 2 status 200 OK, got %d: %s", w2.Code, w2.Body.String())
	}

	reqTokensMu.Lock()
	defer reqTokensMu.Unlock()
	if len(receivedTokens) != 1 {
		t.Fatalf("expected 1 token received for Gemini request, got %d: %v", len(receivedTokens), receivedTokens)
	}
	if receivedTokens[0] != "Bearer token-alice" {
		t.Errorf("CRITICAL BUG: Gemini request used wrong token! Expected 'Bearer token-alice', got %s", receivedTokens[0])
	}

	// Verify Alice's active pointer for Gemini is completely unaffected by Claude exhaustion
	geminiActiveAfter, err := accRepo.GetActiveForCategory(ctx, domain.CategoryGemini)
	if err != nil {
		t.Fatalf("GetActiveForCategory(Gemini): %v", err)
	}
	if geminiActiveAfter.ID != "acc-A" {
		t.Fatalf("expected Gemini active pointer to remain acc-A, got %s", geminiActiveAfter.ID)
	}
}

func TestProxy_ModelScopedQuotaIsolation_ConcurrentSessions_NoCrossInterference(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "proxy_concurrent_test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	accRepo := sqlite.NewAccountRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)
	broadcaster := proxy.NewBroadcaster(100)

	accA := &domain.Account{
		ID:          "acc-A",
		Email:       "alice@example.com",
		AccessToken: "token-alice",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		UpdatedAt:   time.Now().UTC().Add(-10 * time.Minute),
	}
	accB := &domain.Account{
		ID:          "acc-B",
		Email:       "bob@example.com",
		AccessToken: "token-bob",
		IsActive:    false,
		Status:      domain.AccountStatusActive,
		UpdatedAt:   time.Now().UTC().Add(-5 * time.Minute),
	}
	if err := accRepo.Create(ctx, accA); err != nil {
		t.Fatalf("Create accA: %v", err)
	}
	if err := accRepo.Create(ctx, accB); err != nil {
		t.Fatalf("Create accB: %v", err)
	}
	if err := accRepo.SetActive(ctx, "acc-A"); err != nil {
		t.Fatalf("SetActive acc-A: %v", err)
	}

	failoverEngine := proxy.NewFailoverEngine(
		accRepo,
		broadcaster,
		nil,
		proxy.WithQuotaRepository(quotaRepo),
		proxy.WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", false),
	)

	// Mock server: Alice 429s on Opus, but succeeds on Gemini
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		model, _, _ := proxy.ExtractModelFromRequest(r, body)

		if model == "claude-opus-4-6-thinking" {
			if auth == "Bearer token-alice" {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"code":429,"message":"rate limit","status":"RESOURCE_EXHAUSTED"}}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"content":"opus on bob"}`))
			return
		}

		if model == "gemini-3.8-flash-high" {
			// Gemini works fine on Alice!
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"content":"gemini on ` + auth + `"}`))
			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer mockUpstream.Close()

	handler, err := proxy.NewProxyHandler(
		accRepo,
		proxy.WithTargetURL(mockUpstream.URL),
		proxy.WithFailoverEngine(failoverEngine),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler: %v", err)
	}

	// Run concurrent requests: Session 1 (Opus) and Session 2 (Gemini)
	const concurrentRounds = 20
	var wg sync.WaitGroup
	errCh := make(chan error, concurrentRounds*2)

	for i := 0; i < concurrentRounds; i++ {
		wg.Add(2)

		// Session 1: Opus
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1internal:generateContent", bytes.NewReader([]byte(`{"model":"claude-opus-4-6-thinking"}`)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				errCh <- fmt.Errorf("unexpected status %d: %s", w.Code, w.Body.String())
			}
		}()

		// Session 2: Gemini
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1internal:generateContent", bytes.NewReader([]byte(`{"model":"gemini-3.8-flash-high"}`)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				errCh <- fmt.Errorf("unexpected status %d: %s", w.Code, w.Body.String())
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for e := range errCh {
		t.Errorf("request failed with status: %v", e)
	}

	// Verify pointers after concurrent sessions
	claudeActive, err := accRepo.GetActiveForCategory(ctx, domain.CategoryClaudeGPT)
	if err != nil {
		t.Fatalf("GetActiveForCategory Claude: %v", err)
	}
	geminiActive, err := accRepo.GetActiveForCategory(ctx, domain.CategoryGemini)
	if err != nil {
		t.Fatalf("GetActiveForCategory Gemini: %v", err)
	}

	if claudeActive.ID != "acc-B" {
		t.Errorf("expected Claude active on acc-B, got %s", claudeActive.ID)
	}
	if geminiActive.ID != "acc-A" {
		t.Errorf("expected Gemini active on acc-A, got %s", geminiActive.ID)
	}
}

func TestProxy_ModelScopedQuotaIsolation_FallbackEnabled_ClaudeDoesNotFallbackToGeminiFlash(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "proxy_fallback_iso_test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	accRepo := sqlite.NewAccountRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)
	broadcaster := proxy.NewBroadcaster(100)

	accA := &domain.Account{
		ID:          "acc-A",
		Email:       "alice@example.com",
		AccessToken: "token-alice",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		UpdatedAt:   time.Now().UTC().Add(-10 * time.Minute),
	}
	accB := &domain.Account{
		ID:          "acc-B",
		Email:       "bob@example.com",
		AccessToken: "token-bob",
		IsActive:    false,
		Status:      domain.AccountStatusActive,
		UpdatedAt:   time.Now().UTC().Add(-5 * time.Minute),
	}
	if err := accRepo.Create(ctx, accA); err != nil {
		t.Fatalf("Create accA: %v", err)
	}
	if err := accRepo.Create(ctx, accB); err != nil {
		t.Fatalf("Create accB: %v", err)
	}
	if err := accRepo.SetActive(ctx, "acc-A"); err != nil {
		t.Fatalf("SetActive acc-A: %v", err)
	}

	// Enable model fallback: primary = gemini-2.5-pro, secondary = gemini-2.5-flash
	failoverEngine := proxy.NewFailoverEngine(
		accRepo,
		broadcaster,
		nil,
		proxy.WithQuotaRepository(quotaRepo),
		proxy.WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", true),
	)

	var reqTokensMu sync.Mutex
	var receivedRequests []string

	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqTokensMu.Lock()
		auth := r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		model, _, _ := proxy.ExtractModelFromRequest(r, body)
		receivedRequests = append(receivedRequests, fmt.Sprintf("%s:%s", auth, model))
		reqTokensMu.Unlock()

		if model == "claude-3-7-sonnet" {
			if auth == "Bearer token-alice" {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED"}}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"response":"claude ok on bob"}`))
			return
		}

		if model == "gemini-2.5-flash" {
			t.Errorf("FATAL BUG: Claude request was rewritten to gemini-2.5-flash!")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"response":"flash fallback"}`))
			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer mockUpstream.Close()

	handler, err := proxy.NewProxyHandler(
		accRepo,
		proxy.WithTargetURL(mockUpstream.URL),
		proxy.WithFailoverEngine(failoverEngine),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler: %v", err)
	}

	// Send Claude request that 429s on Alice
	claudeReqBody := []byte(`{"model":"claude-3-7-sonnet","prompt":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1internal:generateContent", bytes.NewReader(claudeReqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK after rotation to Bob, got %d: %s", w.Code, w.Body.String())
	}

	reqTokensMu.Lock()
	defer reqTokensMu.Unlock()
	if len(receivedRequests) != 2 {
		t.Fatalf("expected 2 requests (Alice fail + Bob success), got %d: %v", len(receivedRequests), receivedRequests)
	}
	if receivedRequests[0] != "Bearer token-alice:claude-3-7-sonnet" {
		t.Errorf("expected first attempt on Alice for claude-3-7-sonnet, got %s", receivedRequests[0])
	}
	if receivedRequests[1] != "Bearer token-bob:claude-3-7-sonnet" {
		t.Errorf("expected second attempt on Bob for claude-3-7-sonnet, got %s", receivedRequests[1])
	}

	// Verify Claude pointer rotated to Bob, Gemini remains on Alice
	claudeActive, err := accRepo.GetActiveForCategory(ctx, domain.CategoryClaudeGPT)
	if err != nil {
		t.Fatalf("GetActiveForCategory Claude: %v", err)
	}
	if claudeActive.ID != "acc-B" {
		t.Errorf("expected Claude active acc-B, got %s", claudeActive.ID)
	}

	geminiActive, err := accRepo.GetActiveForCategory(ctx, domain.CategoryGemini)
	if err != nil {
		t.Fatalf("GetActiveForCategory Gemini: %v", err)
	}
	if geminiActive.ID != "acc-A" {
		t.Errorf("expected Gemini active acc-A, got %s", geminiActive.ID)
	}
}
