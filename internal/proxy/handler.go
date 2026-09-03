package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

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
	return func(c *Config) { c.MaxRetries = n }
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

// ProxyHandler is the reverse proxy engine routing IDE requests to Google Cloud Code PA.
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

// ServeHTTP handles incoming reverse and forward proxy requests, performs request buffering,
// dynamically injects active account Bearer tokens, retries upon HTTP 429 quota exhaustion,
// and streams SSE responses line-by-line while capturing token usage metrics.
func (h *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Handle HTTP CONNECT tunneling for transparent forward HTTPS proxying
	if r.Method == http.MethodConnect {
		h.handleConnect(w, r)
		return
	}

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

	// 3. Retry loop: attempt upstream request with failover on 429 / RESOURCE_EXHAUSTED
	var lastRespStatusCode int
	var lastRespHeader http.Header
	var lastErrBody []byte

	maxAttempts := h.cfg.MaxRetries
	if isPassThrough {
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
				destURL.Path = r.URL.Path
			} else {
				destURL.Path = strings.TrimRight(destURL.Path, "/") + "/" + strings.TrimLeft(r.URL.Path, "/")
			}
			destURL.RawPath = r.URL.RawPath
			destURL.RawQuery = r.URL.RawQuery
		} else {
			destURL = *r.URL
			if destURL.Scheme == "" {
				destURL.Scheme = "http"
			}
			if destURL.Host == "" {
				destURL.Host = r.Host
			}
		}

		outReq, reqErr := http.NewRequestWithContext(ctx, r.Method, destURL.String(), buffered.NewReader())
		if reqErr != nil {
			http.Error(w, fmt.Sprintf("failed to create upstream request: %v", reqErr), http.StatusInternalServerError)
			return
		}
		outReq.ContentLength = int64(buffered.Size())
		outReq.GetBody = func() (io.ReadCloser, error) {
			return buffered.NewReader(), nil
		}

		// Copy request headers, stripping hop-by-hop
		copyRequestHeaders(outReq.Header, r.Header)

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
			strings.Contains(r.URL.Path, "streamGenerateContent")
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
			_ = resp.Body.Close()
			newAccess, newExpiry, refErr := h.tokenRefresher.RefreshToken(ctx, currentAcc.RefreshToken)
			if refErr == nil {
				_ = h.accountRepo.UpdateToken(ctx, currentAcc.ID, newAccess, newExpiry)
				currentAcc.AccessToken = newAccess
				currentAcc.TokenExpiry = newExpiry
				continue // Retry upstream request with renewed token
			}
		}

		// Check for quota exhaustion: HTTP 429 or HTTP 403 with RESOURCE_EXHAUSTED
		if !isPassThrough && currentAcc != nil && resp.StatusCode == http.StatusTooManyRequests {
			bodyBytes, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			lastRespStatusCode = resp.StatusCode
			lastRespHeader = resp.Header.Clone()
			lastErrBody = bodyBytes

			nextAcc, rotateErr := h.failoverEngine.RotateAccount(ctx, currentAcc)
			if rotateErr != nil {
				// Entire account pool is exhausted! Forward upstream 429 response verbatim
				copyResponseHeaders(w.Header(), resp.Header)
				w.WriteHeader(resp.StatusCode)
				_, _ = w.Write(bodyBytes)
				return
			}

			// Successfully rotated to next account, retry request
			currentAcc = nextAcc
			continue
		}

		if !isPassThrough && currentAcc != nil && resp.StatusCode == http.StatusForbidden {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			_ = resp.Body.Close()

			if IsExhaustionResponse(resp.StatusCode, bodyBytes) {
				lastRespStatusCode = resp.StatusCode
				lastRespHeader = resp.Header.Clone()
				lastErrBody = bodyBytes

				nextAcc, rotateErr := h.failoverEngine.RotateAccount(ctx, currentAcc)
				if rotateErr != nil {
					copyResponseHeaders(w.Header(), resp.Header)
					w.WriteHeader(resp.StatusCode)
					_, _ = w.Write(bodyBytes)
					return
				}
				currentAcc = nextAcc
				continue
			}

			// Non-quota 403 Forbidden: transparent passthrough
			copyResponseHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(bodyBytes)
			return
		}

		// Non-exhausted upstream response (e.g. 200 OK, 400, 500)
		copyResponseHeaders(w.Header(), resp.Header)

		isRespSSE := isSSE || strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")

		w.WriteHeader(resp.StatusCode)

		if isRespSSE {
			_ = StreamAndInterceptSSE(ctx, w, resp.Body, currentAcc.ID, r.URL.Path, h.metricsRepo, h.eventBroadcaster)
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
		}
		w.WriteHeader(lastRespStatusCode)
		if len(lastErrBody) > 0 {
			_, _ = w.Write(lastErrBody)
		}
		return
	}

	http.Error(w, "maximum upstream retries exceeded", http.StatusServiceUnavailable)
}
