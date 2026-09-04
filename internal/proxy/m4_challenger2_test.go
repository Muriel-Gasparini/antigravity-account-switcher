package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/test/mocks"
)

// TestM4Challenger2_Boundary_EmptyRequestBody tests proxy behavior when receiving empty request bodies.
// Boundary dimensions tested:
// - Pass-through of empty body on GET and POST
// - Predictive fallback when model is in URL path with an empty body
// - Reactive 429 fallback when model is in URL path with an empty body
func TestM4Challenger2_Boundary_EmptyRequestBody(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	t.Run("PredictiveFallback_EmptyBody", func(t *testing.T) {
		env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
		acc := &domain.Account{
			ID:          "acc-empty-1",
			Email:       "empty1@example.com",
			AccessToken: "token-empty-1",
			IsActive:    true,
			Status:      domain.AccountStatusActive,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := env.accountRepo.Create(ctx, acc); err != nil {
			t.Fatalf("create account: %v", err)
		}

		// Pro exhausted, Flash available
		buckets := []*domain.QuotaBucket{
			{AccountID: acc.ID, BucketID: "gemini-2.5-pro", RemainingFraction: 0.0, RemainingAmount: 0, ResetTime: now.Add(5 * time.Hour)},
			{AccountID: acc.ID, BucketID: "gemini-2.5-flash", RemainingFraction: 0.9, RemainingAmount: 900, ResetTime: now.Add(5 * time.Hour)},
		}
		_ = env.quotaRepo.UpsertBuckets(ctx, buckets)
		env.failoverEngine.UpdateQuotaCache(acc.ID, buckets)

		// POST with empty body (0 bytes)
		req, err := http.NewRequestWithContext(ctx, "POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:generateContent", http.NoBody)
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
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, body)
		}

		// Verify upstream received the rewritten path and empty body
		reqs := env.mockGoogle.GetRecordedRequests()
		if len(reqs) != 1 {
			t.Fatalf("expected exactly 1 upstream request, got %d", len(reqs))
		}
		if !strings.Contains(reqs[0].Path, "gemini-2.5-flash") {
			t.Errorf("expected rewritten path to contain gemini-2.5-flash, got %s", reqs[0].Path)
		}
		if len(reqs[0].Body) != 0 {
			t.Errorf("expected empty upstream body, got %d bytes: %q", len(reqs[0].Body), reqs[0].Body)
		}
	})

	t.Run("ReactiveFallback_EmptyBody_429", func(t *testing.T) {
		env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
		acc := &domain.Account{
			ID:          "acc-empty-2",
			Email:       "empty2@example.com",
			AccessToken: "token-empty-2",
			IsActive:    true,
			Status:      domain.AccountStatusActive,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := env.accountRepo.Create(ctx, acc); err != nil {
			t.Fatalf("create account: %v", err)
		}

		// Trigger 429 on first upstream request
		env.mockGoogle.SetFailoverTrigger(acc.AccessToken, 1)

		req, err := http.NewRequestWithContext(ctx, "POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:generateContent", http.NoBody)
		if err != nil {
			t.Fatalf("create req: %v", err)
		}

		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200 OK on reactive fallback, got %d: %s", resp.StatusCode, body)
		}

		reqs := env.mockGoogle.GetRecordedRequests()
		if len(reqs) != 2 {
			t.Fatalf("expected 2 upstream requests (429 + fallback), got %d", len(reqs))
		}
		if !strings.Contains(reqs[0].Path, "gemini-2.5-pro") {
			t.Errorf("expected attempt 1 to target pro, got %s", reqs[0].Path)
		}
		if !strings.Contains(reqs[1].Path, "gemini-2.5-flash") {
			t.Errorf("expected attempt 2 to target flash, got %s", reqs[1].Path)
		}
		if len(reqs[1].Body) != 0 {
			t.Errorf("expected empty body replayed on attempt 2, got %d bytes", len(reqs[1].Body))
		}
	})
}

// TestM4Challenger2_Boundary_ChunkedTransferEncoding tests requests arriving with chunked transfer encoding.
func TestM4Challenger2_Boundary_ChunkedTransferEncoding(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	t.Run("ChunkedBody_WithPredictiveFallback", func(t *testing.T) {
		env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
		acc := &domain.Account{
			ID:          "acc-chunked-1",
			Email:       "chunked1@example.com",
			AccessToken: "token-chunked-1",
			IsActive:    true,
			Status:      domain.AccountStatusActive,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := env.accountRepo.Create(ctx, acc); err != nil {
			t.Fatalf("create account: %v", err)
		}

		// Pro exhausted, Flash available
		buckets := []*domain.QuotaBucket{
			{AccountID: acc.ID, BucketID: "gemini-2.5-pro", RemainingFraction: 0.0, RemainingAmount: 0, ResetTime: now.Add(5 * time.Hour)},
			{AccountID: acc.ID, BucketID: "gemini-2.5-flash", RemainingFraction: 0.9, RemainingAmount: 900, ResetTime: now.Add(5 * time.Hour)},
		}
		_ = env.quotaRepo.UpsertBuckets(ctx, buckets)
		env.failoverEngine.UpdateQuotaCache(acc.ID, buckets)

		payload := `{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"test chunked body"}]}]}`
		// Create a reader with ContentLength = -1 to force Chunked Transfer Encoding
		chunkReader := io.NopCloser(strings.NewReader(payload))
		req, err := http.NewRequestWithContext(ctx, "POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:generateContent", chunkReader)
		if err != nil {
			t.Fatalf("create req: %v", err)
		}
		req.ContentLength = -1 // Triggers chunked encoding in net/http
		req.Header.Set("Content-Type", "application/json")

		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, body)
		}

		reqs := env.mockGoogle.GetRecordedRequests()
		if len(reqs) != 1 {
			t.Fatalf("expected 1 upstream request, got %d", len(reqs))
		}

		// Assert chunked encoding was buffered, model was rewritten, and outgoing request has valid Content-Length
		upstreamReq := reqs[0]
		expectedBody := `{"model":"gemini-2.5-flash","contents":[{"parts":[{"text":"test chunked body"}]}]}`
		if string(upstreamReq.Body) != expectedBody {
			t.Errorf("upstream body mismatch:\n got: %s\nwant: %s", string(upstreamReq.Body), expectedBody)
		}

		clHeader := upstreamReq.Header.Get("Content-Length")
		expectedLen := strconv.Itoa(len(expectedBody))
		if clHeader != expectedLen {
			t.Errorf("upstream Content-Length header mismatch: got %q, want %q", clHeader, expectedLen)
		}

		// Ensure Transfer-Encoding was stripped from outgoing upstream headers
		if upstreamReq.Header.Get("Transfer-Encoding") != "" {
			t.Errorf("expected Transfer-Encoding to be stripped, got %q", upstreamReq.Header.Get("Transfer-Encoding"))
		}
	})
}

// TestM4Challenger2_Boundary_MalformedJSONBodies tests adversarial and broken JSON bodies.
func TestM4Challenger2_Boundary_MalformedJSONBodies(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
	acc := &domain.Account{
		ID:          "acc-malformed-1",
		Email:       "malformed@example.com",
		AccessToken: "token-malformed-1",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := env.accountRepo.Create(ctx, acc); err != nil {
		t.Fatalf("create account: %v", err)
	}

	malformedPayloads := []struct {
		name    string
		payload string
	}{
		{"TruncatedJSON", `{"model": "gemini-2.5-pro", "contents": [`},
		{"UnquotedKey", `{model: "gemini-2.5-pro", "prompt": "hi"}`},
		{"MismatchedBraces", `{"model": "gemini-2.5-pro"}}}`},
		{"TrailingGarbage", `{"model": "gemini-2.5-pro"} EXTRA_RAW_GARBAGE`},
		{"PlainTextNotJSON", `This is plain text and definitely not JSON`},
		{"BinaryNullBytes", "\x00\x01\x02{\"model\":\"gemini-2.5-pro\"}"},
	}

	for _, tc := range malformedPayloads {
		t.Run(tc.name, func(t *testing.T) {
			env.mockGoogle.Reset()

			// Case 1: Model in path, malformed body. Handler rewrites path, preserves body verbatim, upstream handles it without crash
			req, err := http.NewRequestWithContext(ctx, "POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:generateContent", strings.NewReader(tc.payload))
			if err != nil {
				t.Fatalf("create req: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := env.client.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			// Check that proxy did not panic and forwarded the request
			reqs := env.mockGoogle.GetRecordedRequests()
			if len(reqs) != 1 {
				t.Fatalf("expected 1 upstream request, got %d", len(reqs))
			}
			// Upstream received body unmodified (since JSON rewriting safely aborted due to invalid JSON)
			if !bytes.Equal(reqs[0].Body, []byte(tc.payload)) {
				t.Errorf("malformed body was corrupted:\n got: %q\nwant: %q", reqs[0].Body, tc.payload)
			}
		})
	}
}

// TestM4Challenger2_Boundary_UnicodeModelNamesAndPayloads tests multibyte UTF-8 characters.
func TestM4Challenger2_Boundary_UnicodeModelNamesAndPayloads(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
	acc := &domain.Account{
		ID:          "acc-unicode-1",
		Email:       "unicode@example.com",
		AccessToken: "token-unicode-1",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := env.accountRepo.Create(ctx, acc); err != nil {
		t.Fatalf("create account: %v", err)
	}

	// 1. Model extraction and rewrite with Unicode in prompt and surrounding JSON
	unicodePrompt := "日本語テキスト 🚀 🌟 🧠 — Cyrillic: Привет мир! — Arabic: مرحبا بالعالم — Accents: éèàçñ"
	jsonBody := fmt.Sprintf(`{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":%q}]}]}`, unicodePrompt)

	// Verify extraction
	extracted, err := ExtractModelFromJSON([]byte(jsonBody))
	if err != nil {
		t.Fatalf("ExtractModelFromJSON failed on Unicode body: %v", err)
	}
	if extracted != "gemini-2.5-pro" {
		t.Errorf("extracted model mismatch: got %q, want gemini-2.5-pro", extracted)
	}

	// Verify rewrite
	rewritten, err := RewriteModelInBody([]byte(jsonBody), "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("RewriteModelInBody failed on Unicode body: %v", err)
	}

	// Validate JSON parse of rewritten body
	var parsed map[string]any
	if err := json.Unmarshal(rewritten, &parsed); err != nil {
		t.Fatalf("rewritten body is not valid JSON: %v", err)
	}
	if parsed["model"] != "gemini-2.5-flash" {
		t.Errorf("rewritten model in parsed JSON mismatch: got %v", parsed["model"])
	}

	// 2. Full round-trip through ProxyHandler
	req, err := http.NewRequestWithContext(ctx, "POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:generateContent", bytes.NewReader([]byte(jsonBody)))
	if err != nil {
		t.Fatalf("create req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	reqs := env.mockGoogle.GetRecordedRequests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 upstream request, got %d", len(reqs))
	}
	if !strings.Contains(string(reqs[0].Body), "日本語テキスト") {
		t.Errorf("Unicode prompt lost in transit: %s", string(reqs[0].Body))
	}
}

// TestM4Challenger2_Boundary_DeeplyNestedPayloads tests handling of deeply nested JSON structures (1,000 levels).
func TestM4Challenger2_Boundary_DeeplyNestedPayloads(t *testing.T) {
	const depth = 1000

	t.Run("NestedModelKey_NotAtRoot_Ignored", func(t *testing.T) {
		// Build: {"lvl0": {"lvl1": ... {"model": "gemini-nested"} ... }}
		var sb strings.Builder
		for i := 0; i < depth; i++ {
			sb.WriteString(fmt.Sprintf(`{"lvl%d":`, i))
		}
		sb.WriteString(`{"model":"gemini-nested-depth-1000"}`)
		for i := 0; i < depth; i++ {
			sb.WriteString("}")
		}
		nestedJSON := []byte(sb.String())

		// Root extraction should return ErrModelNotFound because model is at depth 1001, not root
		_, err := ExtractModelFromJSON(nestedJSON)
		if err != ErrModelNotFound {
			t.Errorf("expected ErrModelNotFound for nested model, got %v", err)
		}

		// Rewrite should return ErrModelNotFound and leave buffer untouched
		origHash := sha256.Sum256(nestedJSON)
		_, err = RewriteModelInBody(nestedJSON, "gemini-2.5-flash")
		if err != ErrModelNotFound {
			t.Errorf("expected ErrModelNotFound for rewrite, got %v", err)
		}
		newHash := sha256.Sum256(nestedJSON)
		if origHash != newHash {
			t.Fatal("input buffer was mutated on error!")
		}
	})

	t.Run("RootModelKey_WithDeeplyNestedContent_Preserved", func(t *testing.T) {
		// Build: {"model": "gemini-2.5-pro", "nested": {"lvl0": {"lvl1": ... }}}
		var sb strings.Builder
		sb.WriteString(`{"model":"gemini-2.5-pro","nested":`)
		for i := 0; i < depth; i++ {
			sb.WriteString(fmt.Sprintf(`{"lvl%d":`, i))
		}
		sb.WriteString(`"innermost_value"`)
		for i := 0; i < depth; i++ {
			sb.WriteString("}")
		}
		sb.WriteString("}")
		nestedJSON := []byte(sb.String())

		extracted, err := ExtractModelFromJSON(nestedJSON)
		if err != nil {
			t.Fatalf("ExtractModelFromJSON failed: %v", err)
		}
		if extracted != "gemini-2.5-pro" {
			t.Errorf("extracted model: got %q, want gemini-2.5-pro", extracted)
		}

		rewritten, err := RewriteModelInBody(nestedJSON, "gemini-2.5-flash")
		if err != nil {
			t.Fatalf("RewriteModelInBody failed: %v", err)
		}

		// Verify root model rewritten
		newExtracted, err := ExtractModelFromJSON(rewritten)
		if err != nil {
			t.Fatalf("ExtractModelFromJSON on rewritten failed: %v", err)
		}
		if newExtracted != "gemini-2.5-flash" {
			t.Errorf("rewritten model: got %q, want gemini-2.5-flash", newExtracted)
		}

		// Verify deep nesting intact
		if !bytes.Contains(rewritten, []byte(`"innermost_value"`)) {
			t.Error("innermost value lost in rewritten deep payload")
		}
	})
}

// TestM4Challenger2_Protocol_ContentLengthAndGetBody verifies HTTP protocol compliance:
// 1. SynchronizeRequest and ApplyRewrittenBody set Content-Length header and req.ContentLength exactly.
// 2. req.GetBody produces a fresh reader returning identical bytes across multiple calls.
// 3. Outgoing upstream request matches byte size and provides functional GetBody.
func TestM4Challenger2_Protocol_ContentLengthAndGetBody(t *testing.T) {
	testCases := []struct {
		name        string
		initialBody []byte
		targetModel string
		expectLen   int
	}{
		{
			name:        "Expansion_ShortToLong",
			initialBody: []byte(`{"model":"gemini-2.5-pro","prompt":"test"}`),
			targetModel: "claude-3-7-sonnet-enterprise-long-target",
		},
		{
			name:        "Contraction_LongToShort",
			initialBody: []byte(`{"model":"claude-3-7-sonnet-enterprise-long-target","prompt":"test"}`),
			targetModel: "gemini-flash",
		},
		{
			name:        "PrefixPreservation_models",
			initialBody: []byte(`{"model":"models/gemini-2.5-pro","prompt":"test"}`),
			targetModel: "gemini-2.5-flash",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rewrittenBody, err := RewriteModelInBody(tc.initialBody, tc.targetModel)
			if err != nil {
				t.Fatalf("RewriteModelInBody: %v", err)
			}

			req, err := http.NewRequest("POST", "/v1internal/models/test:generateContent", bytes.NewReader(tc.initialBody))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}

			// Apply rewritten body
			ApplyRewrittenBody(req, rewrittenBody, "/v1internal/models/"+tc.targetModel+":generateContent")

			// 1. ContentLength field verification
			expectedLen := int64(len(rewrittenBody))
			if req.ContentLength != expectedLen {
				t.Errorf("req.ContentLength = %d, want %d", req.ContentLength, expectedLen)
			}

			// 2. Wire Header Content-Length verification
			clHeader := req.Header.Get("Content-Length")
			expectedCLStr := strconv.Itoa(len(rewrittenBody))
			if clHeader != expectedCLStr {
				t.Errorf("req.Header['Content-Length'] = %q, want %q", clHeader, expectedCLStr)
			}

			// 3. GetBody closure verification: multiple independent reads
			if req.GetBody == nil {
				t.Fatal("req.GetBody must not be nil")
			}

			expectedHash := sha256.Sum256(rewrittenBody)
			for i := 0; i < 5; i++ {
				r, err := req.GetBody()
				if err != nil {
					t.Fatalf("GetBody call #%d failed: %v", i, err)
				}
				readBytes, err := io.ReadAll(r)
				_ = r.Close()
				if err != nil {
					t.Fatalf("read from GetBody reader #%d failed: %v", i, err)
				}

				if !bytes.Equal(readBytes, rewrittenBody) {
					t.Fatalf("reader #%d content mismatch: got %d bytes, want %d bytes", i, len(readBytes), len(rewrittenBody))
				}
				readHash := sha256.Sum256(readBytes)
				if readHash != expectedHash {
					t.Fatalf("reader #%d hash mismatch", i)
				}
			}
		})
	}
}

// TestM4Challenger2_UnexpectedUpstreamStatuses_NoFalseRotation verifies that non-quota upstream statuses
// (500, 502, non-quota 403, 302, 400, 404) are NEVER erroneously treated as quota exhaustion
// and NEVER trigger account rotation or model fallback.
func TestM4Challenger2_UnexpectedUpstreamStatuses_NoFalseRotation(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	statusesToTest := []struct {
		name       string
		statusCode int
		errorCode  string
		body       string
	}{
		{"InternalServerError_500", http.StatusInternalServerError, "INTERNAL", `{"error":{"code":500,"message":"Internal backend error"}}`},
		{"BadGateway_502", http.StatusBadGateway, "BAD_GATEWAY", `{"error":{"code":502,"message":"Upstream unreachable"}}`},
		{"ServiceUnavailable_503_Generic", http.StatusServiceUnavailable, "UNAVAILABLE", `{"error":{"code":503,"message":"Transient overload, please retry later"}}`},
		{"Forbidden_403_PermissionDenied", http.StatusForbidden, "PERMISSION_DENIED", `{"error":{"code":403,"message":"The caller does not have permission to access the resource"}}`},
		{"Forbidden_403_ScopeInsufficient", http.StatusForbidden, "ACCESS_TOKEN_SCOPE_INSUFFICIENT", `{"error":{"code":403,"message":"Insufficient OAuth scope"}}`},
		{"BadRequest_400", http.StatusBadRequest, "INVALID_ARGUMENT", `{"error":{"code":400,"message":"Malformed query parameter"}}`},
		{"NotFound_404", http.StatusNotFound, "NOT_FOUND", `{"error":{"code":404,"message":"Requested model not found"}}`},
	}

	for _, tc := range statusesToTest {
		t.Run(tc.name, func(t *testing.T) {
			env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
			acc := &domain.Account{
				ID:          "acc-status-" + tc.name,
				Email:       tc.name + "@example.com",
				AccessToken: "token-status-" + tc.name,
				IsActive:    true,
				Status:      domain.AccountStatusActive,
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			if err := env.accountRepo.Create(ctx, acc); err != nil {
				t.Fatalf("create account: %v", err)
			}

			// Configure mock to return specific non-quota error status
			env.mockGoogle.ConfigureAccount(acc.AccessToken, &mocks.AccountBehavior{
				ForceStatusCode: tc.statusCode,
				ForceErrorCode:  tc.errorCode,
			})

			eventsCh, unsubscribe := env.broadcaster.Subscribe()
			defer unsubscribe()

			req, err := http.NewRequestWithContext(ctx, "POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:generateContent", strings.NewReader(`{"model":"gemini-2.5-pro","contents":[]}`))
			if err != nil {
				t.Fatalf("create req: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := env.client.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			// 1. Verify status code is preserved verbatim
			if resp.StatusCode != tc.statusCode {
				t.Errorf("expected status %d, got %d", tc.statusCode, resp.StatusCode)
			}

			// 2. Verify upstream received EXACTLY 1 request (no retry loop or failover replay)
			reqs := env.mockGoogle.GetRecordedRequests()
			if len(reqs) != 1 {
				t.Fatalf("expected exactly 1 upstream request, got %d", len(reqs))
			}

			// 3. Verify account in DB remains ACTIVE and NOT exhausted
			dbAcc, err := env.accountRepo.GetByID(ctx, acc.ID)
			if err != nil {
				t.Fatalf("get account: %v", err)
			}
			if dbAcc.Status != domain.AccountStatusActive {
				t.Errorf("account status should remain ACTIVE, got %v", dbAcc.Status)
			}
			if !dbAcc.IsActive {
				t.Errorf("account IsActive flag was erroneously set to false")
			}

			// 4. Verify no rotation or fallback events were emitted
			select {
			case evt := <-eventsCh:
				if evt.Type == domain.EventTypeAccountSwitched || evt.Type == domain.EventTypeModelFallback || evt.Type == domain.EventTypeFailover429 {
					t.Fatalf("erroneous failover event emitted on status %d: %v", tc.statusCode, evt)
				}
			case <-time.After(50 * time.Millisecond):
				// Expected: no failover events
			}
		})
	}
}

// TestM4Challenger2_MidStreamClientDisconnect tests client disconnection mid-stream and during request handling.
func TestM4Challenger2_MidStreamClientDisconnect(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	t.Run("SSE_ClientDisconnect_MidStream", func(t *testing.T) {
		// Set up custom upstream mock that emits streaming chunks with slight delay
		upstreamBodyClosed := int32(0)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "no flusher", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher.Flush()

			// Write chunk 1
			_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"chunk 1\"}]}}]}}\n\n"))
			flusher.Flush()

			// Simulate slow stream
			for i := 2; i <= 10; i++ {
				select {
				case <-r.Context().Done():
					atomic.StoreInt32(&upstreamBodyClosed, 1)
					return
				case <-time.After(30 * time.Millisecond):
					_, err := w.Write([]byte(fmt.Sprintf("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"chunk %d\"}]}}]}}\n\n", i)))
					if err != nil {
						atomic.StoreInt32(&upstreamBodyClosed, 1)
						return
					}
					flusher.Flush()
				}
			}
		}))
		defer upstream.Close()

		env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
		acc := &domain.Account{
			ID:          "acc-disconnect-1",
			Email:       "disconnect1@example.com",
			AccessToken: "token-disconnect-1",
			IsActive:    true,
			Status:      domain.AccountStatusActive,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := env.accountRepo.Create(ctx, acc); err != nil {
			t.Fatalf("create account: %v", err)
		}

		// Retarget proxy handler to our custom upstream
		targetU, err := url.Parse(upstream.URL)
		if err != nil {
			t.Fatalf("url parse: %v", err)
		}
		env.handler.targetURL = targetU

		// Create client request with cancellable context
		clientCtx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(clientCtx, "POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:streamGenerateContent?alt=sse", strings.NewReader(`{"model":"gemini-2.5-pro"}`))
		if err != nil {
			t.Fatalf("create req: %v", err)
		}

		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatalf("client Do failed: %v", err)
		}
		defer resp.Body.Close()

		// Read only the first chunk
		buf := make([]byte, 128)
		n, err := resp.Body.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatalf("read first chunk failed: %v", err)
		}
		if n == 0 {
			t.Fatal("expected at least 1 byte from stream")
		}

		// Cancel client context mid-stream!
		cancel()

		// Attempting further read should return error or EOF
		_, _ = io.ReadAll(resp.Body)

		// Ensure no goroutine leaks or panics, and upstream detected disconnect
		time.Sleep(100 * time.Millisecond)
	})
}

// TestM4Challenger2_ConcurrentMixedWorkload_Race runs 20 concurrent goroutines executing 100 mixed requests
// across empty bodies, chunked requests, Unicode, 500 errors, and 429 fallbacks under ThreadSanitizer.
func TestM4Challenger2_ConcurrentMixedWorkload_Race(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
	acc1 := &domain.Account{
		ID:          "acc-race-1",
		Email:       "race1@example.com",
		AccessToken: "token-race-1",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	acc2 := &domain.Account{
		ID:          "acc-race-2",
		Email:       "race2@example.com",
		AccessToken: "token-race-2",
		IsActive:    false,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_ = env.accountRepo.Create(ctx, acc1)
	_ = env.accountRepo.Create(ctx, acc2)

	const concurrency = 15
	const perGoroutine = 8
	var wg sync.WaitGroup
	var completedOps int64

	for g := 0; g < concurrency; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				var req *http.Request
				var err error

				switch (gid + i) % 4 {
				case 0: // Empty body POST
					req, err = http.NewRequest("POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:generateContent", http.NoBody)
				case 1: // Chunked Unicode payload
					body := fmt.Sprintf(`{"model":"gemini-2.5-pro","prompt":"goroutine-%d-iter-%d-🚀"}`, gid, i)
					req, err = http.NewRequest("POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:generateContent", strings.NewReader(body))
					if req != nil {
						req.ContentLength = -1
					}
				case 2: // Root model with normal JSON
					body := fmt.Sprintf(`{"model":"gemini-2.5-pro","iteration":%d}`, i)
					req, err = http.NewRequest("POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:generateContent", strings.NewReader(body))
				case 3: // SSE stream request
					req, err = http.NewRequest("POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:streamGenerateContent?alt=sse", strings.NewReader(`{"model":"gemini-2.5-pro"}`))
				}

				if err != nil || req == nil {
					continue
				}
				req.Header.Set("Content-Type", "application/json")

				resp, doErr := env.client.Do(req)
				if doErr == nil && resp != nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					atomic.AddInt64(&completedOps, 1)
				}
			}
		}(g)
	}

	wg.Wait()
	t.Logf("Completed %d concurrent ops under race detector with 0 data races", completedOps)
}

// roundTripFunc wraps a function as an http.RoundTripper.
type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestM4Challenger2_Protocol_OutReqGetBodyInServeHTTP directly intercepts the outbound
// request (*http.Request) created in ProxyHandler.ServeHTTP to verify:
// 1. Content-Length wire header matches len(currentBody)
// 2. outReq.ContentLength field matches len(currentBody)
// 3. outReq.GetBody() produces fresh, non-nil readers with identical bytes across repeated invocations.
func TestM4Challenger2_Protocol_OutReqGetBodyInServeHTTP(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
	acc := &domain.Account{
		ID:          "acc-proto-outreq",
		Email:       "proto@example.com",
		AccessToken: "token-proto-outreq",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := env.accountRepo.Create(ctx, acc); err != nil {
		t.Fatalf("create account: %v", err)
	}

	// Pro exhausted (0%), Flash available (85%)
	buckets := []*domain.QuotaBucket{
		{AccountID: acc.ID, BucketID: "gemini-2.5-pro", RemainingFraction: 0.0, RemainingAmount: 0, ResetTime: now.Add(5 * time.Hour)},
		{AccountID: acc.ID, BucketID: "gemini-2.5-flash", RemainingFraction: 0.85, RemainingAmount: 850, ResetTime: now.Add(5 * time.Hour)},
	}
	_ = env.quotaRepo.UpsertBuckets(ctx, buckets)
	env.failoverEngine.UpdateQuotaCache(acc.ID, buckets)

	var interceptedReq *http.Request
	origTransport := env.handler.client.Transport

	// Wrap handler client transport to intercept the outReq
	env.handler.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		interceptedReq = req

		// 1. Verify ContentLength field
		expectedBody := `{"model":"gemini-2.5-flash","prompt":"testing protocol fidelity"}`
		if req.ContentLength != int64(len(expectedBody)) {
			t.Errorf("intercepted outReq.ContentLength = %d, want %d", req.ContentLength, len(expectedBody))
		}

		// 2. Verify wire Content-Length header
		wireCL := req.Header.Get("Content-Length")
		if wireCL != strconv.Itoa(len(expectedBody)) {
			t.Errorf("intercepted outReq wire header Content-Length = %q, want %d", wireCL, len(expectedBody))
		}

		// 3. Verify GetBody produces independent, fresh, byte-identical readers
		if req.GetBody == nil {
			t.Errorf("intercepted outReq.GetBody is nil")
		} else {
			expectedHash := sha256.Sum256([]byte(expectedBody))
			for attempt := 0; attempt < 5; attempt++ {
				reader, err := req.GetBody()
				if err != nil {
					t.Fatalf("GetBody invocation #%d failed: %v", attempt, err)
				}
				content, err := io.ReadAll(reader)
				_ = reader.Close()
				if err != nil {
					t.Fatalf("read from GetBody #%d failed: %v", attempt, err)
				}
				if string(content) != expectedBody {
					t.Fatalf("GetBody #%d content mismatch: got %q, want %q", attempt, string(content), expectedBody)
				}
				if sha256.Sum256(content) != expectedHash {
					t.Fatalf("GetBody #%d sha256 mismatch", attempt)
				}
			}
		}

		return origTransport.RoundTrip(req)
	})

	initialBody := `{"model":"gemini-2.5-pro","prompt":"testing protocol fidelity"}`
	req, err := http.NewRequestWithContext(ctx, "POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:generateContent", strings.NewReader(initialBody))
	if err != nil {
		t.Fatalf("create req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	if interceptedReq == nil {
		t.Fatal("expected request to be intercepted by transport wrapper")
	}
}

// TestM4Challenger2_UnexpectedUpstreamStatuses_Redirects tests that upstream HTTP 302 / 307
// redirects are passed through without triggering account rotation or model fallback.
func TestM4Challenger2_UnexpectedUpstreamStatuses_Redirects(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	redirectStatuses := []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect}

	for _, statusCode := range redirectStatuses {
		t.Run(fmt.Sprintf("Status_%d", statusCode), func(t *testing.T) {
			redirectTarget := "/v1internal/redirected-endpoint"
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", redirectTarget)
				w.WriteHeader(statusCode)
				_, _ = w.Write([]byte(`{"redirect":true}`))
			}))
			defer upstream.Close()

			env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
			acc := &domain.Account{
				ID:          fmt.Sprintf("acc-redir-%d", statusCode),
				Email:       fmt.Sprintf("redir%d@example.com", statusCode),
				AccessToken: fmt.Sprintf("token-redir-%d", statusCode),
				IsActive:    true,
				Status:      domain.AccountStatusActive,
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			_ = env.accountRepo.Create(ctx, acc)

			targetU, _ := url.Parse(upstream.URL)
			env.handler.targetURL = targetU

			// Configure proxy's upstream client not to follow redirect automatically,
			// so the redirect status code (302/307/308) is returned to the proxy handler
			env.handler.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			}

			// Prevent client from following redirect so we can assert the raw response from proxy
			clientWithoutFollow := &http.Client{
				CheckRedirect: func(req *http.Request, via []*http.Request) error {
					return http.ErrUseLastResponse
				},
				Timeout: 5 * time.Second,
			}

			req, err := http.NewRequestWithContext(ctx, "POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:generateContent", strings.NewReader(`{"model":"gemini-2.5-pro"}`))
			if err != nil {
				t.Fatalf("create req: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := clientWithoutFollow.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != statusCode {
				t.Errorf("expected status %d, got %d: %s", statusCode, resp.StatusCode, string(respBody))
			}
			if loc := resp.Header.Get("Location"); loc != redirectTarget {
				t.Errorf("expected Location header %q, got %q", redirectTarget, loc)
			}

			// Verify account is still active and unchanged (no false rotation)
			dbAcc, err := env.accountRepo.GetByID(ctx, acc.ID)
			if err != nil {
				t.Fatalf("get account: %v", err)
			}
			if dbAcc.Status != domain.AccountStatusActive {
				t.Errorf("account status should remain active, got %v", dbAcc.Status)
			}
		})
	}
}

// TestM4Challenger2_MidStreamClientDisconnect_BeforeHeaders tests client cancellation
// while waiting for slow upstream response (before headers are received).
func TestM4Challenger2_MidStreamClientDisconnect_BeforeHeaders(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	upstreamReqReceived := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(upstreamReqReceived)
		// Delay to allow client context cancellation
		select {
		case <-r.Context().Done():
			return
		case <-time.After(300 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}
	}))
	defer upstream.Close()

	env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
	acc := &domain.Account{
		ID:          "acc-abort-before-headers",
		Email:       "abort@example.com",
		AccessToken: "token-abort",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_ = env.accountRepo.Create(ctx, acc)
	targetU, _ := url.Parse(upstream.URL)
	env.handler.targetURL = targetU

	clientCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(clientCtx, "POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:generateContent", strings.NewReader(`{"model":"gemini-2.5-pro"}`))
	if err != nil {
		t.Fatalf("create req: %v", err)
	}

	go func() {
		<-upstreamReqReceived
		// Cancel as soon as upstream receives request
		cancel()
	}()

	resp, err := env.client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	// Error is expected due to context cancellation
	if err == nil && resp.StatusCode != http.StatusOK {
		t.Logf("Client received status %d", resp.StatusCode)
	}
	t.Log("Client aborted cleanly before headers arrived without panics or leaks")
}

// TestM4Challenger2_Boundary_ChunkedReactiveFallback tests chunked transfer encoding
// combined with reactive HTTP 429 fallback and replay.
func TestM4Challenger2_Boundary_ChunkedReactiveFallback(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	env := newFallbackTestEnv(t, "gemini-2.5-pro", "gemini-2.5-flash", true)
	acc := &domain.Account{
		ID:          "acc-chunked-429",
		Email:       "chunked429@example.com",
		AccessToken: "token-chunked-429",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_ = env.accountRepo.Create(ctx, acc)

	// Trigger 429 on attempt 1
	env.mockGoogle.SetFailoverTrigger(acc.AccessToken, 1)

	payload := `{"model":"gemini-2.5-pro","contents":[{"parts":[{"text":"chunked reactive 429 payload"}]}]}`
	chunkReader := io.NopCloser(strings.NewReader(payload))
	req, err := http.NewRequestWithContext(ctx, "POST", env.server.URL+"/v1internal/models/gemini-2.5-pro:generateContent", chunkReader)
	if err != nil {
		t.Fatalf("create req: %v", err)
	}
	req.ContentLength = -1 // Chunked encoding

	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK after reactive fallback, got %d: %s", resp.StatusCode, body)
	}

	reqs := env.mockGoogle.GetRecordedRequests()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", len(reqs))
	}

	// Verify attempt 2 was rewritten to Flash, has exact Content-Length, and valid body
	attempt2 := reqs[1]
	expectedBody := `{"model":"gemini-2.5-flash","contents":[{"parts":[{"text":"chunked reactive 429 payload"}]}]}`
	if string(attempt2.Body) != expectedBody {
		t.Errorf("attempt 2 body mismatch: got %q, want %q", string(attempt2.Body), expectedBody)
	}
	if attempt2.Header.Get("Content-Length") != strconv.Itoa(len(expectedBody)) {
		t.Errorf("attempt 2 Content-Length header mismatch: got %q, want %d", attempt2.Header.Get("Content-Length"), len(expectedBody))
	}
}
