package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps the standard *sql.DB connection pool with SQLite-specific configurations.
type DB struct {
	*sql.DB
}

// BuildDSN constructs an optimized SQLite DSN URI with WAL mode, busy timeout, and immediate locking.
func BuildDSN(dbPath string) string {
	if dbPath == ":memory:" || strings.HasPrefix(dbPath, "file::memory:") {
		return "file::memory:?cache=shared&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_txlock=immediate"
	}
	return fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=cache_size(-20000)&_txlock=immediate",
		dbPath,
	)
}

// Open initializes a SQLite connection pool with WAL mode, busy timeout, and auto-migration.
func Open(dbPath string) (*DB, error) {
	if !isMemoryPath(dbPath) {
		if err := prepareDBFile(dbPath); err != nil {
			return nil, err
		}
	}

	dsn := BuildDSN(dbPath)
	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// Single connection pool guarantees zero lock contention and serialized writes,
	// eliminating SQLITE_BUSY_SNAPSHOT (517) deadlocks.
	rawDB.SetMaxOpenConns(1)
	rawDB.SetMaxIdleConns(1)
	rawDB.SetConnMaxLifetime(0) // Reuse indefinitely

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rawDB.PingContext(ctx); err != nil {
		rawDB.Close()
		return nil, fmt.Errorf("failed to ping sqlite database: %w", err)
	}

	db := &DB{DB: rawDB}

	// Run migrations automatically
	if err := Migrate(db); err != nil {
		rawDB.Close()
		return nil, fmt.Errorf("failed to apply migrations: %w", err)
	}

	return db, nil
}

// isMemoryPath reports whether dbPath names an in-memory database, which needs no file on disk.
func isMemoryPath(dbPath string) bool {
	if dbPath == "" || dbPath == ":memory:" || strings.HasPrefix(dbPath, "file::memory:") {
		return true
	}
	return strings.HasPrefix(dbPath, "file:") && strings.Contains(dbPath, "mode=memory")
}

// prepareDBFile creates the parent directory (0700) and the database file (0600) when missing,
// and tightens the mode of an existing file. The file holds OAuth tokens, so it must not depend
// on the process umask. A pre-existing parent directory is left alone because it may be
// user-supplied. SQLite gives -wal and -shm files the same mode as the main database file.
func prepareDBFile(dbPath string) error {
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("failed to create db directory: %w", err)
		}
	}

	// Create the file before SQLite opens it so it never exists with the umask-derived mode.
	f, err := os.OpenFile(dbPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("failed to create db file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close db file: %w", err)
	}

	// Tighten databases created by earlier versions. On Windows Chmod only toggles the
	// read-only bit, and 0600 keeps it clear, so this is harmless there.
	if err := os.Chmod(dbPath, 0o600); err != nil {
		return fmt.Errorf("failed to set db file permissions: %w", err)
	}
	return nil
}

// OpenDB is an adapter that returns the standard *sql.DB handle.
func OpenDB(dbPath string) (*sql.DB, error) {
	db, err := Open(dbPath)
	if err != nil {
		return nil, err
	}
	return db.DB, nil
}

// parseDBTime parses timestamps from SQLite which may be formatted in various standard layouts.
func parseDBTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}

	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02",
	}

	for _, format := range formats {
		if t, err := time.Parse(format, s); err == nil {
			return t.UTC(), nil
		}
	}

	return time.Time{}, fmt.Errorf("cannot parse datetime %q", s)
}
