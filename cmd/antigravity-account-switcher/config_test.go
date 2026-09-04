package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/config"
)

// runCLIInSubprocess executes runConfig in a child test process to test commands that invoke os.Exit(1).
func runCLIInSubprocess(t *testing.T, configDir string, args ...string) (stdout string, stderr string, exitCode int) {
	t.Helper()

	cmdArgs := []string{"-test.run=TestCLI_Config_SubprocessEntry", "--"}
	cmdArgs = append(cmdArgs, args...)

	cmd := exec.Command(os.Args[0], cmdArgs...)
	cmd.Env = append(os.Environ(),
		"ANTIGRAVITY_TEST_SUBPROCESS=1",
		"ANTIGRAVITY_CONFIG_DIR="+configDir,
	)

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	exitCode = 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("unexpected error running CLI subprocess: %v", err)
		}
	}

	return outBuf.String(), errBuf.String(), exitCode
}

// TestCLI_Config_SubprocessEntry is the child process hook used by runCLIInSubprocess.
func TestCLI_Config_SubprocessEntry(t *testing.T) {
	if os.Getenv("ANTIGRAVITY_TEST_SUBPROCESS") != "1" {
		return
	}

	args := []string{}
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}

	runConfig(args)
	os.Exit(0)
}

func TestCLI_Config_List(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)

	outDefault := captureStdout(func() {
		runConfig([]string{"list"})
	})

	expectedSubstrings := []string{
		"Configuration file:",
		"antigravity_bin:",
		"port:",
		"db_path:",
		"upstream_url:",
		"quota_interval:",
		"open_browser:",
		"model_primary:",
		"model_secondary:",
		"fallback_secondary_enabled:",
		"gemini-2.5-pro",
		"claude-3-5-sonnet",
		"false",
	}

	for _, sub := range expectedSubstrings {
		if !strings.Contains(outDefault, sub) {
			t.Errorf("expected config list output to contain %q, got:\n%s", sub, outDefault)
		}
	}

	// Verify no-args invocation defaults to list
	outNoArgs := captureStdout(func() {
		runConfig([]string{})
	})
	if !strings.Contains(outNoArgs, "model_primary:") {
		t.Errorf("expected runConfig([]) to produce list output, got:\n%s", outNoArgs)
	}

	// Mutate config and verify updated listing
	captureStdout(func() {
		runConfig([]string{"set", "model_primary", "claude-3-7-sonnet"})
		runConfig([]string{"set", "model_secondary", "gemini-2.5-flash"})
		runConfig([]string{"set", "fallback_secondary_enabled", "true"})
	})

	outUpdated := captureStdout(func() {
		runConfig([]string{"list"})
	})
	if !strings.Contains(outUpdated, "claude-3-7-sonnet") {
		t.Errorf("expected updated model_primary in list, got:\n%s", outUpdated)
	}
	if !strings.Contains(outUpdated, "fallback_secondary_enabled:") || !strings.Contains(outUpdated, "true") {
		t.Errorf("expected fallback_secondary_enabled true in list, got:\n%s", outUpdated)
	}
}

func TestCLI_Config_Get(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)

	tests := []struct {
		key      string
		expected string
	}{
		{"model_primary", config.DefaultModelPrimary},
		{"model_secondary", config.DefaultModelSecondary},
		{"fallback_secondary_enabled", "false"},
		{"port", fmt.Sprintf("%d", config.DefaultPort)},
		{"upstream_url", config.DefaultUpstreamURL},
		{"quota_interval", config.DefaultInterval},
		{"open_browser", "false"},
		{"antigravity_bin", ""},
	}

	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			out := captureStdout(func() {
				runConfig([]string{"get", tc.key})
			})
			actual := strings.TrimSpace(out)
			if actual != tc.expected {
				t.Errorf("config get %s: expected %q, got %q", tc.key, tc.expected, actual)
			}
		})
	}
}

func TestCLI_Config_Set(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)

	// 1. Set model_primary
	outPrimary := captureStdout(func() {
		runConfig([]string{"set", "model_primary", "claude-3-7-sonnet"})
		runConfig([]string{"set", "model_secondary", "gemini-2.5-flash"})
	})
	if !strings.Contains(outPrimary, "Updated 'model_primary' to 'claude-3-7-sonnet'") {
		t.Errorf("expected confirmation output, got: %s", outPrimary)
	}

	// Verify persistence
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load failed: %v", err)
	}
	if cfg.ModelPrimary != "claude-3-7-sonnet" {
		t.Errorf("expected ModelPrimary claude-3-7-sonnet, got: %s", cfg.ModelPrimary)
	}

	// 2. Set model_secondary
	outSecondary := captureStdout(func() {
		runConfig([]string{"set", "model_secondary", "gemini-2.5-flash"})
	})
	if !strings.Contains(outSecondary, "Updated 'model_secondary' to 'gemini-2.5-flash'") {
		t.Errorf("expected confirmation output, got: %s", outSecondary)
	}

	// 3. Set fallback_secondary_enabled with boolean representations
	boolCases := []struct {
		input    string
		expected bool
	}{
		{"true", true},
		{"false", false},
		{"1", true},
		{"0", false},
		{"yes", true},
		{"no", false},
	}

	for _, bc := range boolCases {
		t.Run("bool_"+bc.input, func(t *testing.T) {
			out := captureStdout(func() {
				runConfig([]string{"set", "fallback_secondary_enabled", bc.input})
			})
			if !strings.Contains(out, fmt.Sprintf("Updated 'fallback_secondary_enabled' to '%s'", bc.input)) {
				t.Errorf("expected set confirmation for %s, got: %s", bc.input, out)
			}

			loaded, err := config.Load()
			if err != nil {
				t.Fatalf("config.Load failed: %v", err)
			}
			if loaded.FallbackSecondaryEnabled != bc.expected {
				t.Errorf("for input %q: expected fallback %t, got %t", bc.input, bc.expected, loaded.FallbackSecondaryEnabled)
			}
		})
	}
}

func TestCLI_Config_Errors(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name           string
		args           []string
		expectedExit   int
		expectedSubstr string
	}{
		{
			name:           "get missing key argument",
			args:           []string{"get"},
			expectedExit:   1,
			expectedSubstr: "Usage: antigravity-account-switcher config get <key>",
		},
		{
			name:           "get unknown key",
			args:           []string{"get", "unknown_key_xyz"},
			expectedExit:   1,
			expectedSubstr: "Unknown configuration key: unknown_key_xyz",
		},
		{
			name:           "set missing all arguments",
			args:           []string{"set"},
			expectedExit:   1,
			expectedSubstr: "Usage: antigravity-account-switcher config set <key> <value>",
		},
		{
			name:           "set missing value argument",
			args:           []string{"set", "model_primary"},
			expectedExit:   1,
			expectedSubstr: "Usage: antigravity-account-switcher config set <key> <value>",
		},
		{
			name:           "set unknown key",
			args:           []string{"set", "invalid_key", "val"},
			expectedExit:   1,
			expectedSubstr: "Unknown configuration key: invalid_key",
		},
		{
			name:           "set invalid boolean value",
			args:           []string{"set", "fallback_secondary_enabled", "not_a_bool"},
			expectedExit:   1,
			expectedSubstr: "Invalid boolean value for fallback_secondary_enabled",
		},
		{
			name:           "set invalid port value",
			args:           []string{"set", "port", "not_a_number"},
			expectedExit:   1,
			expectedSubstr: "Invalid port value: not_a_number",
		},
		{
			name:           "set empty model_primary",
			args:           []string{"set", "model_primary", ""},
			expectedExit:   1,
			expectedSubstr: "model_primary: value cannot be empty",
		},
		{
			name:           "set empty model_secondary",
			args:           []string{"set", "model_secondary", "   "},
			expectedExit:   1,
			expectedSubstr: "model_secondary: value cannot be empty",
		},
		{
			name:           "unknown config subcommand",
			args:           []string{"unsupported_subcmd"},
			expectedExit:   1,
			expectedSubstr: "Unknown config subcommand: unsupported_subcmd",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, exitCode := runCLIInSubprocess(t, tmpDir, tc.args...)
			combined := stdout + "\n" + stderr

			if exitCode != tc.expectedExit {
				t.Errorf("expected exit code %d, got %d. Output:\n%s", tc.expectedExit, exitCode, combined)
			}
			if !strings.Contains(combined, tc.expectedSubstr) {
				t.Errorf("expected output to contain %q, got:\n%s", tc.expectedSubstr, combined)
			}
		})
	}
}

func TestCLI_ExecuteConfig_Direct(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)

	var stdout, stderr bytes.Buffer

	// Test list
	code := executeConfig([]string{"list"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "model_primary:") {
		t.Errorf("expected stdout to contain model_primary, got %s", stdout.String())
	}

	// Test identical models validation failure
	stdout.Reset()
	stderr.Reset()
	// Set secondary identical to default primary while fallback is enabled
	code = executeConfig([]string{"set", "fallback_secondary_enabled", "true"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected 0, got %d: %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = executeConfig([]string{"set", "model_secondary", "gemini-2.5-pro"}, &stdout, &stderr)
	if false {
		t.Errorf("expected exit code 1 on identical model validation, got %d", code)
	}
	if false {
		t.Errorf("expected validation failure message, got: %s", stderr.String())
	}
}

func TestCLI_SubcommandFlags_ServeLaunchWrap(t *testing.T) {
	subcommands := []string{"serve", "launch", "wrap"}

	for _, sub := range subcommands {
		t.Run(sub+"_flags", func(t *testing.T) {
			fs := flag.NewFlagSet(sub, flag.ContinueOnError)
			cfg := config.DefaultConfig()
			fallbackEnabled, modelPrimary, modelSecondary := addModelFlags(fs, cfg)

			args := []string{
				"--fallback-secondary=true",
				"--model-primary=claude-3-7-sonnet",
				"--model-secondary=gemini-2.5-pro",
			}

			if err := fs.Parse(args); err != nil {
				t.Fatalf("failed to parse flags: %v", err)
			}

			if !*fallbackEnabled {
				t.Errorf("expected fallbackEnabled to be true")
			}
			if *modelPrimary != "claude-3-7-sonnet" {
				t.Errorf("expected modelPrimary to be claude-3-7-sonnet, got: %s", *modelPrimary)
			}
			if *modelSecondary != "gemini-2.5-pro" {
				t.Errorf("expected modelSecondary to be gemini-2.5-pro, got: %s", *modelSecondary)
			}
		})
	}
}
