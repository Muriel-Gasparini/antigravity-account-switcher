package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

// RawUsageMetadata represents the JSON structure of usageMetadata in Cloud Code PA and Gemini SSE chunks.
type RawUsageMetadata struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	TotalTokenCount         int64 `json:"totalTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount,omitempty"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount,omitempty"`
	ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount,omitempty"`
}

// SSEChunk represents an event chunk supporting both nested (Google Cloud Code PA)
// and direct root (Gemini API) usageMetadata schemas.
type SSEChunk struct {
	Response *struct {
		UsageMetadata *RawUsageMetadata `json:"usageMetadata,omitempty"`
	} `json:"response,omitempty"`
	UsageMetadata *RawUsageMetadata `json:"usageMetadata,omitempty"`
}

// SSEInterceptor intercepts and streams Server-Sent Events (SSE) in real-time,
// parsing token usage metadata and recording metrics asynchronously.
type SSEInterceptor struct {
	metricsRepo      domain.MetricsRepository
	eventBroadcaster domain.EventBroadcaster
}

// NewSSEInterceptor constructs a new SSEInterceptor.
func NewSSEInterceptor(metricsRepo domain.MetricsRepository, broadcaster domain.EventBroadcaster) *SSEInterceptor {
	return &SSEInterceptor{
		metricsRepo:      metricsRepo,
		eventBroadcaster: broadcaster,
	}
}

// Intercept streams the SSE body to the client while capturing token usage.
func (s *SSEInterceptor) Intercept(
	ctx context.Context,
	w http.ResponseWriter,
	upstreamBody io.Reader,
	accountID string,
	requestPath string,
) error {
	return StreamAndInterceptSSE(ctx, w, upstreamBody, accountID, requestPath, s.metricsRepo, s.eventBroadcaster)
}

// StreamAndInterceptSSE streams bytes from upstream to downstream line-by-line
// using http.Flusher in real-time for zero streaming latency, and inspects SSE data lines
// to extract usageMetadata. Asynchronously records metrics in a defer block using
// a detached context to ensure tokens are recorded even if the client disconnects.
func StreamAndInterceptSSE(
	ctx context.Context,
	w http.ResponseWriter,
	upstreamBody io.Reader,
	accountID string,
	requestPath string,
	metricsRepo domain.MetricsRepository,
	eventBroadcaster domain.EventBroadcaster,
) error {
	flusher, isFlusher := w.(http.Flusher)
	bufR := bufio.NewReader(upstreamBody)
	var bodyReader io.Reader = bufR

	// Detect and decompress gzip payload if upstream returned compressed stream
	if magic, err := bufR.Peek(2); err == nil && len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		if gzReader, err := gzip.NewReader(bufR); err == nil {
			defer gzReader.Close()
			bodyReader = gzReader
		}
	}
	reader := bufio.NewReader(bodyReader)
	var capturedUsage *RawUsageMetadata

	// Resilience: In defer, if capturedUsage != nil, persist asynchronously
	// with a detached context so mid-stream disconnects never drop token metrics.
	defer func() {
		if capturedUsage != nil && metricsRepo != nil {
			totTokens := capturedUsage.TotalTokenCount
			if totTokens == 0 {
				totTokens = capturedUsage.PromptTokenCount + capturedUsage.CandidatesTokenCount
			}
			metric := &domain.TokenMetric{
				AccountID:           accountID,
				RequestPath:         requestPath,
				PromptTokens:        capturedUsage.PromptTokenCount,
				CandidatesTokens:    capturedUsage.CandidatesTokenCount,
				TotalTokens:         totTokens,
				CachedContentTokens: capturedUsage.CachedContentTokenCount,
				ThoughtsTokens:      capturedUsage.ThoughtsTokenCount,
				Timestamp:           time.Now().UTC(),
			}

			go func(m *domain.TokenMetric, u *RawUsageMetadata) {
				detachedCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = metricsRepo.Record(detachedCtx, m)

				if eventBroadcaster != nil {
					eventBroadcaster.Broadcast(&domain.ProxyEvent{
						Type:      domain.EventTypeTokensCaptured,
						AccountID: accountID,
						Message:   formatUsageMessage(u),
						Details: map[string]any{
							"account_id":            accountID,
							"request_path":          requestPath,
							"prompt_tokens":         u.PromptTokenCount,
							"candidates_tokens":     u.CandidatesTokenCount,
							"total_tokens":          m.TotalTokens,
							"cached_content_tokens": u.CachedContentTokenCount,
							"thoughts_tokens":       u.ThoughtsTokenCount,
						},
						Timestamp: time.Now().UTC(),
					})
				}
			}(metric, capturedUsage)
		}
	}()

	for {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}

		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			// Check for SSE data line containing usageMetadata before writing,
			// ensuring that if the downstream write fails due to client abort,
			// we have already parsed the usage metadata for persistence.
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				dataContent := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
				// Fast path: substring search before JSON unmarshaling
				if bytes.Contains(dataContent, []byte("usageMetadata")) {
					if usage := parseUsageMetadata(dataContent); usage != nil {
						capturedUsage = usage
					}
				}
			}

			// Write immediately to downstream client
			if _, wErr := w.Write(line); wErr != nil {
				return wErr
			}
			if isFlusher {
				flusher.Flush()
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
	}

	return nil
}

// StreamAndIntercept provides backward compatibility without explicit context.
func StreamAndIntercept(
	w http.ResponseWriter,
	upstreamBody io.Reader,
	accountID string,
	requestPath string,
	metricsRepo domain.MetricsRepository,
	eventBroadcaster domain.EventBroadcaster,
) error {
	return StreamAndInterceptSSE(context.Background(), w, upstreamBody, accountID, requestPath, metricsRepo, eventBroadcaster)
}

func parseUsageMetadata(data []byte) *RawUsageMetadata {
	var chunk SSEChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil
	}
	if chunk.Response != nil && chunk.Response.UsageMetadata != nil {
		return chunk.Response.UsageMetadata
	}
	return chunk.UsageMetadata
}

func formatUsageMessage(u *RawUsageMetadata) string {
	parts := make([]string, 0, 5)
	if u.PromptTokenCount > 0 {
		parts = append(parts, fmt.Sprintf("prompt: %d", u.PromptTokenCount))
	}
	if u.CandidatesTokenCount > 0 {
		parts = append(parts, fmt.Sprintf("candidates: %d", u.CandidatesTokenCount))
	}
	if u.TotalTokenCount > 0 {
		parts = append(parts, fmt.Sprintf("total: %d", u.TotalTokenCount))
	}
	if u.CachedContentTokenCount > 0 {
		parts = append(parts, fmt.Sprintf("cached: %d", u.CachedContentTokenCount))
	}
	if u.ThoughtsTokenCount > 0 {
		parts = append(parts, fmt.Sprintf("thoughts: %d", u.ThoughtsTokenCount))
	}
	if len(parts) == 0 {
		return "tokens: 0"
	}
	return strings.Join(parts, ", ")
}
