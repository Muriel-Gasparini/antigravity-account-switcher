package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/config"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/launcher"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/metrics"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/oauth"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/proxy"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/quota"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/web"
)

var (
	// Build information injected via -ldflags during compilation.
	Version = "0.1.0-dev"
	Commit  = "unknown"
	Date    = "unknown"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]
	args := os.Args[2:]

	switch command {
	case "launch":
		runLaunch(args)
	case "serve":
		runServe(args)
	case "wrap":
		runWrap(args)
	case "config":
		runConfig(args)
	case "install-desktop":
		runInstallDesktop(args)
	case "uninstall-desktop":
		runUninstallDesktop(args)
	case "add-account":
		runAddAccount(args)
	case "list-accounts":
		runListAccounts(args)
	case "status":
		runStatus(args)
	case "version", "-v", "--version":
		runVersion()
	case "refresh-quotas":
		runRefreshQuotas(args)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Printf("antigravity-account-switcher v%s (%s)\n\n", Version, Commit)
	fmt.Println("Usage:")
	fmt.Println("  antigravity-account-switcher <command> [arguments]")
	fmt.Println("\nCommands:")
	fmt.Println("  launch             Launch Google Antigravity 2.0 directly with coupled proxy supervisor")
	fmt.Println("  serve              Start the local proxy, quota monitor, and web dashboard")
	fmt.Println("  wrap               Supervise any command with scoped proxy environment (PR_SET_PDEATHSIG)")
	fmt.Println("  config             Get, set, or list persistent configuration (models, port, db, paths)")
	fmt.Println("  install-desktop    Install GNOME / XDG desktop application entry with official icon")
	fmt.Println("  uninstall-desktop  Remove GNOME / XDG desktop application entry")
	fmt.Println("  add-account        Onboard a Google account via 1-click browser OAuth2 flow")
	fmt.Println("  list-accounts      Display registered accounts and their quota availability")
	fmt.Println("  refresh-quotas     Force live quota synchronization from Google for all accounts")
	fmt.Println("  status             Display current active account and switcher health")
	fmt.Println("  version            Display binary version, commit, and build date")
}

func runVersion() {
	fmt.Printf("antigravity-account-switcher %s\n", Version)
	fmt.Printf("Commit: %s\n", Commit)
	fmt.Printf("Built at: %s\n", Date)
}

func defaultDBPath() string {
	cfg, err := config.Load()
	if err == nil && cfg != nil && cfg.DBPath != "" {
		return cfg.DBPath
	}
	return config.DefaultDBPath()
}

// addModelFlags registers model fallback flags on the provided FlagSet,
// with defaults populated from the current loaded configuration.
func addModelFlags(fs *flag.FlagSet, cfg *config.Config) (fallbackSecondary *bool, modelPrimary *string, modelSecondary *string) {
	fallbackSecondary = fs.Bool("fallback-secondary", cfg.FallbackSecondaryEnabled, "Enable intra-account secondary model fallback before account rotation")
	modelPrimary = fs.String("model-primary", cfg.ModelPrimary, "Primary model identifier")
	modelSecondary = fs.String("model-secondary", cfg.ModelSecondary, "Secondary model fallback identifier")
	return
}

func runServe(args []string) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load configuration: %v\n", err)
	}
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	defaultPort := 8080
	if cfg.Port > 0 {
		defaultPort = cfg.Port
	}
	defaultPollInterval := quota.DefaultPollInterval
	if cfg.QuotaInterval != "" {
		if d, err := time.ParseDuration(cfg.QuotaInterval); err == nil {
			defaultPollInterval = d
		}
	}
	defaultTargetURL := proxy.DefaultTargetURL
	if cfg.UpstreamURL != "" {
		defaultTargetURL = cfg.UpstreamURL
	}

	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", defaultPort, "Web dashboard and proxy port")
	bind := fs.String("bind", "127.0.0.1", "Bind address")
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	pollInterval := fs.Duration("poll-interval", defaultPollInterval, "Background quota polling interval")
	targetURL := fs.String("target-url", defaultTargetURL, "Google Cloud Code PA upstream target")
	fallbackSecondary, modelPrimary, modelSecondary := addModelFlags(fs, cfg)
	_ = fs.Parse(args)

	cfg.Port = *port
	cfg.UpstreamURL = *targetURL
	cfg.FallbackSecondaryEnabled = *fallbackSecondary
	cfg.ModelPrimary = *modelPrimary
	cfg.ModelSecondary = *modelSecondary

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "Configuration error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Initializing Antigravity Account Switcher v%s...\n", Version)
	fmt.Printf("Database: %s\n", *dbPath)

	db, err := sqlite.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening SQLite database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	accRepo := sqlite.NewAccountRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)
	metricsRepo := sqlite.NewMetricsRepository(db)
	eventRepo := sqlite.NewEventRepository(db)

	broadcaster := proxy.NewBroadcaster(100)
	failoverEngine := proxy.NewFailoverEngine(
		accRepo,
		broadcaster,
		eventRepo,
		proxy.WithQuotaRepository(quotaRepo),
		proxy.WithModelFallback(cfg.ModelPrimary, cfg.ModelSecondary, cfg.FallbackSecondaryEnabled),
	)
	oauthService := oauth.NewOAuthService(accRepo)

	// Automatically import existing Antigravity login if pool is empty
	if importedAcc, err := oauth.AutoImportExistingAccount(ctx, accRepo, oauthService); err == nil && importedAcc != nil {
		fmt.Printf("Auto-imported existing Antigravity account: %s\n", importedAcc.Email)
	}

	tokenRefresher := quota.TokenRefresherFunc(func(ctx context.Context, rt string) (string, time.Time, error) {
		resp, err := oauthService.RefreshToken(ctx, rt)
		if err != nil {
			return "", time.Time{}, err
		}
		return resp.AccessToken, time.Now().UTC().Add(time.Duration(resp.ExpiresIn) * time.Second), nil
	})

	proxyHandler, err := proxy.NewProxyHandler(
		accRepo,
		proxy.WithTargetURL(*targetURL),
		proxy.WithMetricsRepository(metricsRepo),
		proxy.WithEventBroadcaster(broadcaster),
		proxy.WithEventRepository(eventRepo),
		proxy.WithFailoverEngine(failoverEngine),
		proxy.WithTokenRefresher(tokenRefresher),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing reverse proxy: %v\n", err)
		os.Exit(1)
	}

	poller, err := quota.NewPoller(
		accRepo,
		quotaRepo,
		quota.WithPollInterval(*pollInterval),
		quota.WithBaseURL(*targetURL),
		quota.WithEventBroadcaster(broadcaster),
		quota.WithEventRepository(eventRepo),
		quota.WithTokenRefresher(tokenRefresher),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing quota monitor: %v\n", err)
		os.Exit(1)
	}

	if err := poller.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to start quota poller: %v\n", err)
	}
	defer func() { _ = poller.Stop() }()

	metricsService := metrics.NewService(metricsRepo, accRepo)

	server, err := web.NewServer(
		accRepo,
		quotaRepo,
		metricsService,
		broadcaster,
		eventRepo,
		oauthService,
		web.WithPort(*port),
		web.WithBindAddr(*bind),
		web.WithVersion(Version),
		web.WithProxyHandler(proxyHandler),
		web.WithPoller(poller),
		web.WithConfig(cfg),
		web.WithFallbackConfigSetter(failoverEngine),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating web server: %v\n", err)
		os.Exit(1)
	}

	if err := server.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting server: %v\n", err)
		os.Exit(1)
	}

	boundPort := server.Port()
	fmt.Printf("\n==> Antigravity Account Switcher is running:\n")
	fmt.Printf("    Web Dashboard: http://%s:%d/\n", *bind, boundPort)
	fmt.Printf("    Proxy Port:    http://%s:%d/\n", *bind, boundPort)
	fmt.Printf("    Quota Daemon:  Active (interval: %v)\n", *pollInterval)
	if cfg.FallbackSecondaryEnabled {
		fmt.Printf("    Model Fallback: Enabled (%s -> %s)\n", cfg.ModelPrimary, cfg.ModelSecondary)
	} else {
		fmt.Printf("    Model Fallback: Disabled\n")
	}
	fmt.Println("\nPress Ctrl+C to stop.")

	<-ctx.Done()
	fmt.Println("\nShutting down gracefully...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_ = server.Stop(shutdownCtx)
	_ = poller.Stop()
	fmt.Println("Goodbye.")
}

func runWrap(args []string) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load configuration: %v\n", err)
	}
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	defaultPollInterval := quota.DefaultPollInterval
	if cfg.QuotaInterval != "" {
		if d, err := time.ParseDuration(cfg.QuotaInterval); err == nil {
			defaultPollInterval = d
		}
	}
	defaultTargetURL := proxy.DefaultTargetURL
	if cfg.UpstreamURL != "" {
		defaultTargetURL = cfg.UpstreamURL
	}

	fs := flag.NewFlagSet("wrap", flag.ExitOnError)
	port := fs.Int("port", 0, "Switcher port (0 for random ephemeral port)")
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	targetURL := fs.String("target-url", defaultTargetURL, "Google Cloud Code PA upstream target")
	pollInterval := fs.Duration("poll-interval", defaultPollInterval, "Quota polling interval")

	fallbackSecondary, modelPrimary, modelSecondary := addModelFlags(fs, cfg)

	// Separate launcher flags from command line
	var cmdToRun []string

	dashDashIdx := -1
	for i, arg := range args {
		if arg == "--" {
			dashDashIdx = i
			break
		}
	}

	if dashDashIdx >= 0 {
		_ = fs.Parse(args[:dashDashIdx])
		if dashDashIdx+1 < len(args) {
			cmdToRun = args[dashDashIdx+1:]
		}
	} else {
		_ = fs.Parse(args)
		cmdToRun = fs.Args()
	}

	if *port < 0 || *port > 65535 {
		fmt.Fprintf(os.Stderr, "Configuration error: invalid port %d: must be between 0 and 65535\n", *port)
		os.Exit(1)
	}
	if *port > 0 {
		cfg.Port = *port
	}
	cfg.UpstreamURL = *targetURL
	cfg.FallbackSecondaryEnabled = *fallbackSecondary
	cfg.ModelPrimary = *modelPrimary
	cfg.ModelSecondary = *modelSecondary

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "Configuration error: %v\n", err)
		os.Exit(1)
	}

	if len(cmdToRun) == 0 {
		fmt.Println("Usage: antigravity-account-switcher wrap [flags] -- <command> [args...]")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	exitCode, err := launcher.Wrap(
		ctx,
		cmdToRun,
		launcher.WithPort(*port),
		launcher.WithDBPath(*dbPath),
		launcher.WithTargetURL(*targetURL),
		launcher.WithPollInterval(*pollInterval),
		launcher.WithModelFallback(cfg.ModelPrimary, cfg.ModelSecondary, cfg.FallbackSecondaryEnabled),
	)

	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		if exitCode == 0 {
			exitCode = 1
		}
	}

	os.Exit(exitCode)
}

func runAddAccount(args []string) {
	fs := flag.NewFlagSet("add-account", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	noBrowser := fs.Bool("no-browser", false, "Do not attempt to open browser automatically (useful in SSH/headless)")
	_ = fs.Parse(args)

	db, err := sqlite.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening SQLite database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	accRepo := sqlite.NewAccountRepository(db)
	oauthService := oauth.NewOAuthService(accRepo)

	fmt.Println("Initiating RFC 8252 OAuth2 loopback authentication flow...")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var opener oauth.BrowserOpener
	if *noBrowser {
		opener = func(url string) error { return nil }
	}

	acc, err := oauthService.StartLoopbackFlow(ctx, opener, func(authURL string) {
		if *noBrowser {
			fmt.Printf("\nOpen this URL in your browser to authorize:\n\n%s\n\nWaiting for authorization...\n", authURL)
		} else {
			fmt.Printf("\nIf your browser does not open automatically, open this URL:\n\n%s\n\nWaiting for authorization...\n", authURL)
		}
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "\nOAuth authentication failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\nSuccess! Google Account %s has been registered and activated.\n", acc.Email)
	fmt.Printf("Account ID: %s\n", acc.ID)
}

func runListAccounts(args []string) {
	fs := flag.NewFlagSet("list-accounts", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	_ = fs.Parse(args)

	db, err := sqlite.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx := context.Background()
	accRepo := sqlite.NewAccountRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)

	accounts, err := accRepo.List(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to list accounts: %v\n", err)
		os.Exit(1)
	}

	if len(accounts) == 0 {
		fmt.Println("No accounts registered yet. Run 'add-account' to add one.")
		return
	}

	allBuckets, _ := quotaRepo.ListAll(ctx)

	fmt.Printf("%-36s  %-30s  %-10s  %-8s  %-12s  %-12s\n", "ID", "EMAIL", "STATUS", "ACTIVE", "DAILY QUOTA", "WEEKLY QUOTA")
	fmt.Println("-------------------------------------------------------------------------------------------------------------")

	for _, acc := range accounts {
		activeMark := ""
		if acc.IsActive {
			activeMark = "*"
		}

		dailyStr := "N/A"
		weeklyStr := "N/A"

		if buckets, ok := allBuckets[acc.ID]; ok {
			for _, b := range buckets {
				if b.Window == domain.QuotaWindowDaily {
					dailyStr = fmt.Sprintf("%.0f%%", b.RemainingFraction*100)
				} else if b.Window == domain.QuotaWindowWeekly {
					weeklyStr = fmt.Sprintf("%.0f%%", b.RemainingFraction*100)
				}
			}
		}

		fmt.Printf("%-36s  %-30s  %-10s  %-8s  %-12s  %-12s\n",
			acc.ID,
			acc.Email,
			string(acc.Status),
			activeMark,
			dailyStr,
			weeklyStr,
		)
	}
}

func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	_ = fs.Parse(args)

	db, err := sqlite.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx := context.Background()
	accRepo := sqlite.NewAccountRepository(db)
	metricsRepo := sqlite.NewMetricsRepository(db)
	metricsService := metrics.NewService(metricsRepo, accRepo)

	accounts, _ := accRepo.List(ctx)
	active, err := accRepo.GetActive(ctx)

	fmt.Printf("Antigravity Account Switcher v%s\n", Version)
	fmt.Println("Switcher Status: OK")
	fmt.Printf("Total Accounts:  %d\n", len(accounts))

	if err != nil {
		if errors.Is(err, domain.ErrNoActiveAccount) {
			fmt.Println("Active Account:  None selected")
		} else {
			fmt.Printf("Active Account:  Error (%v)\n", err)
		}
	} else {
		fmt.Printf("Active Account:  %s (%s)\n", active.Email, active.ID)
		fmt.Printf("Account Status:  %s\n", active.Status)
	}

	// Print token summary if available
	summary, err := metricsService.GetGlobalSummary(ctx, domain.PeriodLifetime)
	if err == nil && summary != nil {
		fmt.Printf("Total Tokens:    %d (Prompt: %d, Candidates: %d)\n",
			summary.TotalTokens, summary.TotalPromptTokens, summary.TotalCandidatesTokens)
		fmt.Printf("Total Requests:  %d\n", summary.TotalRequests)
	}
}

func runLaunch(args []string) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load configuration: %v\n", err)
	}
	if cfg == nil {
		cfg = config.DefaultConfig()
	}

	defaultPollInterval := quota.DefaultPollInterval
	if cfg.QuotaInterval != "" {
		if d, err := time.ParseDuration(cfg.QuotaInterval); err == nil {
			defaultPollInterval = d
		}
	}

	fs := flag.NewFlagSet("launch", flag.ExitOnError)
	binFlag := fs.String("bin", "", "Path to Antigravity 2.0 binary (overrides auto-detection)")
	port := fs.Int("port", cfg.Port, "Proxy and web dashboard port (defaults to configured port)")
	openUI := fs.Bool("open", false, "Open web dashboard in default browser on launch")
	ui := fs.Bool("ui", false, "Alias for --open")
	dbPath := fs.String("db", cfg.DBPath, "Path to SQLite database file")
	targetURL := fs.String("target-url", cfg.UpstreamURL, "Google Cloud Code PA upstream target")
	pollInterval := fs.Duration("poll-interval", defaultPollInterval, "Quota polling interval")

	fallbackSecondary, modelPrimary, modelSecondary := addModelFlags(fs, cfg)

	dashDashIdx := -1
	for i, arg := range args {
		if arg == "--" {
			dashDashIdx = i
			break
		}
	}

	var passthroughArgs []string
	if dashDashIdx >= 0 {
		_ = fs.Parse(args[:dashDashIdx])
		if dashDashIdx+1 < len(args) {
			passthroughArgs = args[dashDashIdx+1:]
		}
	} else {
		_ = fs.Parse(args)
		passthroughArgs = fs.Args()
	}

	cfg.Port = *port
	cfg.UpstreamURL = *targetURL
	cfg.FallbackSecondaryEnabled = *fallbackSecondary
	cfg.ModelPrimary = *modelPrimary
	cfg.ModelSecondary = *modelSecondary

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "Configuration error: %v\n", err)
		os.Exit(1)
	}

	antigravityBin, err := config.ResolveAntigravityBin(*binFlag)
	if err != nil {
		if len(passthroughArgs) > 0 {
			if lp, lookErr := exec.LookPath(passthroughArgs[0]); lookErr == nil {
				antigravityBin = lp
				passthroughArgs = passthroughArgs[1:]
				err = nil
			}
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving Antigravity binary: %v\n", err)
		fmt.Fprintf(os.Stderr, "Use '--bin /path/to/antigravity' or run 'antigravity-account-switcher config set antigravity_bin <path>'\n")
		os.Exit(1)
	}

	cmdToRun := append([]string{antigravityBin}, passthroughArgs...)
	fmt.Printf("==> Launching Antigravity 2.0 with coupled switcher supervisor:\n")
	fmt.Printf("    Executable: %s\n", antigravityBin)
	if len(passthroughArgs) > 0 {
		fmt.Printf("    Arguments:  %v\n", passthroughArgs)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	shouldOpenBrowser := *openUI || *ui || cfg.OpenBrowser

	exitCode, err := launcher.Wrap(
		ctx,
		cmdToRun,
		launcher.WithPort(*port),
		launcher.WithOpenBrowser(shouldOpenBrowser),
		launcher.WithDBPath(*dbPath),
		launcher.WithTargetURL(*targetURL),
		launcher.WithPollInterval(*pollInterval),
		launcher.WithModelFallback(cfg.ModelPrimary, cfg.ModelSecondary, cfg.FallbackSecondaryEnabled),
	)

	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		if exitCode == 0 {
			exitCode = 1
		}
	}

	os.Exit(exitCode)
}

func runConfig(args []string) {
	if code := executeConfig(args, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func executeConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "list" {
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintf(stderr, "Warning: failed to load configuration: %v\n", err)
		}
		if cfg == nil {
			cfg = config.DefaultConfig()
		}
		fmt.Fprintf(stdout, "Configuration file: %s\n\n", config.ConfigFilePath())
		fmt.Fprintf(stdout, "  antigravity_bin:            %s\n", cfg.AntigravityBin)
		fmt.Fprintf(stdout, "  port:                       %d\n", cfg.Port)
		fmt.Fprintf(stdout, "  db_path:                    %s\n", cfg.DBPath)
		fmt.Fprintf(stdout, "  upstream_url:               %s\n", cfg.UpstreamURL)
		fmt.Fprintf(stdout, "  quota_interval:             %s\n", cfg.QuotaInterval)
		fmt.Fprintf(stdout, "  open_browser:               %t\n", cfg.OpenBrowser)
		fmt.Fprintf(stdout, "  model_primary:              %s\n", cfg.ModelPrimary)
		fmt.Fprintf(stdout, "  model_secondary:            %s\n", cfg.ModelSecondary)
		fmt.Fprintf(stdout, "  fallback_secondary_enabled: %t\n", cfg.FallbackSecondaryEnabled)
		return 0
	}

	subcmd := args[0]
	switch subcmd {
	case "get":
		if len(args) < 2 {
			fmt.Fprintln(stdout, "Usage: antigravity-account-switcher config get <key>")
			return 1
		}
		key := args[1]
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintf(stderr, "Warning: failed to load configuration: %v\n", err)
		}
		if cfg == nil {
			cfg = config.DefaultConfig()
		}
		switch key {
		case "antigravity_bin":
			fmt.Fprintln(stdout, cfg.AntigravityBin)
		case "port":
			fmt.Fprintln(stdout, cfg.Port)
		case "db_path":
			fmt.Fprintln(stdout, cfg.DBPath)
		case "upstream_url":
			fmt.Fprintln(stdout, cfg.UpstreamURL)
		case "quota_interval":
			fmt.Fprintln(stdout, cfg.QuotaInterval)
		case "open_browser":
			fmt.Fprintln(stdout, cfg.OpenBrowser)
		case "model_primary":
			fmt.Fprintln(stdout, cfg.ModelPrimary)
		case "model_secondary":
			fmt.Fprintln(stdout, cfg.ModelSecondary)
		case "fallback_secondary_enabled":
			fmt.Fprintln(stdout, cfg.FallbackSecondaryEnabled)
		default:
			fmt.Fprintf(stderr, "Unknown configuration key: %s\n", key)
			return 1
		}
		return 0

	case "set":
		if len(args) < 3 {
			fmt.Fprintln(stdout, "Usage: antigravity-account-switcher config set <key> <value>")
			return 1
		}
		key := args[1]
		val := args[2]
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintf(stderr, "Error loading configuration: %v\n", err)
			return 1
		}
		if cfg == nil {
			cfg = config.DefaultConfig()
		}
		switch key {
		case "antigravity_bin":
			cfg.AntigravityBin = val
		case "port":
			p, err := strconv.Atoi(strings.TrimSpace(val))
			if err != nil || p <= 0 {
				fmt.Fprintf(stderr, "Invalid port value: %s\n", val)
				return 1
			}
			cfg.Port = p
		case "db_path":
			cfg.DBPath = val
		case "upstream_url":
			cfg.UpstreamURL = val
		case "quota_interval":
			cfg.QuotaInterval = val
		case "open_browser":
			b, err := config.ParseBool(val)
			if err != nil {
				fmt.Fprintf(stderr, "Invalid boolean value for open_browser: %s\n", val)
				return 1
			}
			cfg.OpenBrowser = b
		case "model_primary":
			trimmed := strings.TrimSpace(val)
			if trimmed == "" {
				fmt.Fprintf(stderr, "Invalid model_primary: value cannot be empty\n")
				return 1
			}
			cfg.ModelPrimary = trimmed
		case "model_secondary":
			trimmed := strings.TrimSpace(val)
			if trimmed == "" {
				fmt.Fprintf(stderr, "Invalid model_secondary: value cannot be empty\n")
				return 1
			}
			cfg.ModelSecondary = trimmed
		case "fallback_secondary_enabled":
			b, err := config.ParseBool(val)
			if err != nil {
				fmt.Fprintf(stderr, "Invalid boolean value for fallback_secondary_enabled: %s (expected true, false, 1, or 0)\n", val)
				return 1
			}
			cfg.FallbackSecondaryEnabled = b
		default:
			fmt.Fprintf(stderr, "Unknown configuration key: %s\n", key)
			return 1
		}

		if err := cfg.Validate(); err != nil {
			fmt.Fprintf(stderr, "Configuration validation failed: %v\n", err)
			return 1
		}

		if err := config.Save(cfg); err != nil {
			fmt.Fprintf(stderr, "Failed to save configuration: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "Updated '%s' to '%s' in %s\n", key, val, config.ConfigFilePath())
		return 0

	default:
		fmt.Fprintf(stderr, "Unknown config subcommand: %s. Use 'list', 'get', or 'set'.\n", subcmd)
		return 1
	}
}

func runInstallDesktop(args []string) {
	fs := flag.NewFlagSet("install-desktop", flag.ExitOnError)
	binFlag := fs.String("bin", "", "Path to Antigravity 2.0 binary (auto-detected if omitted)")
	iconFlag := fs.String("icon", "", "Path to Antigravity icon (auto-detected if omitted)")
	nameFlag := fs.String("name", "Antigravity 2.0", "Desktop application name")
	_ = fs.Parse(args)

	fmt.Println("Installing GNOME/XDG desktop application entry...")

	opts := launcher.DesktopOptions{
		AntigravityBin: *binFlag,
		IconPath:       *iconFlag,
		Name:           *nameFlag,
	}

	res, err := launcher.InstallDesktop(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Installation failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\nSuccess! Antigravity desktop application installed:\n")
	fmt.Printf("  Desktop File:   %s\n", res.DesktopFilePath)
	fmt.Printf("  Icon File:      %s\n", res.IconFilePath)
	fmt.Printf("  Target Binary:  %s\n", res.AntigravityBin)
	fmt.Printf("  Supervisor:     %s\n", res.SwitcherBin)
	fmt.Println("\nAntigravity is now available in your GNOME / XDG application menu and dock!")
}

func runUninstallDesktop(args []string) {
	fmt.Println("Uninstalling GNOME/XDG desktop application entry...")
	if err := launcher.UninstallDesktop(); err != nil {
		fmt.Fprintf(os.Stderr, "Uninstallation failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Success! Antigravity desktop application entry removed.")
}

func runRefreshQuotas(args []string) {
	fs := flag.NewFlagSet("refresh-quotas", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "Path to SQLite database file")
	targetURL := fs.String("target-url", proxy.DefaultTargetURL, "Google Cloud Code PA upstream target")
	_ = fs.Parse(args)

	db, err := sqlite.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	accRepo := sqlite.NewAccountRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)

	poller, err := quota.NewPoller(accRepo, quotaRepo, quota.WithBaseURL(*targetURL))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize poller: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	fmt.Println("Polling live quotas directly from Google across all registered accounts...")
	if err := poller.PollOnce(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "PollOnce failed: %v\n", err)
		os.Exit(1)
	}

	accounts, err := accRepo.List(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to list accounts: %v\n", err)
		os.Exit(1)
	}

	allBuckets, err := quotaRepo.ListAll(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to list buckets: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\nUpdated Live Quotas:")
	for _, acc := range accounts {
		activeMarker := " "
		if acc.IsActive {
			activeMarker = "*"
		}
		fmt.Printf("\n[%s] %s (%s):\n", activeMarker, acc.Email, acc.Status)
		for _, b := range allBuckets[acc.ID] {
			pct := int(b.RemainingFraction * 100)
			resetStr := "-"
			if !b.ResetTime.IsZero() {
				resetStr = b.ResetTime.Format("2006-01-02 15:04 UTC")
			}
			fmt.Printf("  • %-32s [%-6s]: %3d%% (reset: %s)\n", b.DisplayName, b.Window, pct, resetStr)
		}
	}
}
