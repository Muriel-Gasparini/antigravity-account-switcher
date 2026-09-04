package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestChallenger_Config_BoundaryPorts tests port numbers at and beyond limits.
func TestChallenger_Config_BoundaryPorts(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		port    int
		wantErr bool
	}{
		{-1000, true},
		{-1, true},
		{0, true},
		{1, false},
		{80, false},
		{443, false},
		{8080, false},
		{65535, false},
		{65536, true},
		{100000, true},
	}

	for _, tc := range testCases {
		cfg := DefaultConfig()
		cfg.Port = tc.port
		err := cfg.Validate()
		if tc.wantErr && err == nil {
			t.Errorf("Port %d: expected validation error, got nil", tc.port)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("Port %d: expected valid, got error: %v", tc.port, err)
		}
	}
}

// TestChallenger_Config_ModelUnicodeAndExtremeStrings tests unicode, emojis, RTL, and huge strings.
func TestChallenger_Config_ModelUnicodeAndExtremeStrings(t *testing.T) {

	tmpDir := t.TempDir()

	hugeString := strings.Repeat("a", 100000)
	unicodeModels := []struct {
		name      string
		primary   string
		secondary string
		valid     bool
	}{
		{
			name:      "ascii models",
			primary:   "gemini-2.5-pro",
			secondary: "gemini-2.5-flash",
			valid:     true,
		},
		{
			name:      "emoji models",
			primary:   "gemini-🤖",
			secondary: "claude-🧠",
			valid:     true,
		},
		{
			name:      "cjk models",
			primary:   "モデル-プライマリ",
			secondary: "モデル-セカンダリ",
			valid:     true,
		},
		{
			name:      "rtl arabic models",
			primary:   "نموذج-رئيسي",
			secondary: "نموذج-ثانوي",
			valid:     true,
		},
		{
			name:      "huge 100KB model names",
			primary:   "primary-" + hugeString,
			secondary: "secondary-" + hugeString,
			valid:     true,
		},
		{
			name:      "identical emojis",
			primary:   "🤖-model",
			secondary: "🤖-model",
			valid:     false,
		},
		{
			name:      "identical cjk",
			primary:   "模型A",
			secondary: "模型A",
			valid:     false,
		},
	}

	for _, tc := range unicodeModels {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.FallbackSecondaryEnabled = true
			cfg.ModelPrimary = tc.primary
			cfg.ModelSecondary = tc.secondary

			err := cfg.Validate()
			if tc.valid && err != nil {
				t.Errorf("expected valid for %s, got error: %v", tc.name, err)
			}
			if !tc.valid && err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}

			// If valid, test round-trip through JSON Save/Load
			if tc.valid {
				caseDir := filepath.Join(tmpDir, strings.ReplaceAll(tc.name, " ", "_"))
				t.Setenv("ANTIGRAVITY_CONFIG_DIR", caseDir)
				if err := Save(cfg); err != nil {
					t.Fatalf("failed to save config: %v", err)
				}
				loaded, err := Load()
				if err != nil {
					t.Fatalf("failed to load saved config: %v", err)
				}
				if loaded.ModelPrimary != tc.primary {
					t.Errorf("primary mismatch after save/load: expected %d bytes, got %d bytes", len(tc.primary), len(loaded.ModelPrimary))
				}
				if loaded.ModelSecondary != tc.secondary {
					t.Errorf("secondary mismatch after save/load: expected %d bytes, got %d bytes", len(tc.secondary), len(loaded.ModelSecondary))
				}
			}
		})
	}
}

// TestChallenger_Config_WhitespaceAndEmptyVariations tests various whitespace characters.
func TestChallenger_Config_WhitespaceAndEmptyVariations(t *testing.T) {
	t.Parallel()

	whitespaces := []string{
		" ",
		"  ",
		"\t",
		"\n",
		"\r\n",
		" \t\n\r ",
		"\u00A0", // non-breaking space
		"\u2000", // en quad
		"\u3000", // ideographic space
	}

	for i, ws := range whitespaces {
		t.Run(fmt.Sprintf("whitespace_%d", i), func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.FallbackSecondaryEnabled = true
			cfg.ModelPrimary = ws
			cfg.ModelSecondary = "gemini-2.5-flash"

			if err := cfg.Validate(); err == nil {
				t.Errorf("expected error for whitespace-only model_primary (%q), got nil", ws)
			}

			cfg.ModelPrimary = "gemini-2.5-pro"
			cfg.ModelSecondary = ws
			if err := cfg.Validate(); err == nil {
				t.Errorf("expected error for whitespace-only model_secondary (%q), got nil", ws)
			}
		})
	}
}

// TestChallenger_Config_CaseInsensitiveCollisions tests casing folding under various representations.
func TestChallenger_Config_CaseInsensitiveCollisions(t *testing.T) {
	t.Parallel()

	pairs := []struct {
		p string
		s string
	}{
		{"gemini-2.5-pro", "GEMINI-2.5-PRO"},
		{"Claude-3-7-Sonnet", "claude-3-7-sonnet"},
		{"  gpt-4o  ", "GPT-4O"},
		{"\tGEMINI-2.5-FLASH\n", "gemini-2.5-flash"},
	}

	for _, pair := range pairs {
		cfg := DefaultConfig()
		cfg.FallbackSecondaryEnabled = true
		cfg.ModelPrimary = pair.p
		cfg.ModelSecondary = pair.s
		if err := cfg.Validate(); err == nil {
			t.Errorf("expected identical model error for %q and %q, got nil", pair.p, pair.s)
		}
	}
}

// TestChallenger_Config_MalformedJSONFiles tests how Load handles malformed, unreadable, or invalid JSON.
func TestChallenger_Config_MalformedJSONFiles(t *testing.T) {
	malformedCases := []struct {
		name    string
		content string
	}{
		{"empty file", ""},
		{"syntax error", "{port: 8080}"},
		{"truncated json", `{"port": 8080, "model_primary":`},
		{"array json", `[1, 2, 3]`},
		{"type mismatch bool as string", `{"fallback_secondary_enabled": "not_a_bool"}`},
		{"type mismatch port as string", `{"port": "eighty-eighty"}`},
		{"huge invalid number for port", `{"port": 9999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999}`},
	}

	for _, mc := range malformedCases {
		t.Run(mc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
			cfgFile := filepath.Join(tmpDir, ConfigFileName)
			if err := os.WriteFile(cfgFile, []byte(mc.content), 0o644); err != nil {
				t.Fatal(err)
			}

			cfg, err := Load()
			if err == nil {
				t.Errorf("expected error loading malformed JSON %q, got cfg: %+v", mc.name, cfg)
			}
		})
	}
}

// TestChallenger_Config_EnvVarBoundaryAndFuzzing tests extreme and invalid env vars.
func TestChallenger_Config_EnvVarBoundaryAndFuzzing(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)

	// Whitespace-only model env vars should be ignored (fallback to default)
	t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", "   \t\n  ")
	t.Setenv("ANTIGRAVITY_MODEL_SECONDARY", "      ")
	t.Setenv("ANTIGRAVITY_FALLBACK_SECONDARY_ENABLED", "invalid_boolean_string")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.ModelPrimary != DefaultModelPrimary {
		t.Errorf("expected default model primary %q, got %q", DefaultModelPrimary, cfg.ModelPrimary)
	}
	if cfg.ModelSecondary != DefaultModelSecondary {
		t.Errorf("expected default model secondary %q, got %q", DefaultModelSecondary, cfg.ModelSecondary)
	}
	if cfg.FallbackSecondaryEnabled != DefaultFallbackSecondaryEnabled {
		t.Errorf("expected fallback secondary default false, got %t", cfg.FallbackSecondaryEnabled)
	}
}

// TestChallenger_Config_ParseBool_Fuzz tests a comprehensive corpus of inputs for ParseBool.
func TestChallenger_Config_ParseBool_Fuzz(t *testing.T) {
	t.Parallel()

	validTrue := []string{
		"1", "t", "T", "true", "True", "TRUE", "TrUe",
		"yes", "Yes", "YES", "yEs", "y", "Y",
		"on", "On", "ON",
		" 1 ", "\ttrue\t", "\nyes\n", "  on  ",
	}
	for _, s := range validTrue {
		b, err := ParseBool(s)
		if err != nil || !b {
			t.Errorf("ParseBool(%q): expected (true, nil), got (%t, %v)", s, b, err)
		}
	}

	validFalse := []string{
		"0", "f", "F", "false", "False", "FALSE", "FaLsE",
		"no", "No", "NO", "nO", "n", "N",
		"off", "Off", "OFF",
		" 0 ", "\tfalse\t", "\nno\n", "  off  ",
	}
	for _, s := range validFalse {
		b, err := ParseBool(s)
		if err != nil || b {
			t.Errorf("ParseBool(%q): expected (false, nil), got (%t, %v)", s, b, err)
		}
	}

	invalids := []string{
		"", " ", "   ", "\t", "\n",
		"2", "-1", "00", "01", "10",
		"null", "nil", "none", "undefined",
		"enable", "enabled", "disable", "disabled",
		"true1", "false0", "yes please", "no thanks",
		"true\x00", "false\x00",
		"yess", "noo",
	}
	for _, s := range invalids {
		b, err := ParseBool(s)
		if err == nil {
			t.Errorf("ParseBool(%q): expected error, got value %t", s, b)
		}
	}
}

// TestChallenger_Config_ConcurrentAccess_Race runs 100 goroutines validating, default-loading, and parsing.
func TestChallenger_Config_ConcurrentAccess_Race(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	workers := 100

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			cfg := DefaultConfig()
			if id%2 == 0 {
				cfg.FallbackSecondaryEnabled = true
				cfg.ModelPrimary = fmt.Sprintf("model-p-%d", id)
				cfg.ModelSecondary = fmt.Sprintf("model-s-%d", id)
			}
			_ = cfg.Validate()

			// Test ParseBool concurrency
			_, _ = ParseBool("true")
			_, _ = ParseBool("false")
			_, _ = ParseBool("invalid")

			// Test serialization
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Errorf("worker %d: json.Marshal error: %v", id, err)
				return
			}
			var parsed Config
			if err := json.Unmarshal(data, &parsed); err != nil {
				t.Errorf("worker %d: json.Unmarshal error: %v", id, err)
			}
		}(i)
	}

	wg.Wait()
}
