package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/config"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/oauth"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/quota"
)

// FallbackConfigSetter defines an interface for dynamically updating model fallback settings at runtime.
type FallbackConfigSetter interface {
	SetFallbackConfig(primary, secondary string, enabled bool)
}

// APIHandler implements the REST endpoints and SSE real-time streaming for the switcher.
type APIHandler struct {
	accountRepo          domain.AccountRepository
	quotaRepo            domain.QuotaRepository
	metricsService       domain.MetricsService
	broadcaster          domain.EventBroadcaster
	eventRepo            domain.EventRepository
	oauthEngine          oauth.OAuthEngine
	poller               QuotaPoller
	startTime            time.Time
	version              string
	cfgMu                sync.RWMutex
	appConfig            *config.Config
	fallbackConfigSetter FallbackConfigSetter
}

// SetConfig sets the configuration pointer for APIHandler.
func (a *APIHandler) SetConfig(c *config.Config) {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	a.appConfig = c
}

// SetFallbackConfigSetter sets the dynamic fallback setter for live proxy updates.
func (a *APIHandler) SetFallbackConfigSetter(s FallbackConfigSetter) {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	a.fallbackConfigSetter = s
}

// QuotaPoller defines interface for triggering quota polling passes.
type QuotaPoller interface {
	PollOnce(ctx context.Context) error
}

// SetPoller assigns the quota poller.
func (a *APIHandler) SetPoller(p QuotaPoller) {
	a.poller = p
}

// HandleQuotaRefresh serves POST /api/quota/refresh.
func (a *APIHandler) HandleQuotaRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if a.poller != nil {
		_ = a.poller.PollOnce(r.Context())
	}

	a.listAccounts(w, r)
}

// NewAPIHandler constructs an APIHandler with provided dependencies.
func NewAPIHandler(
	accountRepo domain.AccountRepository,
	quotaRepo domain.QuotaRepository,
	metricsService domain.MetricsService,
	broadcaster domain.EventBroadcaster,
	eventRepo domain.EventRepository,
	oauthEngine oauth.OAuthEngine,
	version string,
) *APIHandler {
	if version == "" {
		version = "0.1.0-dev"
	}
	return &APIHandler{
		accountRepo:    accountRepo,
		quotaRepo:      quotaRepo,
		metricsService: metricsService,
		broadcaster:    broadcaster,
		eventRepo:      eventRepo,
		oauthEngine:    oauthEngine,
		startTime:      time.Now(),
		version:        version,
	}
}

// StatusResponse represents server health and active account info.
type StatusResponse struct {
	Status        string          `json:"status"`
	Version       string          `json:"version"`
	UptimeSeconds int64           `json:"uptime_seconds"`
	ActiveAccount *domain.Account `json:"active_account,omitempty"`
	TotalAccounts int             `json:"total_accounts"`
	Timestamp     time.Time       `json:"timestamp"`
}

// HandleStatus serves GET /api/status.
func (a *APIHandler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	var active *domain.Account
	total := 0

	if a.accountRepo != nil {
		active, _ = a.accountRepo.GetActive(ctx)
		if list, err := a.accountRepo.List(ctx); err == nil {
			total = len(list)
		}
	}

	resp := StatusResponse{
		Status:        "ok",
		Version:       a.version,
		UptimeSeconds: int64(time.Since(a.startTime).Seconds()),
		ActiveAccount: active,
		TotalAccounts: total,
		Timestamp:     time.Now().UTC(),
	}

	writeJSON(w, http.StatusOK, resp)
}

// AccountWithBuckets encapsulates an Account and its associated quota buckets.
type AccountWithBuckets struct {
	*domain.Account
	Buckets []*domain.QuotaBucket `json:"buckets"`
}

// HandleAccounts serves GET /api/accounts and DELETE /api/accounts/{id}.
func (a *APIHandler) HandleAccounts(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/accounts")
	path = strings.Trim(path, "/")

	if path == "" {
		if r.Method == http.MethodGet {
			a.listAccounts(w, r)
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	parts := strings.Split(path, "/")
	accountID := parts[0]

	if len(parts) == 2 && parts[1] == "select" {
		if r.Method == http.MethodPost {
			a.selectAccount(w, r, accountID)
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			a.getAccount(w, r, accountID)
			return
		case http.MethodDelete:
			a.deleteAccount(w, r, accountID)
			return
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	}

	http.NotFound(w, r)
}

func (a *APIHandler) listAccounts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if a.accountRepo == nil {
		writeJSON(w, http.StatusOK, []*AccountWithBuckets{})
		return
	}

	accounts, err := a.accountRepo.List(ctx)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to list accounts", err)
		return
	}

	var allBuckets map[string][]*domain.QuotaBucket
	if a.quotaRepo != nil {
		allBuckets, _ = a.quotaRepo.ListAll(ctx)
	}

	result := make([]*AccountWithBuckets, 0, len(accounts))
	for _, acc := range accounts {
		var b []*domain.QuotaBucket
		if allBuckets != nil {
			b = allBuckets[acc.ID]
		}
		if b == nil {
			b = []*domain.QuotaBucket{}
		}
		result = append(result, &AccountWithBuckets{
			Account: acc,
			Buckets: b,
		})
	}

	writeJSON(w, http.StatusOK, result)
}

func (a *APIHandler) getAccount(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	if a.accountRepo == nil {
		http.NotFound(w, r)
		return
	}

	acc, err := a.accountRepo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, domain.ErrAccountNotFound) {
			http.NotFound(w, r)
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, "failed to get account", err)
		return
	}

	var buckets []*domain.QuotaBucket
	if a.quotaRepo != nil {
		buckets, _ = a.quotaRepo.GetByAccountID(ctx, id)
	}
	if buckets == nil {
		buckets = []*domain.QuotaBucket{}
	}

	writeJSON(w, http.StatusOK, &AccountWithBuckets{
		Account: acc,
		Buckets: buckets,
	})
}

func (a *APIHandler) selectAccount(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	if a.accountRepo == nil {
		writeErrorJSON(w, http.StatusServiceUnavailable, "account repository unavailable", nil)
		return
	}

	acc, err := a.accountRepo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, domain.ErrAccountNotFound) {
			writeErrorJSON(w, http.StatusNotFound, "account not found", err)
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, "failed to lookup account", err)
		return
	}

	if err := a.accountRepo.SetActive(ctx, id); err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to activate account", err)
		return
	}

	evt := &domain.ProxyEvent{
		Type:      domain.EventTypeAccountSwitched,
		AccountID: id,
		Message:   fmt.Sprintf("Account %s (%s) set as active via dashboard", acc.Email, id),
		Timestamp: time.Now().UTC(),
	}
	if a.broadcaster != nil {
		a.broadcaster.Broadcast(evt)
	}
	if a.eventRepo != nil {
		_ = a.eventRepo.Record(ctx, evt)
	}

	if a.poller != nil {
		go func() {
			_ = a.poller.PollOnce(context.Background())
		}()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"account_id": id,
		"email":      acc.Email,
	})
}

func (a *APIHandler) deleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	if a.accountRepo == nil {
		writeErrorJSON(w, http.StatusServiceUnavailable, "account repository unavailable", nil)
		return
	}

	acc, err := a.accountRepo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, domain.ErrAccountNotFound) {
			writeErrorJSON(w, http.StatusNotFound, "account not found", err)
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, "failed to lookup account", err)
		return
	}

	wasActive := acc.IsActive

	if err := a.accountRepo.Delete(ctx, id); err != nil {
		if errors.Is(err, domain.ErrAccountNotFound) {
			writeErrorJSON(w, http.StatusNotFound, "account not found", err)
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, "failed to delete account", err)
		return
	}

	if a.quotaRepo != nil {
		_ = a.quotaRepo.DeleteByAccountID(ctx, id)
	}

	// If the deleted account was active, auto-promote next available account
	if wasActive {
		if next, nextErr := a.accountRepo.GetNextAvailable(ctx, ""); nextErr == nil && next != nil {
			_ = a.accountRepo.SetActive(ctx, next.ID)
			if a.broadcaster != nil {
				a.broadcaster.Broadcast(&domain.ProxyEvent{
					Type:      domain.EventTypeAccountSwitched,
					AccountID: next.ID,
					Message:   fmt.Sprintf("Account %s automatically promoted to active following deletion of %s", next.Email, acc.Email),
					Timestamp: time.Now().UTC(),
				})
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"account_id": id,
	})
}

// HandleMetrics serves GET /api/metrics.
func (a *APIHandler) HandleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	if a.metricsService == nil {
		writeJSON(w, http.StatusOK, &domain.MetricsDashboardPayload{
			Summary: domain.GlobalDashboardSummary{
				Today:     &domain.AggregatedMetrics{},
				ThisWeek:  &domain.AggregatedMetrics{},
				ThisMonth: &domain.AggregatedMetrics{},
				AllTime:   &domain.AggregatedMetrics{},
			},
			ByAccount: []*domain.AccountMetricsSummary{},
			Timeline:  []*domain.DailyTokenUsage{},
		})
		return
	}

	accountID := r.URL.Query().Get("account_id")
	periodParam := r.URL.Query().Get("period")
	tzParam := r.URL.Query().Get("tz")
	tzOffsetParam := r.URL.Query().Get("tz_offset")
	loc := parseLocation(tzParam, tzOffsetParam)

	if accountID != "" {
		norm := domain.PeriodLifetime
		if periodParam != "" {
			norm = domain.MetricPeriod(strings.ToLower(periodParam))
		}
		summary, err := a.metricsService.GetSummary(ctx, accountID, norm)
		if err != nil {
			writeErrorJSON(w, http.StatusInternalServerError, "failed to get account summary", err)
			return
		}
		writeJSON(w, http.StatusOK, summary)
		return
	}

	payload, err := a.metricsService.GetDashboardPayloadWithLocation(ctx, 14, loc)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to compute metrics dashboard payload", err)
		return
	}

	writeJSON(w, http.StatusOK, payload)
}

// parseLocation resolves a *time.Location from either an IANA timezone name or a numeric minute offset.
func parseLocation(tzParam, tzOffsetParam string) *time.Location {
	if tzParam != "" {
		if loc, err := time.LoadLocation(tzParam); err == nil {
			return loc
		}
	}
	if tzOffsetParam != "" {
		// tz_offset in minutes: -new Date().getTimezoneOffset()
		// e.g. -180 for UTC-3, +540 for UTC+9
		if offsetMinutes, err := strconv.Atoi(tzOffsetParam); err == nil {
			hours := offsetMinutes / 60
			mins := int(math.Abs(float64(offsetMinutes % 60)))
			name := fmt.Sprintf("UTC%+03d:%02d", hours, mins)
			return time.FixedZone(name, offsetMinutes*60)
		}
	}
	return time.UTC
}

// HandleEvents serves GET /api/events as a real-time SSE stream.
func (a *APIHandler) HandleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported by client connection", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()

	// 1. Send recent historical events if event repository is present
	if a.eventRepo != nil {
		recent, err := a.eventRepo.ListRecent(ctx, 30)
		if err == nil && len(recent) > 0 {
			// ListRecent returns newest first; reverse for chronological playback
			for i := len(recent) - 1; i >= 0; i-- {
				evt := recent[i]
				if data, err := json.Marshal(evt); err == nil {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
				}
			}
			flusher.Flush()
		}
	}

	// 2. Stream real-time events via broadcaster
	if a.broadcaster == nil {
		<-ctx.Done()
		return
	}

	eventChan, unsubscribe := a.broadcaster.Subscribe()
	defer unsubscribe()

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-eventChan:
			if !ok {
				return
			}
			data, err := json.Marshal(evt)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// HandleOAuthStart serves POST/GET /oauth/start.
func (a *APIHandler) HandleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if a.oauthEngine == nil {
		writeErrorJSON(w, http.StatusNotImplemented, "OAuth2 engine not configured", nil)
		return
	}

	urlChan := make(chan string, 1)

	// Non-blocking trigger of loopback flow
	go func() {
		_, err := a.oauthEngine.StartLoopbackFlow(context.Background(), nil, func(authURL string) {
			select {
			case urlChan <- authURL:
			default:
			}
			if a.broadcaster != nil {
				a.broadcaster.Broadcast(&domain.ProxyEvent{
					Type:      domain.EventType("oauth_started"),
					Message:   fmt.Sprintf("OAuth authorization flow initiated: %s", authURL),
					Timestamp: time.Now().UTC(),
				})
			}
		})
		if err != nil && a.broadcaster != nil {
			a.broadcaster.Broadcast(&domain.ProxyEvent{
				Type:      domain.EventTypeError,
				Message:   fmt.Sprintf("OAuth flow failed: %v", err),
				Timestamp: time.Now().UTC(),
			})
		}
	}()

	var generatedAuthURL string
	select {
	case generatedAuthURL = <-urlChan:
	case <-time.After(1 * time.Second):
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "started",
		"message":  "OAuth loopback flow initiated in browser",
		"auth_url": generatedAuthURL,
	})
}

func writeJSON(w http.ResponseWriter, statusCode int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(data)
}

func writeErrorJSON(w http.ResponseWriter, statusCode int, message string, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":    statusCode,
			"message": message,
			"detail":  detail,
		},
	})
}

// ConfigResponse represents the response payload for GET /api/config.
type ConfigResponse struct {
	ModelPrimary             string `json:"model_primary"`
	ModelSecondary           string `json:"model_secondary"`
	FallbackSecondaryEnabled bool   `json:"fallback_secondary_enabled"`
}

// ConfigUpdateRequest represents the payload for POST /api/config.
type ConfigUpdateRequest struct {
	ModelPrimary             *string `json:"model_primary,omitempty"`
	ModelSecondary           *string `json:"model_secondary,omitempty"`
	FallbackSecondaryEnabled *bool   `json:"fallback_secondary_enabled,omitempty"`
}

// HandleConfig serves GET and POST /api/config.
func (a *APIHandler) HandleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.getConfig(w, r)
	case http.MethodPost, http.MethodPut:
		a.updateConfig(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *APIHandler) getConfig(w http.ResponseWriter, _ *http.Request) {
	a.cfgMu.RLock()
	var primary, secondary string
	var enabled bool
	if a.appConfig != nil {
		primary = a.appConfig.ModelPrimary
		secondary = a.appConfig.ModelSecondary
		enabled = a.appConfig.FallbackSecondaryEnabled
	}
	a.cfgMu.RUnlock()

	if primary == "" {
		if diskCfg, err := config.Load(); err == nil && diskCfg != nil {
			primary = diskCfg.ModelPrimary
			secondary = diskCfg.ModelSecondary
			enabled = diskCfg.FallbackSecondaryEnabled
		} else {
			def := config.DefaultConfig()
			primary = def.ModelPrimary
			secondary = def.ModelSecondary
			enabled = def.FallbackSecondaryEnabled
		}
	}

	writeJSON(w, http.StatusOK, ConfigResponse{
		ModelPrimary:             primary,
		ModelSecondary:           secondary,
		FallbackSecondaryEnabled: enabled,
	})
}

func (a *APIHandler) updateConfig(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	var req ConfigUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "invalid request body", err)
		return
	}

	a.cfgMu.Lock()
	var currentCfg *config.Config
	if a.appConfig != nil {
		currentCfg = a.appConfig
	} else {
		loaded, err := config.Load()
		if err != nil || loaded == nil {
			loaded = config.DefaultConfig()
		}
		currentCfg = loaded
		a.appConfig = loaded
	}

	if req.ModelPrimary != nil {
		if trimmed := strings.TrimSpace(*req.ModelPrimary); trimmed != "" {
			currentCfg.ModelPrimary = trimmed
		}
	}
	if req.ModelSecondary != nil {
		if trimmed := strings.TrimSpace(*req.ModelSecondary); trimmed != "" {
			currentCfg.ModelSecondary = trimmed
		}
	}
	if req.FallbackSecondaryEnabled != nil {
		currentCfg.FallbackSecondaryEnabled = *req.FallbackSecondaryEnabled
	}

	if err := currentCfg.Validate(); err != nil {
		a.cfgMu.Unlock()
		writeErrorJSON(w, http.StatusBadRequest, "invalid configuration", err)
		return
	}

	// Preserve existing non-model fields from disk config
	if diskCfg, err := config.Load(); err == nil && diskCfg != nil {
		if currentCfg.AntigravityBin == "" && diskCfg.AntigravityBin != "" {
			currentCfg.AntigravityBin = diskCfg.AntigravityBin
		}
		if currentCfg.DBPath == "" && diskCfg.DBPath != "" {
			currentCfg.DBPath = diskCfg.DBPath
		}
		if currentCfg.UpstreamURL == "" && diskCfg.UpstreamURL != "" {
			currentCfg.UpstreamURL = diskCfg.UpstreamURL
		}
		if currentCfg.QuotaInterval == "" && diskCfg.QuotaInterval != "" {
			currentCfg.QuotaInterval = diskCfg.QuotaInterval
		}
	}

	_ = config.Save(currentCfg)

	if a.fallbackConfigSetter != nil {
		a.fallbackConfigSetter.SetFallbackConfig(
			currentCfg.ModelPrimary,
			currentCfg.ModelSecondary,
			currentCfg.FallbackSecondaryEnabled,
		)
	}

	resp := ConfigResponse{
		ModelPrimary:             currentCfg.ModelPrimary,
		ModelSecondary:           currentCfg.ModelSecondary,
		FallbackSecondaryEnabled: currentCfg.FallbackSecondaryEnabled,
	}
	broadcaster := a.broadcaster
	a.cfgMu.Unlock()

	if broadcaster != nil {
		broadcaster.Broadcast(&domain.ProxyEvent{
			Type:    domain.EventTypeModelFallback,
			Message: fmt.Sprintf("Model fallback updated: Primary=%s, Secondary=%s, Enabled=%t", resp.ModelPrimary, resp.ModelSecondary, resp.FallbackSecondaryEnabled),
			Details: map[string]any{
				"model_primary":              resp.ModelPrimary,
				"model_secondary":            resp.ModelSecondary,
				"fallback_secondary_enabled": resp.FallbackSecondaryEnabled,
			},
			Timestamp: time.Now().UTC(),
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

// ModelsResponse models the JSON payload for GET /api/models.
type ModelsResponse struct {
	Models []*domain.ModelInfo `json:"models"`
	Source string              `json:"source"`
}

// HandleModels serves GET /api/models.
func (a *APIHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	models, source := a.discoverModels(ctx)

	writeJSON(w, http.StatusOK, ModelsResponse{
		Models: models,
		Source: source,
	})
}

func (a *APIHandler) discoverModels(ctx context.Context) ([]*domain.ModelInfo, string) {
	// 1. Try querying running language_server
	if lsModels, err := quota.QueryAvailableModels(ctx); err == nil && len(lsModels) > 0 {
		return a.ensureConfiguredModelsPresent(lsModels), "language_server"
	}

	// 2. Try inspecting quota buckets if language_server is not reachable
	if a.quotaRepo != nil {
		if allBuckets, err := a.quotaRepo.ListAll(ctx); err == nil && len(allBuckets) > 0 {
			seen := make(map[string]bool)
			var bucketModels []*domain.ModelInfo
			for _, buckets := range allBuckets {
				for _, b := range buckets {
					if b == nil || b.BucketID == "" {
						continue
					}
					cleanID := strings.TrimSpace(b.BucketID)
					if idx := strings.LastIndex(cleanID, "-"); idx > 0 {
						cleanID = cleanID[idx+1:]
					}
					if !seen[cleanID] && cleanID != "" {
						seen[cleanID] = true
						cat := "gemini"
						if strings.Contains(strings.ToLower(cleanID), "3p") || strings.Contains(strings.ToLower(b.DisplayName), "claude") {
							cat = "claude_gpt"
						}
						bucketModels = append(bucketModels, &domain.ModelInfo{
							ID:          cleanID,
							DisplayName: b.DisplayName,
							Category:    cat,
							Recommended: false,
						})
					}
				}
			}
			if len(bucketModels) > 0 {
				merged := append(bucketModels, quota.DefaultModelCatalog()...)
				return a.ensureConfiguredModelsPresent(merged), "quota_buckets"
			}
		}
	}

	// 3. Fallback to comprehensive Antigravity model catalog
	return a.ensureConfiguredModelsPresent(quota.DefaultModelCatalog()), "catalog"
}

func (a *APIHandler) ensureConfiguredModelsPresent(models []*domain.ModelInfo) []*domain.ModelInfo {
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		if m != nil {
			seen[m.ID] = true
		}
	}

	a.cfgMu.RLock()
	var primary, secondary string
	if a.appConfig != nil {
		primary = a.appConfig.ModelPrimary
		secondary = a.appConfig.ModelSecondary
	}
	a.cfgMu.RUnlock()

	if primary != "" && !seen[primary] {
		models = append(models, &domain.ModelInfo{
			ID:          primary,
			DisplayName: primary,
			Category:    "gemini",
			Recommended: true,
		})
		seen[primary] = true
	}
	if secondary != "" && !seen[secondary] {
		models = append(models, &domain.ModelInfo{
			ID:          secondary,
			DisplayName: secondary,
			Category:    "gemini",
			Recommended: true,
		})
	}

	return models
}
