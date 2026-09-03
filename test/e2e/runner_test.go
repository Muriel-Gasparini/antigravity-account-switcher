package e2e

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/metrics"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/oauth"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/proxy"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/quota"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/web"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/test/mocks"
)

// TestEnvironment coordinates mock Google servers, SQLite database, and the switcher server.
type TestEnvironment struct {
	MockGoogle     *mocks.MockGoogleServer
	DB             *sqlite.DB
	AccountRepo    domain.AccountRepository
	QuotaRepo      domain.QuotaRepository
	MetricsRepo    domain.MetricsRepository
	EventRepo      domain.EventRepository
	Broadcaster    *proxy.Broadcaster
	FailoverEngine *proxy.FailoverEngine
	ProxyHandler   *proxy.ProxyHandler
	Poller         *quota.Poller
	MetricsService *metrics.Service
	OAuthService   *oauth.OAuthService
	Server         *web.Server
	ServerURL      string
	ServerPort     int
}

// setupE2EEnvironment initializes a full in-process E2E testing rig.
func setupE2EEnvironment(t *testing.T, pollInterval time.Duration) *TestEnvironment {
	t.Helper()

	mockGoogle := mocks.NewMockGoogleServer()
	t.Cleanup(func() { mockGoogle.Close() })

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "e2e_test.db")

	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open sqlite DB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	accRepo := sqlite.NewAccountRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)
	metricsRepo := sqlite.NewMetricsRepository(db)
	eventRepo := sqlite.NewEventRepository(db)

	broadcaster := proxy.NewBroadcaster(200)
	failoverEngine := proxy.NewFailoverEngine(accRepo, broadcaster, eventRepo)

	proxyHandler, err := proxy.NewProxyHandler(
		accRepo,
		proxy.WithTargetURL(mockGoogle.URL),
		proxy.WithMetricsRepository(metricsRepo),
		proxy.WithEventBroadcaster(broadcaster),
		proxy.WithEventRepository(eventRepo),
		proxy.WithFailoverEngine(failoverEngine),
	)
	if err != nil {
		t.Fatalf("failed to create proxy handler: %v", err)
	}

	if pollInterval <= 0 {
		pollInterval = 50 * time.Millisecond
	}

	poller, err := quota.NewPoller(
		accRepo,
		quotaRepo,
		quota.WithBaseURL(mockGoogle.URL),
		quota.WithPollInterval(pollInterval),
		quota.WithEventBroadcaster(broadcaster),
		quota.WithEventRepository(eventRepo),
	)
	if err != nil {
		t.Fatalf("failed to create quota poller: %v", err)
	}

	metricsSvc := metrics.NewService(metricsRepo, accRepo)
	oauthSvc := oauth.NewOAuthService(
		accRepo,
		oauth.WithTokenURL(mockGoogle.URL+"/token"),
		oauth.WithUserInfoURL(mockGoogle.URL+"/oauth2/v3/userinfo"),
	)

	server, err := web.NewServer(
		accRepo,
		quotaRepo,
		metricsSvc,
		broadcaster,
		eventRepo,
		oauthSvc,
		web.WithPort(0), // ephemeral port
		web.WithBindAddr("127.0.0.1"),
		web.WithProxyHandler(proxyHandler),
	)
	if err != nil {
		t.Fatalf("failed to create web server: %v", err)
	}

	if err := server.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Stop(ctx)
		_ = poller.Stop()
	})

	serverPort := server.Port()
	serverURL := fmt.Sprintf("http://127.0.0.1:%d", serverPort)

	// Wait for server health
	ready := false
	for i := 0; i < 30; i++ {
		resp, err := http.Get(serverURL + "/api/status")
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("server failed to become healthy at %s", serverURL)
	}

	return &TestEnvironment{
		MockGoogle:     mockGoogle,
		DB:             db,
		AccountRepo:    accRepo,
		QuotaRepo:      quotaRepo,
		MetricsRepo:    metricsRepo,
		EventRepo:      eventRepo,
		Broadcaster:    broadcaster,
		FailoverEngine: failoverEngine,
		ProxyHandler:   proxyHandler,
		Poller:         poller,
		MetricsService: metricsSvc,
		OAuthService:   oauthSvc,
		Server:         server,
		ServerURL:      serverURL,
		ServerPort:     serverPort,
	}
}
