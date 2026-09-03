package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
)

func captureStdout(f func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	f()

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

func TestCLI_PrintUsage(t *testing.T) {
	out := captureStdout(func() {
		printUsage()
	})

	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage output to contain 'Usage:', got: %s", out)
	}
	if !strings.Contains(out, "serve") || !strings.Contains(out, "wrap") {
		t.Errorf("expected usage output to describe subcommands, got: %s", out)
	}
}

func TestCLI_RunVersion(t *testing.T) {
	out := captureStdout(func() {
		runVersion()
	})

	if !strings.Contains(out, "antigravity-account-switcher") {
		t.Errorf("expected version output, got: %s", out)
	}
}

func TestCLI_DefaultDBPath(t *testing.T) {
	orig := os.Getenv("ANTIGRAVITY_DB_PATH")
	defer os.Setenv("ANTIGRAVITY_DB_PATH", orig)

	os.Setenv("ANTIGRAVITY_DB_PATH", "/tmp/custom-test.db")
	if defaultDBPath() != "/tmp/custom-test.db" {
		t.Errorf("expected /tmp/custom-test.db, got %s", defaultDBPath())
	}
}

func TestCLI_RunStatus_EmptyDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "status_test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	_ = db.Close()

	out := captureStdout(func() {
		runStatus([]string{"-db", dbPath})
	})

	if !strings.Contains(out, "Switcher Status: OK") {
		t.Errorf("expected status OK, got: %s", out)
	}
	if !strings.Contains(out, "None selected") {
		t.Errorf("expected no active account message, got: %s", out)
	}
}

func TestCLI_RunListAccounts_Empty(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "list_test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	_ = db.Close()

	out := captureStdout(func() {
		runListAccounts([]string{"-db", dbPath})
	})

	if !strings.Contains(out, "No accounts registered yet") {
		t.Errorf("expected no accounts message, got: %s", out)
	}
}

func TestCLI_RunConfig(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)

	// Test config list
	outList := captureStdout(func() {
		runConfig([]string{"list"})
	})
	if !strings.Contains(outList, "Configuration file:") {
		t.Errorf("expected config list output, got: %s", outList)
	}

	// Test config set
	outSet := captureStdout(func() {
		runConfig([]string{"set", "port", "9099"})
	})
	if !strings.Contains(outSet, "Updated 'port' to '9099'") {
		t.Errorf("expected config set confirmation, got: %s", outSet)
	}

	// Test config get
	outGet := captureStdout(func() {
		runConfig([]string{"get", "port"})
	})
	if !strings.Contains(outGet, "9099") {
		t.Errorf("expected 9099 from get port, got: %s", outGet)
	}
}

func TestCLI_RunInstallDesktop(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	fakeBin := filepath.Join(tmpDir, "fake-antigravity")
	_ = os.WriteFile(fakeBin, []byte("#!/bin/sh\n"), 0o755)

	out := captureStdout(func() {
		runInstallDesktop([]string{"-bin", fakeBin})
	})

	if !strings.Contains(out, "Success! Antigravity desktop application installed") {
		t.Errorf("expected installation success, got: %s", out)
	}
}
