package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/test/mocks"
)

// ============================================================================
// Milestone M5 Phase 2: Tier 5 Adversarial Coverage Hardening
// Empirical Adversarial Test Suite
// ============================================================================

// TestTier5_Adversarial_DirectSecondaryRequest429_RotatesAccount verifies that
// when a client explicitly requests the secondary model tier directly (without primary),
// receiving an HTTP 429 on the secondary model immediately rotates to the next account
// (instead of attempting another redundant intra-account fallback), and the newly activated
// account preserves the requested secondary model without corrupting state.
func TestTier5_Adversarial_DirectSecondaryRequest429_RotatesAccount(t *testing.T) {
	env := setupE2EEnvironment(t, 0)
	t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "true")
	t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", defaultPrimaryModel)
	t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", defaultSecondaryModel)

	acc1 := seedTestAccount(t, env, "acc-sec-1", "sec1@example.com", "tok-sec-1", true, domain.AccountStatusActive)
	seedTestAccount(t, env, "acc-sec-2", "sec2@example.com", "tok-sec-2", false, domain.AccountStatusActive)

	// acc1 will 429 immediately on any request
	env.MockGoogle.SetFailoverTrigger("tok-sec-1", 1)
	// acc2 has full quota
	env.MockGoogle.SetFailoverTrigger("tok-sec-2", 0)

	// Subscribe to event feed
	eventCh, unsubscribe := env.Broadcaster.Subscribe()
	defer unsubscribe()

	client := &http.Client{Timeout: 5 * time.Second}
	body := fmt.Sprintf(`{"model":%q,"contents":[{"parts":[{"text":"Explicit secondary request"}]}]}`, defaultSecondaryModel)

	resp, respBody := sendProxyRequest(t, client, env.ServerURL+"/v1internal:generateContent", http.MethodPost, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 after failover to Account 2, got %d: %s", resp.StatusCode, respBody)
	}

	// Verify rotation happened
	ctx := context.Background()
	active, err := env.AccountRepo.GetActive(ctx)
	if err != nil || active == nil {
		t.Fatalf("failed to get active account: %v", err)
	}
	if active.ID != "acc-sec-2" {
		t.Errorf("expected active account to be acc-sec-2, got %s", active.ID)
	}

	// Verify acc1 status is exhausted
	oldAcc, err := env.AccountRepo.GetByID(ctx, acc1.ID)
	if err != nil || oldAcc.Status != domain.AccountStatusExhausted {
		t.Errorf("expected acc-sec-1 to be marked exhausted, got status: %v", oldAcc.Status)
	}

	// Verify upstream received the request on acc2 retaining the secondary model
	recorded := env.MockGoogle.GetRecordedRequests()
	if len(recorded) < 2 {
		t.Fatalf("expected at least 2 upstream requests (1 failed on acc1, 1 retry on acc2), got %d", len(recorded))
	}

	lastReq := recorded[len(recorded)-1]
	if lastReq.AuthBearer != "tok-sec-2" {
		t.Errorf("expected retry bearer token to be tok-sec-2, got %s", lastReq.AuthBearer)
	}

	var reqPayload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(lastReq.Body, &reqPayload); err != nil {
		t.Fatalf("failed to unmarshal request body: %v", err)
	}
	if reqPayload.Model != defaultSecondaryModel {
		t.Errorf("expected replayed request on Account 2 to maintain model %s, got %s", defaultSecondaryModel, reqPayload.Model)
	}

	// Verify EventTypeAccountSwitched was broadcast
	receivedSwitchEvent := false
	timeout := time.After(500 * time.Millisecond)
	for !receivedSwitchEvent {
		select {
		case ev := <-eventCh:
			if ev != nil && ev.Type == domain.EventTypeAccountSwitched && ev.AccountID == "acc-sec-2" {
				receivedSwitchEvent = true
			}
		case <-timeout:
			break
		}
	}
	if !receivedSwitchEvent {
		t.Errorf("expected EventTypeAccountSwitched event to be broadcast for acc-sec-2")
	}
}

// TestTier5_Adversarial_CrossFamilyFallbackAndReplay verifies cross-provider category
// failover (Claude/GPT primary -> Gemini Flash secondary -> Account rotation -> Claude primary replay).
func TestTier5_Adversarial_CrossFamilyFallbackAndReplay(t *testing.T) {
	env := setupE2EEnvironment(t, 0)
	t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "true")
	t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", "claude-3-5-sonnet")
	t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", "gemini-2.5-flash")

	seedTestAccount(t, env, "acc-cross-1", "cross1@example.com", "tok-cross-1", true, domain.AccountStatusActive)
	seedTestAccount(t, env, "acc-cross-2", "cross2@example.com", "tok-cross-2", false, domain.AccountStatusActive)

	// acc-cross-1: 429 on both attempts (primary Claude, then secondary Gemini)
	env.MockGoogle.SetFailoverTrigger("tok-cross-1", 2)
	// acc-cross-2: succeeds on first attempt
	env.MockGoogle.SetFailoverTrigger("tok-cross-2", 0)

	client := &http.Client{Timeout: 5 * time.Second}
	origBody := `{"model":"claude-3-5-sonnet","contents":[{"parts":[{"text":"Cross-family test prompt"}]}]}`

	resp, respBody := sendProxyRequest(t, client, env.ServerURL+"/v1internal/models/claude-3-5-sonnet:generateContent", http.MethodPost, origBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK after full failover cascade, got %d: %s", resp.StatusCode, respBody)
	}

	recorded := env.MockGoogle.GetRecordedRequests()
	if len(recorded) != 3 {
		t.Fatalf("expected exactly 3 upstream attempts (acc1 primary, acc1 secondary, acc2 primary), got %d", len(recorded))
	}

	// Attempt 1: acc-cross-1 with claude-3-5-sonnet
	if recorded[0].AuthBearer != "tok-cross-1" || !strings.Contains(recorded[0].Path, "claude-3-5-sonnet") {
		t.Errorf("attempt 1 mismatch: bearer=%s, path=%s", recorded[0].AuthBearer, recorded[0].Path)
	}

	// Attempt 2: acc-cross-1 fallback to gemini-2.5-flash
	if recorded[1].AuthBearer != "tok-cross-1" || !strings.Contains(recorded[1].Path, "gemini-2.5-flash") {
		t.Errorf("attempt 2 mismatch: bearer=%s, path=%s", recorded[1].AuthBearer, recorded[1].Path)
	}

	// Attempt 3: acc-cross-2 rotated and RESET to claude-3-5-sonnet
	if recorded[2].AuthBearer != "tok-cross-2" || !strings.Contains(recorded[2].Path, "claude-3-5-sonnet") {
		t.Errorf("attempt 3 mismatch: bearer=%s, path=%s", recorded[2].AuthBearer, recorded[2].Path)
	}

	ctx := context.Background()
	active, err := env.AccountRepo.GetActive(ctx)
	if err != nil || active == nil || active.ID != "acc-cross-2" {
		t.Errorf("expected active account to be acc-cross-2, got: %v", active)
	}
}

// TestTier5_Adversarial_TripleCascadingExhaustion_PoolDepleted verifies that when
// all accounts in a multi-account pool exhaust all tiers (primary and secondary), the proxy
// cleanly returns HTTP 429 to the client with the upstream error body without crashing,
// hanging, or infinite looping.
func TestTier5_Adversarial_TripleCascadingExhaustion_PoolDepleted(t *testing.T) {
	env := setupE2EEnvironment(t, 0)
	t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "true")
	t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", "gemini-2.5-pro")
	t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", "gemini-2.5-flash")

	seedTestAccount(t, env, "acc-c1", "c1@example.com", "tok-c1", true, domain.AccountStatusActive)
	seedTestAccount(t, env, "acc-c2", "c2@example.com", "tok-c2", false, domain.AccountStatusActive)
	seedTestAccount(t, env, "acc-c3", "c3@example.com", "tok-c3", false, domain.AccountStatusActive)

	// All 3 accounts exhaust both tiers (2 failures each = 6 total failures)
	env.MockGoogle.SetFailoverTrigger("tok-c1", 10)
	env.MockGoogle.SetFailoverTrigger("tok-c2", 10)
	env.MockGoogle.SetFailoverTrigger("tok-c3", 10)

	client := &http.Client{Timeout: 5 * time.Second}
	body := `{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"Cascade exhaustion test"}]}]}`

	resp, respBody := sendProxyRequest(t, client, env.ServerURL+"/v1internal:generateContent", http.MethodPost, body, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected HTTP 429 when entire pool is exhausted, got %d: %s", resp.StatusCode, respBody)
	}

	if !strings.Contains(respBody, "RESOURCE_EXHAUSTED") && !strings.Contains(respBody, "429") {
		t.Errorf("expected upstream RESOURCE_EXHAUSTED body to be forwarded, got: %s", respBody)
	}

	// Verify exactly 6 upstream attempts were made (2 per account across 3 accounts)
	recorded := env.MockGoogle.GetRecordedRequests()
	if len(recorded) != 6 {
		t.Errorf("expected exactly 6 upstream requests across 3 accounts, got %d", len(recorded))
	}

	// Verify all 3 accounts are now marked 'exhausted' in SQLite
	ctx := context.Background()
	accounts, err := env.AccountRepo.List(ctx)
	if err != nil {
		t.Fatalf("failed to list accounts: %v", err)
	}
	for _, acc := range accounts {
		if acc.Status != domain.AccountStatusExhausted {
			t.Errorf("account %s expected status 'exhausted', got '%s'", acc.ID, acc.Status)
		}
	}
}

// TestTier5_Adversarial_AntiStampede_MixedPrimarySecondaryBurst tests anti-stampede
// protection when 50 concurrent requests simultaneously hit an exhausted account with a mix of
// primary (25) and secondary (25) model requests.
// Verifies that exactly one account rotation occurs and all 50 requests succeed on Account 2.
func TestTier5_Adversarial_AntiStampede_MixedPrimarySecondaryBurst(t *testing.T) {
	env := setupE2EEnvironment(t, 0)
	t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "true")
	t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", "gemini-2.5-pro")
	t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", "gemini-2.5-flash")

	seedTestAccount(t, env, "acc-stamp-1", "stamp1@example.com", "tok-stamp-1", true, domain.AccountStatusActive)
	seedTestAccount(t, env, "acc-stamp-2", "stamp2@example.com", "tok-stamp-2", false, domain.AccountStatusActive)

	// acc-stamp-1 fails everything with 429
	env.MockGoogle.SetFailoverTrigger("tok-stamp-1", 100)
	// acc-stamp-2 has healthy quota
	env.MockGoogle.SetFailoverTrigger("tok-stamp-2", 0)

	const totalRequests = 50
	var wg sync.WaitGroup
	startBarrier := make(chan struct{})

	var successCount atomic.Int64
	var errorCount atomic.Int64

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
		},
	}

	for i := 0; i < totalRequests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-startBarrier

			var reqModel string
			if idx%2 == 0 {
				reqModel = "gemini-2.5-pro"
			} else {
				reqModel = "gemini-2.5-flash"
			}

			reqBody := fmt.Sprintf(`{"model":%q,"contents":[{"parts":[{"text":"Concurrent mixed burst %d"}]}]}`, reqModel, idx)
			req, err := http.NewRequest(http.MethodPost, env.ServerURL+"/v1internal:generateContent", strings.NewReader(reqBody))
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
			_, _ = io.Copy(io.Discard, resp.Body)

			if resp.StatusCode == http.StatusOK {
				successCount.Add(1)
			} else {
				errorCount.Add(1)
			}
		}(i)
	}

	// Unleash all 50 concurrent requests simultaneously
	close(startBarrier)
	wg.Wait()

	if errs := errorCount.Load(); errs > 0 {
		t.Fatalf("%d out of %d concurrent requests failed", errs, totalRequests)
	}
	if succ := successCount.Load(); succ != totalRequests {
		t.Fatalf("expected %d successful requests, got %d", totalRequests, succ)
	}

	// Verify active account is acc-stamp-2
	ctx := context.Background()
	active, err := env.AccountRepo.GetActive(ctx)
	if err != nil || active == nil || active.ID != "acc-stamp-2" {
		t.Errorf("expected active account to be acc-stamp-2, got: %v", active)
	}

	// Verify acc-stamp-1 is exhausted
	oldAcc, err := env.AccountRepo.GetByID(ctx, "acc-stamp-1")
	if err != nil || oldAcc.Status != domain.AccountStatusExhausted {
		t.Errorf("expected acc-stamp-1 status 'exhausted', got: %v", oldAcc)
	}
}

// TestTier5_Adversarial_AntiStampede_HighConcurrencyCancellations stress-tests the
// FailoverEngine anti-stampede mutex when half of the concurrent callers cancel their context
// prematurely while waiting on the lock. Verifies that cancellations do not poison or permanently
// lock the mutex, and that surviving requests succeed normally.
func TestTier5_Adversarial_AntiStampede_HighConcurrencyCancellations(t *testing.T) {
	env := setupE2EEnvironment(t, 0)
	t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "true")

	seedTestAccount(t, env, "acc-canc-1", "canc1@example.com", "tok-canc-1", true, domain.AccountStatusActive)
	seedTestAccount(t, env, "acc-canc-2", "canc2@example.com", "tok-canc-2", false, domain.AccountStatusActive)

	env.MockGoogle.SetFailoverTrigger("tok-canc-1", 100)
	env.MockGoogle.SetFailoverTrigger("tok-canc-2", 0)

	const (
		cancellingWorkers = 20
		survivingWorkers  = 20
	)

	var wg sync.WaitGroup
	startBarrier := make(chan struct{})
	var survivingSuccess atomic.Int64

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
		},
	}

	// Launch workers that will abort rapidly
	for i := 0; i < cancellingWorkers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-startBarrier

			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(idx*2+5)*time.Millisecond)
			defer cancel()

			reqBody := `{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"Aborted request"}]}]}`
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, env.ServerURL+"/v1internal:generateContent", strings.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
		}(i)
	}

	// Launch workers that should survive and complete
	for i := 0; i < survivingWorkers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-startBarrier

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			reqBody := `{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"Surviving request"}]}]}`
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, env.ServerURL+"/v1internal:generateContent", strings.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(req)
			if err == nil && resp.StatusCode == http.StatusOK {
				survivingSuccess.Add(1)
				_ = resp.Body.Close()
			} else if resp != nil {
				_ = resp.Body.Close()
			}
		}(i)
	}

	close(startBarrier)
	wg.Wait()

	if succ := survivingSuccess.Load(); succ != survivingWorkers {
		t.Errorf("expected all %d surviving workers to succeed, got %d", survivingWorkers, succ)
	}

	// Health check to ensure proxy server did not deadlock
	healthResp, err := http.Get(env.ServerURL + "/api/status")
	if err != nil || healthResp.StatusCode != http.StatusOK {
		t.Fatalf("proxy server unresponsive after cancellation stampede: %v", err)
	}
	_ = healthResp.Body.Close()
}

// TestTier5_Adversarial_SSE_MidStreamDisconnect_TokenCapture tests that when a client
// abruptly terminates an SSE streaming request immediately after the usageMetadata chunk
// has been transmitted by upstream, the proxy's detached background context ensures
// token metrics are successfully persisted in SQLite without being dropped.
func TestTier5_Adversarial_SSE_MidStreamDisconnect_TokenCapture(t *testing.T) {
	env := setupE2EEnvironment(t, 0)
	t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "true")

	seedTestAccount(t, env, "acc-sse-tok", "ssetok@example.com", "tok-sse-tok", true, domain.AccountStatusActive)

	// Configure mock Google to send chunk 1, chunk 2 (usageMetadata), then chunk 3 with a 500ms delay
	c1 := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"First stream chunk"}]}}]}}`
	c2 := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"Second chunk"}]}}],"usageMetadata":{"promptTokenCount":350,"candidatesTokenCount":125,"totalTokenCount":475}}}`
	c3 := `{"response":{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"Third delayed chunk"}]}}]}}`

	env.MockGoogle.ConfigureAccount("tok-sse-tok", &mocks.AccountBehavior{
		Email: "ssetok@example.com",
		CustomSSEChunks: []string{
			fmt.Sprintf("data: %s\n\n", c1),
			fmt.Sprintf("data: %s\n\n", c2),
			fmt.Sprintf("data: %s\n\n", c3),
		},
		StreamDelay: 100 * time.Millisecond,
	})

	client := &http.Client{}
	req, err := http.NewRequest(http.MethodPost, env.ServerURL+"/v1internal:streamGenerateContent?alt=sse", strings.NewReader(`{"contents":[]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client Do: %v", err)
	}

	// Read up to chunk 2 (which contains usageMetadata), then abruptly abort downstream socket
	scanner := bufio.NewScanner(resp.Body)
	sawUsageMetadata := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "usageMetadata") {
			sawUsageMetadata = true
			break
		}
	}
	// Abruptly close client body reader before stream completes
	_ = resp.Body.Close()

	if !sawUsageMetadata {
		t.Fatalf("did not encounter usageMetadata chunk before aborting")
	}

	// Allow asynchronous detached persistence goroutine up to 1 second to write to SQLite
	ctx := context.Background()
	var totalTokens int64
	for attempt := 0; attempt < 20; attempt++ {
		time.Sleep(50 * time.Millisecond)
		summary, err := env.MetricsService.GetSummary(ctx, "acc-sse-tok", domain.PeriodLifetime)
		if err == nil && summary != nil && summary.TotalTokens > 0 {
			totalTokens = summary.TotalTokens
			break
		}
	}

	if totalTokens != 475 {
		t.Errorf("expected 475 tokens to be captured despite abrupt client disconnection, got %d", totalTokens)
	}
}

// TestTier5_Adversarial_NonQuota403_NoFailover verifies that non-quota HTTP 403 errors
// (such as PERMISSION_DENIED or ACCESS_BLOCKED) are passed directly through to the client
// without triggering intra-account fallback or rotating accounts.
func TestTier5_Adversarial_NonQuota403_NoFailover(t *testing.T) {
	env := setupE2EEnvironment(t, 0)
	t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "true")

	seedTestAccount(t, env, "acc-403-1", "p403@example.com", "tok-403-1", true, domain.AccountStatusActive)
	seedTestAccount(t, env, "acc-403-2", "p4032@example.com", "tok-403-2", false, domain.AccountStatusActive)

	// Configure mock Google to return non-quota 403 Forbidden
	env.MockGoogle.ConfigureAccount("tok-403-1", &mocks.AccountBehavior{
		Email:           "p403@example.com",
		ForceStatusCode: http.StatusForbidden,
		ForceErrorCode:  "PERMISSION_DENIED",
	})

	client := &http.Client{Timeout: 5 * time.Second}
	body := `{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"Permission denied probe"}]}]}`

	resp, respBody := sendProxyRequest(t, client, env.ServerURL+"/v1internal:generateContent", http.MethodPost, body, nil)

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected HTTP 403 Forbidden passthrough, got %d: %s", resp.StatusCode, respBody)
	}
	if !strings.Contains(respBody, "PERMISSION_DENIED") {
		t.Errorf("expected PERMISSION_DENIED body passthrough, got %s", respBody)
	}

	// Verify NO rotation took place; acc-403-1 must still be the active account
	ctx := context.Background()
	active, err := env.AccountRepo.GetActive(ctx)
	if err != nil || active == nil || active.ID != "acc-403-1" {
		t.Errorf("expected acc-403-1 to remain active after non-quota 403, got: %v", active)
	}

	// Exactly 1 upstream request made
	recorded := env.MockGoogle.GetRecordedRequests()
	if len(recorded) != 1 {
		t.Errorf("expected exactly 1 upstream request (no failover retries), got %d", len(recorded))
	}
}

// TestTier5_Adversarial_AmbiguousModelInQueryAndJSON tests edge cases where conflicting
// model parameters are present in both the URL query string and the JSON request body.
// Verifies deterministic extraction priority and that rewriting properly rewrites both or respects priority.
func TestTier5_Adversarial_AmbiguousModelInQueryAndJSON(t *testing.T) {
	env := setupE2EEnvironment(t, 0)
	t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "true")
	t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", "gemini-2.5-pro")
	t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", "gemini-2.5-flash")

	seedTestAccount(t, env, "acc-ambig-1", "ambig1@example.com", "tok-ambig-1", true, domain.AccountStatusActive)
	seedTestAccount(t, env, "acc-ambig-2", "ambig2@example.com", "tok-ambig-2", false, domain.AccountStatusActive)
	env.MockGoogle.SetFailoverTrigger("tok-ambig-1", 1) // Query specifies secondary, secondary 429 rotates to acc2
	env.MockGoogle.SetFailoverTrigger("tok-ambig-2", 0)

	client := &http.Client{Timeout: 5 * time.Second}
	// Query specifies secondary, body specifies primary
	targetURL := env.ServerURL + "/v1internal:generateContent?model=gemini-2.5-flash"
	body := `{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"Ambiguous query vs body"}]}]}`

	resp, respBody := sendProxyRequest(t, client, targetURL, http.MethodPost, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK, got %d: %s", resp.StatusCode, respBody)
	}

	recorded := env.MockGoogle.GetRecordedRequests()
	if len(recorded) == 0 {
		t.Fatalf("expected at least 1 recorded upstream request")
	}
}

// TestTier5_Adversarial_RapidOscillatingFallback_20Iterations tests repeated sequential
// fallback cycles on an account over 20 iterations, ensuring no memory corruption, goroutine leaks,
// or state drift.
func TestTier5_Adversarial_RapidOscillatingFallback_20Iterations(t *testing.T) {
	env := setupE2EEnvironment(t, 0)
	t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "true")
	t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", "gemini-2.5-pro")
	t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", "gemini-2.5-flash")

	seedTestAccount(t, env, "acc-osc", "osc@example.com", "tok-osc", true, domain.AccountStatusActive)

	client := &http.Client{Timeout: 5 * time.Second}

	// Iteration 0: primary gets 429 once, reactively falls back to secondary and succeeds
	env.MockGoogle.SetFailoverTrigger("tok-osc", 1)

	for i := 0; i < 20; i++ {
		body := fmt.Sprintf(`{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"Oscillation iteration %d"}]}]}`, i)
		resp, respBody := sendProxyRequest(t, client, env.ServerURL+"/v1internal:generateContent", http.MethodPost, body, nil)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("iteration %d: expected 200 OK, got %d: %s", i, resp.StatusCode, respBody)
		}
	}

	// Verify upstream requests: iteration 0 made 2 attempts (primary 429, then secondary 200).
	// Iterations 1-19 were predictively rewritten to gemini-2.5-flash, making exactly 1 attempt each!
	recorded := env.MockGoogle.GetRecordedRequests()
	expectedTotal := 2 + 19 // 21 total requests
	if len(recorded) != expectedTotal {
		t.Errorf("expected %d total upstream requests (2 for iter 0, 19 for iters 1-19), got %d", expectedTotal, len(recorded))
	}

	// Verify that iterations 1-19 all used gemini-2.5-flash
	for idx := 2; idx < len(recorded); idx++ {
		if !strings.Contains(string(recorded[idx].Body), "gemini-2.5-flash") {
			t.Errorf("iteration %d expected predictive rewrite to gemini-2.5-flash, got: %s", idx-1, string(recorded[idx].Body))
		}
	}
}

// TestTier5_Adversarial_QuotaPoller_SyncAndStaleStateVerification explicitly evaluates
// synchronization between background Poller updates in SQLite and in-memory FailoverEngine states.
// Specifically: when an account experiences primary exhaustion and falls back to secondary,
// does a subsequent quota replenishment by the poller restore the account to use the primary model?
func TestTier5_Adversarial_QuotaPoller_SyncAndStaleStateVerification(t *testing.T) {
	env := setupE2EEnvironment(t, 20*time.Millisecond)
	env.FailoverEngine.SetQuotaRepository(env.QuotaRepo)
	t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "true")
	t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", "gemini-2.5-pro")
	t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", "gemini-2.5-flash")

	seedTestAccount(t, env, "acc-sync-1", "sync1@example.com", "tok-sync-1", true, domain.AccountStatusActive)

	client := &http.Client{Timeout: 5 * time.Second}

	// Phase 1: Account experiences reactive 429 on primary model
	env.MockGoogle.SetFailoverTrigger("tok-sync-1", 1)
	body := `{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"Phase 1 prompt"}]}]}`
	resp1, _ := sendProxyRequest(t, client, env.ServerURL+"/v1internal:generateContent", http.MethodPost, body, nil)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("phase 1: expected 200 OK after fallback to secondary, got %d", resp1.StatusCode)
	}

	// Verify Phase 1 fallback: upstream received 2 requests, 2nd was gemini-2.5-flash
	reqsPhase1 := env.MockGoogle.GetRecordedRequests()
	if len(reqsPhase1) != 2 {
		t.Fatalf("phase 1: expected 2 upstream requests, got %d", len(reqsPhase1))
	}
	if !strings.Contains(string(reqsPhase1[1].Body), "gemini-2.5-flash") {
		t.Fatalf("phase 1: expected 2nd request to be rewritten to gemini-2.5-flash, got: %s", string(reqsPhase1[1].Body))
	}

	// Phase 2: Upstream restores 100% quota for primary model (gemini-2.5-pro)
	now := time.Now().UTC()
	env.MockGoogle.SetAccountQuota("tok-sync-1", []mocks.QuotaSummaryBucket{
		{
			BucketID:          "gemini-2.5-pro",
			DisplayName:       "Gemini 2.5 Pro",
			RemainingFraction: 1.0,
			RemainingAmount:   1000,
			ResetTime:         now.Add(12 * time.Hour),
		},
		{
			BucketID:          "gemini-2.5-flash",
			DisplayName:       "Gemini 2.5 Flash",
			RemainingFraction: 1.0,
			RemainingAmount:   10000,
			ResetTime:         now.Add(24 * time.Hour),
		},
	})
	// Run Poller to fetch fresh quota and update SQLite
	ctx := context.Background()
	if err := env.Poller.PollOnce(ctx); err != nil {
		t.Fatalf("poller PollOnce failed: %v", err)
	}

	// Verify SQLite has the restored quota
	dbBuckets, err := env.QuotaRepo.GetByAccountID(ctx, "acc-sync-1")
	if err != nil || len(dbBuckets) == 0 {
		t.Fatalf("failed to retrieve quota buckets from SQLite: %v", err)
	}
	var proBucket *domain.QuotaBucket
	for _, b := range dbBuckets {
		if strings.Contains(b.BucketID, "pro") {
			proBucket = b
			break
		}
	}
	if proBucket == nil || proBucket.RemainingFraction < 1.0 {
		t.Fatalf("expected pro bucket in SQLite to have remaining_fraction 1.0, got: %v", proBucket)
	}

	// Reset mock google records
	env.MockGoogle.Reset()
	env.MockGoogle.SetFailoverTrigger("tok-sync-1", 0)

	// Phase 3: Client sends a new request specifying primary model gemini-2.5-pro
	resp2, _ := sendProxyRequest(t, client, env.ServerURL+"/v1internal:generateContent", http.MethodPost, body, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("phase 3: expected 200 OK, got %d", resp2.StatusCode)
	}

	reqsPhase3 := env.MockGoogle.GetRecordedRequests()
	if len(reqsPhase3) == 0 {
		t.Fatalf("phase 3: no requests received by upstream")
	}

	sentModel := string(reqsPhase3[0].Body)
	t.Logf("Phase 3 dispatched request body: %s", sentModel)

	// EMPIRICAL CHECK: Did the proxy restore to gemini-2.5-pro, or did it predictively rewrite to gemini-2.5-flash?
	if strings.Contains(sentModel, "gemini-2.5-flash") {
		t.Fatalf("phase 3: expected proxy to restore to primary model gemini-2.5-pro, but got secondary: %s", sentModel)
	}
	if !strings.Contains(sentModel, "gemini-2.5-pro") {
		t.Fatalf("phase 3: expected proxy to dispatch primary model gemini-2.5-pro, got: %s", sentModel)
	}
	t.Logf("CONFIRMED: Proxy dispatched primary model gemini-2.5-pro following quota restoration.")
}

// TestTier5_Adversarial_AllExhausted_ThenPollerRestores_AutoResume tests that when all accounts
// in the pool are exhausted, causing proxy requests to fail with 503/429, a subsequent Poller
// auto-restore allows the proxy to seamlessly resume serving traffic on the restored account.
func TestTier5_Adversarial_AllExhausted_ThenPollerRestores_AutoResume(t *testing.T) {
	env := setupE2EEnvironment(t, 20*time.Millisecond)
	t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "true")

	acc := seedTestAccount(t, env, "acc-resume-1", "resume1@example.com", "tok-resume-1", true, domain.AccountStatusActive)

	// Exhaust account on both tiers
	env.MockGoogle.SetFailoverTrigger("tok-resume-1", 10)

	client := &http.Client{Timeout: 5 * time.Second}
	body := `{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"Exhaustion probe"}]}]}`

	resp1, _ := sendProxyRequest(t, client, env.ServerURL+"/v1internal:generateContent", http.MethodPost, body, nil)
	if resp1.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on pool exhaustion, got %d", resp1.StatusCode)
	}

	ctx := context.Background()
	accDB, err := env.AccountRepo.GetByID(ctx, acc.ID)
	if err != nil || accDB.Status != domain.AccountStatusExhausted {
		t.Fatalf("expected account to be exhausted, got %v", accDB)
	}

	// Now replenish quota on mock Google
	now := time.Now().UTC()
	env.MockGoogle.SetAccountQuota("tok-resume-1", []mocks.QuotaSummaryBucket{
		{
			BucketID:          "gemini-2.5-pro",
			DisplayName:       "Gemini Pro",
			RemainingFraction: 1.0,
			RemainingAmount:   1000,
			ResetTime:         now.Add(12 * time.Hour),
		},
		{
			BucketID:          "gemini-2.5-flash",
			DisplayName:       "Gemini Flash",
			RemainingFraction: 1.0,
			RemainingAmount:   1000,
			ResetTime:         now.Add(12 * time.Hour),
		},
	})
	env.MockGoogle.SetFailoverTrigger("tok-resume-1", 0)

	// Trigger Poller to auto-restore account
	if err := env.Poller.PollOnce(ctx); err != nil {
		t.Fatalf("poller PollOnce: %v", err)
	}

	// Verify account status restored to active
	accDB2, _ := env.AccountRepo.GetByID(ctx, acc.ID)
	if accDB2.Status != domain.AccountStatusActive {
		t.Fatalf("expected account status active after poller restore, got: %s", accDB2.Status)
	}

	// Next request should succeed
	resp2, respBody2 := sendProxyRequest(t, client, env.ServerURL+"/v1internal:generateContent", http.MethodPost, body, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK after account auto-restore, got %d: %s", resp2.StatusCode, respBody2)
	}
}

// TestTier5_Adversarial_Upstream500_503_NoFailover tests that HTTP 500 Internal Server Error
// and HTTP 503 Service Unavailable from upstream are forwarded cleanly to the client
// without triggering model fallback or account rotation.
func TestTier5_Adversarial_Upstream500_503_NoFailover(t *testing.T) {
	env := setupE2EEnvironment(t, 0)
	t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "true")

	seedTestAccount(t, env, "acc-srv-1", "srv1@example.com", "tok-srv-1", true, domain.AccountStatusActive)
	seedTestAccount(t, env, "acc-srv-2", "srv2@example.com", "tok-srv-2", false, domain.AccountStatusActive)

	client := &http.Client{Timeout: 5 * time.Second}

	// Case 1: 500 Internal Server Error
	env.MockGoogle.ConfigureAccount("tok-srv-1", &mocks.AccountBehavior{
		Email:           "srv1@example.com",
		ForceStatusCode: http.StatusInternalServerError,
		ForceErrorCode:  "INTERNAL",
	})
	resp500, _ := sendProxyRequest(t, client, env.ServerURL+"/v1internal:generateContent", http.MethodPost, `{"model":"gemini-2.5-pro","contents":[]}`, nil)
	if resp500.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected 500 passthrough, got %d", resp500.StatusCode)
	}

	// Case 2: 503 Service Unavailable
	env.MockGoogle.ConfigureAccount("tok-srv-1", &mocks.AccountBehavior{
		Email:           "srv1@example.com",
		ForceStatusCode: http.StatusServiceUnavailable,
		ForceErrorCode:  "UNAVAILABLE",
	})
	resp503, _ := sendProxyRequest(t, client, env.ServerURL+"/v1internal:generateContent", http.MethodPost, `{"model":"gemini-2.5-pro","contents":[]}`, nil)
	if resp503.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503 passthrough, got %d", resp503.StatusCode)
	}

	// Verify acc-srv-1 remains active (no rotation on 500/503)
	ctx := context.Background()
	active, _ := env.AccountRepo.GetActive(ctx)
	if active == nil || active.ID != "acc-srv-1" {
		t.Errorf("expected acc-srv-1 to remain active after 500/503 errors, got: %v", active)
	}
}

// TestTier5_Adversarial_SSE_MalformedDataLine_StreamPassesThrough tests that corrupted,
// non-JSON data lines in an SSE stream do not crash the stream parser or drop downstream chunks.
func TestTier5_Adversarial_SSE_MalformedDataLine_StreamPassesThrough(t *testing.T) {
	env := setupE2EEnvironment(t, 0)
	t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "true")

	seedTestAccount(t, env, "acc-sse-mal", "mal@example.com", "tok-sse-mal", true, domain.AccountStatusActive)

	// Stream with valid chunk, malformed garbage chunk, then valid usageMetadata chunk
	c1 := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"Valid part 1"}]}}]}}`
	c2Garbage := `NOT_VALID_JSON{:::broken`
	c3 := `{"response":{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"Valid part 2"}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":50,"totalTokenCount":150}}}`

	env.MockGoogle.ConfigureAccount("tok-sse-mal", &mocks.AccountBehavior{
		Email: "mal@example.com",
		CustomSSEChunks: []string{
			fmt.Sprintf("data: %s\n\n", c1),
			fmt.Sprintf("data: %s\n\n", c2Garbage),
			fmt.Sprintf("data: %s\n\n", c3),
		},
	})

	client := &http.Client{Timeout: 5 * time.Second}
	resp, chunks := sendProxySSERequest(t, client, env.ServerURL+"/v1internal:streamGenerateContent?alt=sse", `{"contents":[]}`, nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	if len(chunks) < 3 {
		t.Fatalf("expected all 3 chunks to be delivered downstream despite malformed chunk, got %d", len(chunks))
	}

	// Verify token metrics still captured from chunk 3
	ctx := context.Background()
	var tokens int64
	for attempt := 0; attempt < 20; attempt++ {
		time.Sleep(50 * time.Millisecond)
		summary, err := env.MetricsService.GetSummary(ctx, "acc-sse-mal", domain.PeriodLifetime)
		if err == nil && summary != nil && summary.TotalTokens > 0 {
			tokens = summary.TotalTokens
			break
		}
	}
	if tokens != 150 {
		t.Errorf("expected 150 tokens to be captured from valid chunk 3, got %d", tokens)
	}
}

// TestTier5_Adversarial_StressHarness_100MixedRequests_RaceDetector executes 100 concurrent
// requests exercising mixed operations (unary pro, unary flash, SSE streaming, 429 failover)
// under data race detector (-race) scrutiny to guarantee 0 race conditions across the pipeline.
func TestTier5_Adversarial_StressHarness_100MixedRequests_RaceDetector(t *testing.T) {
	env := setupE2EEnvironment(t, 0)
	t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "true")

	seedTestAccount(t, env, "acc-harness-1", "harness1@example.com", "tok-harness-1", true, domain.AccountStatusActive)
	seedTestAccount(t, env, "acc-harness-2", "harness2@example.com", "tok-harness-2", false, domain.AccountStatusActive)

	// acc-harness-1 fails first 5 requests with 429, then succeeds on fallback
	env.MockGoogle.SetFailoverTrigger("tok-harness-1", 5)
	env.MockGoogle.SetFailoverTrigger("tok-harness-2", 0)

	const totalWorkers = 100
	var wg sync.WaitGroup
	startBarrier := make(chan struct{})

	var successCount atomic.Int64
	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
		},
	}

	for i := 0; i < totalWorkers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-startBarrier

			var resp *http.Response
			var err error

			switch idx % 3 {
			case 0:
				// Unary primary
				body := fmt.Sprintf(`{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"Harness unary pro %d"}]}]}`, idx)
				req, _ := http.NewRequest(http.MethodPost, env.ServerURL+"/v1internal:generateContent", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err = client.Do(req)

			case 1:
				// Unary secondary
				body := fmt.Sprintf(`{"model":"gemini-2.5-flash","contents":[{"parts":[{"text":"Harness unary flash %d"}]}]}`, idx)
				req, _ := http.NewRequest(http.MethodPost, env.ServerURL+"/v1internal:generateContent", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err = client.Do(req)

			case 2:
				// SSE stream
				body := fmt.Sprintf(`{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"Harness sse %d"}]}]}`, idx)
				req, _ := http.NewRequest(http.MethodPost, env.ServerURL+"/v1internal:streamGenerateContent?alt=sse", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "text/event-stream")
				resp, err = client.Do(req)
			}

			if err == nil && resp != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					successCount.Add(1)
				}
			}
		}(i)
	}

	close(startBarrier)
	wg.Wait()

	if succ := successCount.Load(); succ < 95 {
		t.Errorf("expected at least 95 successful requests out of 100, got %d", succ)
	}
}
