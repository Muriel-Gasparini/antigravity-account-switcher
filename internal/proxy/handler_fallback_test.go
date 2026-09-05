package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/test/mocks"
)

type fallbackTestEnv struct {
	db             *sqlite.DB
	accountRepo    domain.AccountRepository
	metricsRepo    domain.MetricsRepository
	eventRepo      domain.EventRepository
	quotaRepo      domain.QuotaRepository
	broadcaster    *Broadcaster
	failoverEngine *FailoverEngine
	mockGoogle     *mocks.MockGoogleServer
	handler        *ProxyHandler
	server         *httptest.Server
	client         *http.Client
}

func newFallbackTestEnv(t *testing.T, primary, secondary string, fallbackEnabled bool) *fallbackTestEnv {
	t.Helper()
	mockGoogle := mocks.NewMockGoogleServer()
	t.Cleanup(func() { mockGoogle.Close() })

	dbPath := filepath.Join(t.TempDir(), "fallback_test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	accRepo := sqlite.NewAccountRepository(db)
	metricsRepo := sqlite.NewMetricsRepository(db)
	eventRepo := sqlite.NewEventRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)

	broadcaster := NewBroadcaster(100)
	engine := NewFailoverEngine(
		accRepo,
		broadcaster,
		eventRepo,
		WithQuotaRepository(quotaRepo),
		WithModelFallback(primary, secondary, fallbackEnabled),
	)

	handler, err := NewProxyHandler(
		accRepo,
		WithTargetURL(mockGoogle.URL),
		WithMetricsRepository(metricsRepo),
		WithEventBroadcaster(broadcaster),
		WithEventRepository(eventRepo),
		WithFailoverEngine(engine),
	)
	if err != nil {
		t.Fatalf("proxy handler create: %v", err)
	}

	server := httptest.NewServer(handler)
	t.Cleanup(func() { server.Close() })

	return &fallbackTestEnv{
		db:             db,
		accountRepo:    accRepo,
		metricsRepo:    metricsRepo,
		eventRepo:      eventRepo,
		quotaRepo:      quotaRepo,
		broadcaster:    broadcaster,
		failoverEngine: engine,
		mockGoogle:     mockGoogle,
		handler:        handler,
		server:         server,
		client:         &http.Client{Timeout: 10 * time.Second},
	}
}

// TestPredictiveFallback verifies that an in-flight request targeting a primary model
// with 0% remaining quota is transparently rewritten to the configured secondary model
// before initial upstream dispatch, preventing an unnecessary HTTP 429 round-trip.
func TestPredictiveFallback(t *testing.T) {
	env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
	ctx := context.Background()

	now := time.Now().UTC()
	acc := &domain.Account{
		ID:          "acc-pred-1",
		Email:       "pred1@example.com",
		AccessToken: "token-pred-1",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := env.accountRepo.Create(ctx, acc); err != nil {
		t.Fatalf("create account: %v", err)
	}

	// Set quota buckets: Pro is 0% (exhausted), Flash has 85% remaining
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
			RemainingFraction: 0.85,
			RemainingAmount:   85000,
			ResetTime:         now.Add(5 * time.Hour),
		},
	}
	if err := env.quotaRepo.UpsertBuckets(ctx, buckets); err != nil {
		t.Fatalf("save buckets: %v", err)
	}
	env.failoverEngine.UpdateQuotaCache(acc.ID, buckets)

	eventsCh, unsubscribe := env.broadcaster.Subscribe()
	defer unsubscribe()

	reqBody := `{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"Hello"}]}]}`
	req, err := http.NewRequest("POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:streamGenerateContent?alt=sse", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("create req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(b))
	}

	// Verify upstream received request with rewritten model
	reqs := env.mockGoogle.GetRecordedRequests()
	if len(reqs) != 1 {
		t.Fatalf("expected exactly 1 upstream request (proactive rewrite without failed attempt), got %d", len(reqs))
	}

	recorded := reqs[0]
	if !strings.Contains(recorded.Path, "gemini-2.5-flash") {
		t.Errorf("expected rewritten path to contain gemini-2.5-flash, got: %s", recorded.Path)
	}
	if strings.Contains(recorded.Path, "gemini-2.5-pro") {
		t.Errorf("expected path to NOT contain gemini-2.5-pro, got: %s", recorded.Path)
	}
	if recorded.AuthBearer != "token-pred-1" {
		t.Errorf("expected bearer token-pred-1, got: %s", recorded.AuthBearer)
	}
	if !strings.Contains(string(recorded.Body), "gemini-2.5-flash") {
		t.Errorf("expected rewritten body to contain gemini-2.5-flash, got: %s", string(recorded.Body))
	}

	// Verify telemetry event
	select {
	case evt := <-eventsCh:
		if evt.Type != domain.EventTypeModelFallback {
			t.Errorf("expected EventTypeModelFallback, got %s", evt.Type)
		}
		if evt.Details["mode"] != "predictive" {
			t.Errorf("expected mode predictive, got %v", evt.Details["mode"])
		}
		if evt.Details["target_model"] != "gemini-2.5-flash" {
			t.Errorf("expected target_model gemini-2.5-flash, got %v", evt.Details["target_model"])
		}
	case <-time.After(500 * time.Millisecond):
		t.Errorf("timeout waiting for predictive fallback event")
	}

	// Verify token metrics recorded with effective secondary model path
	time.Sleep(50 * time.Millisecond)
	summary, err := env.metricsRepo.GetSummary(ctx, acc.ID, string(domain.PeriodTotal))
	if err != nil {
		t.Fatalf("get summary: %v", err)
	}
	if summary.TotalRequests < 1 {
		t.Errorf("expected at least 1 recorded request metric, got %d", summary.TotalRequests)
	}
}

// TestReactiveFallback429 verifies that when an upstream request receives HTTP 429
// on the primary model, the proxy transparently rewrites and replays the request
// on the secondary model using the SAME active account, leaking 0 error bytes.
func TestReactiveFallback429(t *testing.T) {
	env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
	ctx := context.Background()

	now := time.Now().UTC()
	acc := &domain.Account{
		ID:          "acc-react-1",
		Email:       "react1@example.com",
		AccessToken: "token-react-1",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := env.accountRepo.Create(ctx, acc); err != nil {
		t.Fatalf("create account: %v", err)
	}

	// 1st request on token-react-1 returns 429, subsequent return 200 OK
	env.mockGoogle.SetFailoverTrigger("token-react-1", 1)

	eventsCh, unsubscribe := env.broadcaster.Subscribe()
	defer unsubscribe()

	reqBody := `{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"Reactive test"}]}]}`
	req, err := http.NewRequest("POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:streamGenerateContent?alt=sse", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("create req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(b))
	}

	// Ensure zero 429 error bytes reached the client
	respBytes, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(respBytes), "RESOURCE_EXHAUSTED") || strings.Contains(string(respBytes), "429") {
		t.Errorf("client received 429 error leak: %s", string(respBytes))
	}

	// Verify exactly 2 upstream requests made: 1st on Pro (failed 429), 2nd on Flash (200 OK)
	reqs := env.mockGoogle.GetRecordedRequests()
	if len(reqs) != 2 {
		t.Fatalf("expected exactly 2 upstream requests, got %d", len(reqs))
	}

	if !strings.Contains(reqs[0].Path, "gemini-2.5-pro") {
		t.Errorf("expected 1st request to target gemini-2.5-pro, got %s", reqs[0].Path)
	}
	if reqs[0].AuthBearer != "token-react-1" {
		t.Errorf("expected 1st bearer token-react-1, got %s", reqs[0].AuthBearer)
	}

	if !strings.Contains(reqs[1].Path, "gemini-2.5-flash") {
		t.Errorf("expected 2nd request to target gemini-2.5-flash, got %s", reqs[1].Path)
	}
	if reqs[1].AuthBearer != "token-react-1" {
		t.Errorf("expected 2nd request to remain on same account token-react-1, got %s", reqs[1].AuthBearer)
	}
	if !strings.Contains(string(reqs[1].Body), "gemini-2.5-flash") {
		t.Errorf("expected 2nd body to be rewritten to gemini-2.5-flash, got %s", string(reqs[1].Body))
	}

	// Verify account in DB: STILL active! No rotation occurred!
	dbAcc, err := env.accountRepo.GetByID(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get acc: %v", err)
	}
	if dbAcc.Status != domain.AccountStatusActive || !dbAcc.IsActive {
		t.Errorf("expected acc to remain active in DB, got status=%s is_active=%v", dbAcc.Status, dbAcc.IsActive)
	}

	// Verify EventTypeModelFallback with mode reactive_429 was emitted
	foundFallbackEvt := false
	for {
		select {
		case evt := <-eventsCh:
			if evt.Type == domain.EventTypeModelFallback && evt.Details["mode"] == "reactive_429" {
				foundFallbackEvt = true
			}
		default:
			goto evtDone
		}
	}
evtDone:
	if !foundFallbackEvt {
		t.Errorf("expected EventTypeModelFallback event with mode reactive_429")
	}
}

// TestAccountRotationOnDoubleExhaustion verifies that when both primary and secondary
// tiers on the active account are exhausted, the proxy rotates to the next available
// account in the pool and RESETS the requested model back to the primary tier.
func TestAccountRotationOnDoubleExhaustion(t *testing.T) {
	env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
	ctx := context.Background()

	now := time.Now().UTC()
	acc1 := &domain.Account{
		ID:          "acc-rot-1",
		Email:       "rot1@example.com",
		AccessToken: "token-rot-1",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	acc2 := &domain.Account{
		ID:          "acc-rot-2",
		Email:       "rot2@example.com",
		AccessToken: "token-rot-2",
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

	// token-rot-1 fails twice (pro -> 429, flash -> 429).
	// token-rot-2 succeeds immediately on pro (200 OK).
	env.mockGoogle.SetFailoverTrigger("token-rot-1", 2)

	eventsCh, unsubscribe := env.broadcaster.Subscribe()
	defer unsubscribe()

	reqBody := `{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"Double exhaustion test"}]}]}`
	req, err := http.NewRequest("POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:streamGenerateContent?alt=sse", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("create req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(b))
	}

	// Verify 3 upstream requests:
	// 1. token-rot-1, gemini-2.5-pro (429)
	// 2. token-rot-1, gemini-2.5-flash (429)
	// 3. token-rot-2, gemini-2.5-pro (reset to primary!) (200 OK)
	reqs := env.mockGoogle.GetRecordedRequests()
	if len(reqs) != 3 {
		t.Fatalf("expected exactly 3 upstream requests, got %d", len(reqs))
	}

	if reqs[0].AuthBearer != "token-rot-1" || !strings.Contains(reqs[0].Path, "gemini-2.5-pro") {
		t.Errorf("req 0: expected token-rot-1 / pro, got %s / %s", reqs[0].AuthBearer, reqs[0].Path)
	}
	if reqs[1].AuthBearer != "token-rot-1" || !strings.Contains(reqs[1].Path, "gemini-2.5-flash") {
		t.Errorf("req 1: expected token-rot-1 / flash, got %s / %s", reqs[1].AuthBearer, reqs[1].Path)
	}
	if reqs[2].AuthBearer != "token-rot-2" || !strings.Contains(reqs[2].Path, "gemini-2.5-pro") {
		t.Errorf("req 2: expected token-rot-2 / pro (model reset to primary), got %s / %s", reqs[2].AuthBearer, reqs[2].Path)
	}

	// Verify Account statuses in DB
	dbAcc1, _ := env.accountRepo.GetByID(ctx, acc1.ID)
	if dbAcc1.Status != domain.AccountStatusExhausted || dbAcc1.IsActive {
		t.Errorf("expected acc1 exhausted, got status=%s is_active=%v", dbAcc1.Status, dbAcc1.IsActive)
	}

	dbAcc2, _ := env.accountRepo.GetByID(ctx, acc2.ID)
	if dbAcc2.Status != domain.AccountStatusActive || !dbAcc2.IsActive {
		t.Errorf("expected acc2 active, got status=%s is_active=%v", dbAcc2.Status, dbAcc2.IsActive)
	}

	// Verify events: model_fallback -> failover_429 -> account_switched
	var eventTypes []domain.EventType
	for {
		select {
		case evt := <-eventsCh:
			eventTypes = append(eventTypes, evt.Type)
		default:
			goto eventsDone
		}
	}
eventsDone:
	hasFallback := false
	hasFailover := false
	hasSwitched := false
	for _, et := range eventTypes {
		if et == domain.EventTypeModelFallback {
			hasFallback = true
		}
		if et == domain.EventTypeFailover429 {
			hasFailover = true
		}
		if et == domain.EventTypeAccountSwitched {
			hasSwitched = true
		}
	}
	if !hasFallback {
		t.Errorf("expected EventTypeModelFallback")
	}
	if !hasFailover {
		t.Errorf("expected EventTypeFailover429")
	}
	if !hasSwitched {
		t.Errorf("expected EventTypeAccountSwitched")
	}
}

// TestFallbackDisabledDirectRotation verifies that when model fallback is disabled,
// an upstream HTTP 429 response on the primary model immediately rotates accounts
// without attempting the secondary model tier.
func TestFallbackDisabledDirectRotation(t *testing.T) {
	env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", false)
	ctx := context.Background()

	now := time.Now().UTC()
	acc1 := &domain.Account{
		ID:          "acc-dis-1",
		Email:       "dis1@example.com",
		AccessToken: "token-dis-1",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	acc2 := &domain.Account{
		ID:          "acc-dis-2",
		Email:       "dis2@example.com",
		AccessToken: "token-dis-2",
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

	env.mockGoogle.SetFailoverTrigger("token-dis-1", 1)

	eventsCh, unsubscribe := env.broadcaster.Subscribe()
	defer unsubscribe()

	reqBody := `{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"Disabled test"}]}]}`
	req, err := http.NewRequest("POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:streamGenerateContent?alt=sse", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("create req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(b))
	}

	// Verify exactly 2 upstream requests:
	// 1. token-dis-1, gemini-2.5-pro (429)
	// 2. token-dis-2, gemini-2.5-pro (200 OK) -> NO attempt on gemini-2.5-flash!
	reqs := env.mockGoogle.GetRecordedRequests()
	if len(reqs) != 2 {
		t.Fatalf("expected exactly 2 upstream requests, got %d", len(reqs))
	}

	if reqs[0].AuthBearer != "token-dis-1" || !strings.Contains(reqs[0].Path, "gemini-2.5-pro") {
		t.Errorf("req 0: expected token-dis-1 / pro, got %s / %s", reqs[0].AuthBearer, reqs[0].Path)
	}
	if reqs[1].AuthBearer != "token-dis-2" || !strings.Contains(reqs[1].Path, "gemini-2.5-pro") {
		t.Errorf("req 1: expected token-dis-2 / pro, got %s / %s", reqs[1].AuthBearer, reqs[1].Path)
	}

	// Verify no EventTypeModelFallback was emitted
	for {
		select {
		case evt := <-eventsCh:
			if evt.Type == domain.EventTypeModelFallback {
				t.Errorf("unexpected EventTypeModelFallback when fallback is disabled")
			}
		default:
			goto evtCheckDone
		}
	}
evtCheckDone:
}

// TestSSEStreamingWithFallback verifies SSE streaming fidelity during intra-account
// fallback, confirming line-by-line flushes, valid SSE frames, and accurate token metric recording.
func TestSSEStreamingWithFallback(t *testing.T) {
	env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
	ctx := context.Background()

	now := time.Now().UTC()
	acc := &domain.Account{
		ID:          "acc-sse-fb",
		Email:       "ssefb@example.com",
		AccessToken: "token-sse-fb",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := env.accountRepo.Create(ctx, acc); err != nil {
		t.Fatalf("create acc: %v", err)
	}

	// token-sse-fb triggers 429 on first request, then serves custom SSE chunks on second
	customChunks := []string{
		"data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Streaming \"}]}}]}}\n\n",
		"data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"with fallback!\"}]}}]}}\n\n",
		"data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"\"}]}}],\"usageMetadata\":{\"promptTokenCount\":42,\"candidatesTokenCount\":18,\"totalTokenCount\":60}}}\n\n",
	}
	env.mockGoogle.ConfigureAccount("token-sse-fb", &mocks.AccountBehavior{
		FailoverRemaining: 1,
		CustomSSEChunks:   customChunks,
		Usage: &mocks.UsageMetadata{
			PromptTokenCount:     42,
			CandidatesTokenCount: 18,
			TotalTokenCount:      60,
		},
	})

	reqBody := `{"contents":[{"parts":[{"text":"Stream test"}]}]}`
	req, err := http.NewRequest("POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:streamGenerateContent?alt=sse", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("create req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(b))
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("expected Content-Type text/event-stream, got %s", ct)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	bodyStr := string(bodyBytes)

	if !strings.Contains(bodyStr, "Streaming ") || !strings.Contains(bodyStr, "with fallback!") {
		t.Errorf("missing expected SSE token text in body: %s", bodyStr)
	}

	// Verify token metrics recorded in SQLite
	time.Sleep(50 * time.Millisecond)
	summary, err := env.metricsRepo.GetSummary(ctx, acc.ID, string(domain.PeriodTotal))
	if err != nil {
		t.Fatalf("get summary: %v", err)
	}
	if summary.TotalPromptTokens != 42 || summary.TotalCandidatesTokens != 18 || summary.TotalTokens != 60 {
		t.Errorf("expected 42/18/60 tokens, got prompt=%d candidates=%d total=%d",
			summary.TotalPromptTokens, summary.TotalCandidatesTokens, summary.TotalTokens)
	}
}

// TestAllAccountsExhaustedReturns503 verifies that when all accounts in the database
// are marked exhausted, incoming requests immediately receive HTTP 503 Service Unavailable.
func TestAllAccountsExhaustedReturns503(t *testing.T) {
	env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
	ctx := context.Background()

	now := time.Now().UTC()
	// Create accounts that are already exhausted
	acc1 := &domain.Account{
		ID:          "acc-exh-1",
		Email:       "exh1@example.com",
		AccessToken: "token-exh-1",
		IsActive:    false,
		Status:      domain.AccountStatusExhausted,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	acc2 := &domain.Account{
		ID:          "acc-exh-2",
		Email:       "exh2@example.com",
		AccessToken: "token-exh-2",
		IsActive:    false,
		Status:      domain.AccountStatusExhausted,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := env.accountRepo.Create(ctx, acc1); err != nil {
		t.Fatalf("create acc1: %v", err)
	}
	if err := env.accountRepo.Create(ctx, acc2); err != nil {
		t.Fatalf("create acc2: %v", err)
	}

	req, err := http.NewRequest("POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:streamGenerateContent?alt=sse", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("create req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 503 Service Unavailable, got %d: %s", resp.StatusCode, string(b))
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "UNAVAILABLE") {
		t.Errorf("expected UNAVAILABLE status in body, got: %s", string(body))
	}
}

// TestJSONBodyModelRewriting verifies that requests specifying the model exclusively
// within the root-level JSON body have their payloads rewritten and Content-Length updated.
func TestJSONBodyModelRewriting(t *testing.T) {
	env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
	ctx := context.Background()

	now := time.Now().UTC()
	acc := &domain.Account{
		ID:          "acc-json-1",
		Email:       "json1@example.com",
		AccessToken: "token-json-1",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := env.accountRepo.Create(ctx, acc); err != nil {
		t.Fatalf("create acc: %v", err)
	}

	env.mockGoogle.SetFailoverTrigger("token-json-1", 1)

	// Model specified only in JSON body, path has no model
	reqBody := `{"model":"gemini-2.5-pro","prompt":{"text":"test JSON body rewrite"}}`
	req, err := http.NewRequest("POST", env.server.URL+"/v1internal:streamGenerateContent?alt=sse", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("create req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(b))
	}

	reqs := env.mockGoogle.GetRecordedRequests()
	if len(reqs) != 2 {
		t.Fatalf("expected exactly 2 upstream requests, got %d", len(reqs))
	}

	// 2nd request body should have model rewritten to gemini-2.5-flash
	body2 := string(reqs[1].Body)
	if !strings.Contains(body2, `"model":"gemini-2.5-flash"`) {
		t.Errorf("expected rewritten JSON body with gemini-2.5-flash, got: %s", body2)
	}

	// Verify Content-Length header matches len(Body) exactly
	cl := reqs[1].Header.Get("Content-Length")
	expectedCL := fmt.Sprintf("%d", len(reqs[1].Body))
	if cl != expectedCL {
		t.Errorf("expected Content-Length %s, got %s", expectedCL, cl)
	}
}

// TestPoolExhaustionVerbatim429 verifies that when all accounts in the pool exhaust
// during failover retries, the final upstream 429 response is propagated verbatim.
func TestPoolExhaustionVerbatim429(t *testing.T) {
	env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
	ctx := context.Background()

	now := time.Now().UTC()
	acc1 := &domain.Account{
		ID:          "acc-tot-1",
		Email:       "tot1@example.com",
		AccessToken: "token-tot-1",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	acc2 := &domain.Account{
		ID:          "acc-tot-2",
		Email:       "tot2@example.com",
		AccessToken: "token-tot-2",
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

	// Both accounts trigger 429 indefinitely
	env.mockGoogle.SetFailoverTrigger("token-tot-1", 100)
	env.mockGoogle.SetFailoverTrigger("token-tot-2", 100)

	req, err := http.NewRequest("POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:streamGenerateContent", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("create req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// When all accounts exhausted during failover, client receives HTTP 429 verbatim
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected HTTP 429 on pool exhaustion, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "RESOURCE_EXHAUSTED") {
		t.Errorf("expected RESOURCE_EXHAUSTED in body, got: %s", string(body))
	}
}
