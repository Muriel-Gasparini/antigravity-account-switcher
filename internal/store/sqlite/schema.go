package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// Migration represents a versioned DDL change.
type Migration struct {
	Version     int
	Description string
	SQL         string
}

// Migrations contains all migration steps in sequential order.
var Migrations = []Migration{
	{
		Version:     1,
		Description: "initial_schema",
		SQL: `
-- 1. Migration tracking
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    description TEXT NOT NULL,
    applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- 2. Accounts table
CREATE TABLE IF NOT EXISTS accounts (
    id TEXT PRIMARY KEY,
    email TEXT UNIQUE NOT NULL,
    refresh_token TEXT NOT NULL,
    access_token TEXT NOT NULL DEFAULT '',
    token_expiry DATETIME NOT NULL DEFAULT '1970-01-01 00:00:00',
    is_active INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'active',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Partial unique index: enforces at most ONE account with is_active = 1
CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_single_active 
ON accounts(is_active) WHERE is_active = 1;

CREATE INDEX IF NOT EXISTS idx_accounts_status ON accounts(status);
CREATE INDEX IF NOT EXISTS idx_accounts_status_updated ON accounts(status, updated_at);

-- 3. Quota buckets table
CREATE TABLE IF NOT EXISTS quota_buckets (
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    bucket_id TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    window TEXT NOT NULL,
    remaining_fraction REAL NOT NULL DEFAULT 1.0,
    remaining_amount INTEGER NOT NULL DEFAULT 0,
    reset_time DATETIME NOT NULL,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (account_id, bucket_id)
);

CREATE INDEX IF NOT EXISTS idx_quota_buckets_account ON quota_buckets(account_id);
CREATE INDEX IF NOT EXISTS idx_quota_buckets_reset_time ON quota_buckets(reset_time);

-- 4. Token metrics table
CREATE TABLE IF NOT EXISTS token_metrics (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    request_path TEXT NOT NULL DEFAULT '',
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    candidates_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0,
    cached_content_tokens INTEGER NOT NULL DEFAULT 0,
    thoughts_tokens INTEGER NOT NULL DEFAULT 0,
    timestamp DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_token_metrics_account_time ON token_metrics(account_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_token_metrics_timestamp ON token_metrics(timestamp);

-- 5. Proxy events table
CREATE TABLE IF NOT EXISTS proxy_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_type TEXT NOT NULL,
    account_id TEXT REFERENCES accounts(id) ON DELETE SET NULL,
    message TEXT NOT NULL,
    details TEXT DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_proxy_events_created_at ON proxy_events(created_at);
`,
	},
}

// Migrate applies all pending schema migrations to the database.
func Migrate(db *DB) error {
	ctx := context.Background()

	// Ensure schema_migrations exists first
	initSQL := `
	CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		description TEXT NOT NULL,
		applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);`
	if _, err := db.ExecContext(ctx, initSQL); err != nil {
		return fmt.Errorf("failed to create schema_migrations table: %w", err)
	}

	for _, m := range Migrations {
		var count int
		err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = ?", m.Version).Scan(&count)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("failed to query migration version %d: %w", m.Version, err)
		}
		if count > 0 {
			continue // Already applied
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to begin tx for migration %d: %w", m.Version, err)
		}

		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("failed to execute migration %d (%s): %w", m.Version, m.Description, err)
		}

		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, description) VALUES (?, ?)", m.Version, m.Description); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("failed to record migration %d: %w", m.Version, err)
		}

		if err := tx.Commit(); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("failed to commit migration %d: %w", m.Version, err)
		}
	}

	return nil
}
