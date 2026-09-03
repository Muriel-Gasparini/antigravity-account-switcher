package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

type mockResponseWriter struct {
	header     http.Header
	buf        bytes.Buffer
	statusCode int
	flushCount int
	writeErr   error
	mu         sync.Mutex
}

func newMockResponseWriter() *mockResponseWriter {
	return &mockResponseWriter{
		header: make(http.Header),
	}
}

func (m *mockResponseWriter) Header() http.Header {
	return m.header
}

func (m *mockResponseWriter) Write(b []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	return m.buf.Write(b)
}

func (m *mockResponseWriter) WriteHeader(statusCode int) {
	m.statusCode = statusCode
}

func (m *mockResponseWriter) Flush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushCount++
}

type mockMetricsRepo struct {
	mu      sync.Mutex
	records []*domain.TokenMetric
}

func (m *mockMetricsRepo) Record(ctx context.Context, metric *domain.TokenMetric) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, metric)
	return nil
}

func (m *mockMetricsRepo) GetSummary(ctx context.Context, accountID string, period string) (*domain.AggregatedMetrics, error) {
	return &domain.AggregatedMetrics{}, nil
}

func (m *mockMetricsRepo) GetDailyHistory(ctx context.Context, accountID string, days int) ([]*domain.DailyTokenUsage, error) {
	return nil, nil
}

func (m *mockMetricsRepo) getRecords() []*domain.TokenMetric {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := make([]*domain.TokenMetric, len(m.records))
	copy(copied, m.records)
	return copied
}

func TestSSEInterceptor_DualSchemaParsing(t *testing.T) {
	tests := []struct {
		name                 string
		payload              string
		expectedPrompt       int64
		expectedCandidates   int64
		expectedTotal        int64
		expectedCached       int64
		expectedThoughts     int64
	}{
		{
			name: "nested response.usageMetadata (Google Cloud Code PA format)",
			payload: `data: {"response":{"candidates":[{"content":{"parts":[{"text":"Hello"}]}}],"usageMetadata":{"promptTokenCount":125,"candidatesTokenCount":42,"totalTokenCount":167,"cachedContentTokenCount":10,"thoughtsTokenCount":5}}}
`,
			expectedPrompt:     125,
			expectedCandidates: 42,
			expectedTotal:      167,
			expectedCached:     10,
			expectedThoughts:   5,
		},
		{
			name: "direct root usageMetadata (Gemini API format)",
			payload: `data: {"candidates":[{"content":{"parts":[{"text":"World"}]}}],"usageMetadata":{"promptTokenCount":80,"candidatesTokenCount":20,"totalTokenCount":100}}
`,
			expectedPrompt:     80,
			expectedCandidates: 20,
			expectedTotal:      100,
			expectedCached:     0,
			expectedThoughts:   0,
		},
		{
			name: "zero total token count auto-calculation",
			payload: `data: {"response":{"usageMetadata":{"promptTokenCount":50,"candidatesTokenCount":25,"totalTokenCount":0}}}
`,
			expectedPrompt:     50,
			expectedCandidates: 25,
			expectedTotal:      75,
			expectedCached:     0,
			expectedThoughts:   0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newMockResponseWriter()
			metricsRepo := &mockMetricsRepo{}
			broadcaster := NewBroadcaster(10)

			err := StreamAndInterceptSSE(
				context.Background(),
				w,
				strings.NewReader(tc.payload),
				"acc-123",
				"/v1internal:streamGenerateContent",
				metricsRepo,
				broadcaster,
			)
			if err != nil {
				t.Fatalf("StreamAndInterceptSSE failed: %v", err)
			}

			// Allow background goroutine in defer to finish
			var records []*domain.TokenMetric
			for i := 0; i < 50; i++ {
				records = metricsRepo.getRecords()
				if len(records) > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}

			if len(records) != 1 {
				t.Fatalf("expected 1 recorded metric, got %d", len(records))
			}

			rec := records[0]
			if rec.AccountID != "acc-123" {
				t.Errorf("expected account ID acc-123, got %s", rec.AccountID)
			}
			if rec.PromptTokens != tc.expectedPrompt {
				t.Errorf("expected prompt tokens %d, got %d", tc.expectedPrompt, rec.PromptTokens)
			}
			if rec.CandidatesTokens != tc.expectedCandidates {
				t.Errorf("expected candidates tokens %d, got %d", tc.expectedCandidates, rec.CandidatesTokens)
			}
			if rec.TotalTokens != tc.expectedTotal {
				t.Errorf("expected total tokens %d, got %d", tc.expectedTotal, rec.TotalTokens)
			}
			if rec.CachedContentTokens != tc.expectedCached {
				t.Errorf("expected cached tokens %d, got %d", tc.expectedCached, rec.CachedContentTokens)
			}
			if rec.ThoughtsTokens != tc.expectedThoughts {
				t.Errorf("expected thoughts tokens %d, got %d", tc.expectedThoughts, rec.ThoughtsTokens)
			}
		})
	}
}

func TestSSEInterceptor_ZeroLatencyFlushing(t *testing.T) {
	w := newMockResponseWriter()
	metricsRepo := &mockMetricsRepo{}
	broadcaster := NewBroadcaster(10)

	sseData := "data: line 1\n\ndata: line 2\n\ndata: line 3\n\n"
	err := StreamAndInterceptSSE(
		context.Background(),
		w,
		strings.NewReader(sseData),
		"acc-123",
		"/stream",
		metricsRepo,
		broadcaster,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if w.flushCount < 3 {
		t.Errorf("expected at least 3 flushes for 3 lines, got %d", w.flushCount)
	}
	if w.buf.String() != sseData {
		t.Errorf("downstream received content mismatch: got %q, want %q", w.buf.String(), sseData)
	}
}

func TestSSEInterceptor_LargeLinesOver64KB(t *testing.T) {
	// bufio.Scanner fails with ErrTooLong on lines > 64KB.
	largeText := strings.Repeat("A", 128*1024)
	payload := fmt.Sprintf(`data: {"response":{"candidates":[{"content":{"parts":[{"text":%q}]}}],"usageMetadata":{"promptTokenCount":500,"candidatesTokenCount":1000,"totalTokenCount":1500}}}`+"\n", largeText)

	w := newMockResponseWriter()
	metricsRepo := &mockMetricsRepo{}
	broadcaster := NewBroadcaster(10)

	err := StreamAndInterceptSSE(
		context.Background(),
		w,
		strings.NewReader(payload),
		"acc-large",
		"/large-stream",
		metricsRepo,
		broadcaster,
	)
	if err != nil {
		t.Fatalf("failed to process 128KB line: %v", err)
	}

	if w.buf.Len() != len(payload) {
		t.Fatalf("expected written len %d, got %d", len(payload), w.buf.Len())
	}

	// Verify tokens were recorded
	var records []*domain.TokenMetric
	for i := 0; i < 50; i++ {
		records = metricsRepo.getRecords()
		if len(records) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(records) != 1 {
		t.Fatalf("expected 1 recorded metric, got %d", len(records))
	}
	if records[0].TotalTokens != 1500 {
		t.Errorf("expected 1500 total tokens, got %d", records[0].TotalTokens)
	}
}

func TestSSEInterceptor_ResilienceOnClientDisconnect(t *testing.T) {
	// Simulate client disconnecting on downstream write
	w := newMockResponseWriter()
	w.writeErr = errors.New("client connection reset by peer")

	metricsRepo := &mockMetricsRepo{}
	broadcaster := NewBroadcaster(10)

	payload := `data: {"response":{"usageMetadata":{"promptTokenCount":40,"candidatesTokenCount":60,"totalTokenCount":100}}}
`
	err := StreamAndInterceptSSE(
		context.Background(),
		w,
		strings.NewReader(payload),
		"acc-disconnect",
		"/stream",
		metricsRepo,
		broadcaster,
	)

	// An error is expected because w.Write failed
	if err == nil {
		t.Fatal("expected write error on client disconnect, got nil")
	}

	// But despite client disconnect, because the data line was parsed before/in defer,
	// the token metric MUST still be safely persisted to SQLite!
	var records []*domain.TokenMetric
	for i := 0; i < 50; i++ {
		records = metricsRepo.getRecords()
		if len(records) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(records) != 1 {
		t.Fatalf("resilience failed: expected metric recorded even after client disconnect, got %d records", len(records))
	}
	if records[0].TotalTokens != 100 {
		t.Errorf("expected 100 tokens, got %d", records[0].TotalTokens)
	}
}

func TestSSEInterceptor_EventBroadcasting(t *testing.T) {
	w := newMockResponseWriter()
	metricsRepo := &mockMetricsRepo{}
	broadcaster := NewBroadcaster(10)

	eventsCh, unsubscribe := broadcaster.Subscribe()
	defer unsubscribe()

	payload := `data: {"response":{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":50,"totalTokenCount":150,"cachedContentTokenCount":20,"thoughtsTokenCount":10}}}
`
	err := StreamAndInterceptSSE(
		context.Background(),
		w,
		strings.NewReader(payload),
		"acc-broadcast",
		"/stream",
		metricsRepo,
		broadcaster,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case ev := <-eventsCh:
		if ev.Type != domain.EventTypeTokensCaptured {
			t.Errorf("expected event type %s, got %s", domain.EventTypeTokensCaptured, ev.Type)
		}
		if ev.AccountID != "acc-broadcast" {
			t.Errorf("expected account ID acc-broadcast, got %s", ev.AccountID)
		}
		if ev.Details["prompt_tokens"] != int64(100) {
			t.Errorf("expected prompt_tokens 100, got %v", ev.Details["prompt_tokens"])
		}
		if ev.Details["total_tokens"] != int64(150) {
			t.Errorf("expected total_tokens 150, got %v", ev.Details["total_tokens"])
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for EventTypeTokensCaptured event")
	}
}

func TestSSEInterceptor_StructWrapper(t *testing.T) {
	metricsRepo := &mockMetricsRepo{}
	broadcaster := NewBroadcaster(10)
	interceptor := NewSSEInterceptor(metricsRepo, broadcaster)

	w := newMockResponseWriter()
	payload := `data: {"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":20,"totalTokenCount":30}}
`
	err := interceptor.Intercept(context.Background(), w, strings.NewReader(payload), "acc-wrap", "/path")
	if err != nil {
		t.Fatalf("interceptor.Intercept failed: %v", err)
	}

	// Also test backward-compatible StreamAndIntercept
	err2 := StreamAndIntercept(w, strings.NewReader(payload), "acc-wrap2", "/path", metricsRepo, broadcaster)
	if err2 != nil {
		t.Fatalf("StreamAndIntercept failed: %v", err2)
	}
}

func TestSSEInterceptor_GzipStream(t *testing.T) {
	rawPayload := `data: {"response":{"usageMetadata":{"promptTokenCount":200,"candidatesTokenCount":80,"totalTokenCount":280}}}` + "\n\n"

	// Compress raw payload with gzip
	var gzBuf bytes.Buffer
	gzWriter := gzip.NewWriter(&gzBuf)
	_, _ = gzWriter.Write([]byte(rawPayload))
	_ = gzWriter.Close()

	w := newMockResponseWriter()
	metricsRepo := &mockMetricsRepo{}
	broadcaster := NewBroadcaster(10)

	err := StreamAndInterceptSSE(
		context.Background(),
		w,
		&gzBuf,
		"acc-gzip-test",
		"/v1internal:streamGenerateContent",
		metricsRepo,
		broadcaster,
	)
	if err != nil {
		t.Fatalf("failed to process gzipped SSE stream: %v", err)
	}

	// Verify decompressed content delivered downstream
	if w.buf.String() != rawPayload {
		t.Errorf("expected decompressed payload %q, got %q", rawPayload, w.buf.String())
	}

	// Verify token metrics recorded
	var records []*domain.TokenMetric
	for i := 0; i < 50; i++ {
		records = metricsRepo.getRecords()
		if len(records) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(records) != 1 {
		t.Fatalf("expected 1 metric recorded from gzipped stream, got %d", len(records))
	}
	if records[0].PromptTokens != 200 || records[0].CandidatesTokens != 80 || records[0].TotalTokens != 280 {
		t.Errorf("token counts mismatch: %+v", records[0])
	}
}
