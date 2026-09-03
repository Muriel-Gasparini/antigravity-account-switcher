package mocks_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/test/mocks"
)

func TestMockGoogleServer_StreamGenerateContent_SSE(t *testing.T) {
	server := mocks.NewMockGoogleServer()
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1internal:streamGenerateContent?alt=sse", strings.NewReader(`{"prompt":"test"}`))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token-1")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed to execute request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "text/event-stream") {
		t.Errorf("expected text/event-stream, got %s", contentType)
	}

	chunks, err := mocks.ParseSSEChunks(resp.Body)
	if err != nil {
		t.Fatalf("failed to parse SSE chunks: %v", err)
	}

	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}

	// Verify the last chunk has usageMetadata
	var lastPayload mocks.SSEChunkPayload
	if err := json.Unmarshal([]byte(chunks[len(chunks)-1]), &lastPayload); err != nil {
		t.Fatalf("failed to unmarshal chunk payload: %v", err)
	}

	usage := lastPayload.Response.UsageMetadata
	if usage == nil {
		usage = lastPayload.UsageMetadata
	}
	if usage == nil {
		t.Fatalf("expected usageMetadata in last chunk, got nil")
	}
	if usage.PromptTokenCount <= 0 || usage.CandidatesTokenCount <= 0 {
		t.Errorf("unexpected token counts: %+v", usage)
	}

	// Verify request recording
	reqs := server.GetRecordedRequests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 recorded request, got %d", len(reqs))
	}
	if reqs[0].AuthBearer != "test-token-1" {
		t.Errorf("expected Bearer token test-token-1, got %s", reqs[0].AuthBearer)
	}
}

func TestMockGoogleServer_FailoverTrigger_429(t *testing.T) {
	server := mocks.NewMockGoogleServer()
	defer server.Close()

	token := "exhausted-token"
	server.SetFailoverTrigger(token, 2)

	// First request -> 429
	req1, _ := http.NewRequest(http.MethodPost, server.URL+"/v1internal:streamGenerateContent", strings.NewReader(`{}`))
	req1.Header.Set("Authorization", "Bearer "+token)
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("req1 failed: %v", err)
	}
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429 Too Many Requests, got %d", resp1.StatusCode)
	}

	var errPayload map[string]any
	_ = json.NewDecoder(resp1.Body).Decode(&errPayload)
	errObj, ok := errPayload["error"].(map[string]any)
	if !ok || errObj["status"] != "RESOURCE_EXHAUSTED" {
		t.Errorf("expected RESOURCE_EXHAUSTED error payload, got %+v", errPayload)
	}

	// Second request -> 429
	req2, _ := http.NewRequest(http.MethodPost, server.URL+"/v1internal:streamGenerateContent", strings.NewReader(`{}`))
	req2.Header.Set("Authorization", "Bearer "+token)
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", resp2.StatusCode)
	}

	// Third request -> 200 OK (failover count consumed)
	req3, _ := http.NewRequest(http.MethodPost, server.URL+"/v1internal:streamGenerateContent", strings.NewReader(`{}`))
	req3.Header.Set("Authorization", "Bearer "+token)
	resp3, _ := http.DefaultClient.Do(req3)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK on third request, got %d", resp3.StatusCode)
	}
}

func TestMockGoogleServer_RetrieveUserQuotaSummary(t *testing.T) {
	server := mocks.NewMockGoogleServer()
	defer server.Close()

	token := "quota-token"
	resetTime := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	server.SetAccountQuota(token, []mocks.QuotaSummaryBucket{
		{
			BucketID:          "gemini-2.5-pro",
			DisplayName:       "Gemini Pro Quota",
			Window:            "DAILY",
			RemainingFraction: 0.45,
			RemainingAmount:   450,
			ResetTime:         resetTime,
		},
	})

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1internal:retrieveUserQuotaSummary", bytes.NewReader([]byte(`{"project":""}`)))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("quota request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	var quotaResp mocks.RetrieveUserQuotaSummaryResponse
	if err := json.NewDecoder(resp.Body).Decode(&quotaResp); err != nil {
		t.Fatalf("failed to decode quota response: %v", err)
	}

	if len(quotaResp.Groups) == 0 || len(quotaResp.Groups[0].Buckets) == 0 {
		t.Fatalf("expected quota buckets in response, got %+v", quotaResp)
	}

	bucket := quotaResp.Groups[0].Buckets[0]
	if bucket.BucketID != "gemini-2.5-pro" || bucket.RemainingFraction != 0.45 {
		t.Errorf("unexpected bucket data: %+v", bucket)
	}
}

func TestMockGoogleServer_OAuthEndpoints(t *testing.T) {
	server := mocks.NewMockGoogleServer()
	defer server.Close()

	// Token refresh
	tokenReq, _ := http.NewRequest(http.MethodPost, server.URL+"/token", strings.NewReader("grant_type=refresh_token&refresh_token=valid_refresh"))
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(tokenReq)
	if err != nil {
		t.Fatalf("token request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for valid refresh, got %d", resp.StatusCode)
	}

	var tokenBody map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&tokenBody)
	if tokenBody["access_token"] == "" {
		t.Errorf("expected non-empty access_token, got %+v", tokenBody)
	}

	// Revoked token handling
	revokedReq, _ := http.NewRequest(http.MethodPost, server.URL+"/token", strings.NewReader("grant_type=refresh_token&refresh_token=revoked"))
	revokedReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	revokedResp, err := http.DefaultClient.Do(revokedReq)
	if err != nil {
		t.Fatalf("revoked token request failed: %v", err)
	}
	defer revokedResp.Body.Close()

	if revokedResp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for revoked token, got %d", revokedResp.StatusCode)
	}

	// UserInfo endpoint
	userReq, _ := http.NewRequest(http.MethodGet, server.URL+"/oauth2/v3/userinfo", nil)
	userReq.Header.Set("Authorization", "Bearer "+tokenBody["access_token"].(string))
	userResp, err := http.DefaultClient.Do(userReq)
	if err != nil {
		t.Fatalf("userinfo request failed: %v", err)
	}
	defer userResp.Body.Close()

	if userResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for userinfo, got %d", userResp.StatusCode)
	}
	var userInfo map[string]any
	_ = json.NewDecoder(userResp.Body).Decode(&userInfo)
	if userInfo["email"] == "" {
		t.Errorf("expected email in userinfo response, got %+v", userInfo)
	}
}
