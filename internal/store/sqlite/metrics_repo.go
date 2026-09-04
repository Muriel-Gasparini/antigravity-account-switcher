package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

// MetricsRepository implements domain.MetricsRepository backed by SQLite.
type MetricsRepository struct {
	db *DB
}

// NewMetricsRepository creates a new SQLite MetricsRepository.
func NewMetricsRepository(db *DB) *MetricsRepository {
	return &MetricsRepository{db: db}
}

// Record logs an individual token usage event into token_metrics.
func (r *MetricsRepository) Record(ctx context.Context, m *domain.TokenMetric) error {
	now := time.Now().UTC()
	if m.Timestamp.IsZero() {
		m.Timestamp = now
	}
	if m.TotalTokens == 0 {
		m.TotalTokens = m.PromptTokens + m.CandidatesTokens
	}

	query := `
		INSERT INTO token_metrics (
			account_id, request_path, prompt_tokens, candidates_tokens,
			total_tokens, cached_content_tokens, thoughts_tokens, timestamp
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`
	res, err := r.db.ExecContext(ctx, query,
		m.AccountID,
		m.RequestPath,
		m.PromptTokens,
		m.CandidatesTokens,
		m.TotalTokens,
		m.CachedContentTokens,
		m.ThoughtsTokens,
		m.Timestamp.Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("failed to record token metric: %w", err)
	}

	id, err := res.LastInsertId()
	if err == nil {
		m.ID = id
	}

	return nil
}

// GetSummary calculates aggregated usage for an account (or global pool if accountID is empty).
func (r *MetricsRepository) GetSummary(ctx context.Context, accountID string, period string) (*domain.AggregatedMetrics, error) {
	now := time.Now().UTC()
	var since time.Time

	switch strings.ToLower(strings.TrimSpace(period)) {
	case "day", "today", "daily":
		since = now.Add(-24 * time.Hour)
	case "week", "weekly":
		since = now.Add(-7 * 24 * time.Hour)
	case "month", "monthly":
		since = now.Add(-30 * 24 * time.Hour)
	case "lifetime", "total", "all", "":
		// No time cutoff
	default:
		return nil, fmt.Errorf("unknown summary period: %q", period)
	}

	sinceStr := ""
	if !since.IsZero() {
		sinceStr = since.Format(time.RFC3339)
	}

	query := `
		SELECT
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(candidates_tokens), 0),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(cached_content_tokens), 0),
			COALESCE(SUM(thoughts_tokens), 0),
			COUNT(*)
		FROM token_metrics
		WHERE (? = '' OR account_id = ?)
		  AND (? = '' OR timestamp >= ?)
	`

	var agg domain.AggregatedMetrics
	err := r.db.QueryRowContext(ctx, query, accountID, accountID, sinceStr, sinceStr).Scan(
		&agg.TotalPromptTokens,
		&agg.TotalCandidatesTokens,
		&agg.TotalTokens,
		&agg.TotalCachedContentTokens,
		&agg.TotalThoughtsTokens,
		&agg.TotalRequests,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to aggregate token metrics: %w", err)
	}

	return &agg, nil
}

// GetAccountSummaries aggregates token metrics grouped by account_id for a given period.
func (r *MetricsRepository) GetAccountSummaries(ctx context.Context, period string) (map[string]*domain.AggregatedMetrics, error) {
	now := time.Now().UTC()
	var since time.Time

	switch strings.ToLower(strings.TrimSpace(period)) {
	case "day", "today", "daily":
		since = now.Add(-24 * time.Hour)
	case "week", "weekly":
		since = now.Add(-7 * 24 * time.Hour)
	case "month", "monthly":
		since = now.Add(-30 * 24 * time.Hour)
	case "lifetime", "total", "all", "":
		// No time cutoff
	default:
		return nil, fmt.Errorf("unknown summary period: %q", period)
	}

	sinceStr := ""
	if !since.IsZero() {
		sinceStr = since.Format(time.RFC3339)
	}

	query := `
		SELECT
			account_id,
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(candidates_tokens), 0),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(cached_content_tokens), 0),
			COALESCE(SUM(thoughts_tokens), 0),
			COUNT(*)
		FROM token_metrics
		WHERE (? = '' OR timestamp >= ?)
		GROUP BY account_id
	`

	rows, err := r.db.QueryContext(ctx, query, sinceStr, sinceStr)
	if err != nil {
		return nil, fmt.Errorf("failed to query account token summaries: %w", err)
	}
	defer rows.Close()

	summaries := make(map[string]*domain.AggregatedMetrics)
	for rows.Next() {
		var accountID string
		var agg domain.AggregatedMetrics
		err := rows.Scan(
			&accountID,
			&agg.TotalPromptTokens,
			&agg.TotalCandidatesTokens,
			&agg.TotalTokens,
			&agg.TotalCachedContentTokens,
			&agg.TotalThoughtsTokens,
			&agg.TotalRequests,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan account summary row: %w", err)
		}
		summaries[accountID] = &agg
	}

	return summaries, rows.Err()
}

// GetDailyHistory retrieves daily aggregated token usage for chart rendering over the past N days in UTC.
func (r *MetricsRepository) GetDailyHistory(ctx context.Context, accountID string, days int) ([]*domain.DailyTokenUsage, error) {
	return r.GetDailyHistoryInLocation(ctx, accountID, days, time.UTC)
}

// GetDailyHistoryInLocation retrieves daily aggregated token usage for chart rendering over the past N days in a specified location.
func (r *MetricsRepository) GetDailyHistoryInLocation(ctx context.Context, accountID string, days int, loc *time.Location) ([]*domain.DailyTokenUsage, error) {
	if days <= 0 {
		days = 14
	}
	if loc == nil {
		loc = time.UTC
	}

	now := time.Now().In(loc)
	earliestDay := now.AddDate(0, 0, -(days - 1))
	startOfWindow := time.Date(earliestDay.Year(), earliestDay.Month(), earliestDay.Day(), 0, 0, 0, 0, loc)
	sinceStr := startOfWindow.UTC().Format(time.RFC3339)

	_, offsetSec := now.Zone()
	modifier := fmt.Sprintf("%+d seconds", offsetSec)

	query := `
		SELECT
			strftime('%Y-%m-%d', timestamp, ?) AS day,
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(candidates_tokens), 0),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(cached_content_tokens), 0),
			COALESCE(SUM(thoughts_tokens), 0),
			COUNT(*)
		FROM token_metrics
		WHERE (? = '' OR account_id = ?)
		  AND timestamp >= ?
		GROUP BY day
		ORDER BY day ASC
	`

	rows, err := r.db.QueryContext(ctx, query, modifier, accountID, accountID, sinceStr)
	if err != nil {
		return nil, fmt.Errorf("failed to query daily token history: %w", err)
	}
	defer rows.Close()

	var history []*domain.DailyTokenUsage
	for rows.Next() {
		var u domain.DailyTokenUsage
		err := rows.Scan(
			&u.Date,
			&u.PromptTokens,
			&u.CandidatesTokens,
			&u.TotalTokens,
			&u.CachedContentTokens,
			&u.ThoughtsTokens,
			&u.RequestCount,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan daily usage row: %w", err)
		}
		history = append(history, &u)
	}

	return history, rows.Err()
}

