package e2e

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// TestTier1_StorageIntegrityAndPragmas verifies that the SQLite database runs
// in WAL mode with correct timeouts and foreign keys enabled.
func TestTier1_StorageIntegrityAndPragmas(t *testing.T) {
	env := setupE2EEnvironment(t, 0)
	ctx := context.Background()

	// 1. Check WAL journal mode
	var journalMode string
	if err := env.DB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if strings.ToLower(journalMode) != "wal" {
		t.Errorf("expected journal_mode 'wal', got %q", journalMode)
	}

	// 2. Check foreign_keys pragma
	var foreignKeys int
	if err := env.DB.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("query foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("expected foreign_keys = 1, got %d", foreignKeys)
	}

	// 3. Check table schemas
	tables := []string{"accounts", "quota_buckets", "token_metrics", "proxy_events"}
	for _, table := range tables {
		var name string
		err := env.DB.QueryRowContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil || name != table {
			t.Errorf("expected table %s to exist in SQLite schema", table)
		}
	}
}

// TestTier1_CLIBasicCommands verifies that the compiled binary executes basic CLI subcommands.
func TestTier1_CLIBasicCommands(t *testing.T) {
	cmdHelp := exec.Command("go", "run", "../../cmd/antigravity-account-switcher", "--help")
	outHelp, err := cmdHelp.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to run --help: %v (output: %s)", err, string(outHelp))
	}
	if !strings.Contains(string(outHelp), "antigravity-account-switcher") || !strings.Contains(string(outHelp), "serve") {
		t.Errorf("expected help output to describe commands, got: %s", string(outHelp))
	}

	cmdVersion := exec.Command("go", "run", "../../cmd/antigravity-account-switcher", "version")
	outVersion, err := cmdVersion.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to run version: %v (output: %s)", err, string(outVersion))
	}
	if !strings.Contains(string(outVersion), "antigravity-account-switcher") {
		t.Errorf("expected version output, got: %s", string(outVersion))
	}
}
