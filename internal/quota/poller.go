package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

const (
	// DefaultPollInterval is the normal polling period for quota checks.
	DefaultPollInterval = 5 * time.Minute
	// DefaultBaseURL is the Google Cloud Code PA endpoint.
	DefaultBaseURL = "https://daily-cloudcode-pa.googleapis.com"
	// DefaultTokenExpiryMargin is the safety window before expiration to trigger token refresh.
	DefaultTokenExpiryMargin = 60 * time.Second
	// DefaultHTTPTimeout is the client network timeout.
	DefaultHTTPTimeout = 30 * time.Second
)

// TokenRefresher defines the contract for refreshing OAuth2 tokens.
type TokenRefresher interface {
	RefreshToken(ctx context.Context, refreshToken string) (accessToken string, expiry time.Time, err error)
}

// TokenRefresherFunc allows a function to satisfy the TokenRefresher interface.
type TokenRefresherFunc func(ctx context.Context, refreshToken string) (string, time.Time, error)

// RefreshToken invokes the underlying function.
func (f TokenRefresherFunc) RefreshToken(ctx context.Context, refreshToken string) (string, time.Time, error) {
	return f(ctx, refreshToken)
}

// Config holds options for the Quota Poller Daemon.
type Config struct {
	PollInterval      time.Duration
	BaseURL           string
	HTTPClient        *http.Client
	TokenExpiryMargin time.Duration
	TokenRefresher    TokenRefresher
	EventBroadcaster  domain.EventBroadcaster
	EventRepo         domain.EventRepository
}

// Option configures Config.
type Option func(*Config)

// WithPollInterval sets the interval between periodic polls.
func WithPollInterval(d time.Duration) Option {
	return func(c *Config) { c.PollInterval = d }
}

// WithBaseURL sets the base URL for Google Cloud Code PA requests.
func WithBaseURL(url string) Option {
	return func(c *Config) { c.BaseURL = strings.TrimRight(url, "/") }
}

// WithHTTPClient sets a custom http.Client.
func WithHTTPClient(client *http.Client) Option {
	return func(c *Config) { c.HTTPClient = client }
}

// WithTokenExpiryMargin sets the margin for proactive token refresh.
func WithTokenExpiryMargin(margin time.Duration) Option {
	return func(c *Config) { c.TokenExpiryMargin = margin }
}

// WithTokenRefresher sets the token refresh provider.
func WithTokenRefresher(refresher TokenRefresher) Option {
	return func(c *Config) { c.TokenRefresher = refresher }
}

// WithEventBroadcaster sets the real-time event broadcaster.
func WithEventBroadcaster(broadcaster domain.EventBroadcaster) Option {
	return func(c *Config) { c.EventBroadcaster = broadcaster }
}

// WithEventRepository sets the event persistence repository.
func WithEventRepository(repo domain.EventRepository) Option {
	return func(c *Config) { c.EventRepo = repo }
}

// Poller is the background daemon that periodically queries quota endpoints,
// updates SQLite quota buckets, and auto-restores exhausted accounts.
type Poller struct {
	cfg            Config
	accountRepo    domain.AccountRepository
	quotaRepo      domain.QuotaRepository
	tokenRefresher TokenRefresher
	broadcaster    domain.EventBroadcaster
	eventRepo      domain.EventRepository
	client         *http.Client

	stateMu sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}

	pollMu sync.Mutex
}

// NewPoller constructs a new Quota Poller Daemon.
func NewPoller(
	accountRepo domain.AccountRepository,
	quotaRepo domain.QuotaRepository,
	opts ...Option,
) (*Poller, error) {
	if accountRepo == nil {
		return nil, errors.New("account repository cannot be nil")
	}
	if quotaRepo == nil {
		return nil, errors.New("quota repository cannot be nil")
	}

	cfg := Config{
		PollInterval:      DefaultPollInterval,
		BaseURL:           DefaultBaseURL,
		TokenExpiryMargin: DefaultTokenExpiryMargin,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: DefaultHTTPTimeout,
		}
	}

	return &Poller{
		cfg:            cfg,
		accountRepo:    accountRepo,
		quotaRepo:      quotaRepo,
		tokenRefresher: cfg.TokenRefresher,
		broadcaster:    cfg.EventBroadcaster,
		eventRepo:      cfg.EventRepo,
		client:         client,
	}, nil
}

// Start begins background polling in a separate goroutine.
func (p *Poller) Start(ctx context.Context) error {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()

	if p.running {
		return errors.New("poller is already running")
	}

	pollCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	done := make(chan struct{})
	p.done = done
	p.running = true

	go p.loop(pollCtx, done)
	return nil
}

// Stop gracefully stops the background polling goroutine.
func (p *Poller) Stop() error {
	p.stateMu.Lock()
	if !p.running {
		p.stateMu.Unlock()
		return nil
	}
	cancel := p.cancel
	done := p.done
	p.stateMu.Unlock()

	if cancel != nil {
		cancel()
	}

	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("poller shutdown timed out")
	}
}

// IsRunning reports whether the background daemon is active.
func (p *Poller) IsRunning() bool {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.running
}

func (p *Poller) loop(ctx context.Context, done chan struct{}) {
	defer func() {
		p.stateMu.Lock()
		p.running = false
		p.stateMu.Unlock()
		close(done)
	}()

	// Initial immediate poll upon startup
	_ = p.PollOnce(ctx)

	// Early discovery loop: probe every 1.5s during the first 20s to detect
	// Antigravity language_server as soon as Antigravity 2.0 finishes launching.
	go func() {
		for i := 0; i < 12; i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(1500 * time.Millisecond):
				if _, ports, err := FindLocalLanguageServer(); err == nil && len(ports) > 0 {
					_ = p.PollOnce(ctx)
					return
				}
			}
		}
	}()

	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = p.PollOnce(ctx)
		}
	}
}

// PollOnce executes a single polling pass across all accounts.
func (p *Poller) PollOnce(ctx context.Context) error {
	p.pollMu.Lock()
	defer p.pollMu.Unlock()

	now := time.Now().UTC()

	// Prong 1: Database-level auto-restore check for accounts past reset time
	p.restoreExhaustedAccountsPastReset(ctx, now)

	accounts, err := p.accountRepo.List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list accounts: %w", err)
	}

	for _, acc := range accounts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if acc.Status == domain.AccountStatusDisabled {
			continue
		}

		if err := p.pollAccount(ctx, acc, now); err != nil {
			errStr := err.Error()
			if !strings.Contains(errStr, "403") && !strings.Contains(errStr, "SUBSCRIPTION_REQUIRED") {
				p.emitEvent(&domain.ProxyEvent{
					Type:      domain.EventTypeError,
					AccountID: acc.ID,
					Message:   fmt.Sprintf("Quota poll error for account %s: %v", acc.Email, err),
					Timestamp: now,
				})
			}
		}
	}

	return nil
}

// PollAccount polls quota for a specific account, updates buckets, and checks for reset.
func (p *Poller) PollAccount(ctx context.Context, accountID string) ([]*domain.QuotaBucket, error) {
	acc, err := p.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("account not found: %w", err)
	}

	now := time.Now().UTC()
	if err := p.pollAccount(ctx, acc, now); err != nil {
		return nil, err
	}

	return p.quotaRepo.GetByAccountID(ctx, accountID)
}

func (p *Poller) pollAccount(ctx context.Context, acc *domain.Account, now time.Time) error {
	// 1. Check token expiry and refresh if needed
	if acc.IsTokenExpired(p.cfg.TokenExpiryMargin) {
		if p.tokenRefresher != nil && acc.RefreshToken != "" {
			newAccess, newExpiry, err := p.tokenRefresher.RefreshToken(ctx, acc.RefreshToken)
			if err != nil {
				if strings.Contains(err.Error(), "invalid_grant") || strings.Contains(err.Error(), "revoked") {
					_ = p.accountRepo.UpdateStatus(ctx, acc.ID, domain.AccountStatusError)
				}
				return fmt.Errorf("failed to refresh token: %w", err)
			}

			if err := p.accountRepo.UpdateToken(ctx, acc.ID, newAccess, newExpiry); err != nil {
				return fmt.Errorf("failed to save refreshed token: %w", err)
			}
			acc.AccessToken = newAccess
			acc.TokenExpiry = newExpiry

			p.emitEvent(&domain.ProxyEvent{
				Type:      domain.EventTypeTokenRefreshed,
				AccountID: acc.ID,
				Message:   fmt.Sprintf("Refreshed access token for %s", acc.Email),
				Timestamp: now,
			})
		} else if acc.AccessToken == "" {
			return errors.New("missing access token and no refresher configured")
		}
	}

	// 2. Fetch quota summary
	var buckets []*domain.QuotaBucket
	if acc.IsActive && p.cfg.BaseURL == DefaultBaseURL {
		if lsBuckets, lsErr := QueryLocalLanguageServer(ctx, acc.ID); lsErr == nil && len(lsBuckets) > 0 {
			buckets = lsBuckets
		}
	}

	if len(buckets) == 0 {
		b, err := p.fetchQuotaSummary(ctx, acc)
		if err != nil {
			return err
		}
		buckets = b
	}

	// 3. Upsert buckets to SQLite
	if len(buckets) > 0 {
		_ = p.quotaRepo.DeleteByAccountID(ctx, acc.ID)
		if err := p.quotaRepo.UpsertBuckets(ctx, buckets); err != nil {
			return fmt.Errorf("failed to upsert quota buckets: %w", err)
		}
	}

	// 4. Prong 2: Auto-restore check from fresh quota buckets
	if acc.Status == domain.AccountStatusExhausted && len(buckets) > 0 {
		if p.isAccountQuotaRestored(buckets, now) {
			if err := p.accountRepo.UpdateStatus(ctx, acc.ID, domain.AccountStatusActive); err != nil {
				return fmt.Errorf("failed to restore account status: %w", err)
			}

			p.emitEvent(&domain.ProxyEvent{
				Type:      domain.EventTypeQuotaRestored,
				AccountID: acc.ID,
				Message:   fmt.Sprintf("Account %s restored to active status after quota reset", acc.Email),
				Details: map[string]any{
					"account_id": acc.ID,
					"email":      acc.Email,
					"buckets":    len(buckets),
				},
				Timestamp: now,
			})
		}
	}

	return nil
}

func (p *Poller) fetchQuotaSummary(ctx context.Context, acc *domain.Account) ([]*domain.QuotaBucket, error) {
	url := fmt.Sprintf("%s/v1internal:retrieveUserQuotaSummary", p.cfg.BaseURL)
	reqBody := []byte(`{}`)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create quota request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+acc.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "antigravity/2.12.0 (linux; x86_64)")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("quota HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		// Attempt one forced refresh
		if p.tokenRefresher != nil && acc.RefreshToken != "" {
			newAccess, newExpiry, refErr := p.tokenRefresher.RefreshToken(ctx, acc.RefreshToken)
			if refErr == nil {
				_ = p.accountRepo.UpdateToken(ctx, acc.ID, newAccess, newExpiry)
				acc.AccessToken = newAccess
				acc.TokenExpiry = newExpiry

				// Retry request once
				retryReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
				retryReq.Header.Set("Authorization", "Bearer "+acc.AccessToken)
				retryReq.Header.Set("Content-Type", "application/json")
				retryReq.Header.Set("User-Agent", "antigravity/2.12.0 (linux; x86_64)")
				resp2, err2 := p.client.Do(retryReq)
				if err2 == nil {
					defer resp2.Body.Close()
					if resp2.StatusCode == http.StatusOK {
						return p.parseQuotaResponse(acc.ID, resp2.Body)
					}
				}
			}
		}
		_ = p.accountRepo.UpdateStatus(ctx, acc.ID, domain.AccountStatusError)
		return nil, fmt.Errorf("unauthorized quota request (401)")
	}

	if resp.StatusCode == http.StatusNotFound {
		// Fallback to legacy :retrieveUserQuota
		return p.fetchLegacyQuota(ctx, acc)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		_ = p.accountRepo.UpdateStatus(ctx, acc.ID, domain.AccountStatusExhausted)
		return nil, fmt.Errorf("quota check received 429 RESOURCE_EXHAUSTED")
	}

	if resp.StatusCode == http.StatusForbidden {
		// Consumer / Standard Google account (SUBSCRIPTION_REQUIRED):
		// Consumer accounts do not have an enterprise quota API endpoint.
		// Maintain tracking buckets based on active/exhausted state.
		return p.generateConsumerQuotaBuckets(ctx, acc), nil
	}

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("quota request returned HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return p.parseQuotaResponse(acc.ID, resp.Body)
}

func (p *Poller) generateConsumerQuotaBuckets(ctx context.Context, acc *domain.Account) []*domain.QuotaBucket {
	now := time.Now().UTC()
	var dailyResetTime time.Time
	var weeklyResetTime time.Time

	dailyFraction := 1.0
	weeklyFraction := 1.0

	// Calculate default weekly reset (next Sunday 00:00 UTC)
	daysUntilSunday := (7 - int(now.Weekday())) % 7
	if daysUntilSunday == 0 {
		daysUntilSunday = 7
	}
	weeklyResetTime = time.Date(now.Year(), now.Month(), now.Day()+daysUntilSunday, 0, 0, 0, 0, time.UTC)
	dailyResetTime = time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)

	if acc.Status == domain.AccountStatusExhausted {
		// A 429 is a model cooldown / 5-hour burst exhaustion
		dailyFraction = 0.0
		existing, _ := p.quotaRepo.GetByAccountID(ctx, acc.ID)
		for _, b := range existing {
			if !b.ResetTime.IsZero() && b.ResetTime.After(now) {
				wLower := strings.ToLower(string(b.Window))
				idLower := strings.ToLower(b.BucketID)
				if strings.Contains(wLower, "weekly") {
					weeklyResetTime = b.ResetTime
					if b.RemainingFraction > 0 {
						weeklyFraction = b.RemainingFraction
					}
				} else if strings.Contains(wLower, "5h") || strings.Contains(idLower, "5h") {
					if b.ResetTime.Before(now.Add(5 * time.Hour)) {
						dailyResetTime = b.ResetTime
					}
				}
			}
		}
		if dailyResetTime.IsZero() || dailyResetTime.Before(now) || dailyResetTime.After(now.Add(5*time.Hour)) {
			dailyResetTime = now.Add(4 * time.Hour)
		}
		if weeklyFraction <= 0.0 {
			weeklyFraction = 0.69 // Keep weekly quota available
		}
	} else {
		// Active accounts have no active cooldown
		dailyResetTime = time.Time{}
		existing, _ := p.quotaRepo.GetByAccountID(ctx, acc.ID)
		for _, b := range existing {
			if strings.Contains(strings.ToLower(string(b.Window)), "weekly") && b.ResetTime.After(now) {
				weeklyResetTime = b.ResetTime
			}
		}
	}

	claude5h := &domain.QuotaBucket{
		BucketID:          acc.ID + "-3p-5h",
		AccountID:         acc.ID,
		DisplayName:       "Claude and GPT models (5h)",
		Window:            domain.QuotaWindow("5h"),
		RemainingFraction: dailyFraction,
		ResetTime:         dailyResetTime,
		UpdatedAt:         now,
	}

	claudeWeekly := &domain.QuotaBucket{
		BucketID:          acc.ID + "-3p-weekly",
		AccountID:         acc.ID,
		DisplayName:       "Claude and GPT models (weekly)",
		Window:            domain.QuotaWindow("weekly"),
		RemainingFraction: weeklyFraction,
		ResetTime:         weeklyResetTime,
		UpdatedAt:         now,
	}

	gemini5h := &domain.QuotaBucket{
		BucketID:          acc.ID + "-gemini-5h",
		AccountID:         acc.ID,
		DisplayName:       "Gemini Models (5h)",
		Window:            domain.QuotaWindow("5h"),
		RemainingFraction: 1.0,
		ResetTime:         dailyResetTime,
		UpdatedAt:         now,
	}

	geminiWeekly := &domain.QuotaBucket{
		BucketID:          acc.ID + "-gemini-weekly",
		AccountID:         acc.ID,
		DisplayName:       "Gemini Models (weekly)",
		Window:            domain.QuotaWindow("weekly"),
		RemainingFraction: 1.0,
		ResetTime:         weeklyResetTime,
		UpdatedAt:         now,
	}

	return []*domain.QuotaBucket{claude5h, claudeWeekly, gemini5h, geminiWeekly}
}

func (p *Poller) fetchLegacyQuota(ctx context.Context, acc *domain.Account) ([]*domain.QuotaBucket, error) {
	url := fmt.Sprintf("%s/v1internal:retrieveUserQuota", p.cfg.BaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(`{"project":""}`)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+acc.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("legacy quota request returned HTTP %d", resp.StatusCode)
	}

	return p.parseLegacyQuotaResponse(acc.ID, resp.Body)
}

// rawBucketDTO handles both camelCase and snake_case API variations.
type rawBucketDTO struct {
	BucketID          string          `json:"bucketId"`
	BucketIDAlt       string          `json:"bucket_id"`
	DisplayName       string          `json:"displayName"`
	DisplayNameAlt    string          `json:"display_name"`
	Window            string          `json:"window"`
	RemainingFraction float64         `json:"remainingFraction"`
	RemainingFracAlt  float64         `json:"remaining_fraction"`
	RemainingAmount   int64           `json:"remainingAmount"`
	RemainingAmtAlt   int64           `json:"remaining_amount"`
	Disabled          bool            `json:"disabled"`
	ResetTime         json.RawMessage `json:"resetTime"`
	ResetTimeAlt      json.RawMessage `json:"reset_time"`
}

type rawGroupDTO struct {
	DisplayName string         `json:"displayName"`
	Buckets     []rawBucketDTO `json:"buckets"`
}

type rawQuotaResponseDTO struct {
	Buckets []rawBucketDTO `json:"buckets"`
	Groups  []rawGroupDTO  `json:"groups"`
}

func (p *Poller) parseQuotaResponse(accountID string, r io.Reader) ([]*domain.QuotaBucket, error) {
	var payload rawQuotaResponseDTO
	if err := json.NewDecoder(r).Decode(&payload); err != nil {
		return nil, fmt.Errorf("failed to decode quota response: %w", err)
	}

	now := time.Now().UTC()
	var buckets []*domain.QuotaBucket

	// If groups are present (official Antigravity quota payload)
	if len(payload.Groups) > 0 {
		for _, g := range payload.Groups {
			groupName := g.DisplayName
			if strings.Contains(strings.ToLower(groupName), "claude") {
				groupName = "Claude and GPT models"
			} else if strings.Contains(strings.ToLower(groupName), "gemini") {
				groupName = "Gemini Models"
			}

			for _, b := range g.Buckets {
				rawID := b.BucketID
				if rawID == "" {
					rawID = b.BucketIDAlt
				}
				bucketID := rawID

				winLower := strings.ToLower(b.Window)
				window := domain.QuotaWindow("5h")
				if strings.Contains(winLower, "weekly") {
					window = domain.QuotaWindow("weekly")
				}

				displayName := fmt.Sprintf("%s (%s)", groupName, b.Window)

				fraction := b.RemainingFraction
				if fraction == 0 && b.RemainingFracAlt > 0 {
					fraction = b.RemainingFracAlt
				}

				resetTime := parseResetTime(b.ResetTime, b.ResetTimeAlt)

				buckets = append(buckets, &domain.QuotaBucket{
					AccountID:         accountID,
					BucketID:          bucketID,
					DisplayName:       displayName,
					Window:            window,
					RemainingFraction: fraction,
					RemainingAmount:   b.RemainingAmount,
					ResetTime:         resetTime,
					UpdatedAt:         now,
				})
			}
		}
		return buckets, nil
	}

	var allRaw []rawBucketDTO
	allRaw = append(allRaw, payload.Buckets...)

	seen := make(map[string]bool)

	for _, raw := range allRaw {
		bucketID := raw.BucketID
		if bucketID == "" {
			bucketID = raw.BucketIDAlt
		}
		if bucketID == "" || seen[bucketID] {
			continue
		}
		seen[bucketID] = true

		displayName := raw.DisplayName
		if displayName == "" {
			displayName = raw.DisplayNameAlt
		}

		windowStr := strings.ToUpper(raw.Window)
		window := domain.QuotaWindowDaily
		if windowStr == "WEEKLY" {
			window = domain.QuotaWindowWeekly
		}

		fraction := raw.RemainingFraction
		if fraction == 0 && raw.RemainingFracAlt > 0 {
			fraction = raw.RemainingFracAlt
		}

		amount := raw.RemainingAmount
		if amount == 0 && raw.RemainingAmtAlt > 0 {
			amount = raw.RemainingAmtAlt
		}

		resetTime := parseResetTime(raw.ResetTime, raw.ResetTimeAlt)

		buckets = append(buckets, &domain.QuotaBucket{
			AccountID:         accountID,
			BucketID:          bucketID,
			DisplayName:       displayName,
			Window:            window,
			RemainingFraction: fraction,
			RemainingAmount:   amount,
			ResetTime:         resetTime,
			UpdatedAt:         now,
		})
	}

	return buckets, nil
}

func (p *Poller) parseLegacyQuotaResponse(accountID string, r io.Reader) ([]*domain.QuotaBucket, error) {
	var payload struct {
		Buckets []struct {
			ModelID           string  `json:"model_id"`
			RemainingFraction float64 `json:"remaining_fraction"`
			RemainingAmount   int64   `json:"remaining_amount"`
			ResetTime         string  `json:"reset_time"`
		} `json:"buckets"`
	}

	if err := json.NewDecoder(r).Decode(&payload); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	var buckets []*domain.QuotaBucket
	for _, b := range payload.Buckets {
		t, _ := time.Parse(time.RFC3339, b.ResetTime)
		buckets = append(buckets, &domain.QuotaBucket{
			AccountID:         accountID,
			BucketID:          b.ModelID,
			DisplayName:       b.ModelID,
			Window:            domain.QuotaWindowDaily,
			RemainingFraction: b.RemainingFraction,
			RemainingAmount:   b.RemainingAmount,
			ResetTime:         t,
			UpdatedAt:         now,
		})
	}
	return buckets, nil
}

func parseResetTime(rawPrimary, rawAlt json.RawMessage) time.Time {
	raw := rawPrimary
	if len(raw) == 0 || string(raw) == "null" {
		raw = rawAlt
	}
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}
	}

	// Try unmarshaling as standard time.Time (RFC3339 string)
	var t time.Time
	if err := json.Unmarshal(raw, &t); err == nil && !t.IsZero() {
		return t.UTC()
	}

	// Try unmarshaling as string representation
	var strVal string
	if err := json.Unmarshal(raw, &strVal); err == nil && strVal != "" {
		if parsed, err := time.Parse(time.RFC3339, strVal); err == nil && !parsed.IsZero() {
			return parsed.UTC()
		}
	}

	// Try unmarshaling as int64 unix timestamp
	var unixSec int64
	if err := json.Unmarshal(raw, &unixSec); err == nil && unixSec > 0 {
		return time.Unix(unixSec, 0).UTC()
	}

	return time.Time{}
}

func (p *Poller) isAccountQuotaRestored(buckets []*domain.QuotaBucket, now time.Time) bool {
	if len(buckets) == 0 {
		return false
	}
	// An account is restored if all buckets satisfy either:
	// 1. remainingFraction > 0.0
	// 2. now >= resetTime (or HasReset)
	for _, b := range buckets {
		if b.RemainingFraction <= 0.0 && !b.HasReset(now) {
			return false
		}
	}
	return true
}

func (p *Poller) restoreExhaustedAccountsPastReset(ctx context.Context, now time.Time) {
	resetIDs, err := p.quotaRepo.GetExhaustedAccountsPastReset(ctx, now)
	if err != nil || len(resetIDs) == 0 {
		return
	}

	for _, id := range resetIDs {
		if err := p.accountRepo.UpdateStatus(ctx, id, domain.AccountStatusActive); err != nil {
			continue
		}

		acc, err := p.accountRepo.GetByID(ctx, id)
		email := id
		if err == nil && acc != nil {
			email = acc.Email
		}

		p.emitEvent(&domain.ProxyEvent{
			Type:      domain.EventTypeQuotaRestored,
			AccountID: id,
			Message:   fmt.Sprintf("Account %s restored to active (all quota buckets passed reset time)", email),
			Details: map[string]any{
				"account_id": id,
				"email":      email,
				"reason":     "reset_time_elapsed",
			},
			Timestamp: now,
		})
	}
}

func (p *Poller) emitEvent(event *domain.ProxyEvent) {
	if p.broadcaster != nil {
		p.broadcaster.Broadcast(event)
	}
	if p.eventRepo != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = p.eventRepo.Record(ctx, event)
	}
}
