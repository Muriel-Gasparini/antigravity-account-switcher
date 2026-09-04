package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/config"
)

// TestChallenger_CLI_ConfigErrors_ExitCode1 tests that all malformed/invalid config invocations return exit code 1.
func TestChallenger_CLI_ConfigErrors_ExitCode1(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)

	tests := []struct {
		name         string
		args         []string
		expectedExit int
		errSubstr    string
	}{
		// Missing arguments
		{"get without key", []string{"get"}, 1, "Usage: antigravity-account-switcher config get <key>"},
		{"set without args", []string{"set"}, 1, "Usage: antigravity-account-switcher config set <key> <value>"},
		{"set with only key", []string{"set", "model_primary"}, 1, "Usage: antigravity-account-switcher config set <key> <value>"},
		// Unknown subcommands & keys
		{"unknown subcmd", []string{"invalid_subcmd"}, 1, "Unknown config subcommand: invalid_subcmd"},
		{"get unknown key", []string{"get", "non_existent_key"}, 1, "Unknown configuration key: non_existent_key"},
		{"set unknown key", []string{"set", "non_existent_key", "val"}, 1, "Unknown configuration key: non_existent_key"},
		// Invalid ports
		{"set port zero", []string{"set", "port", "0"}, 1, "Invalid port value: 0"},
		{"set port negative", []string{"set", "port", "-1"}, 1, "Invalid port value: -1"},
		{"set port string", []string{"set", "port", "eighty"}, 1, "Invalid port value: eighty"},
		{"set port with suffix", []string{"set", "port", "8080abc"}, 1, "Invalid port value: 8080abc"},
		{"set port too high", []string{"set", "port", "65536"}, 1, "Configuration validation failed: invalid port 65536"},
		{"set port way too high", []string{"set", "port", "999999"}, 1, "Configuration validation failed: invalid port 999999"},
		// Invalid booleans
		{"set open_browser invalid", []string{"set", "open_browser", "notabool"}, 1, "Invalid boolean value for open_browser: notabool"},
		{"set fallback_secondary_enabled invalid", []string{"set", "fallback_secondary_enabled", "maybe"}, 1, "Invalid boolean value for fallback_secondary_enabled: maybe"},
		{"set fallback_secondary_enabled number 2", []string{"set", "fallback_secondary_enabled", "2"}, 1, "Invalid boolean value for fallback_secondary_enabled: 2"},
		// Empty model names
		{"set model_primary empty", []string{"set", "model_primary", ""}, 1, "Invalid model_primary: value cannot be empty"},
		{"set model_primary whitespace", []string{"set", "model_primary", "   \t\n"}, 1, "Invalid model_primary: value cannot be empty"},
		{"set model_secondary empty", []string{"set", "model_secondary", ""}, 1, "Invalid model_secondary: value cannot be empty"},
		{"set model_secondary whitespace", []string{"set", "model_secondary", "   "}, 1, "Invalid model_secondary: value cannot be empty"},
		// Invalid upstream URL & quota interval
		{"set upstream_url ftp scheme", []string{"set", "upstream_url", "ftp://cloudcode.google.com"}, 1, "Configuration validation failed: invalid upstream_url"},
		{"set quota_interval invalid", []string{"set", "quota_interval", "invalid_dur"}, 1, "Configuration validation failed: invalid quota_interval"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caseDir := filepath.Join(tmpDir, strings.ReplaceAll(tc.name, " ", "_"))
			stdout, stderr, exitCode := runCLIInSubprocess(t, caseDir, tc.args...)
			combined := stdout + "\n" + stderr

			if exitCode != tc.expectedExit {
				t.Errorf("expected exit code %d, got %d. Output:\n%s", tc.expectedExit, exitCode, combined)
			}
			if !strings.Contains(combined, tc.errSubstr) {
				t.Errorf("expected output to contain %q, got:\n%s", tc.errSubstr, combined)
			}
		})
	}
}

// TestChallenger_CLI_ModelInvariantsUnderConfigSet tests model collision prevention during config set.
func TestChallenger_CLI_ModelInvariantsUnderConfigSet(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)

	var stdout, stderr bytes.Buffer

	// Step 1: Enable fallback secondary
	code := executeConfig([]string{"set", "fallback_secondary_enabled", "true"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected 0, got %d: %s", code, stderr.String())
	}

	// Step 2: Attempt to set model_secondary identical to default primary (gemini-2.5-pro)
	stdout.Reset()
	stderr.Reset()
	code = executeConfig([]string{"set", "model_secondary", "gemini-2.5-pro"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1 when setting identical secondary model, got %d", code)
	}
	if !strings.Contains(stderr.String(), "cannot be identical") {
		t.Errorf("expected error to mention 'cannot be identical', got: %s", stderr.String())
	}

	// Step 3: Attempt to set case-insensitively identical model_secondary
	stdout.Reset()
	stderr.Reset()
	code = executeConfig([]string{"set", "model_secondary", "GEMINI-2.5-PRO"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1 when setting case-insensitive identical secondary model, got %d", code)
	}

	// Step 4: Attempt to set model_primary identical to secondary
	stdout.Reset()
	stderr.Reset()
	code = executeConfig([]string{"set", "model_primary", "claude-3-5-sonnet"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1 when setting identical primary model, got %d", code)
	}

	// Step 5: Disable fallback secondary, then setting identical models should succeed
	stdout.Reset()
	stderr.Reset()
	code = executeConfig([]string{"set", "fallback_secondary_enabled", "false"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected 0, got %d: %s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = executeConfig([]string{"set", "model_secondary", "gemini-2.5-pro"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected 0 when fallback disabled, got %d: %s", code, stderr.String())
	}
	_ = tmpDir
}

// TestChallenger_CLI_SubcommandFlags_ValidationErrors tests flag validation in serve/launch/wrap.
func TestChallenger_CLI_SubcommandFlags_ValidationErrors(t *testing.T) {
	subcommands := []string{"serve", "launch", "wrap"}

	for _, sub := range subcommands {
		t.Run(sub+"_identical_models_validation_error", func(t *testing.T) {
			fs := flag.NewFlagSet(sub, flag.ContinueOnError)
			cfg := config.DefaultConfig()
			fallbackEnabled, modelPrimary, modelSecondary := addModelFlags(fs, cfg)

			args := []string{
				"--fallback-secondary=true",
				"--model-primary=gemini-2.5-pro",
				"--model-secondary=gemini-2.5-pro",
			}

			if err := fs.Parse(args); err != nil {
				t.Fatalf("unexpected flag parse error: %v", err)
			}

			cfg.FallbackSecondaryEnabled = *fallbackEnabled
			cfg.ModelPrimary = *modelPrimary
			cfg.ModelSecondary = *modelSecondary

			err := cfg.Validate()
			if err == nil {
				t.Errorf("expected validation error for identical models in %s flags, got nil", sub)
			}
			if !strings.Contains(err.Error(), "cannot be identical") {
				t.Errorf("expected error to mention 'cannot be identical', got: %v", err)
			}
		})
	}
}

// TestChallenger_CLI_ConcurrentExecuteConfig_Race runs 50 concurrent goroutines executing config operations.
func TestChallenger_CLI_ConcurrentExecuteConfig_Race(t *testing.T) {
	baseDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", baseDir)
	var wg sync.WaitGroup
	workers := 50

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			var stdout, stderr bytes.Buffer

			// Test list
			_ = executeConfig([]string{"list"}, &stdout, &stderr)

			// Test get
			stdout.Reset()
			stderr.Reset()
			_ = executeConfig([]string{"get", "model_primary"}, &stdout, &stderr)

			// Test invalid set
			stdout.Reset()
			stderr.Reset()
			code := executeConfig([]string{"set", "port", "-1"}, &stdout, &stderr)
			if code != 1 {
				t.Errorf("worker %d: expected code 1 for invalid port, got %d", id, code)
			}
		}(i)
	}

	wg.Wait()
}

// TestChallenger_CLI_ConfigSet_PreservesMalformedFileOnLoadError verifies that if config.json
// is malformed, 'config set' aborts with exit code 1 and does NOT overwrite the existing file with defaults.
func TestChallenger_CLI_ConfigSet_PreservesMalformedFileOnLoadError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", dir)
	cfgFile := filepath.Join(dir, "config.json")
	corruptContent := []byte("{malformed json:")
	if err := os.WriteFile(cfgFile, corruptContent, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := executeConfig([]string{"set", "port", "9090"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected code 1 on malformed config load, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Error loading configuration") {
		t.Fatalf("expected stderr to report 'Error loading configuration', got: %s", stderr.String())
	}

	data, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(data) != string(corruptContent) {
		t.Fatalf("expected file to preserve original content %q, got %q", string(corruptContent), string(data))
	}
}

// TestChallenger_CLI_ConfigSet_PortValidation tests port string parsing, trimming, suffixes, and bounds.
func TestChallenger_CLI_ConfigSet_PortValidation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", dir)

	var stdout, stderr bytes.Buffer

	// 1. Whitespace around valid port should succeed
	code := executeConfig([]string{"set", "port", "  9090  "}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected code 0 for trimmed valid port, got %d: %s", code, stderr.String())
	}

	// Verify get returns 9090
	stdout.Reset()
	stderr.Reset()
	code = executeConfig([]string{"get", "port"}, &stdout, &stderr)
	if code != 0 || strings.TrimSpace(stdout.String()) != "9090" {
		t.Fatalf("expected port 9090, got: %s", stdout.String())
	}

	// 2. Out of range port (>65535) should fail
	stdout.Reset()
	stderr.Reset()
	code = executeConfig([]string{"set", "port", "70000"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected code 1 for port 70000, got %d", code)
	}

	// 3. Port with invalid suffix should fail
	stdout.Reset()
	stderr.Reset()
	code = executeConfig([]string{"set", "port", "8080abc"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected code 1 for port 8080abc, got %d", code)
	}

	// 4. Negative port should fail
	stdout.Reset()
	stderr.Reset()
	code = executeConfig([]string{"set", "port", "-1"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected code 1 for port -1, got %d", code)
	}
}

// TestChallenger_CLI_SubcommandFlags_PortAndTargetURL_ValidationErrors tests that CLI flags
// for --port and --target-url are caught by cfg.Validate() across serve, launch, and wrap.
func TestChallenger_CLI_SubcommandFlags_PortAndTargetURL_ValidationErrors(t *testing.T) {
	subcommands := []string{"serve", "launch", "wrap"}
	for _, sub := range subcommands {
		t.Run(sub+"_invalid_port", func(t *testing.T) {
			cfg := config.DefaultConfig()
			fs := flag.NewFlagSet(sub, flag.ContinueOnError)
			port := fs.Int("port", 8080, "")
			targetURL := fs.String("target-url", cfg.UpstreamURL, "")
			fallbackEnabled, modelPrimary, modelSecondary := addModelFlags(fs, cfg)

			args := []string{"--port=70000"}
			if err := fs.Parse(args); err != nil {
				t.Fatalf("parse err: %v", err)
			}
			cfg.Port = *port
			cfg.UpstreamURL = *targetURL
			cfg.FallbackSecondaryEnabled = *fallbackEnabled
			cfg.ModelPrimary = *modelPrimary
			cfg.ModelSecondary = *modelSecondary

			if err := cfg.Validate(); err == nil {
				t.Errorf("[%s] expected validation error for port 70000, got nil", sub)
			}
		})

		t.Run(sub+"_invalid_target_url", func(t *testing.T) {
			cfg := config.DefaultConfig()
			fs := flag.NewFlagSet(sub, flag.ContinueOnError)
			port := fs.Int("port", 8080, "")
			targetURL := fs.String("target-url", cfg.UpstreamURL, "")
			fallbackEnabled, modelPrimary, modelSecondary := addModelFlags(fs, cfg)

			args := []string{"--target-url=ftp://invalid-scheme"}
			if err := fs.Parse(args); err != nil {
				t.Fatalf("parse err: %v", err)
			}
			cfg.Port = *port
			cfg.UpstreamURL = *targetURL
			cfg.FallbackSecondaryEnabled = *fallbackEnabled
			cfg.ModelPrimary = *modelPrimary
			cfg.ModelSecondary = *modelSecondary

			if err := cfg.Validate(); err == nil {
				t.Errorf("[%s] expected validation error for ftp scheme, got nil", sub)
			}
		})
	}
}
