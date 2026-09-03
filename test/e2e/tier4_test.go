package e2e

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/launcher"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
)

// TestTier4_LauncherCoupledLifecycle_AndZeroEnvironmentPollution validates that:
// 1. Child process inherits strictly scoped proxy environment.
// 2. Global process environment (os.Getenv) remains completely unmutated.
// 3. Child exit cleanly and immediately terminates the switcher server.
func TestTier4_LauncherCoupledLifecycle_AndZeroEnvironmentPollution(t *testing.T) {
	// Baseline snapshot of global environment before wrap
	baselineHTTP := os.Getenv("HTTP_PROXY")
	baselineHTTPS := os.Getenv("HTTPS_PROXY")
	baselineCloudCode := os.Getenv("CLOUD_CODE_URL")

	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	defer db.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Child prints its environment variables
	cmdArgs := []string{"sh", "-c", "echo PROXY=$HTTP_PROXY; echo CLOUD=$CLOUD_CODE_URL"}

	exitCode, err := launcher.Wrap(
		ctx,
		cmdArgs,
		launcher.WithDB(db),
		launcher.WithPort(0),
		launcher.WithIO(nil, &stdout, &stderr),
	)

	if err != nil {
		t.Fatalf("Wrap failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}

	outStr := stdout.String()

	// 1. Child received scoped proxy environment
	if !strings.Contains(outStr, "PROXY=http://127.0.0.1:") {
		t.Errorf("child did not receive scoped HTTP_PROXY, output was:\n%s", outStr)
	}
	if !strings.Contains(outStr, "CLOUD=http://127.0.0.1:") {
		t.Errorf("child did not receive scoped CLOUD_CODE_URL, output was:\n%s", outStr)
	}

	// 2. Invariant: Global environment was not mutated
	if os.Getenv("HTTP_PROXY") != baselineHTTP {
		t.Errorf("global HTTP_PROXY was mutated! baseline=%q, current=%q", baselineHTTP, os.Getenv("HTTP_PROXY"))
	}
	if os.Getenv("HTTPS_PROXY") != baselineHTTPS {
		t.Errorf("global HTTPS_PROXY was mutated! baseline=%q, current=%q", baselineHTTPS, os.Getenv("HTTPS_PROXY"))
	}
	if os.Getenv("CLOUD_CODE_URL") != baselineCloudCode {
		t.Errorf("global CLOUD_CODE_URL was mutated! baseline=%q, current=%q", baselineCloudCode, os.Getenv("CLOUD_CODE_URL"))
	}
}

// TestTier4_LauncherScriptExecution validates that scripts/launch-antigravity.sh works.
func TestTier4_LauncherScriptExecution(t *testing.T) {
	scriptPath, err := filepath.Abs("../../scripts/launch-antigravity.sh")
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, scriptPath, "echo", "launcher script test successful")
	cmd.Dir = filepath.Dir(scriptPath)
	cmd.Env = append(os.Environ(), "ANTIGRAVITY_PORT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script execution failed: %v (output: %s)", err, string(out))
	}

	if !strings.Contains(string(out), "launcher script test successful") {
		t.Errorf("expected script output to contain target command result, got: %s", string(out))
	}
}

// TestTier4_EmbeddedUI_DashboardAssets validates that embedded assets are delivered over HTTP.
func TestTier4_EmbeddedUI_DashboardAssets(t *testing.T) {
	env := setupE2EEnvironment(t, 0)

	// 1. GET /
	respHTML, err := http.Get(env.ServerURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer respHTML.Body.Close()
	if respHTML.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for /, got %d", respHTML.StatusCode)
	}
	bodyHTML, _ := io.ReadAll(respHTML.Body)
	if !strings.Contains(string(bodyHTML), "Antigravity Account Switcher") {
		t.Errorf("HTML missing expected title")
	}

	// 2. GET /dist/app.js
	respJS, err := http.Get(env.ServerURL + "/dist/app.js")
	if err != nil {
		t.Fatalf("GET /dist/app.js: %v", err)
	}
	defer respJS.Body.Close()
	if respJS.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for /dist/app.js, got %d", respJS.StatusCode)
	}

	// 3. GET /dist/style.css
	respCSS, err := http.Get(env.ServerURL + "/dist/style.css")
	if err != nil {
		t.Fatalf("GET /dist/style.css: %v", err)
	}
	defer respCSS.Body.Close()
	if respCSS.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for /dist/style.css, got %d", respCSS.StatusCode)
	}
}
