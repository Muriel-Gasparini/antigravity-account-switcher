package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/metrics"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/proxy"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
)

func setupTestWeb(t *testing.T) (*sqlite.DB, domain.AccountRepository, domain.QuotaRepository, domain.MetricsRepository, *metrics.Service, *proxy.Broadcaster, domain.EventRepository) {
	t.Helper()
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
