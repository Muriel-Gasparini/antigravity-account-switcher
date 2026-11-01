package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/config"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

const originTestConfigBody = `{"model_primary":"gemini-3.8-flash-high"}`

type originTestEnv struct {
	ts      *httptest.Server
	setter  *mockFallbackSetter
	proxied *bool
	port    string
}

func newOriginTestEnv(t *testing.T) *originTestEnv {
	t.Helper()
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)

	now := time.Now().UTC()
	if err := accRepo.Create(context.Background(), &domain.Account{
		ID: "acc-1", Email: "user1@gmail.com", AccessToken: "tok-1",
		IsActive: true, Status: domain.AccountStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}

	proxied := false
	mockProxy := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied = true
		w.WriteHeader(http.StatusOK)
	})
	setter := &mockFallbackSetter{}
	server, err := NewServer(
		accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil,
		WithConfig(&config.Config{
			Port:           8080,
			ModelPrimary:   "gemini-2.5-pro",
			ModelSecondary: "gemini-2.5-flash",
		}),
		WithFallbackConfigSetter(setter),
		WithProxyHandler(mockProxy),
	)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	return &originTestEnv{ts: ts, setter: setter, proxied: &proxied, port: u.Port()}
}

func (e *originTestEnv) do(t *testing.T, method, path, body string, headers map[string]string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data), resp.Header
}

func (e *originTestEnv) currentPrimary(t *testing.T) string {
	t.Helper()
	status, body, _ := e.do(t, http.MethodGet, "/api/config", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/config: status %d", status)
	}
	var cfg ConfigResponse
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return cfg.ModelPrimary
}

func TestOriginCheck_CrossOriginConfigPostRejected(t *testing.T) {
	env := newOriginTestEnv(t)

	origins := []string{
		"https://evil.example",
		"http://evil.example",
		"http://evil.example:" + env.port, // DNS rebinding: right port, wrong host
		"http://localhost:1",              // right host, wrong port
		"https://localhost:" + env.port,   // right host and port, wrong scheme
		"http://localhost.evil.example:" + env.port,
		"null",
	}
	for _, origin := range origins {
		t.Run(origin, func(t *testing.T) {
			status, body, _ := env.do(t, http.MethodPost, "/api/config", originTestConfigBody,
				map[string]string{"Origin": origin, "Content-Type": "text/plain"})
			if status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %s)", status, body)
			}
			var errResp struct {
				Error struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(body), &errResp); err != nil || errResp.Error.Code != http.StatusForbidden || errResp.Error.Message == "" {
				t.Fatalf("expected JSON error body with code 403, got %q (err %v)", body, err)
			}
			if got := env.currentPrimary(t); got != "gemini-2.5-pro" {
				t.Fatalf("config changed by rejected request: model_primary = %q", got)
			}
			env.setter.mu.Lock()
			calls := env.setter.calls
			env.setter.mu.Unlock()
			if calls != 0 {
				t.Fatalf("fallback setter called %d times by rejected request", calls)
			}
		})
	}
}

func TestOriginCheck_SameOriginAllowed(t *testing.T) {
	forms := []string{
		"http://localhost:%s",
		"http://127.0.0.1:%s",
		"http://[::1]:%s",
	}
	for _, form := range forms {
		t.Run(form, func(t *testing.T) {
			env := newOriginTestEnv(t)
			origin := fmt.Sprintf(form, env.port)
			status, body, _ := env.do(t, http.MethodPost, "/api/config", originTestConfigBody,
				map[string]string{"Origin": origin, "Sec-Fetch-Site": "same-origin"})
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", status, body)
			}
			if got := env.currentPrimary(t); got != "gemini-3.8-flash-high" {
				t.Fatalf("model_primary = %q, want update applied", got)
			}
		})
	}
}

func TestOriginCheck_FetchMetadataFallback(t *testing.T) {
	tests := []struct {
		name       string
		secFetch   string
		wantStatus int
	}{
		{"cross-site rejected", "cross-site", http.StatusForbidden},
		{"cross-site mixed case rejected", "Cross-Site", http.StatusForbidden},
		{"same-origin allowed", "same-origin", http.StatusOK},
		{"none allowed", "none", http.StatusOK},
		{"absent allowed", "", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newOriginTestEnv(t)
			headers := map[string]string{}
			if tt.secFetch != "" {
				headers["Sec-Fetch-Site"] = tt.secFetch
			}
			status, body, _ := env.do(t, http.MethodPost, "/api/config", originTestConfigBody, headers)
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", status, tt.wantStatus, body)
			}
			wantPrimary := "gemini-2.5-pro"
			if tt.wantStatus == http.StatusOK {
				wantPrimary = "gemini-3.8-flash-high"
			}
			if got := env.currentPrimary(t); got != wantPrimary {
				t.Fatalf("model_primary = %q, want %q", got, wantPrimary)
			}
		})
	}
}

func TestOriginCheck_MatchingOriginWinsOverFetchMetadata(t *testing.T) {
	env := newOriginTestEnv(t)
	// Origin is authoritative when present; Sec-Fetch-Site is only a fallback.
	status, body, _ := env.do(t, http.MethodPost, "/api/config", originTestConfigBody, map[string]string{
		"Origin":         "http://127.0.0.1:" + env.port,
		"Sec-Fetch-Site": "cross-site",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
}

func TestOriginCheck_NoOriginHeadersAllowed(t *testing.T) {
	env := newOriginTestEnv(t)
	status, body, _ := env.do(t, http.MethodPost, "/api/config", originTestConfigBody, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	if got := env.currentPrimary(t); got != "gemini-3.8-flash-high" {
		t.Fatalf("model_primary = %q, want update applied", got)
	}
}

func TestOriginCheck_CoversAllStateChangingManagementEndpoints(t *testing.T) {
	cross := map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}
	tests := []struct {
		method, path string
	}{
		{http.MethodPut, "/api/config"},
		{http.MethodPost, "/api/accounts/acc-1/select"},
		{http.MethodDelete, "/api/accounts/acc-1"},
		{http.MethodPost, "/api/quota/refresh"},
		{http.MethodPost, "/oauth/start"},
		{http.MethodDelete, "/oauth/start"},
		{http.MethodPost, "/api/oauth/start"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			env := newOriginTestEnv(t)
			status, body, _ := env.do(t, tt.method, tt.path, `{}`, cross)
			if status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %s)", status, body)
			}
			if *env.proxied {
				t.Fatalf("rejected request reached the proxy handler")
			}
			status, body, _ = env.do(t, http.MethodGet, "/api/accounts/acc-1", "", nil)
			if status != http.StatusOK {
				t.Fatalf("account lookup after rejected request: status %d (body %s)", status, body)
			}
		})
	}
}

func TestOriginCheck_ReadOnlyRequestsUnaffected(t *testing.T) {
	env := newOriginTestEnv(t)
	cross := map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}

	for _, path := range []string{"/api/status", "/api/config", "/api/accounts", "/api/models", "/"} {
		status, body, _ := env.do(t, http.MethodGet, path, "", cross)
		if status != http.StatusOK {
			t.Errorf("GET %s with cross-site headers: status %d, want 200 (body %.80s)", path, status, body)
		}
	}
	for _, method := range []string{http.MethodHead, http.MethodOptions} {
		status, _, _ := env.do(t, method, "/api/config", "", cross)
		if status == http.StatusForbidden {
			t.Errorf("%s /api/config was rejected as cross-origin; only state-changing methods are checked", method)
		}
	}
}

func TestOriginCheck_ProxyTrafficUnaffected(t *testing.T) {
	env := newOriginTestEnv(t)
	status, _, _ := env.do(t, http.MethodPost, "/v1internal:streamGenerateContent?alt=sse", `{}`,
		map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"})
	if status != http.StatusOK || !*env.proxied {
		t.Fatalf("proxy request: status %d, proxied %v; the origin check must only guard the management API", status, *env.proxied)
	}
}

func TestOAuthStart_OnlyPost(t *testing.T) {
	env := newOriginTestEnv(t)
	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPut, http.MethodHead} {
		for _, path := range []string{"/oauth/start", "/api/oauth/start"} {
			status, body, hdr := env.do(t, method, path, "", nil)
			if status != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: status %d, want 405 (body %.80s)", method, path, status, body)
				continue
			}
			if allow := hdr.Get("Allow"); allow != http.MethodPost {
				t.Errorf("%s %s: Allow = %q, want POST", method, path, allow)
			}
		}
	}

	// POST still reaches the handler (no engine is configured in this fixture).
	status, _, _ := env.do(t, http.MethodPost, "/oauth/start", "", nil)
	if status != http.StatusNotImplemented {
		t.Errorf("POST /oauth/start: status %d, want 501 from the handler", status)
	}
}
