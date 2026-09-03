package launcher

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
)

func TestBuildScopedEnv(t *testing.T) {
	baseEnv := []string{
		"PATH=/usr/bin:/bin",
		"USER=testuser",
		"HTTP_PROXY=http://old-corporate-proxy:8080",
		"HTTPS_PROXY=http://old-corporate-proxy:8080",
		"http_proxy=http://old-corporate-proxy:8080",
		"CLOUD_CODE_URL=http://old-url",
	}

	proxyURL := "http://127.0.0.1:45678"
	scoped := BuildScopedEnv(baseEnv, proxyURL)

	// Check that old values are removed and new values injected
	foundHTTP := 0
	foundHTTPS := 0
	foundCloudCode := 0

	for _, v := range scoped {
		if strings.HasPrefix(v, "HTTP_PROXY=") || strings.HasPrefix(v, "http_proxy=") {
			foundHTTP++
			if !strings.HasSuffix(v, proxyURL) {
				t.Errorf("expected HTTP_PROXY to be %s, got %s", proxyURL, v)
			}
		}
		if strings.HasPrefix(v, "HTTPS_PROXY=") || strings.HasPrefix(v, "https_proxy=") {
			foundHTTPS++
			if !strings.HasSuffix(v, proxyURL) {
				t.Errorf("expected HTTPS_PROXY to be %s, got %s", proxyURL, v)
			}
		}
		if strings.HasPrefix(v, "CLOUD_CODE_URL=") {
			foundCloudCode++
			if !strings.HasSuffix(v, proxyURL) {
				t.Errorf("expected CLOUD_CODE_URL to be %s, got %s", proxyURL, v)
			}
		}
	}

	if foundHTTP != 2 {
		t.Errorf("expected 2 HTTP proxy entries (upper/lower), got %d", foundHTTP)
	}
	if foundHTTPS != 2 {
		t.Errorf("expected 2 HTTPS proxy entries (upper/lower), got %d", foundHTTPS)
	}
	if foundCloudCode != 1 {
		t.Errorf("expected 1 CLOUD_CODE_URL entry, got %d", foundCloudCode)
	}

	// Invariant: os.Environ was NOT modified
	if os.Getenv("CLOUD_CODE_URL") == proxyURL {
		t.Errorf("CRITICAL BUG: global os environment was mutated!")
	}
}

func TestSetDeathSig(t *testing.T) {
	cmd := exec.Command("true")
	SetDeathSig(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("expected SysProcAttr to be non-nil after SetDeathSig")
	}
}

func TestWrap_EchoCommand_ZeroExit(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	defer db.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmdArgs := []string{"sh", "-c", "echo 'hello from coupled child'; echo $HTTP_PROXY"}

	exitCode, err := Wrap(ctx, cmdArgs,
		WithDB(db),
		WithPort(0),
		WithIO(nil, &stdout, &stderr),
	)

	if err != nil {
		t.Fatalf("Wrap failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}

	outStr := stdout.String()
	if !strings.Contains(outStr, "hello from coupled child") {
		t.Errorf("expected output to contain greeting, got: %s", outStr)
	}
	if !strings.Contains(outStr, "http://127.0.0.1:") {
		t.Errorf("expected output to contain injected HTTP_PROXY url, got: %s", outStr)
	}
}

func TestWrap_ExitCodePropagation(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmdArgs := []string{"sh", "-c", "exit 42"}

	exitCode, err := Wrap(ctx, cmdArgs,
		WithDB(db),
		WithPort(0),
		WithIO(nil, nil, nil),
	)

	if exitCode != 42 {
		t.Fatalf("expected exit code 42, got %d (err: %v)", exitCode, err)
	}
}

func TestWrap_ContextCancellation(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	cmdArgs := []string{"sh", "-c", "sleep 10"}

	exitCode, err := Wrap(ctx, cmdArgs,
		WithDB(db),
		WithPort(0),
		WithIO(nil, nil, nil),
	)

	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("expected rapid cancellation, took %v", elapsed)
	}

	if exitCode == 0 && err == nil {
		t.Fatalf("expected non-zero exit or error on cancellation")
	}
}

func TestWrap_MissingCommandReturnsDescriptiveError(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmdArgs := []string{"non_existent_binary_xyz_12345"}
	exitCode, err := Wrap(ctx, cmdArgs,
		WithDB(db),
		WithPort(0),
		WithIO(nil, nil, nil),
	)

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
	if err == nil {
		t.Fatal("expected non-nil error for missing executable")
	}
	if !strings.Contains(err.Error(), "failed to launch target command") {
		t.Errorf("expected descriptive error, got %v", err)
	}
}

func TestWrap_EmptyArgsError(t *testing.T) {
	ctx := context.Background()
	exitCode, err := Wrap(ctx, []string{})
	if exitCode != 1 || err == nil {
		t.Errorf("expected exitCode=1 and non-nil error, got code=%d, err=%v", exitCode, err)
	}
}

func TestWrap_InjectsAppImageEnv(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var stdout bytes.Buffer
	exitCode, err := Wrap(ctx, []string{"sh", "-c", "echo -n $APPIMAGE"},
		WithDB(db),
		WithPort(0),
		WithIO(nil, &stdout, nil),
	)
	if err != nil {
		t.Fatalf("Wrap failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}

	appImageVal := strings.TrimSpace(stdout.String())
	if appImageVal == "" {
		t.Errorf("expected APPIMAGE to be non-empty, got empty")
	}
	if !strings.Contains(appImageVal, "sh") {
		t.Errorf("expected APPIMAGE to contain 'sh', got %q", appImageVal)
	}
}

