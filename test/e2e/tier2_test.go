package e2e

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/test/mocks"
)

// TestTier2_Transparent429Failover validates that when the upstream Google server returns
// HTTP 429 RESOURCE_EXHAUSTED for Account A, the proxy transparently rotates to Account B
// and completes the request with HTTP 200 OK without client error.
func TestTier2_Transparent429Failover(t *testing.T) {
	env := setupE2EEnvironment(t, 0)
	ctx := context.Background()

	now := time.Now().UTC()
	// Create Account A (active)
	accA := &domain.Account{
		ID:          "acc-alpha",
		Email:       "alpha@example.com",
		AccessToken: "token-alpha",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	// Create Account B (healthy standby)
	accB := &domain.Account{
		ID:          "acc-beta",
		Email:       "beta@example.com",
		AccessToken: "token-beta",
		IsActive:    false,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := env.AccountRepo.Create(ctx, accA); err != nil {
		t.Fatalf("create accA: %v", err)
	}
	if err := env.AccountRepo.Create(ctx, accB); err != nil {
		t.Fatalf("create accB: %v", err)
	}

	// Configure mock Google server:
	// token-alpha returns 429 RESOURCE_EXHAUSTED on its next request
	env.MockGoogle.SetFailoverTrigger("token-alpha", 1)
	// token-beta returns 200 OK
	env.MockGoogle.ConfigureAccount("token-beta", &mocks.AccountBehavior{
		Email:             "beta@example.com",
		FailoverRemaining: 0,
	})

	// Client sends request to the local switcher proxy
	reqBody := `{"contents":[{"role":"user","parts":[{"text":"Explain transparent failover"}]}]}`
	req, err := http.NewRequest(http.MethodPost, env.ServerURL+"/v1internal:generateContent", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client request failed: %v", err)
	}
	defer resp.Body.Close()

	// ASSERTION 1: Transparent 200 OK received by client
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected HTTP 200 OK from transparent failover, got %d: %s", resp.StatusCode, string(body))
	}

	// ASSERTION 2: Account A marked exhausted in SQLite
	updatedA, err := env.AccountRepo.GetByID(ctx, "acc-alpha")
	if err != nil {
		t.Fatalf("get accA: %v", err)
	}
	if updatedA.Status != domain.AccountStatusExhausted {
		t.Errorf("expected Account A status to be 'exhausted', got %q", updatedA.Status)
	}

	// ASSERTION 3: Account B marked active in SQLite
	active, err := env.AccountRepo.GetActive(ctx)
	if err != nil {
		t.Fatalf("get active account: %v", err)
	}
	if active.ID != "acc-beta" {
		t.Errorf("expected Account B ('acc-beta') to be active, got %s (%s)", active.Email, active.ID)
	}

	// ASSERTION 4: Upstream received requests with appropriate Bearer tokens
	recorded := env.MockGoogle.GetRecordedRequests()
	foundAlphaAttempt := false
	foundBetaAttempt := false
	for _, r := range recorded {
		if r.AuthBearer == "token-alpha" {
			foundAlphaAttempt = true
		}
		if r.AuthBearer == "token-beta" {
			foundBetaAttempt = true
		}
	}
	if !foundAlphaAttempt {
		t.Errorf("expected upstream request with Bearer token-alpha")
	}
	if !foundBetaAttempt {
		t.Errorf("expected retried upstream request with Bearer token-beta")
	}
}

// TestTier2_SSETokenCapture validates that the proxy interceptor parses real SSE streams
// and extracts promptTokenCount, candidatesTokenCount, and cachedContentTokenCount to SQLite.
func TestTier2_SSETokenCapture(t *testing.T) {
	env := setupE2EEnvironment(t, 0)
	ctx := context.Background()

	now := time.Now().UTC()
	acc := &domain.Account{
		ID:          "acc-sse-user",
		Email:       "sse_user@example.com",
		AccessToken: "token-sse",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := env.AccountRepo.Create(ctx, acc); err != nil {
		t.Fatalf("create account: %v", err)
	}

	// Configure mock Google PA with custom token metrics in SSE chunk
	env.MockGoogle.ConfigureAccount("token-sse", &mocks.AccountBehavior{
		Email: "sse_user@example.com",
		Usage: &mocks.UsageMetadata{
			PromptTokenCount:        350,
			CandidatesTokenCount:    120,
			TotalTokenCount:         470,
			CachedContentTokenCount: 50,
		},
	})

	req, err := http.NewRequest(
		http.MethodPost,
		env.ServerURL+"/v1internal:streamGenerateContent?alt=sse",
		strings.NewReader(`{"contents":[{"parts":[{"text":"Stream test"}]}]}`),
	)
	if err != nil {
		t.Fatalf("create sse req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("SSE request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	// Consume entire SSE stream
	scanner := bufio.NewScanner(resp.Body)
	receivedChunks := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			receivedChunks++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning SSE stream: %v", err)
	}
	if receivedChunks == 0 {
		t.Fatalf("expected SSE chunks, received 0")
	}

	// Verify metrics recorded in SQLite (persisted asynchronously in StreamAndInterceptSSE)
	var summary *domain.AggregatedMetrics
	for i := 0; i < 40; i++ {
		summary, err = env.MetricsService.GetSummary(ctx, "acc-sse-user", domain.PeriodLifetime)
		if err == nil && summary != nil && summary.TotalRequests > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("get metrics summary: %v", err)
	}

	if summary.TotalPromptTokens != 350 {
		t.Errorf("expected 350 prompt tokens, got %d", summary.TotalPromptTokens)
	}
	if summary.TotalCandidatesTokens != 120 {
		t.Errorf("expected 120 candidate tokens, got %d", summary.TotalCandidatesTokens)
	}
	if summary.TotalTokens != 470 {
		t.Errorf("expected 470 total tokens, got %d", summary.TotalTokens)
	}
	if summary.TotalCachedContentTokens != 50 {
		t.Errorf("expected 50 cached content tokens, got %d", summary.TotalCachedContentTokens)
	}
	if summary.TotalRequests != 1 {
		t.Errorf("expected 1 request recorded, got %d", summary.TotalRequests)
	}
}
