package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

// QuotaRepository implements domain.QuotaRepository backed by SQLite.
type QuotaRepository struct {
	db *DB
}

// NewQuotaRepository creates a new SQLite QuotaRepository.
func NewQuotaRepository(db *DB) *QuotaRepository {
	return &QuotaRepository{db: db}
}

// UpsertBuckets inserts or updates quota buckets atomically.
func (r *QuotaRepository) UpsertBuckets(ctx context.Context, buckets []*domain.QuotaBucket) error {
	if len(buckets) == 0 {
		return nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	query := `
		INSERT INTO quota_buckets (
			account_id, bucket_id, display_name, window,
			remaining_fraction, remaining_amount, reset_time, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, bucket_id) DO UPDATE SET
			display_name = excluded.display_name,
			window = excluded.window,
			remaining_fraction = excluded.remaining_fraction,
			remaining_amount = excluded.remaining_amount,
			reset_time = excluded.reset_time,
			updated_at = excluded.updated_at
	`
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to prepare upsert statement: %w", err)
	}
	defer stmt.Close()

	now := time.Now().UTC()
	for _, b := range buckets {
		updatedAt := b.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = now
		}

		_, err := stmt.ExecContext(ctx,
			b.AccountID,
			b.BucketID,
			b.DisplayName,
			string(b.Window),
			b.RemainingFraction,
			b.RemainingAmount,
			b.ResetTime.Format(time.RFC3339),
			updatedAt.Format(time.RFC3339),
		)
		if err != nil {
			return fmt.Errorf("failed to upsert bucket %s/%s: %w", b.AccountID, b.BucketID, err)
		}
	}

	return tx.Commit()
}

// GetByAccountID retrieves all quota buckets for a specific account.
func (r *QuotaRepository) GetByAccountID(ctx context.Context, accountID string) ([]*domain.QuotaBucket, error) {
	query := `
		SELECT account_id, bucket_id, display_name, window, remaining_fraction, remaining_amount, reset_time, updated_at
		FROM quota_buckets
		WHERE account_id = ?
		ORDER BY bucket_id ASC
	`
	rows, err := r.db.QueryContext(ctx, query, accountID)
	if err != nil {
		return nil, fmt.Errorf("failed to query quota buckets: %w", err)
	}
	defer rows.Close()

	var buckets []*domain.QuotaBucket
	for rows.Next() {
		b, err := r.scanBucket(rows)
		if err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
	}

	return buckets, rows.Err()
}

// ListAll retrieves all quota buckets across all accounts grouped by account ID.
func (r *QuotaRepository) ListAll(ctx context.Context) (map[string][]*domain.QuotaBucket, error) {
	query := `
		SELECT account_id, bucket_id, display_name, window, remaining_fraction, remaining_amount, reset_time, updated_at
		FROM quota_buckets
		ORDER BY account_id ASC, bucket_id ASC
	`
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to list all quota buckets: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]*domain.QuotaBucket)
	for rows.Next() {
		b, err := r.scanBucket(rows)
		if err != nil {
			return nil, err
		}
		result[b.AccountID] = append(result[b.AccountID], b)
	}

	return result, rows.Err()
}

// DeleteByAccountID removes all quota buckets associated with an account.
func (r *QuotaRepository) DeleteByAccountID(ctx context.Context, accountID string) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM quota_buckets WHERE account_id = ?", accountID)
	if err != nil {
		return fmt.Errorf("failed to delete quota buckets: %w", err)
	}
	return nil
}

// GetExhaustedAccountsPastReset returns account IDs marked 'exhausted' where all quota buckets
// have passed their reset time.
func (r *QuotaRepository) GetExhaustedAccountsPastReset(ctx context.Context, now time.Time) ([]string, error) {
	nowStr := now.UTC().Format(time.RFC3339)
	query := `
		SELECT a.id
		FROM accounts a
		WHERE a.status = 'exhausted'
		  AND EXISTS (SELECT 1 FROM quota_buckets qb WHERE qb.account_id = a.id)
		  AND NOT EXISTS (
			  SELECT 1 FROM quota_buckets qb
			  WHERE qb.account_id = a.id AND qb.reset_time > ?
		  )
		ORDER BY a.updated_at ASC
	`
	rows, err := r.db.QueryContext(ctx, query, nowStr)
	if err != nil {
		return nil, fmt.Errorf("failed to query reset accounts: %w", err)
	}
	defer rows.Close()

	var accountIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan reset account id: %w", err)
		}
		accountIDs = append(accountIDs, id)
	}

	return accountIDs, rows.Err()
}

func (r *QuotaRepository) scanBucket(scanner rowScanner) (*domain.QuotaBucket, error) {
	var b domain.QuotaBucket
	var windowStr, resetStr, updatedStr string

	err := scanner.Scan(
		&b.AccountID,
		&b.BucketID,
		&b.DisplayName,
		&windowStr,
		&b.RemainingFraction,
		&b.RemainingAmount,
		&resetStr,
		&updatedStr,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to scan quota bucket: %w", err)
	}

	b.Window = domain.QuotaWindow(windowStr)
	b.ResetTime, _ = parseDBTime(resetStr)
	b.UpdatedAt, _ = parseDBTime(updatedStr)

	return &b, nil
}
