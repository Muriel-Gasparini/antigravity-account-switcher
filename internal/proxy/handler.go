package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/config"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

const (
	// DefaultTargetURL is the production Google Cloud Code PA endpoint.
	DefaultTargetURL = "https://daily-cloudcode-pa.googleapis.com"
	// DefaultMaxRetries is the default number of failover retry attempts.
	DefaultMaxRetries = 3
	// DefaultTokenMargin is the safety window before actual token expiration.
	DefaultTokenMargin = 60 * time.Second
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

// Config holds configuration parameters for the reverse proxy.
type Config struct {
	TargetURL         string
	MaxRetries        int
	maxRetriesSet     bool
	MaxBodyBytes      int64
	HTTPClient        *http.Client
	TokenExpiryMargin time.Duration
	TokenRefresher    TokenRefresher
	MetricsRepo       domain.MetricsRepository
	EventBroadcaster  domain.EventBroadcaster
	EventRepo         domain.EventRepository
	FailoverEngine    *FailoverEngine
}

// Option configures ProxyHandler behavior.
type Option func(*Config)

// WithTokenRefresher injects a TokenRefresher for proactive and reactive token renewal.
func WithTokenRefresher(refresher TokenRefresher) Option {
	return func(c *Config) { c.TokenRefresher = refresher }
}

// WithTargetURL overrides the upstream destination (e.g. for mock server testing).
func WithTargetURL(u string) Option {
	return func(c *Config) { c.TargetURL = u }
}

// WithMaxRetries configures the maximum number of failover retries.
func WithMaxRetries(n int) Option {
	return func(c *Config) {
		c.MaxRetries = n
		c.maxRetriesSet = true
	}
}

// WithMaxBodyBytes configures the in-memory request buffering upper bound.
func WithMaxBodyBytes(b int64) Option {
	return func(c *Config) { c.MaxBodyBytes = b }
}

// WithHTTPClient provides a custom *http.Client for upstream communication.
func WithHTTPClient(client *http.Client) Option {
	return func(c *Config) { c.HTTPClient = client }
}

// WithTokenExpiryMargin sets the safety margin for token expiration checks.
func WithTokenExpiryMargin(margin time.Duration) Option {
	return func(c *Config) { c.TokenExpiryMargin = margin }
}

// WithMetricsRepository injects a domain.MetricsRepository for SSE token persistence.
func WithMetricsRepository(repo domain.MetricsRepository) Option {
	return func(c *Config) { c.MetricsRepo = repo }
}

// WithEventBroadcaster injects a domain.EventBroadcaster for real-time telemetry.
func WithEventBroadcaster(broadcaster domain.EventBroadcaster) Option {
	return func(c *Config) { c.EventBroadcaster = broadcaster }
}

// WithEventRepository injects a domain.EventRepository for historical event storage.
func WithEventRepository(repo domain.EventRepository) Option {
	return func(c *Config) { c.EventRepo = repo }
}

// WithFailoverEngine injects a pre-configured FailoverEngine.
func WithFailoverEngine(engine *FailoverEngine) Option {
	return func(c *Config) { c.FailoverEngine = engine }
}

// Broadcaster implements domain.EventBroadcaster in memory for real-time subscribers.
type Broadcaster struct {
	mu          sync.RWMutex
	subscribers map[chan *domain.ProxyEvent]struct{}
	bufferSize  int
}

// NewBroadcaster constructs a thread-safe in-memory EventBroadcaster.
func NewBroadcaster(bufferSize int) *Broadcaster {
	if bufferSize <= 0 {
		bufferSize = 100
	}
	return &Broadcaster{
		subscribers: make(map[chan *domain.ProxyEvent]struct{}),
		bufferSize:  bufferSize,
	}
}

// Broadcast dispatches an event to all subscribers in a non-blocking manner.
func (b *Broadcaster) Broadcast(event *domain.ProxyEvent) {
	if b == nil {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subscribers {
		select {
		case ch <- event:
		default:
			// Drop on full channel to prevent blocking the proxy pipeline
		}
	}
}

// Subscribe returns a channel of events and an unsubscribe cleanup function.
func (b *Broadcaster) Subscribe() (<-chan *domain.ProxyEvent, func()) {
	ch := make(chan *domain.ProxyEvent, b.bufferSize)
	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subscribers, ch)
			close(ch)
			b.mu.Unlock()
		})
	}
	return ch, unsubscribe
}

// ProxyHandler is the reverse proxy engine routing Antigravity 2.0 requests to Google Cloud Code PA.
type ProxyHandler struct {
	cfg              Config
	targetURL        *url.URL
	accountRepo      domain.AccountRepository
	metricsRepo      domain.MetricsRepository
	eventBroadcaster domain.EventBroadcaster
	eventRepo        domain.EventRepository
	failoverEngine   *FailoverEngine
	tokenRefresher   TokenRefresher
	client           *http.Client
}

// NewProxyHandler creates an initialized ProxyHandler.
func NewProxyHandler(accountRepo domain.AccountRepository, opts ...Option) (*ProxyHandler, error) {
	if accountRepo == nil {
		return nil, errors.New("account repository cannot be nil")
	}

	cfg := Config{
		TargetURL:         DefaultTargetURL,
		MaxRetries:        DefaultMaxRetries,
		MaxBodyBytes:      DefaultMaxBodyBytes,
		TokenExpiryMargin: DefaultTokenMargin,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	parsedURL, err := url.Parse(cfg.TargetURL)
	if err != nil {
		return nil, fmt.Errorf("invalid target URL %q: %w", cfg.TargetURL, err)
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: 0, // No timeout to allow indefinite SSE streaming
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}

	broadcaster := cfg.EventBroadcaster
	if broadcaster == nil {
		broadcaster = NewBroadcaster(100)
	}

	failoverEngine := cfg.FailoverEngine
	if failoverEngine == nil {
		failoverEngine = NewFailoverEngine(accountRepo, broadcaster, cfg.EventRepo)
	}

	return &ProxyHandler{
		cfg:              cfg,
		targetURL:        parsedURL,
		accountRepo:      accountRepo,
		metricsRepo:      cfg.MetricsRepo,
		eventBroadcaster: broadcaster,
		eventRepo:        cfg.EventRepo,
		failoverEngine:   failoverEngine,
		tokenRefresher:   cfg.TokenRefresher,
		client:           client,
	}, nil
}

// TargetURL returns the parsed upstream base URL.
func (h *ProxyHandler) TargetURL() *url.URL {
	return h.targetURL
}

// FailoverEngine returns the underlying failover coordinator.
func (h *ProxyHandler) FailoverEngine() *FailoverEngine {
	return h.failoverEngine
}

// Broadcaster returns the event broadcaster.
func (h *ProxyHandler) Broadcaster() domain.EventBroadcaster {
	return h.eventBroadcaster
}

var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"trailers":            true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"proxy-connection":    true,
}

func copyRequestHeaders(dst, src http.Header) {
	customHops := make(map[string]bool)
	for _, val := range src["Connection"] {
		for _, token := range strings.Split(val, ",") {
			tok := strings.ToLower(strings.TrimSpace(token))
			if tok != "" {
				customHops[tok] = true
			}
		}
	}

	for k, vv := range src {
		lowerK := strings.ToLower(k)
		if hopByHopHeaders[lowerK] || customHops[lowerK] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	customHops := make(map[string]bool)
	for _, val := range src["Connection"] {
		for _, token := range strings.Split(val, ",") {
			tok := strings.ToLower(strings.TrimSpace(token))
			if tok != "" {
				customHops[tok] = true
			}
		}
	}

	for k, vv := range src {
		lowerK := strings.ToLower(k)
		if hopByHopHeaders[lowerK] || customHops[lowerK] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func setForwardingHeaders(outReq *http.Request, r *http.Request) {
	clientIP := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		clientIP = host
	}

	if prior := r.Header.Get("X-Forwarded-For"); prior != "" {
		outReq.Header.Set("X-Forwarded-For", prior+", "+clientIP)
	} else if clientIP != "" {
		outReq.Header.Set("X-Forwarded-For", clientIP)
	}

	proto := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		proto = "https"
	}
	outReq.Header.Set("X-Forwarded-Proto", proto)

	if r.Host != "" {
		outReq.Header.Set("X-Forwarded-Host", r.Host)
	}
}

func (h *ProxyHandler) handleConnect(w http.ResponseWriter, r *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, rw, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	destConn, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		_, _ = clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		_ = clientConn.Close()
		return
	}

	// Send standard RFC 7231 / RFC 9110 response directly to client socket without chunked encoding
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = destConn.Close()
		_ = clientConn.Close()
		return
	}

	// Bidirectional full-duplex tunnel
	go func() {
		defer destConn.Close()
		defer clientConn.Close()
		if rw != nil && rw.Reader.Buffered() > 0 {
			buf := make([]byte, rw.Reader.Buffered())
			_, _ = rw.Reader.Read(buf)
			_, _ = destConn.Write(buf)
		}
		_, _ = io.Copy(destConn, clientConn)
	}()

	go func() {
		defer destConn.Close()
		defer clientConn.Close()
		_, _ = io.Copy(clientConn, destConn)
	}()
}

// rewriteRequestModel rewrites the model in request URL path, query string,
// in-memory buffered body, and transport headers without modifying other parts of the request.
func (h *ProxyHandler) rewriteRequestModel(
	r *http.Request,
	buffered *BufferedRequest,
	targetModel string,
) *BufferedRequest {
	if r == nil || targetModel == "" || buffered == nil {
		return buffered
	}

	newPath := RewriteModelInPath(r.URL.Path, targetModel)
	newQuery := r.URL.RawQuery
	if newQuery != "" {
		newQuery = RewriteModelInQuery(newQuery, targetModel)
	}

	newBodyBytes := buffered.Bytes()
	if len(newBodyBytes) > 0 {
		if rewrittenBody, err := RewriteModelInBody(newBodyBytes, targetModel); err == nil {
			newBodyBytes = rewrittenBody
		}
	}

	newBuffered := &BufferedRequest{Body: newBodyBytes}
	SynchronizeRequest(r, newBodyBytes, newPath, newQuery)
	return newBuffered
}

// applyPredictiveFallback checks whether the incoming request targets an exhausted primary
// model on the active account, and if so, proactively rewrites it to the secondary model.
// Returns the updated BufferedRequest, the effective model name, and whether rewrite occurred.
func (h *ProxyHandler) applyPredictiveFallback(
	ctx context.Context,
	acc *domain.Account,
	r *http.Request,
	buffered *BufferedRequest,
) (*BufferedRequest, string, bool) {
	if h.failoverEngine == nil || acc == nil || r == nil || buffered == nil {
		return buffered, "", false
	}

	reqModel, _, _ := ExtractModelFromRequest(r, buffered.Bytes())
	if reqModel == "" {
		return buffered, "", false
	}

	shouldRewrite, targetModel, err := h.failoverEngine.PredictiveCheck(ctx, acc, reqModel)
	if err != nil || !shouldRewrite || targetModel == "" {
		return buffered, reqModel, false
	}

	newBuffered := h.rewriteRequestModel(r, buffered, targetModel)
	return newBuffered, targetModel, true
}

// syncFallbackConfigFromEnv dynamically reconciles the failover engine's fallback configuration
// with environment variable overrides if they are set (e.g. in E2E tests or runtime reconfigurations).
func (h *ProxyHandler) syncFallbackConfigFromEnv() {
	if h.failoverEngine == nil {
		return
	}
	envFallback := os.Getenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED")
	envPri := os.Getenv("ANTIGRAVITY_MODEL_PRIMARY")
	envSec := os.Getenv("ANTIGRAVITY_MODEL_SECONDARY")
	if envFallback == "" && envPri == "" && envSec == "" {
		return
	}

	h.failoverEngine.mu.RLock()
	pri := h.failoverEngine.modelPrimary
	sec := h.failoverEngine.modelSecondary
	enabled := h.failoverEngine.fallbackSecondaryEnabled
	h.failoverEngine.mu.RUnlock()

	changed := false
	if envPri != "" && strings.TrimSpace(envPri) != "" && strings.TrimSpace(envPri) != pri {
		pri = strings.TrimSpace(envPri)
		changed = true
	}
	if envSec != "" && strings.TrimSpace(envSec) != "" && strings.TrimSpace(envSec) != sec {
		sec = strings.TrimSpace(envSec)
		changed = true
	}
	if envFallback != "" {
		if b, err := config.ParseBool(envFallback); err == nil && b != enabled {
			enabled = b
			changed = true
		}
	}
	if changed {
		h.failoverEngine.SetFallbackConfig(pri, sec, enabled)
	}
}

// ServeHTTP handles incoming reverse and forward proxy requests, performs request buffering,
// dynamically injects active account Bearer tokens, retries upon HTTP 429 quota exhaustion,
// and streams SSE responses line-by-line while capturing token usage metrics.
func (h *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Handle HTTP CONNECT tunneling for transparent forward HTTPS proxying
	if r.Method == http.MethodConnect {
		h.handleConnect(w, r)
		return
	}

	// Reconcile fallback configuration with environment variables if set
	h.syncFallbackConfigFromEnv()

	// 1. Buffer request body up to configured limit for zero-error re-readability on failover
	buffered, err := NewBufferedRequestWithLimit(r, h.cfg.MaxBodyBytes)
	if err != nil {
		if errors.Is(err, ErrRequestBodyTooLarge) {
			w.Header().Set("Content-Type", "application/json; charset=UTF-8")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"code":413,"message":"request body exceeds maximum allowed size","status":"INVALID_ARGUMENT"}}`))
			return
		}
		http.Error(w, fmt.Sprintf("failed to buffer request body: %v", err), http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// A request is a reverse proxy request targeting Cloud Code PA if r.URL.Host is empty
	// (relative URI received by the reverse proxy), or if it explicitly targets Cloud Code PA.
	// Only explicit forward proxy requests to other hosts (e.g. speech.googleapis.com
	// or local host-bridge on another port) are handled in pass-through mode.
	isExplicitForwardToOther := r.URL.Host != "" && !strings.Contains(r.URL.Host, "cloudcode")

	isCloudCode := !isExplicitForwardToOther

	var currentAcc *domain.Account
	isPassThrough := false

	if isCloudCode {
		// Resolve active account from repository
		acc, accErr := h.accountRepo.GetActive(ctx)
		if accErr != nil || acc == nil {
			nextAcc, nextErr := h.accountRepo.GetNextAvailable(ctx, "")
			if nextErr == nil && nextAcc != nil {
				_ = h.accountRepo.SetActive(ctx, nextAcc.ID)
				currentAcc = nextAcc
			} else {
				// No accounts in pool: if client provided an Authorization header, pass it through;
				// otherwise, return 503 Service Unavailable
				if r.Header.Get("Authorization") != "" {
					isPassThrough = true
				} else {
					w.Header().Set("Content-Type", "application/json; charset=UTF-8")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":{"code":503,"message":"no active or available Google account","status":"UNAVAILABLE"}}`))
					return
				}
			}
		} else {
			currentAcc = acc
		}
	} else {
		isPassThrough = true
	}

	// 2. Capture original request baseline
	origModel, _, _ := ExtractModelFromRequest(r, buffered.Bytes())
	origPath := r.URL.Path
	origQuery := r.URL.RawQuery
	origBody := buffered.Bytes()

	currentModel := origModel
	currentPath := origPath
	currentQuery := origQuery
	currentBody := origBody

	// Predictive fallback check before initial upstream dispatch
	if isCloudCode && !isPassThrough && currentAcc != nil && origModel != "" {
		newBuffered, targetModel, rewritten := h.applyPredictiveFallback(ctx, currentAcc, r, buffered)
		if rewritten {
			buffered = newBuffered
			currentModel = targetModel
			currentPath = r.URL.Path
			currentQuery = r.URL.RawQuery
			currentBody = buffered.Bytes()
		}
	}

	// 3. Retry loop: attempt upstream request with failover on 429 / RESOURCE_EXHAUSTED
	var lastRespStatusCode int
	var lastRespHeader http.Header
	var lastErrBody []byte

	maxAttempts := h.cfg.MaxRetries
	if !isPassThrough {
		if !h.cfg.maxRetriesSet {
			totalAccounts := 1
			if accounts, accErr := h.accountRepo.List(ctx); accErr == nil && len(accounts) > 0 {
				totalAccounts = len(accounts)
			}
			tiers := 1
			if h.failoverEngine != nil {
				h.failoverEngine.mu.RLock()
				if h.failoverEngine.fallbackSecondaryEnabled {
					tiers = 2
				}
				h.failoverEngine.mu.RUnlock()
			}
			if dynamicBound := totalAccounts * tiers; dynamicBound > maxAttempts {
				maxAttempts = dynamicBound
			}
		}
		if maxAttempts > 50 {
			maxAttempts = 50
		}
	} else {
		maxAttempts = 0 // Pass-through doesn't rotate accounts
	}

	for attempt := 0; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return
		}

		// Proactively refresh access token if expired or near expiration window
		if !isPassThrough && currentAcc != nil && currentAcc.IsTokenExpired(h.cfg.TokenExpiryMargin) && h.tokenRefresher != nil && currentAcc.RefreshToken != "" {
			newAccess, newExpiry, refErr := h.tokenRefresher.RefreshToken(ctx, currentAcc.RefreshToken)
			if refErr == nil {
				_ = h.accountRepo.UpdateToken(ctx, currentAcc.ID, newAccess, newExpiry)
				currentAcc.AccessToken = newAccess
				currentAcc.TokenExpiry = newExpiry
			}
		}

		// Rebuild target URL
		var destURL url.URL
		if isCloudCode {
			destURL = *h.targetURL
			if destURL.Path == "" || destURL.Path == "/" {
				destURL.Path = currentPath
			} else {
				destURL.Path = strings.TrimRight(destURL.Path, "/") + "/" + strings.TrimLeft(currentPath, "/")
			}
			destURL.RawPath = r.URL.RawPath
			destURL.RawQuery = currentQuery
		} else {
			destURL = *r.URL
			if destURL.Scheme == "" {
				destURL.Scheme = "http"
			}
			if destURL.Host == "" {
				destURL.Host = r.Host
			}
		}

		outReq, reqErr := http.NewRequestWithContext(ctx, r.Method, destURL.String(), bytes.NewReader(currentBody))
		if reqErr != nil {
			http.Error(w, fmt.Sprintf("failed to create upstream request: %v", reqErr), http.StatusInternalServerError)
			return
		}
		outReq.ContentLength = int64(len(currentBody))
		outReq.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(currentBody)), nil
		}

		// Copy request headers, stripping hop-by-hop
		copyRequestHeaders(outReq.Header, r.Header)
		if len(currentBody) > 0 {
			outReq.Header.Set("Content-Length", strconv.Itoa(len(currentBody)))
		}

		// Retarget Host header
		if isCloudCode {
			outReq.Host = h.targetURL.Host
		} else {
			outReq.Host = destURL.Host
		}

		// Set standard forwarding headers
		setForwardingHeaders(outReq, r)

		// Authorization header handling
		if isPassThrough || currentAcc == nil {
			if clientAuth := r.Header.Get("Authorization"); clientAuth != "" {
				outReq.Header.Set("Authorization", clientAuth)
			}
		} else {
			outReq.Header.Set("Authorization", "Bearer "+currentAcc.AccessToken)
		}

		// Check if request expects SSE streaming
		isSSE := strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream") ||
			r.URL.Query().Get("alt") == "sse" ||
			strings.Contains(currentPath, "streamGenerateContent")
		if isSSE {
			// Disable upstream gzip compression so SSE data lines can be intercepted in plaintext in real-time
			outReq.Header.Set("Accept-Encoding", "identity")
		}

		// Send upstream
		resp, doErr := h.client.Do(outReq)
		if doErr != nil {
			if ctx.Err() != nil {
				return // Client disconnected
			}
			http.Error(w, fmt.Sprintf("upstream gateway error: %v", doErr), http.StatusBadGateway)
			return
		}

		// Check for expired/invalid credentials: HTTP 401 Unauthorized
		if !isPassThrough && currentAcc != nil && resp.StatusCode == http.StatusUnauthorized && h.tokenRefresher != nil && currentAcc.RefreshToken != "" {
			newAccess, newExpiry, refErr := h.tokenRefresher.RefreshToken(ctx, currentAcc.RefreshToken)
			if refErr == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512*1024))
				_ = resp.Body.Close()
				_ = h.accountRepo.UpdateToken(ctx, currentAcc.ID, newAccess, newExpiry)
				currentAcc.AccessToken = newAccess
				currentAcc.TokenExpiry = newExpiry
				continue // Retry upstream request with renewed token
			}
		}

		// Check for quota exhaustion: HTTP 429 or HTTP 403 with RESOURCE_EXHAUSTED
		if !isPassThrough && currentAcc != nil {
			var bodyBytes []byte
			isExhausted := false

			if resp.StatusCode == http.StatusTooManyRequests {
				bodyBytes, _ = io.ReadAll(io.LimitReader(resp.Body, 64*1024))
				isExhausted = true
			} else if (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound) && currentModel != origModel {
				bodyBytes, _ = io.ReadAll(io.LimitReader(resp.Body, 64*1024))
				isExhausted = true
			} else if resp.StatusCode == http.StatusForbidden {
				bodyBytes, _ = io.ReadAll(io.LimitReader(resp.Body, 64*1024))
				if IsExhaustionResponse(resp.StatusCode, bodyBytes) {
					isExhausted = true
				} else {
					// Non-quota 403 Forbidden passthrough: stream prefix + rest of body without truncating
					defer resp.Body.Close()
					copyResponseHeaders(w.Header(), resp.Header)
					w.WriteHeader(resp.StatusCode)
					if len(bodyBytes) > 0 {
						_, _ = w.Write(bodyBytes)
					}
					_, _ = io.Copy(w, resp.Body)
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
					return
				}
			}

			if isExhausted {
				lastRespStatusCode = resp.StatusCode
				lastRespHeader = resp.Header.Clone()
				lastErrBody = bodyBytes

				forwardTerminal := func() {
					defer resp.Body.Close()
					copyResponseHeaders(w.Header(), resp.Header)
					w.WriteHeader(resp.StatusCode)
					if len(bodyBytes) > 0 {
						_, _ = w.Write(bodyBytes)
					}
					_, _ = io.Copy(w, resp.Body)
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
				}

				discardAndClose := func() {
					_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512*1024))
					_ = resp.Body.Close()
				}

				if h.failoverEngine != nil {
					action, targetModel, nextAcc, failoverErr := h.failoverEngine.HandleExhaustion(ctx, currentAcc, currentModel)
					if failoverErr != nil || nextAcc == nil {
						// Entire account pool is exhausted! Forward upstream 429 response verbatim
						forwardTerminal()
						return
					}

					switch action {
					case ActionFallbackSecondary:
						discardAndClose()
						// Intra-account fallback: rewrite to secondary model on SAME account
						currentModel = targetModel
						currentPath = RewriteModelInPath(origPath, targetModel)
						if origQuery != "" {
							currentQuery = RewriteModelInQuery(origQuery, targetModel)
						} else {
							currentQuery = ""
						}
						currentBody = origBody
						if len(origBody) > 0 {
							if rewritten, rwErr := RewriteModelInBody(origBody, targetModel); rwErr == nil {
								currentBody = rewritten
							}
						}
						buffered = &BufferedRequest{Body: currentBody}
						SynchronizeRequest(r, currentBody, currentPath, currentQuery)
						currentAcc = nextAcc // Same account
						continue

					case ActionRotateAccount:
						discardAndClose()
						// Account rotated: switch to nextAcc, reset back to primary model and original body
						currentAcc = nextAcc
						currentModel = origModel
						currentPath = origPath
						currentQuery = origQuery
						currentBody = origBody
						buffered = &BufferedRequest{Body: currentBody}
						SynchronizeRequest(r, currentBody, currentPath, currentQuery)

						// Proactive check on new account
						if origModel != "" {
							if shouldRewrite, tgtModel, pErr := h.failoverEngine.PredictiveCheck(ctx, currentAcc, origModel); pErr == nil && shouldRewrite {
								currentModel = tgtModel
								currentPath = RewriteModelInPath(origPath, tgtModel)
								if origQuery != "" {
									currentQuery = RewriteModelInQuery(origQuery, tgtModel)
								}
								if len(origBody) > 0 {
									if rewritten, rwErr := RewriteModelInBody(origBody, tgtModel); rwErr == nil {
										currentBody = rewritten
									}
								}
								buffered = &BufferedRequest{Body: currentBody}
								SynchronizeRequest(r, currentBody, currentPath, currentQuery)
							}
						}
						continue

					default:
						// ActionNone: return upstream response
						forwardTerminal()
						return
					}
				} else {
					nextAcc, rotateErr := h.accountRepo.GetNextAvailable(ctx, currentAcc.ID)
					if rotateErr != nil || nextAcc == nil {
						forwardTerminal()
						return
					}
					discardAndClose()
					_ = h.accountRepo.UpdateStatus(ctx, currentAcc.ID, domain.AccountStatusExhausted)
					_ = h.accountRepo.SetActive(ctx, nextAcc.ID)
					currentAcc = nextAcc
					continue
				}
			}
		}

		// Non-exhausted upstream response (e.g. 200 OK, 400, 500)
		copyResponseHeaders(w.Header(), resp.Header)

		isRespSSE := isSSE || strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")

		w.WriteHeader(resp.StatusCode)

		var accID string
		if currentAcc != nil {
			accID = currentAcc.ID
		}

		if isRespSSE {
			_ = StreamAndInterceptSSE(ctx, w, resp.Body, accID, currentPath, h.metricsRepo, h.eventBroadcaster)
			_ = resp.Body.Close()
			return
		}

		// Unary response: copy body directly
		_, _ = io.Copy(w, resp.Body)
		_ = resp.Body.Close()
		return
	}

	// Max retries exceeded
	if lastRespStatusCode > 0 {
		if lastRespHeader != nil {
			copyResponseHeaders(w.Header(), lastRespHeader)
			w.Header().Del("Content-Length")
		}
		w.WriteHeader(lastRespStatusCode)
		if len(lastErrBody) > 0 {
			_, _ = w.Write(lastErrBody)
		}
		return
	}

	http.Error(w, "maximum upstream retries exceeded", http.StatusServiceUnavailable)
}
