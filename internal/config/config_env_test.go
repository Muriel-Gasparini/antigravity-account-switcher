package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig_IgnoresDBPathEnv(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("ANTIGRAVITY_DB_PATH", filepath.Join(tmpDir, "from-env.db"))

	want := filepath.Join(tmpDir, DefaultDBFileName)
	if got := DefaultConfig().DBPath; got != want {
		t.Errorf("DefaultConfig().DBPath = %q, want env-free default %q", got, want)
	}
}

func TestLoadDiskConfig_SaveDoesNotPersistDBPathEnv(t *testing.T) {
	envDB := "/tmp/should-not-be-persisted.db"

	cases := []struct {
		name     string
		onDisk   string // initial config.json content, empty means no file
		wantPath func(dir string) string
	}{
		{
			name:     "no config file",
			wantPath: func(dir string) string { return filepath.Join(dir, DefaultDBFileName) },
		},
		{
			name:     "config file without db_path",
			onDisk:   `{"port": 9191}`,
			wantPath: func(dir string) string { return filepath.Join(dir, DefaultDBFileName) },
		},
		{
			name:     "config file with db_path",
			onDisk:   `{"port": 9191, "db_path": "/data/disk.db"}`,
			wantPath: func(string) string { return "/data/disk.db" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
			t.Setenv("ANTIGRAVITY_DB_PATH", envDB)
			if tc.onDisk != "" {
				if err := os.WriteFile(ConfigFilePath(), []byte(tc.onDisk), 0o600); err != nil {
					t.Fatalf("seed config: %v", err)
				}
			}

			cfg, err := LoadDiskConfig()
			if err != nil {
				t.Fatalf("LoadDiskConfig: %v", err)
			}
			if err := Save(cfg); err != nil {
				t.Fatalf("Save: %v", err)
			}

			raw, err := os.ReadFile(ConfigFilePath())
			if err != nil {
				t.Fatalf("read config.json: %v", err)
			}
			var saved Config
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatalf("parse config.json: %v", err)
			}
			if saved.DBPath == envDB {
				t.Fatalf("config.json persisted ANTIGRAVITY_DB_PATH value %q", envDB)
			}
			if want := tc.wantPath(tmpDir); saved.DBPath != want {
				t.Errorf("saved db_path = %q, want %q", saved.DBPath, want)
			}
		})
	}
}

func TestLoad_AppliesDBPathEnv(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	if err := os.WriteFile(ConfigFilePath(), []byte(`{"db_path": "/data/disk.db"}`), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	t.Setenv("ANTIGRAVITY_DB_PATH", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DBPath != "/data/disk.db" {
		t.Errorf("without env, Load().DBPath = %q, want disk value", cfg.DBPath)
	}

	t.Setenv("ANTIGRAVITY_DB_PATH", "/tmp/env.db")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DBPath != "/tmp/env.db" {
		t.Errorf("with env, Load().DBPath = %q, want env value", cfg.DBPath)
	}
}
