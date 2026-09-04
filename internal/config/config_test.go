package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Port != DefaultPort {
		t.Errorf("expected port %d, got %d", DefaultPort, cfg.Port)
	}
	if cfg.UpstreamURL != DefaultUpstreamURL {
		t.Errorf("expected upstream %s, got %s", DefaultUpstreamURL, cfg.UpstreamURL)
	}
	if cfg.ModelPrimary != DefaultModelPrimary {
		t.Errorf("expected model_primary %s, got %s", DefaultModelPrimary, cfg.ModelPrimary)
	}
	if cfg.ModelSecondary != DefaultModelSecondary {
		t.Errorf("expected model_secondary %s, got %s", DefaultModelSecondary, cfg.ModelSecondary)
	}
	if cfg.FallbackSecondaryEnabled != DefaultFallbackSecondaryEnabled {
		t.Errorf("expected fallback_secondary_enabled %t, got %t", DefaultFallbackSecondaryEnabled, cfg.FallbackSecondaryEnabled)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected DefaultConfig to be valid, got: %v", err)
	}
}

func TestConfig_ModelDefaults(t *testing.T) {
	t.Parallel()

	if DefaultModelPrimary != "gemini-2.5-pro" {
		t.Errorf("expected DefaultModelPrimary to be gemini-2.5-pro, got %s", DefaultModelPrimary)
	}
	if DefaultModelSecondary != "gemini-2.5-flash" {
		t.Errorf("expected DefaultModelSecondary to be gemini-2.5-flash, got %s", DefaultModelSecondary)
	}
	if DefaultFallbackSecondaryEnabled != false {
		t.Errorf("expected DefaultFallbackSecondaryEnabled to be false, got %t", DefaultFallbackSecondaryEnabled)
	}
}

func TestSaveAndLoadConfig(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)

	cfg := &Config{
		Port:                     9999,
		DBPath:                   filepath.Join(tmpDir, "custom.db"),
		AntigravityBin:           "/custom/bin/antigravity",
		UpstreamURL:              "https://custom-upstream.example.com",
		QuotaInterval:            "30s",
		OpenBrowser:              true,
		ModelPrimary:             "claude-3-7-sonnet",
		ModelSecondary:           "gemini-2.5-flash",
		FallbackSecondaryEnabled: true,
	}

	if err := Save(cfg); err != nil {
		t.Fatalf("failed to save config: %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if loaded.Port != 9999 {
		t.Errorf("expected port 9999, got %d", loaded.Port)
	}
	if loaded.AntigravityBin != "/custom/bin/antigravity" {
		t.Errorf("expected bin %s, got %s", "/custom/bin/antigravity", loaded.AntigravityBin)
	}
	if loaded.ModelPrimary != "claude-3-7-sonnet" {
		t.Errorf("expected ModelPrimary 'claude-3-7-sonnet', got %s", loaded.ModelPrimary)
	}
	if loaded.ModelSecondary != "gemini-2.5-flash" {
		t.Errorf("expected ModelSecondary 'gemini-2.5-flash', got %s", loaded.ModelSecondary)
	}
	if !loaded.FallbackSecondaryEnabled {
		t.Errorf("expected FallbackSecondaryEnabled true, got %t", loaded.FallbackSecondaryEnabled)
	}
	if !loaded.OpenBrowser {
		t.Errorf("expected OpenBrowser true, got %t", loaded.OpenBrowser)
	}
}

func TestConfig_JSONSerialization_RoundTrip(t *testing.T) {
	t.Parallel()

	orig := &Config{
		Port:                     8080,
		DBPath:                   "/tmp/accounts.db",
		AntigravityBin:           "/usr/bin/antigravity",
		UpstreamURL:              "https://daily-cloudcode-pa.googleapis.com",
		QuotaInterval:            "5m",
		OpenBrowser:              false,
		ModelPrimary:             "claude-3-5-sonnet",
		ModelSecondary:           "gemini-2.5-flash",
		FallbackSecondaryEnabled: true,
	}

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("failed to marshal config: %v", err)
	}

	rawJSON := string(data)
	if !strings.Contains(rawJSON, `"model_primary":"claude-3-5-sonnet"`) {
		t.Errorf("expected json to contain model_primary, got %s", rawJSON)
	}
	if !strings.Contains(rawJSON, `"model_secondary":"gemini-2.5-flash"`) {
		t.Errorf("expected json to contain model_secondary, got %s", rawJSON)
	}
	if !strings.Contains(rawJSON, `"fallback_secondary_enabled":true`) {
		t.Errorf("expected json to contain fallback_secondary_enabled, got %s", rawJSON)
	}

	var parsed Config
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal config: %v", err)
	}

	if parsed.ModelPrimary != orig.ModelPrimary {
		t.Errorf("expected ModelPrimary %s, got %s", orig.ModelPrimary, parsed.ModelPrimary)
	}
	if parsed.ModelSecondary != orig.ModelSecondary {
		t.Errorf("expected ModelSecondary %s, got %s", orig.ModelSecondary, parsed.ModelSecondary)
	}
	if parsed.FallbackSecondaryEnabled != orig.FallbackSecondaryEnabled {
		t.Errorf("expected FallbackSecondaryEnabled %t, got %t", orig.FallbackSecondaryEnabled, parsed.FallbackSecondaryEnabled)
	}
}

func TestConfig_BackwardCompatibility_DefaultsPreserved(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)

	legacyJSON := `{
  "port": 8888,
  "db_path": "/tmp/legacy.db",
  "upstream_url": "https://daily-cloudcode-pa.googleapis.com",
  "quota_interval": "10m"
}`
	configPath := filepath.Join(tmpDir, ConfigFileName)
	if err := os.WriteFile(configPath, []byte(legacyJSON), 0o644); err != nil {
		t.Fatalf("failed to write legacy config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("failed to load legacy config: %v", err)
	}

	if cfg.Port != 8888 {
		t.Errorf("expected port 8888, got %d", cfg.Port)
	}
	if cfg.ModelPrimary != DefaultModelPrimary {
		t.Errorf("expected default model_primary %s, got %s", DefaultModelPrimary, cfg.ModelPrimary)
	}
	if cfg.ModelSecondary != DefaultModelSecondary {
		t.Errorf("expected default model_secondary %s, got %s", DefaultModelSecondary, cfg.ModelSecondary)
	}
	if cfg.FallbackSecondaryEnabled != false {
		t.Errorf("expected default fallback_secondary_enabled false, got %t", cfg.FallbackSecondaryEnabled)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected loaded legacy config to be valid, got: %v", err)
	}
}

func TestConfig_EnvOverrides_Models(t *testing.T) {
	tests := []struct {
		name             string
		envPrimary       string
		envSecondary     string
		envFallback      string
		expectedPrimary  string
		expectedSecond   string
		expectedFallback bool
	}{
		{
			name:             "override primary only",
			envPrimary:       "claude-3-7-sonnet",
			expectedPrimary:  "claude-3-7-sonnet",
			expectedSecond:   DefaultModelSecondary,
			expectedFallback: false,
		},
		{
			name:             "override secondary only",
			envSecondary:     "gemini-2.5-pro",
			expectedPrimary:  DefaultModelPrimary,
			expectedSecond:   "gemini-2.5-pro",
			expectedFallback: false,
		},
		{
			name:             "override fallback true",
			envFallback:      "true",
			expectedPrimary:  DefaultModelPrimary,
			expectedSecond:   DefaultModelSecondary,
			expectedFallback: true,
		},
		{
			name:             "override fallback with numeric 1",
			envFallback:      "1",
			expectedPrimary:  DefaultModelPrimary,
			expectedSecond:   DefaultModelSecondary,
			expectedFallback: true,
		},
		{
			name:             "override fallback with yes",
			envFallback:      "yes",
			expectedPrimary:  DefaultModelPrimary,
			expectedSecond:   DefaultModelSecondary,
			expectedFallback: true,
		},
		{
			name:             "override fallback with false",
			envFallback:      "false",
			expectedPrimary:  DefaultModelPrimary,
			expectedSecond:   DefaultModelSecondary,
			expectedFallback: false,
		},
		{
			name:             "override all three",
			envPrimary:       "claude-3-7-sonnet",
			envSecondary:     "gpt-4o",
			envFallback:      "on",
			expectedPrimary:  "claude-3-7-sonnet",
			expectedSecond:   "gpt-4o",
			expectedFallback: true,
		},
		{
			name:             "invalid boolean ignored",
			envFallback:      "invalid_bool",
			expectedPrimary:  DefaultModelPrimary,
			expectedSecond:   DefaultModelSecondary,
			expectedFallback: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)

			if tc.envPrimary != "" {
				t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", tc.envPrimary)
			} else {
				t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", "")
			}

			if tc.envSecondary != "" {
				t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", tc.envSecondary)
			} else {
				t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", "")
			}

			if tc.envFallback != "" {
				t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", tc.envFallback)
			} else {
				t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "")
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}

			if cfg.ModelPrimary != tc.expectedPrimary {
				t.Errorf("expected ModelPrimary %q, got %q", tc.expectedPrimary, cfg.ModelPrimary)
			}
			if cfg.ModelSecondary != tc.expectedSecond {
				t.Errorf("expected ModelSecondary %q, got %q", tc.expectedSecond, cfg.ModelSecondary)
			}
			if cfg.FallbackSecondaryEnabled != tc.expectedFallback {
				t.Errorf("expected FallbackSecondaryEnabled %t, got %t", tc.expectedFallback, cfg.FallbackSecondaryEnabled)
			}
		})
	}
}

func TestParseBool(t *testing.T) {
	t.Parallel()

	truthy := []string{"1", "t", "T", "true", "True", "TRUE", "yes", "Yes", "YES", "y", "Y", "on", "On", "ON", "  true  ", " yes "}
	for _, s := range truthy {
		val, err := ParseBool(s)
		if err != nil {
			t.Errorf("expected ParseBool(%q) to succeed, got error: %v", s, err)
		}
		if !val {
			t.Errorf("expected ParseBool(%q) to be true, got false", s)
		}
	}

	falsy := []string{"0", "f", "F", "false", "False", "FALSE", "no", "No", "NO", "n", "N", "off", "Off", "OFF", "  false  ", " no "}
	for _, s := range falsy {
		val, err := ParseBool(s)
		if err != nil {
			t.Errorf("expected ParseBool(%q) to succeed, got error: %v", s, err)
		}
		if val {
			t.Errorf("expected ParseBool(%q) to be false, got true", s)
		}
	}

	invalid := []string{"", "   ", "2", "-1", "maybe", "enabled", "disabled", "null", "foo"}
	for _, s := range invalid {
		val, err := ParseBool(s)
		if err == nil {
			t.Errorf("expected ParseBool(%q) to fail, got value: %t", s, val)
		}
	}
}

func TestConfig_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		modify    func(c *Config)
		expectErr bool
		errSubstr string
	}{
		{
			name:      "default config is valid",
			modify:    func(c *Config) {},
			expectErr: false,
		},
		{
			name: "fallback enabled with valid distinct models",
			modify: func(c *Config) {
				c.FallbackSecondaryEnabled = true
				c.ModelPrimary = "gemini-2.5-pro"
				c.ModelSecondary = "gemini-2.5-flash"
			},
			expectErr: false,
		},
		{
			name: "fallback enabled with empty model_primary",
			modify: func(c *Config) {
				c.FallbackSecondaryEnabled = true
				c.ModelPrimary = ""
				c.ModelSecondary = "gemini-2.5-flash"
			},
			expectErr: true,
			errSubstr: "model_primary cannot be empty",
		},
		{
			name: "fallback enabled with whitespace model_primary",
			modify: func(c *Config) {
				c.FallbackSecondaryEnabled = true
				c.ModelPrimary = "   "
				c.ModelSecondary = "gemini-2.5-flash"
			},
			expectErr: true,
			errSubstr: "model_primary cannot be empty",
		},
		{
			name: "fallback enabled with empty model_secondary",
			modify: func(c *Config) {
				c.FallbackSecondaryEnabled = true
				c.ModelPrimary = "gemini-2.5-pro"
				c.ModelSecondary = ""
			},
			expectErr: true,
			errSubstr: "model_secondary cannot be empty",
		},
		{
			name: "fallback enabled with whitespace model_secondary",
			modify: func(c *Config) {
				c.FallbackSecondaryEnabled = true
				c.ModelPrimary = "gemini-2.5-pro"
				c.ModelSecondary = "   \t\n"
			},
			expectErr: true,
			errSubstr: "model_secondary cannot be empty",
		},
		{
			name: "fallback enabled with identical models",
			modify: func(c *Config) {
				c.FallbackSecondaryEnabled = true
				c.ModelPrimary = "gemini-2.5-pro"
				c.ModelSecondary = "gemini-2.5-pro"
			},
			expectErr: true,
			errSubstr: "cannot be identical",
		},
		{
			name: "fallback enabled with case-insensitive identical models",
			modify: func(c *Config) {
				c.FallbackSecondaryEnabled = true
				c.ModelPrimary = "Gemini-2.5-Pro"
				c.ModelSecondary = "gemini-2.5-pro"
			},
			expectErr: true,
			errSubstr: "cannot be identical",
		},
		{
			name: "fallback enabled with whitespace-padded identical models",
			modify: func(c *Config) {
				c.FallbackSecondaryEnabled = true
				c.ModelPrimary = "  gemini-2.5-pro  "
				c.ModelSecondary = "gemini-2.5-pro"
			},
			expectErr: true,
			errSubstr: "cannot be identical",
		},
		{
			name: "fallback disabled with empty models does not fail",
			modify: func(c *Config) {
				c.FallbackSecondaryEnabled = false
				c.ModelPrimary = ""
				c.ModelSecondary = ""
			},
			expectErr: false,
		},
		{
			name: "fallback disabled with identical models does not fail",
			modify: func(c *Config) {
				c.FallbackSecondaryEnabled = false
				c.ModelPrimary = "gemini-2.5-pro"
				c.ModelSecondary = "gemini-2.5-pro"
			},
			expectErr: false,
		},
		{
			name: "invalid port zero",
			modify: func(c *Config) {
				c.Port = 0
			},
			expectErr: true,
			errSubstr: "invalid port",
		},
		{
			name: "invalid port negative",
			modify: func(c *Config) {
				c.Port = -1
			},
			expectErr: true,
			errSubstr: "invalid port",
		},
		{
			name: "invalid port too high",
			modify: func(c *Config) {
				c.Port = 65536
			},
			expectErr: true,
			errSubstr: "invalid port",
		},
		{
			name: "valid boundary port 1",
			modify: func(c *Config) {
				c.Port = 1
			},
			expectErr: false,
		},
		{
			name: "valid boundary port 65535",
			modify: func(c *Config) {
				c.Port = 65535
			},
			expectErr: false,
		},
		{
			name: "invalid quota interval duration",
			modify: func(c *Config) {
				c.QuotaInterval = "invalid_duration"
			},
			expectErr: true,
			errSubstr: "invalid quota_interval",
		},
		{
			name: "valid quota interval duration",
			modify: func(c *Config) {
				c.QuotaInterval = "10m"
			},
			expectErr: false,
		},
		{
			name: "invalid upstream URL scheme",
			modify: func(c *Config) {
				c.UpstreamURL = "ftp://invalid-scheme.com"
			},
			expectErr: true,
			errSubstr: "scheme must be http or https",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.modify(cfg)
			err := cfg.Validate()
			if tc.expectErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.errSubstr)
				}
				if !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("expected error containing %q, got %q", tc.errSubstr, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestResolveAntigravityBin_ExplicitOverride(t *testing.T) {
	tmpDir := t.TempDir()
	fakeBin := filepath.Join(tmpDir, "antigravity")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\necho test\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	resolved, err := ResolveAntigravityBin(fakeBin)
	if err != nil {
		t.Fatalf("expected resolution to succeed, got %v", err)
	}

	absFake, _ := filepath.Abs(fakeBin)
	if resolved != absFake {
		t.Errorf("expected %s, got %s", absFake, resolved)
	}
}

func TestFindAntigravityIcon(t *testing.T) {
	tmpDir := t.TempDir()
	binPath := filepath.Join(tmpDir, "Antigravity-x64", "antigravity")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	iconPath := filepath.Join(tmpDir, "icon.png")
	if err := os.WriteFile(iconPath, []byte("fake icon png"), 0o644); err != nil {
		t.Fatal(err)
	}

	found := FindAntigravityIcon(binPath)
	if found != iconPath {
		t.Errorf("expected found icon %s, got %s", iconPath, found)
	}
}
