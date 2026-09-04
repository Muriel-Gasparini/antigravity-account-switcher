package mocks

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// QuotaSummaryBucket represents a single model or tier quota bucket.
type QuotaSummaryBucket struct {
	BucketID          string    `json:"bucketId"`
	DisplayName       string    `json:"displayName"`
	Description       string    `json:"description,omitempty"`
	Window            string    `json:"window"` // "DAILY" or "WEEKLY"
	RemainingFraction float64   `json:"remainingFraction"`
	RemainingAmount   int64     `json:"remainingAmount,omitempty"`
	Disabled          bool      `json:"disabled,omitempty"`
	ResetTime         time.Time `json:"resetTime"`
}

// QuotaSummaryGroup represents a group of quota buckets.
type QuotaSummaryGroup struct {
	DisplayName string               `json:"displayName"`
	Description string               `json:"description,omitempty"`
	Buckets     []QuotaSummaryBucket `json:"buckets"`
}

// RetrieveUserQuotaSummaryResponse is the payload returned by :retrieveUserQuotaSummary.
type RetrieveUserQuotaSummaryResponse struct {
	Buckets     []QuotaSummaryBucket `json:"buckets,omitempty"`
	Groups      []QuotaSummaryGroup  `json:"groups"`
	Description string               `json:"description,omitempty"`
}

// UsageMetadata captures token counts as emitted by Gemini / Cloud Code PA models.
type UsageMetadata struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	TotalTokenCount         int64 `json:"totalTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount,omitempty"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount,omitempty"`
	ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount,omitempty"`
}

// Candidate represents a generated model response candidate.
type Candidate struct {
	Content struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
		Role string `json:"role"`
	} `json:"content"`
	FinishReason string `json:"finishReason,omitempty"`
}

// SSEChunkPayload represents the payload wrapped in an SSE data: message.
type SSEChunkPayload struct {
	Response struct {
		Candidates    []Candidate    `json:"candidates,omitempty"`
		UsageMetadata *UsageMetadata `json:"usageMetadata,omitempty"`
	} `json:"response,omitempty"`
	Candidates    []Candidate    `json:"candidates,omitempty"`
	UsageMetadata *UsageMetadata `json:"usageMetadata,omitempty"`
	TraceID       string         `json:"traceId,omitempty"`
}

// RecordedRequest records an incoming HTTP request for test assertions.
type RecordedRequest struct {
	Method     string
	Path       string
	RawQuery   string
	Header     http.Header
	Body       []byte
	Timestamp  time.Time
	AuthBearer string
}

// AccountBehavior defines mock responses for a specific Bearer token.
type AccountBehavior struct {
	Email             string
	FailoverRemaining int // Number of 429 RESOURCE_EXHAUSTED errors before returning 200 OK
	ForceStatusCode   int // If > 0, overrides response code
	ForceErrorCode    string
	CustomSSEChunks   []string
	Usage             *UsageMetadata
	QuotaBuckets      []QuotaSummaryBucket
	StreamDelay       time.Duration
}

// MockGoogleServer simulates Google Cloud Code PA (daily-cloudcode-pa.googleapis.com)
// and Google OAuth2 authorization & token services.
type MockGoogleServer struct {
	Server   *httptest.Server
	URL      string
	mu       sync.RWMutex
	requests []RecordedRequest
	accounts map[string]*AccountBehavior // Keyed by Bearer access token
	defaultB *AccountBehavior
}

// NewMockGoogleServer creates and starts a new MockGoogleServer.
func NewMockGoogleServer() *MockGoogleServer {
	m := &MockGoogleServer{
		accounts: make(map[string]*AccountBehavior),
		defaultB: &AccountBehavior{
			Usage: &UsageMetadata{
				PromptTokenCount:     125,
				CandidatesTokenCount: 42,
				TotalTokenCount:      167,
			},
			QuotaBuckets: []QuotaSummaryBucket{
				{
					BucketID:          "gemini-2.5-pro",
					DisplayName:       "Gemini 2.5 Pro Daily Quota",
					Window:            "DAILY",
					RemainingFraction: 0.75,
					RemainingAmount:   750,
					ResetTime:         time.Now().Add(12 * time.Hour),
				},
				{
					BucketID:          "gemini-2.5-flash",
					DisplayName:       "Gemini 2.5 Flash Weekly Quota",
					Window:            "WEEKLY",
					RemainingFraction: 0.90,
					RemainingAmount:   9000,
					ResetTime:         time.Now().Add(48 * time.Hour),
				},
			},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1internal:streamGenerateContent", m.handleStreamGenerateContent)
	mux.HandleFunc("/v1internal:generateContent", m.handleStreamGenerateContent)
	mux.HandleFunc("/v1internal/models/", m.handleStreamGenerateContent)
	mux.HandleFunc("/v1internal:retrieveUserQuotaSummary", m.handleRetrieveUserQuotaSummary)
	mux.HandleFunc("/v1internal:retrieveUserQuota", m.handleRetrieveUserQuota)
	mux.HandleFunc("/token", m.handleOAuthToken)
	mux.HandleFunc("/oauth2/v4/token", m.handleOAuthToken)
	mux.HandleFunc("/oauth2/v3/userinfo", m.handleOAuthUserInfo)
	mux.HandleFunc("/userinfo", m.handleOAuthUserInfo)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	m.Server = httptest.NewServer(mux)
	m.URL = m.Server.URL
	return m
}

// Close terminates the mock HTTP server.
func (m *MockGoogleServer) Close() {
	if m.Server != nil {
		m.Server.Close()
	}
}

// Reset clears recorded requests and custom account configurations.
func (m *MockGoogleServer) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = nil
	m.accounts = make(map[string]*AccountBehavior)
}

// ConfigureAccount sets the behavior for requests bearing the specified token.
func (m *MockGoogleServer) ConfigureAccount(token string, behavior *AccountBehavior) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accounts[token] = behavior
}

// SetFailoverTrigger causes the next n requests with token to fail with HTTP 429 RESOURCE_EXHAUSTED.
func (m *MockGoogleServer) SetFailoverTrigger(token string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.accounts[token]
	if !ok {
		b = &AccountBehavior{
			Usage: &UsageMetadata{
				PromptTokenCount:     100,
				CandidatesTokenCount: 50,
				TotalTokenCount:      150,
			},
		}
		m.accounts[token] = b
	}
	b.FailoverRemaining = n
}

// SetAccountQuota updates the quota buckets returned for a specific token.
func (m *MockGoogleServer) SetAccountQuota(token string, buckets []QuotaSummaryBucket) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.accounts[token]
	if !ok {
		b = &AccountBehavior{}
		m.accounts[token] = b
	}
	b.QuotaBuckets = buckets
}

// GetRecordedRequests returns a copy of all requests received by the mock server.
func (m *MockGoogleServer) GetRecordedRequests() []RecordedRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]RecordedRequest, len(m.requests))
	copy(out, m.requests)
	return out
}

// RequestCount returns total number of requests received.
func (m *MockGoogleServer) RequestCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.requests)
}

func (m *MockGoogleServer) record(r *http.Request) ([]byte, string) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))

	auth := r.Header.Get("Authorization")
	bearer := ""
	if strings.HasPrefix(auth, "Bearer ") {
		bearer = strings.TrimPrefix(auth, "Bearer ")
	}

	m.mu.Lock()
	m.requests = append(m.requests, RecordedRequest{
		Method:     r.Method,
		Path:       r.URL.Path,
		RawQuery:   r.URL.RawQuery,
		Header:     r.Header.Clone(),
		Body:       body,
		Timestamp:  time.Now(),
		AuthBearer: bearer,
	})
	m.mu.Unlock()

	return body, bearer
}

func (m *MockGoogleServer) getBehavior(token string) *AccountBehavior {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if b, ok := m.accounts[token]; ok {
		return b
	}
	return m.defaultB
}

func (m *MockGoogleServer) handleStreamGenerateContent(w http.ResponseWriter, r *http.Request) {
	_, bearer := m.record(r)
	b := m.getBehavior(bearer)

	m.mu.Lock()
	if b.FailoverRemaining > 0 {
		b.FailoverRemaining--
		m.mu.Unlock()
		m.writeResourceExhausted(w)
		return
	}
	m.mu.Unlock()

	if b.ForceStatusCode > 0 && b.ForceStatusCode != http.StatusOK {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(b.ForceStatusCode)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    b.ForceStatusCode,
				"message": "Forced mock error",
				"status":  b.ForceErrorCode,
			},
		})
		return
	}

	altSSE := r.URL.Query().Get("alt") == "sse" || strings.Contains(r.Header.Get("Accept"), "text/event-stream")
	if !altSSE {
		// Non-streaming response
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		resp := SSEChunkPayload{
			Candidates: []Candidate{
				{
					FinishReason: "STOP",
				},
			},
			UsageMetadata: b.Usage,
		}
		resp.Candidates[0].Content.Role = "model"
		resp.Candidates[0].Content.Parts = []struct {
			Text string `json:"text"`
		}{{Text: "Opaque box test response from mock Google server."}}
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	// SSE streaming response
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	chunks := b.CustomSSEChunks
	if len(chunks) == 0 {
		// Default realistic SSE chunks with usageMetadata
		c1 := SSEChunkPayload{}
		c1.Response.Candidates = []Candidate{{FinishReason: ""}}
		c1.Response.Candidates[0].Content.Role = "model"
		c1.Response.Candidates[0].Content.Parts = []struct {
			Text string `json:"text"`
		}{{Text: "Hello, "}}
		c1JSON, _ := json.Marshal(c1)

		c2 := SSEChunkPayload{}
		c2.Response.Candidates = []Candidate{{FinishReason: "STOP"}}
		c2.Response.Candidates[0].Content.Role = "model"
		c2.Response.Candidates[0].Content.Parts = []struct {
			Text string `json:"text"`
		}{{Text: "world!"}}
		c2.Response.UsageMetadata = b.Usage
		c2JSON, _ := json.Marshal(c2)

		chunks = []string{
			fmt.Sprintf("data: %s\n\n", c1JSON),
			fmt.Sprintf("data: %s\n\n", c2JSON),
		}
	}

	for _, chunk := range chunks {
		if b.StreamDelay > 0 {
			time.Sleep(b.StreamDelay)
		}
		_, _ = w.Write([]byte(chunk))
		flusher.Flush()
	}
}

func (m *MockGoogleServer) writeResourceExhausted(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusTooManyRequests)
	payload := map[string]any{
		"error": map[string]any{
			"code":    429,
			"message": "Resource has been exhausted (e.g. check quota).",
			"status":  "RESOURCE_EXHAUSTED",
			"details": []any{
				map[string]any{
					"@type":  "type.googleapis.com/google.rpc.ErrorInfo",
					"reason": "RATE_LIMIT_EXCEEDED",
					"domain": "googleapis.com",
					"metadata": map[string]string{
						"service":     "cloudcode-pa.googleapis.com",
						"quota_limit": "GenerateContentRequestsPerDay",
					},
				},
				map[string]any{
					"@type": "type.googleapis.com/google.rpc.QuotaFailure",
					"violations": []any{
						map[string]string{
							"subject":     "project:cloudcode-consumers",
							"description": "Daily request quota exceeded",
						},
					},
				},
			},
		},
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func (m *MockGoogleServer) handleRetrieveUserQuotaSummary(w http.ResponseWriter, r *http.Request) {
	_, bearer := m.record(r)
	b := m.getBehavior(bearer)

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)

	buckets := b.QuotaBuckets
	if len(buckets) == 0 {
		buckets = m.defaultB.QuotaBuckets
	}

	resp := RetrieveUserQuotaSummaryResponse{
		Groups: []QuotaSummaryGroup{
			{
				DisplayName: "Gemini Model Quotas",
				Description: "Quotas for Gemini pro and flash models",
				Buckets:     buckets,
			},
		},
		Description: "User quota summary response",
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (m *MockGoogleServer) handleRetrieveUserQuota(w http.ResponseWriter, r *http.Request) {
	_, bearer := m.record(r)
	b := m.getBehavior(bearer)

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)

	buckets := b.QuotaBuckets
	if len(buckets) == 0 {
		buckets = m.defaultB.QuotaBuckets
	}

	type legacyBucket struct {
		RemainingFraction float64 `json:"remaining_fraction"`
		RemainingAmount   int64   `json:"remaining_amount"`
		ResetTime         string  `json:"reset_time"`
		ModelID           string  `json:"model_id"`
		TokenType         int     `json:"token_type"`
	}

	var legacy []legacyBucket
	for _, b := range buckets {
		legacy = append(legacy, legacyBucket{
			RemainingFraction: b.RemainingFraction,
			RemainingAmount:   b.RemainingAmount,
			ResetTime:         b.ResetTime.Format(time.RFC3339),
			ModelID:           b.BucketID,
			TokenType:         1,
		})
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"buckets": legacy,
	})
}

func (m *MockGoogleServer) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	body, _ := m.record(r)

	// Check form fields
	query, _ := io.ReadAll(bytes.NewReader(body))
	params := string(query)

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")

	if strings.Contains(params, "refresh_token=revoked") || strings.Contains(params, "code=invalid_code") {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":             "invalid_grant",
			"error_description": "Token has been expired or revoked.",
		})
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  "mock_access_token_" + fmt.Sprintf("%d", time.Now().UnixNano()),
		"token_type":    "Bearer",
		"expires_in":    3600,
		"refresh_token": "mock_refresh_token_" + fmt.Sprintf("%d", time.Now().UnixNano()),
		"scope":         "openid https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/cloud-platform",
	})
}

func (m *MockGoogleServer) handleOAuthUserInfo(w http.ResponseWriter, r *http.Request) {
	_, bearer := m.record(r)
	b := m.getBehavior(bearer)

	email := b.Email
	if email == "" {
		email = "developer@mockgoogle.com"
	}

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":             "google_sub_123456789",
		"email":          email,
		"verified_email": true,
		"name":           "Antigravity Developer",
		"picture":        "https://lh3.googleusercontent.com/a/mock-avatar",
	})
}

// ParseSSEChunks parses an SSE HTTP response stream into a slice of raw data payloads.
func ParseSSEChunks(body io.Reader) ([]string, error) {
	var payloads []string
	scanner := bufio.NewScanner(body)
	var currentData strings.Builder

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			dataContent := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if currentData.Len() > 0 {
				currentData.WriteString("\n")
			}
			currentData.WriteString(dataContent)
		} else if line == "" {
			if currentData.Len() > 0 {
				payloads = append(payloads, currentData.String())
				currentData.Reset()
			}
		}
	}
	if currentData.Len() > 0 {
		payloads = append(payloads, currentData.String())
	}
	return payloads, scanner.Err()
}
