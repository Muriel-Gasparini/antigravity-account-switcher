package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/config"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/metrics"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/oauth"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/proxy"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/quota"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/web"
)

// Config holds options for the Wrap supervisor.
type Config struct {
	Port                     int
	DBPath                   string
	TargetURL                string
	PollInterval             time.Duration
	OpenBrowser              bool
	ModelPrimary             string
	ModelSecondary           string
	FallbackSecondaryEnabled bool
	Stdin                    io.Reader
	Stdout                   io.Writer
	Stderr                   io.Writer

	// Injectable dependencies for unit and integration testing
	DB     *sqlite.DB
	Server *web.Server
	Poller *quota.Poller
}

// Option configures Wrap behavior.
type Option func(*Config)

// WithModelFallback configures the model fallback primary, secondary, and enabled flag.
func WithModelFallback(primary, secondary string, enabled bool) Option {
	return func(c *Config) {
		c.ModelPrimary = primary
		c.ModelSecondary = secondary
		c.FallbackSecondaryEnabled = enabled
	}
}

// WithPort sets the switcher listening port (0 allocates a random ephemeral port).
func WithPort(p int) Option {
	return func(c *Config) { c.Port = p }
}

// WithDBPath sets the path to the SQLite database file.
func WithDBPath(path string) Option {
	return func(c *Config) { c.DBPath = path }
}

// WithTargetURL sets the Google Cloud Code PA endpoint.
func WithTargetURL(url string) Option {
	return func(c *Config) { c.TargetURL = url }
}

// WithPollInterval sets the background quota poller frequency.
func WithPollInterval(d time.Duration) Option {
	return func(c *Config) { c.PollInterval = d }
}

// WithOpenBrowser enables automatic opening of the web dashboard in default browser.
func WithOpenBrowser(open bool) Option {
	return func(c *Config) { c.OpenBrowser = open }
}

// WithIO redirects child process standard streams.
func WithIO(stdin io.Reader, stdout, stderr io.Writer) Option {
	return func(c *Config) {
		c.Stdin = stdin
		c.Stdout = stdout
		c.Stderr = stderr
	}
}

// WithServer injects an existing web.Server instance.
func WithServer(s *web.Server) Option {
	return func(c *Config) { c.Server = s }
}

// WithPoller injects an existing quota.Poller instance.
func WithPoller(p *quota.Poller) Option {
	return func(c *Config) { c.Poller = p }
}

// WithDB injects an existing sqlite.DB instance.
func WithDB(db *sqlite.DB) Option {
	return func(c *Config) { c.DB = db }
}

// BuildScopedEnv constructs a clean environment slice with scoped proxy settings,
// without mutating the global process environment.
func BuildScopedEnv(baseEnv []string, proxyURL string) []string {
	keysToOmit := map[string]bool{
		"http_proxy":     true,
		"https_proxy":    true,
		"HTTP_PROXY":     true,
		"HTTPS_PROXY":    true,
		"cloud_code_url": true,
		"CLOUD_CODE_URL": true,
		"appimage":       true,
		"APPIMAGE":       true,
	}

	result := make([]string, 0, len(baseEnv)+5)
	for _, envVar := range baseEnv {
		parts := strings.SplitN(envVar, "=", 2)
		if len(parts) > 0 {
			if keysToOmit[parts[0]] || keysToOmit[strings.ToUpper(parts[0])] || keysToOmit[strings.ToLower(parts[0])] {
				continue
			}
		}
		result = append(result, envVar)
	}

	result = append(result,
		"HTTP_PROXY="+proxyURL,
		"HTTPS_PROXY="+proxyURL,
		"http_proxy="+proxyURL,
		"https_proxy="+proxyURL,
		"NO_PROXY=localhost,127.0.0.1,::1,speech.googleapis.com",
		"no_proxy=localhost,127.0.0.1,::1,speech.googleapis.com",
		"CLOUD_CODE_URL="+proxyURL,
	)

	return result
}

// Wrap launches the in-process switcher server, scopes proxy variables to cmdArgs,
// couples process lifecycles via PR_SET_PDEATHSIG, and terminates the switcher
// immediately upon child completion.
func Wrap(ctx context.Context, cmdArgs []string, opts ...Option) (int, error) {
	if len(cmdArgs) == 0 {
		return 1, errors.New("no command specified to wrap")
	}

	cfg := Config{
		Port:         0, // Default to ephemeral port for collision-free launch
		PollInterval: 5 * time.Minute,
		Stdin:        os.Stdin,
		Stdout:       os.Stdout,
		Stderr:       os.Stderr,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	var dbToClose *sqlite.DB
	server := cfg.Server
	poller := cfg.Poller

	var (
		accRepo        *sqlite.AccountRepository
		quotaRepo      *sqlite.QuotaRepository
		metricsRepo    *sqlite.MetricsRepository
		eventRepo      *sqlite.EventRepository
		broadcaster    *proxy.Broadcaster
		metricsService *metrics.Service
		oauthService   *oauth.OAuthService
		proxyHandler   http.Handler
		failoverEngine *proxy.FailoverEngine
		appCfg         *config.Config
	)

	// 1. Initialize dependencies if server is not pre-injected
	if server == nil {
		var db *sqlite.DB
		var err error
		if cfg.DB != nil {
			db = cfg.DB
		} else {
			dbPath := cfg.DBPath
			if dbPath == "" {
				dbPath = defaultDBPath()
			}
			db, err = sqlite.Open(dbPath)
			if err != nil {
				return 1, fmt.Errorf("failed to open sqlite database at %s: %w", dbPath, err)
			}
			dbToClose = db
		}

		accRepo = sqlite.NewAccountRepository(db)
		quotaRepo = sqlite.NewQuotaRepository(db)
		metricsRepo = sqlite.NewMetricsRepository(db)
		eventRepo = sqlite.NewEventRepository(db)

		loadedCfg, _ := config.Load()
		if loadedCfg != nil {
			appCfg = loadedCfg
		} else {
			appCfg = config.DefaultConfig()
		}
		if cfg.Port > 0 {
			appCfg.Port = cfg.Port
		}
		if cfg.TargetURL != "" {
			appCfg.UpstreamURL = cfg.TargetURL
		}
		if cfg.ModelPrimary != "" {
			appCfg.ModelPrimary = cfg.ModelPrimary
		}
		if cfg.ModelSecondary != "" {
			appCfg.ModelSecondary = cfg.ModelSecondary
		}
		appCfg.FallbackSecondaryEnabled = cfg.FallbackSecondaryEnabled

		broadcaster = proxy.NewBroadcaster(100)
		failoverEngine = proxy.NewFailoverEngine(
			accRepo,
			broadcaster,
			eventRepo,
			proxy.WithQuotaRepository(quotaRepo),
			proxy.WithModelFallback(cfg.ModelPrimary, cfg.ModelSecondary, cfg.FallbackSecondaryEnabled),
		)
		oauthService = oauth.NewOAuthService(accRepo)

		// Automatically import existing Antigravity login if pool is empty
		_, _ = oauth.AutoImportExistingAccount(ctx, accRepo, oauthService)

		tokenRefresher := quota.TokenRefresherFunc(func(ctx context.Context, rt string) (string, time.Time, error) {
			resp, err := oauthService.RefreshToken(ctx, rt)
			if err != nil {
				return "", time.Time{}, err
			}
			return resp.AccessToken, time.Now().UTC().Add(time.Duration(resp.ExpiresIn) * time.Second), nil
		})

		proxyOpts := []proxy.Option{
			proxy.WithMetricsRepository(metricsRepo),
			proxy.WithEventBroadcaster(broadcaster),
			proxy.WithEventRepository(eventRepo),
			proxy.WithFailoverEngine(failoverEngine),
			proxy.WithTokenRefresher(tokenRefresher),
		}
		if cfg.TargetURL != "" {
			proxyOpts = append(proxyOpts, proxy.WithTargetURL(cfg.TargetURL))
		}

		var pErr error
		proxyHandler, pErr = proxy.NewProxyHandler(accRepo, proxyOpts...)
		if pErr != nil {
			if dbToClose != nil {
				_ = dbToClose.Close()
			}
			return 1, fmt.Errorf("failed to initialize proxy handler: %w", pErr)
		}

		pollerOpts := []quota.Option{
			quota.WithPollInterval(cfg.PollInterval),
			quota.WithEventBroadcaster(broadcaster),
			quota.WithEventRepository(eventRepo),
			quota.WithTokenRefresher(tokenRefresher),
		}
		if cfg.TargetURL != "" {
			pollerOpts = append(pollerOpts, quota.WithBaseURL(cfg.TargetURL))
		}
		pollerInstance, err := quota.NewPoller(accRepo, quotaRepo, pollerOpts...)
		if err != nil {
			if dbToClose != nil {
				_ = dbToClose.Close()
			}
			return 1, fmt.Errorf("failed to initialize quota poller: %w", err)
		}
		poller = pollerInstance

		metricsService = metrics.NewService(metricsRepo, accRepo)

		serverInstance, err := web.NewServer(
			accRepo,
			quotaRepo,
			metricsService,
			broadcaster,
			eventRepo,
			oauthService,
			web.WithPort(cfg.Port),
			web.WithBindAddr("127.0.0.1"),
			web.WithProxyHandler(proxyHandler),
			web.WithPoller(poller),
			web.WithFallbackConfigSetter(failoverEngine),
			web.WithConfig(appCfg),
		)
		if err != nil {
			if dbToClose != nil {
				_ = dbToClose.Close()
			}
			return 1, fmt.Errorf("failed to initialize web server: %w", err)
		}
		server = serverInstance
	}

	// 2. Start background poller if available
	if poller != nil {
		_ = poller.Start(ctx)
		defer func() {
			_ = poller.Stop()
		}()
	}

	// 3. Start web/proxy server
	if err := server.Start(); err != nil {
		if strings.Contains(err.Error(), "address already in use") && cfg.Port > 0 {
			fmt.Printf("Notice: Port %d is already in use, falling back to ephemeral port...\n", cfg.Port)
			serverOpts := []web.Option{
				web.WithPort(0),
				web.WithBindAddr("127.0.0.1"),
				web.WithProxyHandler(proxyHandler),
			}
			if poller != nil {
				serverOpts = append(serverOpts, web.WithPoller(poller))
			}
			if failoverEngine != nil {
				serverOpts = append(serverOpts, web.WithFallbackConfigSetter(failoverEngine))
			}
			if appCfg != nil {
				serverOpts = append(serverOpts, web.WithConfig(appCfg))
			}
			serverInstance, fallbackErr := web.NewServer(
				accRepo,
				quotaRepo,
				metricsService,
				broadcaster,
				eventRepo,
				oauthService,
				serverOpts...,
			)
			if fallbackErr == nil && serverInstance.Start() == nil {
				server = serverInstance
			} else {
				if dbToClose != nil {
					_ = dbToClose.Close()
				}
				return 1, fmt.Errorf("failed to start switcher server: %w", err)
			}
		} else {
			if dbToClose != nil {
				_ = dbToClose.Close()
			}
			return 1, fmt.Errorf("failed to start switcher server: %w", err)
		}
	}

	defer func() {
		shutdownCtx, sCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer sCancel()
		_ = server.Stop(shutdownCtx)
		if dbToClose != nil {
			_ = dbToClose.Close()
		}
	}()

	boundPort := server.Port()
	dashboardURL := fmt.Sprintf("http://127.0.0.1:%d", boundPort)
	proxyURL := dashboardURL

	fmt.Printf("\n==> Antigravity Switcher Supervisor Running:\n")
	fmt.Printf("    Web Dashboard: %s/\n", dashboardURL)
	fmt.Printf("    Proxy Address: %s\n", proxyURL)
	fmt.Printf("    Supervising:   %s\n\n", cmdArgs[0])

	if cfg.OpenBrowser {
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = openInBrowser(dashboardURL)
		}()
	}

	// 4. Construct child process
	childCmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	scopedEnv := BuildScopedEnv(os.Environ(), proxyURL)

	// Ensure APPIMAGE is defined for Electron auto-updater (AppImageUpdater) to work seamlessly.
	// When Antigravity 2.0 is extracted from tar.gz or AppImage, electron-updater requires
	// process.env.APPIMAGE to check for updates and download new versions.
	hasAppImage := false
	for _, envVar := range scopedEnv {
		if strings.HasPrefix(envVar, "APPIMAGE=") {
			hasAppImage = true
			break
		}
	}
	if !hasAppImage {
		targetExe := cmdArgs[0]
		if abs, err := filepath.Abs(targetExe); err == nil {
			targetExe = abs
		}
		scopedEnv = append(scopedEnv, "APPIMAGE="+targetExe)
	}

	childCmd.Env = scopedEnv
	childCmd.Stdin = cfg.Stdin
	childCmd.Stdout = cfg.Stdout
	childCmd.Stderr = cfg.Stderr

	// 5. Couple process lifecycle via PR_SET_PDEATHSIG (Linux)
	SetDeathSig(childCmd)

	// 6. Launch child process
	if err := childCmd.Start(); err != nil {
		return 1, fmt.Errorf("failed to launch target command %s: %w", cmdArgs[0], err)
	}

	// 7. Signal forwarding goroutine
	sigChan := make(chan os.Signal, 2)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	stopForwarding := make(chan struct{})
	sigDone := make(chan struct{})
	go func() {
		defer close(sigDone)
		for {
			select {
			case <-stopForwarding:
				return
			case sig, ok := <-sigChan:
				if !ok {
					return
				}
				if childCmd.Process != nil && sig != nil {
					_ = childCmd.Process.Signal(sig)
				}
			case <-ctx.Done():
				if childCmd.Process != nil {
					_ = childCmd.Process.Signal(syscall.SIGTERM)
				}
				return
			}
		}
	}()

	// 8. Wait for child command to exit
	waitErr := childCmd.Wait()
	signal.Stop(sigChan)
	close(stopForwarding)
	<-sigDone

	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	return exitCode, waitErr
}

func defaultDBPath() string {
	if envPath := os.Getenv("ANTIGRAVITY_DB_PATH"); envPath != "" {
		return envPath
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "antigravity-accounts.db"
	}
	return fmt.Sprintf("%s/.config/antigravity-account-switcher/accounts.db", home)
}

func openInBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
