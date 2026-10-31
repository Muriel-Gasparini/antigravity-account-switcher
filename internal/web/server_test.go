package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/config"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/metrics"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/proxy"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
)

func setupTestWeb(t *testing.T) (*sqlite.DB, domain.AccountRepository, domain.QuotaRepository, domain.MetricsRepository, *metrics.Service, *proxy.Broadcaster, domain.EventRepository) {
	t.Helper()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", t.TempDir())
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open sqlite in memory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	accRepo := sqlite.NewAccountRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)
	metricsRepo := sqlite.NewMetricsRepository(db)
	eventRepo := sqlite.NewEventRepository(db)
	metricsSvc := metrics.NewService(metricsRepo, accRepo)
	broadcaster := proxy.NewBroadcaster(100)

	return db, accRepo, quotaRepo, metricsRepo, metricsSvc, broadcaster, eventRepo
}

func TestServer_StaticFiles(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)

	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	// 1. Root /
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for /, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Antigravity Account Switcher") {
		t.Errorf("expected HTML body to contain title, got: %s", string(body[:min(len(body), 200)]))
	}

	// 2. /dist/app.js
	respJS, err := http.Get(ts.URL + "/dist/app.js")
	if err != nil {
		t.Fatalf("GET /dist/app.js failed: %v", err)
	}
	defer respJS.Body.Close()
	if respJS.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for /dist/app.js, got %d", respJS.StatusCode)
	}
	if !strings.Contains(respJS.Header.Get("Content-Type"), "javascript") {
		t.Errorf("expected javascript content-type, got %s", respJS.Header.Get("Content-Type"))
	}

	// 3. /dist/style.css
	respCSS, err := http.Get(ts.URL + "/dist/style.css")
	if err != nil {
		t.Fatalf("GET /dist/style.css failed: %v", err)
	}
	defer respCSS.Body.Close()
	if respCSS.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for /dist/style.css, got %d", respCSS.StatusCode)
	}

	// 4. SPA fallback
	respSPA, err := http.Get(ts.URL + "/dashboard/subpage")
	if err != nil {
		t.Fatalf("GET SPA fallback failed: %v", err)
	}
	defer respSPA.Body.Close()
	if respSPA.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for SPA fallback, got %d", respSPA.StatusCode)
	}
}

func TestServer_StaticFiles_NoExternalDependencies(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)

	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	// 1. Validate index.html has no CDN script dependencies (pure offline capable)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)
	body := string(bodyBytes)

	if strings.Contains(body, "cdn.tailwindcss.com") {
		t.Errorf("expected index.html to be completely free of cdn.tailwindcss.com dependency")
	}
	if !strings.Contains(body, "timeline-chart-card") {
		t.Errorf("expected index.html to contain chart card container")
	}
	if !strings.Contains(body, "chart-floating-tooltip") {
		t.Errorf("expected index.html to contain chart-floating-tooltip element")
	}
	if !strings.Contains(body, "chart-inspector") {
		t.Errorf("expected index.html to contain chart-inspector element")
	}
	if !strings.Contains(body, "toast-container") {
		t.Errorf("expected index.html to contain toast-container element")
	}

	// 2. Validate style.css contains design tokens and chart tooltip styling
	respCSS, err := http.Get(ts.URL + "/dist/style.css")
	if err != nil {
		t.Fatalf("GET /dist/style.css failed: %v", err)
	}
	defer respCSS.Body.Close()
	cssBytes, _ := io.ReadAll(respCSS.Body)
	css := string(cssBytes)

	if !strings.Contains(css, "--bg-canvas") {
		t.Errorf("expected style.css to contain design tokens like --bg-canvas")
	}
	if !strings.Contains(css, "chart-floating-tooltip") {
		t.Errorf("expected style.css to contain chart-floating-tooltip styling")
	}
	if !strings.Contains(css, "overflow: visible") {
		t.Errorf("expected style.css to contain overflow: visible for chart elements to prevent clipping")
	}

	// 3. Validate app.js contains tooltip clamping and non-blocking toast logic
	respJS, err := http.Get(ts.URL + "/dist/app.js")
	if err != nil {
		t.Fatalf("GET /dist/app.js failed: %v", err)
	}
	defer respJS.Body.Close()
	jsBytes, _ := io.ReadAll(respJS.Body)
	js := string(jsBytes)

	if !strings.Contains(js, "chartTooltip") {
		t.Errorf("expected app.js to contain chartTooltip logic")
	}
	if !strings.Contains(js, "showToast") {
		t.Errorf("expected app.js to contain showToast notification logic")
	}
	if !strings.Contains(js, "updateCooldownTimers") {
		t.Errorf("expected app.js to contain live cooldown timer update ticker")
	}
	if !strings.Contains(js, "togglePrivacyMode") {
		t.Errorf("expected app.js to contain togglePrivacyMode handler")
	}
	if !strings.Contains(js, "redactEmails") {
		t.Errorf("expected app.js to contain redactEmails helper")
	}
}

func TestServer_PrivacyModeStaticIntegration(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)

	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	// 1. Verify index.html contains Privacy Mode button and attributes
	respHTML, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	defer respHTML.Body.Close()
	htmlBytes, _ := io.ReadAll(respHTML.Body)
	html := string(htmlBytes)

	if !strings.Contains(html, `id="btn-privacy"`) {
		t.Errorf("expected index.html to contain #btn-privacy element")
	}
	if !strings.Contains(html, `aria-label="Toggle Privacy Mode"`) {
		t.Errorf("expected index.html to contain accessible aria-label on privacy button")
	}

	// 2. Verify style.css contains Privacy Mode blur and styling rules
	respCSS, err := http.Get(ts.URL + "/dist/style.css")
	if err != nil {
		t.Fatalf("GET /dist/style.css failed: %v", err)
	}
	defer respCSS.Body.Close()
	cssBytes, _ := io.ReadAll(respCSS.Body)
	css := string(cssBytes)

	for _, rule := range []string{
		"body.privacy-mode .route-email",
		"body.privacy-mode .account-email",
		"body.privacy-mode .blurred-email",
		"filter: blur(",
		".btn-privacy-active",
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("expected style.css to contain rule %q", rule)
		}
	}

	// 3. Verify app.js contains Privacy Mode logic, key shortcut, and localStorage key
	respJS, err := http.Get(ts.URL + "/dist/app.js")
	if err != nil {
		t.Fatalf("GET /dist/app.js failed: %v", err)
	}
	defer respJS.Body.Close()
	jsBytes, _ := io.ReadAll(respJS.Body)
	js := string(jsBytes)

	for _, token := range []string{
		"antigravity_privacy_mode",
		"togglePrivacyMode",
		"updatePrivacyUI",
		"redactEmails",
		"formatLogMessage",
		"blurred-email",
		"[redacted@email.com]",
	} {
		if !strings.Contains(js, token) {
			t.Errorf("expected app.js to contain token %q", token)
		}
	}
}

func TestServer_API_Status(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)

	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil, WithVersion("1.2.3"))
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	var status StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status JSON failed: %v", err)
	}

	if status.Status != "ok" {
		t.Errorf("expected status 'ok', got %q", status.Status)
	}
	if status.Version != "1.2.3" {
		t.Errorf("expected version '1.2.3', got %q", status.Version)
	}
	if status.TotalAccounts != 0 {
		t.Errorf("expected 0 accounts, got %d", status.TotalAccounts)
	}
}

func TestServer_API_Accounts_CRUD_And_Select(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)
	ctx := context.Background()

	// Seed 2 accounts
	now := time.Now().UTC()
	acc1 := &domain.Account{
		ID:          "acc-1",
		Email:       "user1@gmail.com",
		AccessToken: "tok-1",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	acc2 := &domain.Account{
		ID:          "acc-2",
		Email:       "user2@gmail.com",
		AccessToken: "tok-2",
		IsActive:    false,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := accRepo.Create(ctx, acc1); err != nil {
		t.Fatalf("create acc1: %v", err)
	}
	if err := accRepo.Create(ctx, acc2); err != nil {
		t.Fatalf("create acc2: %v", err)
	}

	// Seed quota buckets for acc1
	b1 := &domain.QuotaBucket{
		AccountID:         "acc-1",
		BucketID:          "gemini-2.5-pro",
		DisplayName:       "Gemini 2.5 Pro",
		Window:            domain.QuotaWindowDaily,
		RemainingFraction: 0.75,
		RemainingAmount:   750,
		ResetTime:         now.Add(2 * time.Hour),
		UpdatedAt:         now,
	}
	if err := quotaRepo.UpsertBuckets(ctx, []*domain.QuotaBucket{b1}); err != nil {
		t.Fatalf("upsert bucket: %v", err)
	}

	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	// 1. List accounts
	respList, err := http.Get(ts.URL + "/api/accounts")
	if err != nil {
		t.Fatalf("GET /api/accounts: %v", err)
	}
	defer respList.Body.Close()

	var list []*AccountWithBuckets
	if err := json.NewDecoder(respList.Body).Decode(&list); err != nil {
		t.Fatalf("decode accounts list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(list))
	}

	// Verify buckets attached to acc1
	var foundAcc1 *AccountWithBuckets
	for _, a := range list {
		if a.ID == "acc-1" {
			foundAcc1 = a
		}
	}
	if foundAcc1 == nil || len(foundAcc1.Buckets) != 1 {
		t.Fatalf("acc-1 missing expected quota bucket")
	}

	// 2. Select acc-2 as active
	respSelect, err := http.Post(ts.URL+"/api/accounts/acc-2/select", "application/json", nil)
	if err != nil {
		t.Fatalf("POST select acc-2: %v", err)
	}
	defer respSelect.Body.Close()
	if respSelect.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for select, got %d", respSelect.StatusCode)
	}

	active, err := accRepo.GetActive(ctx)
	if err != nil || active.ID != "acc-2" {
		t.Fatalf("expected acc-2 to be active, got: %v", active)
	}

	// 3. Delete acc-1
	reqDel, err := http.NewRequest(http.MethodDelete, ts.URL+"/api/accounts/acc-1", nil)
	if err != nil {
		t.Fatalf("create delete req: %v", err)
	}
	respDel, err := http.DefaultClient.Do(reqDel)
	if err != nil {
		t.Fatalf("DELETE acc-1: %v", err)
	}
	defer respDel.Body.Close()
	if respDel.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for delete, got %d", respDel.StatusCode)
	}

	remaining, _ := accRepo.List(ctx)
	if len(remaining) != 1 || remaining[0].ID != "acc-2" {
		t.Fatalf("expected 1 remaining account acc-2, got: %v", remaining)
	}
}

func TestServer_API_Metrics(t *testing.T) {
	_, accRepo, quotaRepo, metricsRepo, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)
	ctx := context.Background()

	// Seed account and metrics
	now := time.Now().UTC()
	acc := &domain.Account{
		ID:          "acc-1",
		Email:       "user1@gmail.com",
		AccessToken: "tok-1",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := accRepo.Create(ctx, acc); err != nil {
		t.Fatalf("create account: %v", err)
	}

	m := &domain.TokenMetric{
		AccountID:        "acc-1",
		RequestPath:      "/v1internal:generateContent",
		PromptTokens:     100,
		CandidatesTokens: 50,
		TotalTokens:      150,
		Timestamp:        now,
	}
	if err := metricsRepo.Record(ctx, m); err != nil {
		t.Fatalf("record metric: %v", err)
	}

	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/metrics")
	if err != nil {
		t.Fatalf("GET /api/metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	var payload domain.MetricsDashboardPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode metrics payload: %v", err)
	}

	if payload.Summary.AllTime.TotalTokens != 150 {
		t.Errorf("expected 150 all time tokens, got %d", payload.Summary.AllTime.TotalTokens)
	}

	// Test with tz and tz_offset parameters (e.g. America/Sao_Paulo)
	respTZ, err := http.Get(ts.URL + "/api/metrics?tz=America/Sao_Paulo&tz_offset=-180")
	if err != nil {
		t.Fatalf("GET /api/metrics?tz=...: %v", err)
	}
	defer respTZ.Body.Close()

	if respTZ.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", respTZ.StatusCode)
	}

	var payloadTZ domain.MetricsDashboardPayload
	if err := json.NewDecoder(respTZ.Body).Decode(&payloadTZ); err != nil {
		t.Fatalf("decode tz metrics payload: %v", err)
	}

	locSP, _ := time.LoadLocation("America/Sao_Paulo")
	expectedTodaySP := time.Now().In(locSP).Format("2006-01-02")
	if len(payloadTZ.Timeline) != 14 {
		t.Fatalf("expected 14 timeline items, got %d", len(payloadTZ.Timeline))
	}
	lastItem := payloadTZ.Timeline[len(payloadTZ.Timeline)-1]
	if lastItem.Date != expectedTodaySP {
		t.Errorf("expected last timeline item to be %s (SP today), got %s", expectedTodaySP, lastItem.Date)
	}
}

func TestServer_API_Events_SSE(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)

	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %s", resp.Header.Get("Content-Type"))
	}

	reader := bufio.NewReader(resp.Body)

	// Broadcast an event
	go func() {
		time.Sleep(50 * time.Millisecond)
		broadcaster.Broadcast(&domain.ProxyEvent{
			Type:      domain.EventTypeFailover429,
			AccountID: "acc-test",
			Message:   "Simulated 429 failover event",
			Timestamp: time.Now().UTC(),
		})
	}()

	// Read SSE line
	received := false
	for i := 0; i < 10; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, "data:") && strings.Contains(line, "Simulated 429 failover event") {
			received = true
			break
		}
	}

	if !received {
		t.Errorf("did not receive broadcasted SSE event within timeout")
	}
}

func TestServer_ProxyInterception(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)

	proxyHandled := false
	mockProxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHandled = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"proxy":"intercepted"}`))
	})

	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil, WithProxyHandler(mockProxy))
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	// 1. Path starting with /v1internal:
	resp1, err := http.Get(ts.URL + "/v1internal:retrieveUserQuotaSummary")
	if err != nil {
		t.Fatalf("GET /v1internal: %v", err)
	}
	_ = resp1.Body.Close()
	if !proxyHandled {
		t.Errorf("expected mock proxy to handle /v1internal request")
	}

	// 2. Stream generation
	proxyHandled = false
	resp2, err := http.Post(ts.URL+"/v1internal:streamGenerateContent?alt=sse", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST /v1internal stream: %v", err)
	}
	_ = resp2.Body.Close()
	if !proxyHandled {
		t.Errorf("expected mock proxy to handle streamGenerateContent request")
	}
}

func TestServer_Lifecycle_StartStop(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)

	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil,
		WithPort(0), // ephemeral port
		WithBindAddr("127.0.0.1"),
	)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	if err := server.Start(); err != nil {
		t.Fatalf("server.Start() failed: %v", err)
	}

	port := server.Port()
	if port <= 0 {
		t.Fatalf("expected valid port > 0, got %d", port)
	}

	// Verify server responds
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/status", port))
	if err != nil {
		t.Fatalf("GET /api/status on ephemeral server: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", resp.StatusCode)
	}

	// Stop server
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Stop(shutdownCtx); err != nil {
		t.Fatalf("server.Stop() failed: %v", err)
	}
}

func TestServer_ConcurrentWorkload_RaceDetector(t *testing.T) {
	_, accRepo, quotaRepo, metricsRepo, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)
	ctx := context.Background()

	// Seed 3 accounts
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("acc-race-%d", i)
		acc := &domain.Account{
			ID:          id,
			Email:       fmt.Sprintf("race%d@example.com", i),
			AccessToken: fmt.Sprintf("tok-%d", i),
			IsActive:    i == 0,
			Status:      domain.AccountStatusActive,
		}
		_ = accRepo.Create(ctx, acc)
	}

	mockProxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil, WithProxyHandler(mockProxy))
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	var wg sync.WaitGroup
	workers := 10

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			client := &http.Client{Timeout: 2 * time.Second}

			for j := 0; j < 10; j++ {
				switch j % 5 {
				case 0:
					resp, err := client.Get(ts.URL + "/")
					if err == nil {
						_ = resp.Body.Close()
					}
				case 1:
					resp, err := client.Get(ts.URL + "/api/status")
					if err == nil {
						_ = resp.Body.Close()
					}
				case 2:
					resp, err := client.Get(ts.URL + "/api/accounts")
					if err == nil {
						_ = resp.Body.Close()
					}
				case 3:
					resp, err := client.Get(ts.URL + "/api/metrics")
					if err == nil {
						_ = resp.Body.Close()
					}
				case 4:
					targetAcc := fmt.Sprintf("acc-race-%d", (workerID+j)%3)
					resp, err := client.Post(fmt.Sprintf("%s/api/accounts/%s/select", ts.URL, targetAcc), "application/json", nil)
					if err == nil {
						_ = resp.Body.Close()
					}
				}

				// Concurrent metric record and broadcast
				_ = metricsRepo.Record(context.Background(), &domain.TokenMetric{
					AccountID:        "acc-race-0",
					PromptTokens:     10,
					CandidatesTokens: 5,
					TotalTokens:      15,
					Timestamp:        time.Now().UTC(),
				})
				broadcaster.Broadcast(&domain.ProxyEvent{
					Type:      domain.EventTypeTokensCaptured,
					Message:   "Concurrent token captured",
					Timestamp: time.Now().UTC(),
				})
			}
		}(i)
	}

	wg.Wait()
}

type mockFallbackSetter struct {
	mu        sync.Mutex
	primary   string
	secondary string
	enabled   bool
	calls     int
}

func (m *mockFallbackSetter) SetFallbackConfig(p, s string, e bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.primary = p
	m.secondary = s
	m.enabled = e
	m.calls++
}

func (m *mockFallbackSetter) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func TestServer_ConfigAPI(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)

	initialCfg := &config.Config{
		Port:                     8080,
		ModelPrimary:             "gemini-2.5-pro",
		ModelSecondary:           "gemini-2.5-flash",
		FallbackSecondaryEnabled: false,
	}
	setter := &mockFallbackSetter{}

	server, err := NewServer(
		accRepo,
		quotaRepo,
		metricsSvc,
		broadcaster,
		eventRepo,
		nil,
		WithConfig(initialCfg),
		WithFallbackConfigSetter(setter),
	)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	// 1. GET /api/config
	resp, err := http.Get(ts.URL + "/api/config")
	if err != nil {
		t.Fatalf("GET /api/config failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	var cfgResp ConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&cfgResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if cfgResp.ModelPrimary != "gemini-2.5-pro" || cfgResp.ModelSecondary != "gemini-2.5-flash" || cfgResp.FallbackSecondaryEnabled {
		t.Errorf("unexpected initial config response: %+v", cfgResp)
	}

	// 2. POST /api/config with updates
	updatePayload := `{"model_primary":"gemini-3.8-flash-high","model_secondary":"claude-sonnet-4-6","fallback_secondary_enabled":true}`
	respPost, err := http.Post(ts.URL+"/api/config", "application/json", strings.NewReader(updatePayload))
	if err != nil {
		t.Fatalf("POST /api/config failed: %v", err)
	}
	defer respPost.Body.Close()

	if respPost.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from POST /api/config, got %d", respPost.StatusCode)
	}
	var updatedResp ConfigResponse
	if err := json.NewDecoder(respPost.Body).Decode(&updatedResp); err != nil {
		t.Fatalf("failed to decode post response: %v", err)
	}
	if updatedResp.ModelPrimary != "gemini-3.8-flash-high" || updatedResp.ModelSecondary != "claude-sonnet-4-6" || !updatedResp.FallbackSecondaryEnabled {
		t.Errorf("unexpected updated response: %+v", updatedResp)
	}

	// Verify fallback setter was invoked
	setter.mu.Lock()
	if setter.calls != 1 || setter.primary != "gemini-3.8-flash-high" || setter.secondary != "claude-sonnet-4-6" || !setter.enabled {
		t.Errorf("setter state unexpected: calls=%d, primary=%s, sec=%s, enabled=%t", setter.calls, setter.primary, setter.secondary, setter.enabled)
	}
	setter.mu.Unlock()

	// 3. Re-query GET /api/config to verify cache
	respGet2, err := http.Get(ts.URL + "/api/config")
	if err != nil {
		t.Fatalf("GET /api/config #2 failed: %v", err)
	}
	defer respGet2.Body.Close()
	var cfgResp2 ConfigResponse
	if err := json.NewDecoder(respGet2.Body).Decode(&cfgResp2); err != nil {
		t.Fatalf("failed to decode response #2: %v", err)
	}
	if cfgResp2.ModelPrimary != "gemini-3.8-flash-high" || !cfgResp2.FallbackSecondaryEnabled {
		t.Errorf("config was not updated in subsequent GET: %+v", cfgResp2)
	}

	// 4. POST /api/config with invalid JSON
	respBad, err := http.Post(ts.URL+"/api/config", "application/json", strings.NewReader("invalid-json"))
	if err != nil {
		t.Fatalf("POST bad json failed: %v", err)
	}
	defer respBad.Body.Close()
	if respBad.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for malformed json, got %d", respBad.StatusCode)
	}
}

func TestServer_ModelsAPI(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)

	server, err := NewServer(
		accRepo,
		quotaRepo,
		metricsSvc,
		broadcaster,
		eventRepo,
		nil,
	)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	// 1. GET /api/models
	resp, err := http.Get(ts.URL + "/api/models")
	if err != nil {
		t.Fatalf("GET /api/models failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	var modelsResp ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		t.Fatalf("failed to decode models response: %v", err)
	}

	if len(modelsResp.Models) == 0 {
		t.Fatal("expected at least one model in response")
	}
	if modelsResp.Source == "" {
		t.Error("expected non-empty source in models response")
	}

	foundClaude := false
	foundGemini := false
	for _, m := range modelsResp.Models {
		if m.ID == "" {
			t.Error("expected model to have non-empty ID")
		}
		if m.DisplayName == "" {
			t.Errorf("expected model %s to have display name", m.ID)
		}
		if m.Category == "claude_gpt" {
			foundClaude = true
		}
		if m.Category == "gemini" {
			foundGemini = true
		}
	}

	if !foundGemini {
		t.Error("expected Gemini model in models response")
	}
	if !foundClaude {
		t.Error("expected Claude/GPT model in models response")
	}

	// 2. Method Not Allowed check
	respPost, err := http.Post(ts.URL+"/api/models", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/models failed: %v", err)
	}
	defer respPost.Body.Close()
	if respPost.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 Method Not Allowed for POST /api/models, got %d", respPost.StatusCode)
	}
}

// newConfigTestServer starts a web server whose live config is liveCfg and
// returns it with the recording fallback setter. The config directory is the
// per-test temp dir created by setupTestWeb.
func newConfigTestServer(t *testing.T, liveCfg *config.Config) (*httptest.Server, *mockFallbackSetter) {
	t.Helper()
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)
	setter := &mockFallbackSetter{}
	server, err := NewServer(
		accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil,
		WithConfig(liveCfg),
		WithFallbackConfigSetter(setter),
	)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	return ts, setter
}

func postConfig(t *testing.T, ts *httptest.Server, payload string) int {
	t.Helper()
	resp, err := http.Post(ts.URL+"/api/config", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST /api/config failed: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func getConfigResponse(t *testing.T, ts *httptest.Server) ConfigResponse {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/config")
	if err != nil {
		t.Fatalf("GET /api/config failed: %v", err)
	}
	defer resp.Body.Close()
	var cfgResp ConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&cfgResp); err != nil {
		t.Fatalf("failed to decode GET /api/config: %v", err)
	}
	return cfgResp
}

func assertConfigResponse(t *testing.T, got, want ConfigResponse) {
	t.Helper()
	if got != want {
		t.Errorf("config response = %+v, want %+v", got, want)
	}
}

func TestServer_ConfigAPI_InvalidUpdateLeavesLiveConfigUntouched(t *testing.T) {
	original := ConfigResponse{ModelPrimary: "gemini-2.5-pro", ModelSecondary: "gemini-2.5-flash", FallbackSecondaryEnabled: false}
	ts, setter := newConfigTestServer(t, &config.Config{
		Port:           8080,
		ModelPrimary:   original.ModelPrimary,
		ModelSecondary: original.ModelSecondary,
	})

	status := postConfig(t, ts, `{"model_secondary":"gemini-2.5-pro","fallback_secondary_enabled":true}`)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for identical primary/secondary, got %d", status)
	}

	assertConfigResponse(t, getConfigResponse(t, ts), original)
	if setter.callCount() != 0 {
		t.Errorf("fallback setter must not be called on a rejected update, got %d calls", setter.callCount())
	}
	if _, err := os.Stat(config.ConfigFilePath()); !os.IsNotExist(err) {
		t.Errorf("config file must not be written on a rejected update, stat err = %v", err)
	}
}

func TestServer_ConfigAPI_SaveDoesNotPersistRuntimeOverrides(t *testing.T) {
	// The live config carries values that came from flags or environment
	// variables. Only the three fields this endpoint edits may reach disk.
	live := &config.Config{
		Port:           9999,
		UpstreamURL:    "http://override.invalid",
		ClientSecret:   "secret-from-env",
		ModelPrimary:   "gemini-2.5-pro",
		ModelSecondary: "gemini-2.5-flash",
	}
	ts, _ := newConfigTestServer(t, live)

	onDisk := config.DefaultConfig()
	onDisk.Port = 7001
	onDisk.UpstreamURL = "https://disk.example.com"
	onDisk.QuotaInterval = "10m"
	onDisk.ModelPrimary = "gemini-2.5-pro"
	onDisk.ModelSecondary = "gemini-2.5-flash"
	if err := config.Save(onDisk); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if status := postConfig(t, ts, `{"model_primary":"gemini-3.8-flash-high"}`); status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}

	saved, err := config.LoadDiskConfig()
	if err != nil {
		t.Fatalf("LoadDiskConfig: %v", err)
	}
	if saved.ModelPrimary != "gemini-3.8-flash-high" {
		t.Errorf("model_primary on disk = %q, want the edited value", saved.ModelPrimary)
	}
	if saved.Port != 7001 {
		t.Errorf("port on disk = %d, want 7001 (runtime override 9999 leaked)", saved.Port)
	}
	if saved.UpstreamURL != "https://disk.example.com" {
		t.Errorf("upstream_url on disk = %q, runtime override leaked", saved.UpstreamURL)
	}
	if saved.QuotaInterval != "10m" {
		t.Errorf("quota_interval on disk = %q, want 10m", saved.QuotaInterval)
	}
	if saved.ClientSecret != "" {
		t.Errorf("client_secret from the environment was written to disk")
	}

	// The running config keeps its overrides and picks up the edit.
	if live.Port != 9999 || live.ClientSecret != "secret-from-env" {
		t.Errorf("live config lost its overrides: %+v", live)
	}
	if live.ModelPrimary != "gemini-3.8-flash-high" {
		t.Errorf("live model_primary = %q, want the edited value", live.ModelPrimary)
	}
}

func TestServer_ConfigAPI_SaveFailureReturns500AndKeepsLiveConfig(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("cannot make the config directory unwritable on this platform or as root")
	}

	original := ConfigResponse{ModelPrimary: "gemini-2.5-pro", ModelSecondary: "gemini-2.5-flash", FallbackSecondaryEnabled: false}
	ts, setter := newConfigTestServer(t, &config.Config{
		Port:           8080,
		ModelPrimary:   original.ModelPrimary,
		ModelSecondary: original.ModelSecondary,
	})

	dir := config.ConfigDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod config dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	status := postConfig(t, ts, `{"model_primary":"gemini-3.8-flash-high"}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("expected 500 when the config cannot be saved, got %d", status)
	}

	assertConfigResponse(t, getConfigResponse(t, ts), original)
	if setter.callCount() != 0 {
		t.Errorf("fallback setter must not be called when saving fails, got %d calls", setter.callCount())
	}
}

func TestServer_ConfigAPI_UnreadableDiskConfigReturns500(t *testing.T) {
	original := ConfigResponse{ModelPrimary: "gemini-2.5-pro", ModelSecondary: "gemini-2.5-flash", FallbackSecondaryEnabled: false}
	ts, setter := newConfigTestServer(t, &config.Config{
		Port:           8080,
		ModelPrimary:   original.ModelPrimary,
		ModelSecondary: original.ModelSecondary,
	})

	corrupt := []byte("{ not json")
	if err := os.WriteFile(config.ConfigFilePath(), corrupt, 0o600); err != nil {
		t.Fatalf("write corrupt config: %v", err)
	}

	status := postConfig(t, ts, `{"model_primary":"gemini-3.8-flash-high"}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("expected 500 when the on-disk config cannot be parsed, got %d", status)
	}

	assertConfigResponse(t, getConfigResponse(t, ts), original)
	if setter.callCount() != 0 {
		t.Errorf("fallback setter must not be called, got %d calls", setter.callCount())
	}
	if data, _ := os.ReadFile(config.ConfigFilePath()); string(data) != string(corrupt) {
		t.Errorf("unparseable config file was overwritten: %q", data)
	}
}
