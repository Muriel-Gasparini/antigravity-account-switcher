package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/proxy"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/test/mocks"
)

// TestTier5_Adversarial_DefaultWiring_StaleWithoutManualSetQuotaRepo tests the production
// behavior as constructed in cmd/main.go and wrap.go, where FailoverEngine is initialized
// via proxy.NewFailoverEngine(accRepo, broadcaster, eventRepo) WITHOUT WithQuotaRepository.
func TestTier5_Adversarial_DefaultWiring_StaleWithoutManualSetQuotaRepo(t *testing.T) {
	env := setupE2EEnvironment(t, 20*time.Millisecond)
	// NOTE: Notice we do NOT call env.FailoverEngine.SetQuotaRepository(env.QuotaRepo) here!
	// This mirrors cmd/antigravity-account-switcher/main.go line 173 and internal/launcher/wrap.go line 192.
	t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "true")
	t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", "gemini-2.5-pro")
	t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", "gemini-2.5-flash")

	seedTestAccount(t, env, "acc-wire-1", "wire1@example.com", "tok-wire-1", true, domain.AccountStatusActive)
	client := &http.Client{Timeout: 5 * time.Second}

	// Phase 1: Account experiences reactive 429 on primary model
	env.MockGoogle.SetFailoverTrigger("tok-wire-1", 1)
	body := `{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"Phase 1 prompt"}]}]}`
	resp1, _ := sendProxyRequest(t, client, env.ServerURL+"/v1internal:generateContent", http.MethodPost, body, nil)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("phase 1: expected status 200, got: %d", resp1.StatusCode)
	}

	// Verify that Phase 1 fell back to secondary
	recs := env.MockGoogle.GetRecordedRequests()
	if len(recs) < 2 {
		t.Fatalf("phase 1: expected at least 2 requests (attempt + fallback), got %d", len(recs))
	}
	lastReq := recs[len(recs)-1]
	if !strings.Contains(string(lastReq.Body), "gemini-2.5-flash") {
		t.Fatalf("phase 1: expected secondary gemini-2.5-flash, got: %s", string(lastReq.Body))
	}

	// Phase 2: Upstream quota is replenished (100% quota)
	now := time.Now().UTC()
	env.MockGoogle.SetAccountQuota("tok-wire-1", []mocks.QuotaSummaryBucket{
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

	ctx := context.Background()
	if err := env.Poller.PollOnce(ctx); err != nil {
		t.Fatalf("poller PollOnce failed: %v", err)
	}

	// Verify SQLite has the restored quota
	dbBuckets, err := env.QuotaRepo.GetByAccountID(ctx, "acc-wire-1")
	if err != nil || len(dbBuckets) == 0 {
		t.Fatalf("expected SQLite to have quota buckets, got err=%v, buckets=%d", err, len(dbBuckets))
	}

	// Reset mock google records
	env.MockGoogle.Reset()
	env.MockGoogle.SetFailoverTrigger("tok-wire-1", 0)

	// Phase 3: Send new request for primary model
	resp3, _ := sendProxyRequest(t, client, env.ServerURL+"/v1internal:generateContent", http.MethodPost, body, nil)
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("phase 3: expected status 200, got: %d", resp3.StatusCode)
	}

	recs3 := env.MockGoogle.GetRecordedRequests()
	if len(recs3) == 0 {
		t.Fatalf("phase 3: no requests recorded")
	}
	sentModel := string(recs3[0].Body)
	t.Logf("phase 3 dispatched request: %s", sentModel)

	if strings.Contains(sentModel, "gemini-2.5-flash") {
		t.Errorf("EMPIRICAL DEFECT CONFIRMED: Default wiring in main.go/wrap.go lacks WithQuotaRepository; proxy dispatched secondary gemini-2.5-flash instead of restored primary gemini-2.5-pro")
	}
	if strings.Contains(sentModel, "gemini-2.5-pro") {
		t.Logf("Primary model gemini-2.5-pro was correctly dispatched")
	}
}

// TestTier5_Adversarial_ConfigFileOrCLIConfigNotWiredToFailoverEngine tests whether
// fallback configured via CLI flags or config file (without env vars) reaches FailoverEngine.
func TestTier5_Adversarial_ConfigFileOrCLIConfigNotWiredToFailoverEngine(t *testing.T) {
	// Replicate cmd/main.go lines 145-173:
	// A user runs: antigravity-account-switcher serve --fallback-secondary=true
	// but does NOT set ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED in the environment.
	env := setupE2EEnvironment(t, 20*time.Millisecond)
	seedTestAccount(t, env, "acc-cli-1", "cli1@example.com", "tok-cli-1", true, domain.AccountStatusActive)
	seedTestAccount(t, env, "acc-cli-2", "cli2@example.com", "tok-cli-2", false, domain.AccountStatusActive)

	// Since main.go never passes cfg to NewFailoverEngine, and only relies on
	// syncFallbackConfigFromEnv, fallback remains disabled (false).
	acc, _ := env.AccountRepo.GetActive(context.Background())
	action, targetModel, nextAcc, err := env.FailoverEngine.HandleExhaustion(context.Background(), acc, "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if action == proxy.ActionFallbackSecondary {
		t.Logf("Fallback enabled: targetModel=%s", targetModel)
	} else {
		t.Errorf("EMPIRICAL DEFECT CONFIRMED: FailoverEngine defaults to fallbackSecondaryEnabled=false; action=%v nextAcc=%v instead of ActionFallbackSecondary", action, nextAcc.Email)
	}
}
