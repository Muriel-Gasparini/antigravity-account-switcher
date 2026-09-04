package domain

import (
	"context"
	"errors"
	"time"
)

// ErrInvalidPeriod indicates an unrecognized aggregation time window.
var ErrInvalidPeriod = errors.New("invalid metric period")

// MetricPeriod defines the aggregation time window.
type MetricPeriod string

const (
	PeriodDay      MetricPeriod = "day"      // Past 24 hours
	PeriodWeek     MetricPeriod = "week"     // Past 7 days
	PeriodMonth    MetricPeriod = "month"    // Past 30 days
	PeriodLifetime MetricPeriod = "lifetime" // Lifetime / all-time
	PeriodTotal    MetricPeriod = "total"    // Equivalent to lifetime
)

// TokenMetric stores an individual generation request's token consumption.
type TokenMetric struct {
	ID                  int64     `json:"id"`
	AccountID           string    `json:"account_id"`
	RequestPath         string    `json:"request_path"`
	PromptTokens        int64     `json:"prompt_tokens"`
	CandidatesTokens    int64     `json:"candidates_tokens"`
	TotalTokens         int64     `json:"total_tokens"`
	CachedContentTokens int64     `json:"cached_content_tokens"`
	ThoughtsTokens      int64     `json:"thoughts_tokens,omitempty"`
	Timestamp           time.Time `json:"timestamp"`
}

// AggregatedMetrics represents summarized token metrics over a given period.
type AggregatedMetrics struct {
	TotalPromptTokens        int64 `json:"total_prompt_tokens"`
	TotalCandidatesTokens    int64 `json:"total_candidates_tokens"`
	TotalTokens              int64 `json:"total_tokens"`
	TotalCachedContentTokens int64 `json:"total_cached_content_tokens"`
	TotalThoughtsTokens      int64 `json:"total_thoughts_tokens"`
	TotalRequests            int64 `json:"total_requests"`
}

// DailyTokenUsage represents daily grouped usage for chart rendering.
type DailyTokenUsage struct {
	Date                string `json:"date"` // "YYYY-MM-DD"
	PromptTokens        int64  `json:"prompt_tokens"`
	CandidatesTokens    int64  `json:"candidates_tokens"`
	TotalTokens         int64  `json:"total_tokens"`
	CachedContentTokens int64  `json:"cached_content_tokens,omitempty"`
	ThoughtsTokens      int64  `json:"thoughts_tokens,omitempty"`
	RequestCount        int64  `json:"request_count"`
}

// AccountMetricsSummary associates an account's identity with its aggregated consumption.
type AccountMetricsSummary struct {
	AccountID string             `json:"account_id"`
	Email     string             `json:"email"`
	Status    AccountStatus      `json:"status"`
	IsActive  bool               `json:"is_active"`
	Metrics   *AggregatedMetrics `json:"metrics"`
}

// GlobalDashboardSummary encapsulates multi-window summaries for dashboard cards.
type GlobalDashboardSummary struct {
	Today     *AggregatedMetrics `json:"today"`
	ThisWeek  *AggregatedMetrics `json:"this_week"`
	ThisMonth *AggregatedMetrics `json:"this_month"`
	AllTime   *AggregatedMetrics `json:"all_time"`
}

// MetricsDashboardPayload provides the unified payload for GET /api/metrics.
type MetricsDashboardPayload struct {
	Summary   GlobalDashboardSummary   `json:"summary"`
	ByAccount []*AccountMetricsSummary `json:"by_account"`
	Timeline  []*DailyTokenUsage       `json:"timeline"`
}

// MetricsRepository defines persistence and aggregation operations for token metrics.
type MetricsRepository interface {
	// Record logs an individual token usage event.
	Record(ctx context.Context, m *TokenMetric) error

	// GetSummary calculates aggregated usage for an account (or global if accountID is empty)
	// across the specified time period.
	GetSummary(ctx context.Context, accountID string, period string) (*AggregatedMetrics, error)

	// GetDailyHistory retrieves daily grouped usage for the past N days for charting.
	GetDailyHistory(ctx context.Context, accountID string, days int) ([]*DailyTokenUsage, error)
}

// AccountSummariesRepository defines the batch account aggregation capability.
type AccountSummariesRepository interface {
	GetAccountSummaries(ctx context.Context, period string) (map[string]*AggregatedMetrics, error)
}

// TimezoneDailyHistoryRepository defines the timezone-aware daily aggregation capability.
type TimezoneDailyHistoryRepository interface {
	GetDailyHistoryInLocation(ctx context.Context, accountID string, days int, loc *time.Location) ([]*DailyTokenUsage, error)
}

// MetricsService specifies the use-case methods exposed to HTTP handlers and CLI.
type MetricsService interface {
	// GetSummary returns token consumption for a single account over the specified period.
	GetSummary(ctx context.Context, accountID string, period MetricPeriod) (*AggregatedMetrics, error)

	// GetGlobalSummary returns aggregate consumption across all accounts over the specified period.
	GetGlobalSummary(ctx context.Context, period MetricPeriod) (*AggregatedMetrics, error)

	// GetAccountBreakdown returns token metrics for each configured account over the specified period.
	GetAccountBreakdown(ctx context.Context, period MetricPeriod) ([]*AccountMetricsSummary, error)

	// GetDailyUsage returns daily time-series usage for charting over the past N days.
	GetDailyUsage(ctx context.Context, accountID string, days int, zeroFill bool) ([]*DailyTokenUsage, error)

	// GetDailyUsageInLocation returns daily time-series usage for charting over the past N days in a specific timezone location.
	GetDailyUsageInLocation(ctx context.Context, accountID string, days int, zeroFill bool, loc *time.Location) ([]*DailyTokenUsage, error)

	// GetDashboardPayload returns the comprehensive dataset required by M4 UI.
	GetDashboardPayload(ctx context.Context, timelineDays int) (*MetricsDashboardPayload, error)

	// GetDashboardPayloadWithLocation returns the comprehensive dataset adjusted for a specific timezone location.
	GetDashboardPayloadWithLocation(ctx context.Context, timelineDays int, loc *time.Location) (*MetricsDashboardPayload, error)

	// Record logs an individual token usage metric (delegates to repository).
	Record(ctx context.Context, m *TokenMetric) error
}
