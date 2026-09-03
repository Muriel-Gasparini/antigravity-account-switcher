package config

import (
	"os"
	"path/filepath"
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
}

func TestSaveAndLoadConfig(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)

	cfg := &Config{
		Port:           9999,
		DBPath:         filepath.Join(tmpDir, "custom.db"),
		AntigravityBin: "/custom/bin/antigravity",
		UpstreamURL:    "https://custom-upstream.example.com",
		QuotaInterval:  "30s",
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

	// Place icon in parent folder (e.g. ~/tools/Antigravity/icon.png)
	iconPath := filepath.Join(tmpDir, "icon.png")
	if err := os.WriteFile(iconPath, []byte("fake icon png"), 0o644); err != nil {
		t.Fatal(err)
	}

	found := FindAntigravityIcon(binPath)
	if found != iconPath {
		t.Errorf("expected found icon %s, got %s", iconPath, found)
	}
}
