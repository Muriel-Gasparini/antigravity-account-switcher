package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

type failingSummaryService struct {
	domain.MetricsService
	err error
}

func (f failingSummaryService) GetSummary(_ context.Context, _ string, _ domain.MetricPeriod) (*domain.AggregatedMetrics, error) {
	return nil, f.err
}

func seedAccount(t *testing.T, repo domain.AccountRepository, id string) {
	t.Helper()
	now := time.Now().UTC()
	acc := &domain.Account{
		ID:          id,
		Email:       id + "@example.com",
		AccessToken: "tok-" + id,
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := repo.Create(context.Background(), acc); err != nil {
		t.Fatalf("create account: %v", err)
	}
}

func decodeAPIError(t *testing.T, resp *http.Response) (code int, message string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return body.Error.Code, body.Error.Message
}

func TestAPI_Metrics_AccountSummaryErrorStatuses(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)
	seedAccount(t, accRepo, "acc-1")

	server, err := NewServer(accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	ts := httptest.NewServer(server)
	defer ts.Close()

	tests := []struct {
		name        string
		query       string
		wantStatus  int
		wantMessage string
	}{
		{"valid period", "?account_id=acc-1&period=week", http.StatusOK, ""},
		{"default period", "?account_id=acc-1", http.StatusOK, ""},
		{"bogus period", "?account_id=acc-1&period=fortnight", http.StatusBadRequest, "day, week, month, lifetime"},
		{"unknown account", "?account_id=missing&period=day", http.StatusNotFound, "account not found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(ts.URL + "/api/metrics" + tc.query)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantMessage == "" {
				return
			}
			code, msg := decodeAPIError(t, resp)
			if code != tc.wantStatus {
				t.Errorf("error.code = %d, want %d", code, tc.wantStatus)
			}
			if !strings.Contains(msg, tc.wantMessage) {
				t.Errorf("error.message = %q, want it to contain %q", msg, tc.wantMessage)
			}
		})
	}
}

func TestAPI_Metrics_UnexpectedErrorStays500(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)
	seedAccount(t, accRepo, "acc-1")
	stub := failingSummaryService{MetricsService: metricsSvc, err: errors.New("disk on fire")}

	server, err := NewServer(accRepo, quotaRepo, stub, broadcaster, eventRepo, nil)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	ts := httptest.NewServer(server)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/metrics?account_id=acc-1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}
