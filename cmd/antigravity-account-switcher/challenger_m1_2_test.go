package main

import (
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/config"
)

// TestChallenger_PrecedenceMatrix verifies the complete 4-tier precedence hierarchy:
// Tier 1 (Lowest): Built-in Defaults
// Tier 2: ~/.config/antigravity-account-switcher/config.json
// Tier 3: Environment Variables (ANTIGRAVITY_*)
// Tier 4 (Highest): CLI Flags (--fallback-secondary, --model-primary, --model-secondary)
func TestChallenger_PrecedenceMatrix(t *testing.T) {
	tmpDir := t.TempDir()
	configFilePath := filepath.Join(tmpDir, "config.json")

	// Helper to write config.json
	writeConfigJSON := func(primary, secondary string, fallback bool) {
		cfg := map[string]any{
			"port":                       8080,
			"model_primary":              primary,
			"model_secondary":            secondary,
			"fallback_secondary_enabled": fallback,
		}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("failed to marshal test config: %v", err)
		}
		if err := os.WriteFile(configFilePath, data, 0o644); err != nil {
			t.Fatalf("failed to write test config: %v", err)
		}
	}

	t.Run("Level1_Defaults_WhenNothingSet", func(t *testing.T) {
		emptyDir := t.TempDir()
		t.Setenv("ANTIGRAVITY_CONFIG_DIR", emptyDir)
		t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", "")
		t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", "")
		t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "")

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}

		if cfg.ModelPrimary != config.DefaultModelPrimary {
			t.Errorf("expected default model_primary %q, got %q", config.DefaultModelPrimary, cfg.ModelPrimary)
		}
		if cfg.ModelSecondary != config.DefaultModelSecondary {
			t.Errorf("expected default model_secondary %q, got %q", config.DefaultModelSecondary, cfg.ModelSecondary)
		}
		if cfg.FallbackSecondaryEnabled != config.DefaultFallbackSecondaryEnabled {
			t.Errorf("expected default fallback_secondary_enabled %t, got %t", config.DefaultFallbackSecondaryEnabled, cfg.FallbackSecondaryEnabled)
		}
	})

	t.Run("Level2_ConfigFile_Overrides_Defaults", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ANTIGRAVITY_CONFIG_DIR", dir)
		t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", "")
		t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", "")
		t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "")

		cfgPath := filepath.Join(dir, "config.json")
		data := []byte(`{
			"model_primary": "config-primary-model",
			"model_secondary": "config-secondary-model",
			"fallback_secondary_enabled": true
		}`)
		if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}

		if cfg.ModelPrimary != "config-primary-model" {
			t.Errorf("expected config.json model_primary %q, got %q", "config-primary-model", cfg.ModelPrimary)
		}
		if cfg.ModelSecondary != "config-secondary-model" {
			t.Errorf("expected config.json model_secondary %q, got %q", "config-secondary-model", cfg.ModelSecondary)
		}
		if !cfg.FallbackSecondaryEnabled {
			t.Errorf("expected config.json fallback_secondary_enabled true, got %t", cfg.FallbackSecondaryEnabled)
		}
	})

	t.Run("Level3_EnvVars_Override_ConfigFile", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ANTIGRAVITY_CONFIG_DIR", dir)
		cfgPath := filepath.Join(dir, "config.json")
		data := []byte(`{
			"model_primary": "config-file-primary",
			"model_secondary": "config-file-secondary",
			"fallback_secondary_enabled": false
		}`)
		if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}

		t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", "env-primary-override")
		t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", "env-secondary-override")
		t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "true")

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}

		if cfg.ModelPrimary != "env-primary-override" {
			t.Errorf("expected env override model_primary %q, got %q", "env-primary-override", cfg.ModelPrimary)
		}
		if cfg.ModelSecondary != "env-secondary-override" {
			t.Errorf("expected env override model_secondary %q, got %q", "env-secondary-override", cfg.ModelSecondary)
		}
		if !cfg.FallbackSecondaryEnabled {
			t.Errorf("expected env override fallback_secondary_enabled true, got %t", cfg.FallbackSecondaryEnabled)
		}
	})

	t.Run("Level4_CLIFlags_Override_EnvVars_And_ConfigFile", func(t *testing.T) {
		subcommands := []string{"serve", "launch", "wrap"}

		for _, subcmd := range subcommands {
			t.Run(subcmd, func(t *testing.T) {
				dir := t.TempDir()
				t.Setenv("ANTIGRAVITY_CONFIG_DIR", dir)
				cfgPath := filepath.Join(dir, "config.json")
				data := []byte(`{
					"model_primary": "config-file-primary",
					"model_secondary": "config-file-secondary",
					"fallback_secondary_enabled": false
				}`)
				if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
					t.Fatalf("failed to write config: %v", err)
				}

				t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", "env-primary")
				t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", "env-secondary")
				t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "false")

				// Load config as main commands do
				cfg, err := config.Load()
				if err != nil {
					t.Fatalf("Load() error: %v", err)
				}
				// Verify env loaded into cfg before flags
				if cfg.ModelPrimary != "env-primary" {
					t.Fatalf("precondition failed: env did not override config in cfg")
				}

				fs := flag.NewFlagSet(subcmd, flag.ContinueOnError)
				fallbackSecondary, modelPrimary, modelSecondary := addModelFlags(fs, cfg)

				args := []string{
					"--model-primary=cli-flag-primary",
					"--model-secondary=cli-flag-secondary",
					"--fallback-secondary=true",
				}
				if err := fs.Parse(args); err != nil {
					t.Fatalf("fs.Parse failed: %v", err)
				}

				// Apply parsed flags back to cfg as main.go does
				cfg.FallbackSecondaryEnabled = *fallbackSecondary
				cfg.ModelPrimary = *modelPrimary
				cfg.ModelSecondary = *modelSecondary

				if cfg.ModelPrimary != "cli-flag-primary" {
					t.Errorf("[%s] CLI flag did not override env var: expected %q, got %q", subcmd, "cli-flag-primary", cfg.ModelPrimary)
				}
				if cfg.ModelSecondary != "cli-flag-secondary" {
					t.Errorf("[%s] CLI flag did not override env var: expected %q, got %q", subcmd, "cli-flag-secondary", cfg.ModelSecondary)
				}
				if !cfg.FallbackSecondaryEnabled {
					t.Errorf("[%s] CLI flag did not override env var: expected true, got %t", subcmd, cfg.FallbackSecondaryEnabled)
				}

				// Verify cfg is valid
				if err := cfg.Validate(); err != nil {
					t.Errorf("[%s] parsed cfg failed validation: %v", subcmd, err)
				}
			})
		}
	})

	t.Run("Level4_CLIFlags_PartialOverride_PreservesEnvVars", func(t *testing.T) {
		// When user only specifies --model-primary on CLI, model-secondary and fallback-secondary must retain env/config values
		subcommands := []string{"serve", "launch", "wrap"}

		for _, subcmd := range subcommands {
			t.Run(subcmd, func(t *testing.T) {
				dir := t.TempDir()
				t.Setenv("ANTIGRAVITY_CONFIG_DIR", dir)
				writeConfigJSON("config-p", "config-s", false)

				t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", "env-p")
				t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", "env-s")
				t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "true")

				cfg, _ := config.Load()
				fs := flag.NewFlagSet(subcmd, flag.ContinueOnError)
				fallbackSecondary, modelPrimary, modelSecondary := addModelFlags(fs, cfg)

				// User provides ONLY --model-primary
				args := []string{"--model-primary=only-cli-primary"}
				if err := fs.Parse(args); err != nil {
					t.Fatalf("fs.Parse failed: %v", err)
				}

				cfg.FallbackSecondaryEnabled = *fallbackSecondary
				cfg.ModelPrimary = *modelPrimary
				cfg.ModelSecondary = *modelSecondary

				if cfg.ModelPrimary != "only-cli-primary" {
					t.Errorf("model_primary should be overridden by CLI flag: got %q", cfg.ModelPrimary)
				}
				if cfg.ModelSecondary != "env-s" {
					t.Errorf("model_secondary should retain env var value: got %q, expected 'env-s'", cfg.ModelSecondary)
				}
				if !cfg.FallbackSecondaryEnabled {
					t.Errorf("fallback_secondary_enabled should retain env var value true, got %t", cfg.FallbackSecondaryEnabled)
				}
			})
		}
	})
}

// TestChallenger_CLIFlag_BooleanPermutations tests all standard Go boolean flag formats
func TestChallenger_CLIFlag_BooleanPermutations(t *testing.T) {
	cases := []struct {
		name        string
		flagArgs    []string
		expectedVal bool
	}{
		{"boolean_flag_standalone", []string{"--fallback-secondary"}, true},
		{"boolean_flag_explicit_true", []string{"--fallback-secondary=true"}, true},
		{"boolean_flag_explicit_false", []string{"--fallback-secondary=false"}, false},
		{"boolean_flag_numeric_1", []string{"--fallback-secondary=1"}, true},
		{"boolean_flag_numeric_0", []string{"--fallback-secondary=0"}, false},
		{"boolean_flag_single_dash_true", []string{"-fallback-secondary=true"}, true},
		{"boolean_flag_single_dash_false", []string{"-fallback-secondary=false"}, false},
		{"boolean_flag_single_dash_standalone", []string{"-fallback-secondary"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.FallbackSecondaryEnabled = !tc.expectedVal // start with opposite

			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fb, _, _ := addModelFlags(fs, cfg)

			if err := fs.Parse(tc.flagArgs); err != nil {
				t.Fatalf("fs.Parse(%v) failed: %v", tc.flagArgs, err)
			}

			if *fb != tc.expectedVal {
				t.Errorf("for args %v: expected %t, got %t", tc.flagArgs, tc.expectedVal, *fb)
			}
		})
	}
}

// TestChallenger_DefaultBehaviorPreservation verifies that when fallback is disabled,
// existing systems and legacy setups behave exactly as before.
func TestChallenger_DefaultBehaviorPreservation(t *testing.T) {
	t.Run("DefaultConfig_HasFallbackDisabled", func(t *testing.T) {
		cfg := config.DefaultConfig()
		if cfg.FallbackSecondaryEnabled {
			t.Errorf("DefaultConfig MUST have FallbackSecondaryEnabled = false, got true")
		}
	})

	t.Run("LegacyConfigFile_WithoutFallbackKeys_MaintainsDisabled", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ANTIGRAVITY_CONFIG_DIR", dir)
		legacyJSON := `{
			"port": 7070,
			"db_path": "/tmp/test.db",
			"upstream_url": "https://daily-cloudcode-pa.googleapis.com",
			"quota_interval": "10m",
			"open_browser": false
		}`
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(legacyJSON), 0o644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}

		if cfg.FallbackSecondaryEnabled {
			t.Errorf("legacy config must have FallbackSecondaryEnabled false, got true")
		}
		if cfg.ModelPrimary != config.DefaultModelPrimary {
			t.Errorf("legacy config must inherit default ModelPrimary, got %q", cfg.ModelPrimary)
		}
		if cfg.ModelSecondary != config.DefaultModelSecondary {
			t.Errorf("legacy config must inherit default ModelSecondary, got %q", cfg.ModelSecondary)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("legacy config should pass Validate(): %v", err)
		}
	})

	t.Run("FallbackDisabled_AllowsIdenticalOrEmptyModelsWithoutError", func(t *testing.T) {
		// When fallback is disabled, model_primary and model_secondary being identical or empty
		// should NOT cause validation failure because fallback logic is never triggered.
		cfg := config.DefaultConfig()
		cfg.FallbackSecondaryEnabled = false
		cfg.ModelPrimary = "identical-model"
		cfg.ModelSecondary = "identical-model"

		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() should NOT fail on identical models when fallback is disabled: %v", err)
		}

		cfg.ModelPrimary = ""
		cfg.ModelSecondary = ""
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() should NOT fail on empty models when fallback is disabled: %v", err)
		}
	})

	t.Run("FallbackEnabled_StrictlyRejectsIdenticalOrEmptyModels", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.FallbackSecondaryEnabled = true

		// 1. Identical models
		cfg.ModelPrimary = "gemini-2.5-pro"
		cfg.ModelSecondary = "gemini-2.5-pro"
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate() MUST reject identical models when fallback is enabled")
		}

		// 2. Case-insensitive identical
		cfg.ModelPrimary = "Gemini-2.5-Pro"
		cfg.ModelSecondary = "gemini-2.5-pro"
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate() MUST reject case-insensitive identical models when fallback is enabled")
		}

		// 3. Whitespace-padded identical
		cfg.ModelPrimary = "  gemini-2.5-pro  "
		cfg.ModelSecondary = "gemini-2.5-pro"
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate() MUST reject whitespace-padded identical models when fallback is enabled")
		}

		// 4. Empty primary
		cfg.ModelPrimary = "  "
		cfg.ModelSecondary = "gemini-2.5-flash"
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate() MUST reject empty primary model when fallback is enabled")
		}

		// 5. Empty secondary
		cfg.ModelPrimary = "gemini-2.5-pro"
		cfg.ModelSecondary = ""
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate() MUST reject empty secondary model when fallback is enabled")
		}

		// 6. Distinct valid models
		cfg.ModelPrimary = "gemini-2.5-pro"
		cfg.ModelSecondary = "gemini-2.5-flash"
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() should accept valid distinct models: %v", err)
		}
	})
}

// TestChallenger_SubprocessBinaryExecution runs the actual compiled binary from bin/antigravity-account-switcher
// to test CLI flag and environment variable precedence in a real process environment.
func TestChallenger_SubprocessBinaryExecution(t *testing.T) {
	binPath := filepath.Join("..", "..", "bin", "antigravity-account-switcher")
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("binary %s not found; skipping binary test", binPath)
	}

	t.Run("Binary_Config_List_Default", func(t *testing.T) {
		dir := t.TempDir()
		cmd := exec.Command(binPath, "config", "list")
		cmd.Env = []string{
			"ANTIGRAVITY_CONFIG_DIR=" + dir,
			"PATH=" + os.Getenv("PATH"),
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("command failed: %v, output:\n%s", err, out)
		}

		outputStr := string(out)
		expectedKeys := []string{
			"model_primary:              gemini-2.5-pro",
			"model_secondary:            gemini-2.5-flash",
			"fallback_secondary_enabled: false",
		}
		for _, ek := range expectedKeys {
			if !strings.Contains(outputStr, ek) {
				t.Errorf("expected config list to contain %q, got:\n%s", ek, outputStr)
			}
		}
	})

	t.Run("Binary_Config_Set_And_Get_RoundTrip", func(t *testing.T) {
		dir := t.TempDir()
		env := []string{
			"ANTIGRAVITY_CONFIG_DIR=" + dir,
			"PATH=" + os.Getenv("PATH"),
		}

		// 1. Set model_primary
		cmdSetP := exec.Command(binPath, "config", "set", "model_primary", "claude-3-7-sonnet")
		cmdSetP.Env = env
		if out, err := cmdSetP.CombinedOutput(); err != nil {
			t.Fatalf("set model_primary failed: %v, out:\n%s", err, out)
		}

		// 2. Set fallback_secondary_enabled to true
		cmdSetF := exec.Command(binPath, "config", "set", "fallback_secondary_enabled", "true")
		cmdSetF.Env = env
		if out, err := cmdSetF.CombinedOutput(); err != nil {
			t.Fatalf("set fallback failed: %v, out:\n%s", err, out)
		}

		// 3. Get model_primary
		cmdGetP := exec.Command(binPath, "config", "get", "model_primary")
		cmdGetP.Env = env
		outP, err := cmdGetP.CombinedOutput()
		if err != nil {
			t.Fatalf("get model_primary failed: %v, out:\n%s", err, outP)
		}
		if strings.TrimSpace(string(outP)) != "claude-3-7-sonnet" {
			t.Errorf("expected claude-3-7-sonnet, got: %s", string(outP))
		}

		// 4. Get fallback_secondary_enabled
		cmdGetF := exec.Command(binPath, "config", "get", "fallback_secondary_enabled")
		cmdGetF.Env = env
		outF, err := cmdGetF.CombinedOutput()
		if err != nil {
			t.Fatalf("get fallback failed: %v, out:\n%s", err, outF)
		}
		if strings.TrimSpace(string(outF)) != "true" {
			t.Errorf("expected true, got: %s", string(outF))
		}
	})

	t.Run("Binary_EnvVar_Overrides_ConfigJSON_In_Config_Get", func(t *testing.T) {
		dir := t.TempDir()
		env := []string{
			"ANTIGRAVITY_CONFIG_DIR=" + dir,
			"PATH=" + os.Getenv("PATH"),
		}

		// First set value in config.json
		cmdSet := exec.Command(binPath, "config", "set", "model_primary", "from-config-file")
		cmdSet.Env = env
		if out, err := cmdSet.CombinedOutput(); err != nil {
			t.Fatalf("set failed: %v, out:\n%s", err, out)
		}

		// Now query with ANTIGRAVITY_MODEL_PRIMARY set in environment
		cmdGet := exec.Command(binPath, "config", "get", "model_primary")
		cmdGet.Env = append(env, "ANTIGRAVITY_MODEL_PRIMARY=from-environment-var")
		outGet, err := cmdGet.CombinedOutput()
		if err != nil {
			t.Fatalf("get failed: %v, out:\n%s", err, outGet)
		}
		if strings.TrimSpace(string(outGet)) != "from-environment-var" {
			t.Errorf("expected environment variable to override config file: got %q, expected 'from-environment-var'", strings.TrimSpace(string(outGet)))
		}
	})
}

// TestChallenger_AdversarialEdgeCases tests edge cases, whitespace handling, and flag delimiters
func TestChallenger_AdversarialEdgeCases(t *testing.T) {
	t.Run("EnvVar_WhitespaceHandling", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ANTIGRAVITY_CONFIG_DIR", dir)

		// 1. Env vars with padded whitespace should be trimmed
		t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", "   trimmed-primary   ")
		t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", " \t trimmed-secondary \n ")
		t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "   true \t")

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}
		if cfg.ModelPrimary != "trimmed-primary" {
			t.Errorf("expected trimmed primary, got %q", cfg.ModelPrimary)
		}
		if cfg.ModelSecondary != "trimmed-secondary" {
			t.Errorf("expected trimmed secondary, got %q", cfg.ModelSecondary)
		}
		if !cfg.FallbackSecondaryEnabled {
			t.Errorf("expected trimmed boolean to be parsed as true")
		}

		// 2. Env vars with ONLY whitespace should NOT wipe out defaults
		t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", "    ")
		t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", " \t\n ")
		t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "  not-a-bool  ")

		cfg2, err := config.Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}
		if cfg2.ModelPrimary != config.DefaultModelPrimary {
			t.Errorf("whitespace env primary should not overwrite default, got %q", cfg2.ModelPrimary)
		}
		if cfg2.ModelSecondary != config.DefaultModelSecondary {
			t.Errorf("whitespace env secondary should not overwrite default, got %q", cfg2.ModelSecondary)
		}
		if cfg2.FallbackSecondaryEnabled != config.DefaultFallbackSecondaryEnabled {
			t.Errorf("invalid boolean env should not overwrite default, got %t", cfg2.FallbackSecondaryEnabled)
		}
	})

	t.Run("FlagDelimiter_DashDash_Separation_WrapAndLaunch", func(t *testing.T) {
		// Verify flag parsing with '--' delimiter (standard in wrap and launch)
		cfg := config.DefaultConfig()

		fs := flag.NewFlagSet("wrap", flag.ContinueOnError)
		fb, mp, ms := addModelFlags(fs, cfg)

		args := []string{
			"--model-primary=wrap-primary",
			"--model-secondary=wrap-secondary",
			"--fallback-secondary=true",
			"--",
			"my-command",
			"--flag-for-command",
			"--model-primary=ignored-command-arg",
		}

		dashDashIdx := -1
		for i, arg := range args {
			if arg == "--" {
				dashDashIdx = i
				break
			}
		}

		if dashDashIdx < 0 {
			t.Fatalf("expected dashDashIdx >= 0")
		}

		if err := fs.Parse(args[:dashDashIdx]); err != nil {
			t.Fatalf("failed to parse before --: %v", err)
		}

		cmdToRun := args[dashDashIdx+1:]

		if *mp != "wrap-primary" {
			t.Errorf("expected wrap-primary, got %s", *mp)
		}
		if *ms != "wrap-secondary" {
			t.Errorf("expected wrap-secondary, got %s", *ms)
		}
		if !*fb {
			t.Errorf("expected fallbackEnabled true")
		}

		// Verify passthrough command received its own arguments without modification
		expectedCmd := []string{"my-command", "--flag-for-command", "--model-primary=ignored-command-arg"}
		if len(cmdToRun) != len(expectedCmd) {
			t.Fatalf("cmdToRun len mismatch: got %d, expected %d", len(cmdToRun), len(expectedCmd))
		}
		for i, v := range cmdToRun {
			if v != expectedCmd[i] {
				t.Errorf("cmdToRun[%d]: expected %q, got %q", i, expectedCmd[i], v)
			}
		}
	})

	t.Run("Config_JSON_DefensiveDefaults_OnExplicitEmptyFields", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ANTIGRAVITY_CONFIG_DIR", dir)
		t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", "")
		t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", "")
		t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "")

		// Even if config.json explicitly sets empty strings for models
		cfgPath := filepath.Join(dir, "config.json")
		data := []byte(`{
			"model_primary": "",
			"model_secondary": ""
		}`)
		if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}
		if cfg.ModelPrimary != config.DefaultModelPrimary {
			t.Errorf("empty model_primary in json should be defensively defaulted, got %q", cfg.ModelPrimary)
		}
		if cfg.ModelSecondary != config.DefaultModelSecondary {
			t.Errorf("empty model_secondary in json should be defensively defaulted, got %q", cfg.ModelSecondary)
		}
	})
}
