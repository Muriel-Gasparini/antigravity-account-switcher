package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
)

func TestOpen_FileDB_Pragmas(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// 1. Verify WAL journal mode
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("expected journal_mode 'wal', got %q", journalMode)
	}

	// 2. Verify Foreign Keys enabled
	var foreignKeys int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("query foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("expected foreign_keys 1, got %d", foreignKeys)
	}

	// 3. Verify Busy Timeout is 5000ms
	var busyTimeout int
	if err := db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("expected busy_timeout 5000, got %d", busyTimeout)
	}

	// 4. Verify Migrations applied
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("query schema_migrations count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 applied migration, got %d", count)
	}

	// 5. Verify MaxOpenConns is 1
	stats := db.Stats()
	if stats.MaxOpenConnections != 1 {
		t.Errorf("expected MaxOpenConnections 1, got %d", stats.MaxOpenConnections)
	}
}

func TestOpen_MemoryDB(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open(:memory:) failed: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	var foreignKeys int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("query foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("expected foreign_keys 1 in memory db, got %d", foreignKeys)
	}
}

func TestOpenDB_Adapter(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "adapter.db")
	rawDB, err := sqlite.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("sqlite.OpenDB failed: %v", err)
	}
	defer rawDB.Close()

	if err := rawDB.Ping(); err != nil {
		t.Fatalf("ping failed: %v", err)
	}
}
