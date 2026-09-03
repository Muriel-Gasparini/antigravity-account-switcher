package metrics

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

// Service implements domain.MetricsService.
type Service struct {
	metricsRepo domain.MetricsRepository
	accountRepo domain.AccountRepository
}

// NewService constructs a new MetricsService.
func NewService(metricsRepo domain.MetricsRepository, accountRepo domain.AccountRepository) *Service {
	return &Service{
		metricsRepo: metricsRepo,
		accountRepo: accountRepo,
	}
}

// NormalizePeriod parses and standardizes period strings into domain.MetricPeriod.
func NormalizePeriod(period domain.MetricPeriod) (domain.MetricPeriod, error) {
	switch strings.ToLower(strings.TrimSpace(string(period))) {
	case "day", "today", "daily":
		return domain.PeriodDay, nil
	case "week", "weekly", "this_week":
		return domain.PeriodWeek, nil
	case "month", "monthly", "this_month":
		return domain.PeriodMonth, nil
	case "lifetime", "total", "all", "all_time", "":
		return domain.PeriodLifetime, nil
	default:
		return "", fmt.Errorf("%w: %q", domain.ErrInvalidPeriod, period)
	}
}

// GetSummary retrieves aggregated metrics for an account or the global pool if accountID is empty.
func (s *Service) GetSummary(ctx context.Context, accountID string, period domain.MetricPeriod) (*domain.AggregatedMetrics, error) {
	norm, err := NormalizePeriod(period)
	if err != nil {
		return nil, err
	}

	if accountID != "" && s.accountRepo != nil {
		if _, err := s.accountRepo.GetByID(ctx, accountID); err != nil {
			return nil, err
		}
	}

	return s.metricsRepo.GetSummary(ctx, accountID, string(norm))
}

// GetGlobalSummary retrieves aggregated metrics across all accounts.
func (s *Service) GetGlobalSummary(ctx context.Context, period domain.MetricPeriod) (*domain.AggregatedMetrics, error) {
	return s.GetSummary(ctx, "", period)
}

// GetAccountBreakdown generates per-account summaries including accounts with 0 usage.
func (s *Service) GetAccountBreakdown(ctx context.Context, period domain.MetricPeriod) ([]*domain.AccountMetricsSummary, error) {
	norm, err := NormalizePeriod(period)
	if err != nil {
		return nil, err
	}

	if s.accountRepo == nil {
		return nil, nil
	}

	accounts, err := s.accountRepo.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list accounts for metrics breakdown: %w", err)
	}

	var summariesByAccount map[string]*domain.AggregatedMetrics
	if getter, ok := s.metricsRepo.(domain.AccountSummariesRepository); ok {
		summariesByAccount, err = getter.GetAccountSummaries(ctx, string(norm))
	}
	if summariesByAccount == nil || err != nil {
		summariesByAccount = make(map[string]*domain.AggregatedMetrics)
		for _, acc := range accounts {
			agg, sumErr := s.metricsRepo.GetSummary(ctx, acc.ID, string(norm))
			if sumErr != nil {
				return nil, sumErr
			}
			summariesByAccount[acc.ID] = agg
		}
	}

	result := make([]*domain.AccountMetricsSummary, 0, len(accounts))
	for _, acc := range accounts {
		agg, ok := summariesByAccount[acc.ID]
		if !ok || agg == nil {
			agg = &domain.AggregatedMetrics{}
		}
		result = append(result, &domain.AccountMetricsSummary{
			AccountID: acc.ID,
			Email:     acc.Email,
			Status:    acc.Status,
			IsActive:  acc.IsActive,
			Metrics:   agg,
		})
	}

	return result, nil
}

// GetDailyUsage retrieves daily grouped usage, with optional contiguous zero-filling.
func (s *Service) GetDailyUsage(ctx context.Context, accountID string, days int, zeroFill bool) ([]*domain.DailyTokenUsage, error) {
	if days <= 0 {
		days = 14
	}

	if accountID != "" && s.accountRepo != nil {
		if _, err := s.accountRepo.GetByID(ctx, accountID); err != nil {
			return nil, err
		}
	}

	history, err := s.metricsRepo.GetDailyHistory(ctx, accountID, days)
	if err != nil {
		return nil, err
	}

	if !zeroFill {
		return history, nil
	}

	lookup := make(map[string]*domain.DailyTokenUsage, len(history))
	for _, h := range history {
		lookup[h.Date] = h
	}

	now := time.Now().UTC()
	filled := make([]*domain.DailyTokenUsage, 0, days)
	for i := days - 1; i >= 0; i-- {
		dateStr := now.AddDate(0, 0, -i).Format("2006-01-02")
		if entry, exists := lookup[dateStr]; exists {
			filled = append(filled, entry)
		} else {
			filled = append(filled, &domain.DailyTokenUsage{
				Date: dateStr,
			})
		}
	}

	return filled, nil
}

// GetDashboardPayload composes the multi-window summary, breakdown, and timeline.
func (s *Service) GetDashboardPayload(ctx context.Context, timelineDays int) (*domain.MetricsDashboardPayload, error) {
	today, err := s.GetGlobalSummary(ctx, domain.PeriodDay)
	if err != nil {
		return nil, err
	}
	week, err := s.GetGlobalSummary(ctx, domain.PeriodWeek)
	if err != nil {
		return nil, err
	}
	month, err := s.GetGlobalSummary(ctx, domain.PeriodMonth)
	if err != nil {
		return nil, err
	}
	lifetime, err := s.GetGlobalSummary(ctx, domain.PeriodLifetime)
	if err != nil {
		return nil, err
	}

	breakdown, err := s.GetAccountBreakdown(ctx, domain.PeriodDay)
	if err != nil {
		return nil, err
	}

	timeline, err := s.GetDailyUsage(ctx, "", timelineDays, true)
	if err != nil {
		return nil, err
	}

	return &domain.MetricsDashboardPayload{
		Summary: domain.GlobalDashboardSummary{
			Today:     today,
			ThisWeek:  week,
			ThisMonth: month,
			AllTime:   lifetime,
		},
		ByAccount: breakdown,
		Timeline:  timeline,
	}, nil
}

// Record records a token metric directly.
func (s *Service) Record(ctx context.Context, m *domain.TokenMetric) error {
	return s.metricsRepo.Record(ctx, m)
}
