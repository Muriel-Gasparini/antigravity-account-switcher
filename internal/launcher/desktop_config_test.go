package launcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/config"
)

// InstallDesktop remembers the resolved Antigravity binary in config.json. It
// must only write back what is already on disk plus that one field, never the
// ANTIGRAVITY_* environment overrides (which can include the OAuth client
// secret) that config.Load layers on top.
func TestInstallDesktop_DoesNotPersistEnvOverrides(t *testing.T) {
	home := t.TempDir()
	cfgDir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", cfgDir)

	onDisk := `{"port": 7070, "model_primary": "disk-primary", "model_secondary": "disk-secondary"}`
	cfgPath := filepath.Join(cfgDir, config.ConfigFileName)
	if err := os.WriteFile(cfgPath, []byte(onDisk), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ANTIGRAVITY_CLIENT_ID", "env-only-client-id")
	t.Setenv("ANTIGRAVITY_CLIENT_SECRET", "env-only-client-secret")
	t.Setenv("ANTIGRAVITY_PORT", "9191")
	t.Setenv("ANTIGRAVITY_MODEL_PRIMARY", "env-primary")
	t.Setenv("ANTIGRAVITY_UPSTREAM_URL", "http://127.0.0.1:1")

	fakeBin := filepath.Join(home, "fake-antigravity")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallDesktop(DesktopOptions{
		SwitcherBin:    "/usr/bin/antigravity-account-switcher",
		AntigravityBin: fakeBin,
	}); err != nil {
		t.Fatalf("InstallDesktop: %v", err)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"env-only-client-id", "env-only-client-secret", "env-primary", "9191", "127.0.0.1:1"} {
		if strings.Contains(string(raw), leaked) {
			t.Errorf("config.json contains environment override %q:\n%s", leaked, raw)
		}
	}

	saved, err := config.LoadDiskConfig()
	if err != nil {
		t.Fatalf("LoadDiskConfig: %v", err)
	}
	if saved.AntigravityBin != fakeBin {
		t.Errorf("AntigravityBin = %q, want %q", saved.AntigravityBin, fakeBin)
	}
	if saved.Port != 7070 {
		t.Errorf("Port = %d, want the on-disk 7070", saved.Port)
	}
	if saved.ModelPrimary != "disk-primary" {
		t.Errorf("ModelPrimary = %q, want the on-disk %q", saved.ModelPrimary, "disk-primary")
	}
}
