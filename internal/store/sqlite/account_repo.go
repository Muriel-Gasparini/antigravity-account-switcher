package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/google/uuid"
)

// AccountRepository implements domain.AccountRepository backed by SQLite.
type AccountRepository struct {
	db *DB
}

// NewAccountRepository creates a new SQLite AccountRepository.
func NewAccountRepository(db *DB) *AccountRepository {
	return &AccountRepository{db: db}
}

// Create inserts a new account record into SQLite.
func (r *AccountRepository) Create(ctx context.Context, acc *domain.Account) error {
	if acc.ID == "" {
		acc.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	if acc.CreatedAt.IsZero() {
		acc.CreatedAt = now
	}
	if acc.UpdatedAt.IsZero() {
		acc.UpdatedAt = now
	}
	if acc.Status == "" {
		acc.Status = domain.AccountStatusActive
	}

	expiryStr := acc.TokenExpiry.Format(time.RFC3339)
	if acc.TokenExpiry.IsZero() {
		expiryStr = "1970-01-01T00:00:00Z"
	}

	query := `
		INSERT INTO accounts (
			id, email, refresh_token, access_token, token_expiry,
			is_active, status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := r.db.ExecContext(ctx, query,
		acc.ID,
		acc.Email,
		acc.RefreshToken,
		acc.AccessToken,
		expiryStr,
		acc.IsActive,
		string(acc.Status),
		acc.CreatedAt.Format(time.RFC3339),
		acc.UpdatedAt.Format(time.RFC3339),
	)
	if err != nil {
		if strings.Contains(err.Error(), "accounts.email") && strings.Contains(err.Error(), "UNIQUE") {
			return domain.ErrAccountEmailExists
		}
		return fmt.Errorf("failed to create account: %w", err)
	}

	return nil
}

// GetByID retrieves an account by ID.
func (r *AccountRepository) GetByID(ctx context.Context, id string) (*domain.Account, error) {
	query := `
		SELECT id, email, refresh_token, access_token, token_expiry, is_active, status, created_at, updated_at
		FROM accounts
		WHERE id = ?
	`
	row := r.db.QueryRowContext(ctx, query, id)
	return r.scanAccount(row)
}

// GetByEmail retrieves an account by email.
func (r *AccountRepository) GetByEmail(ctx context.Context, email string) (*domain.Account, error) {
	query := `
		SELECT id, email, refresh_token, access_token, token_expiry, is_active, status, created_at, updated_at
		FROM accounts
		WHERE email = ?
	`
	row := r.db.QueryRowContext(ctx, query, email)
	return r.scanAccount(row)
}

// GetActive retrieves the single currently active account.
func (r *AccountRepository) GetActive(ctx context.Context) (*domain.Account, error) {
	query := `
		SELECT id, email, refresh_token, access_token, token_expiry, is_active, status, created_at, updated_at
		FROM accounts
		WHERE is_active = 1
		LIMIT 1
	`
	row := r.db.QueryRowContext(ctx, query)
	acc, err := r.scanAccount(row)
	if errors.Is(err, domain.ErrAccountNotFound) {
		return nil, domain.ErrNoActiveAccount
	}
	return acc, err
}

// List returns all accounts in the pool ordered by creation date.
func (r *AccountRepository) List(ctx context.Context) ([]*domain.Account, error) {
	query := `
		SELECT id, email, refresh_token, access_token, token_expiry, is_active, status, created_at, updated_at
		FROM accounts
		ORDER BY created_at ASC
	`
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to list accounts: %w", err)
	}
	defer rows.Close()

	var accounts []*domain.Account
	for rows.Next() {
		acc, err := r.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, acc)
	}

	return accounts, rows.Err()
}

// SetActive atomically sets the designated account active and clears active status from all others.
func (r *AccountRepository) SetActive(ctx context.Context, id string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	nowStr := time.Now().UTC().Format(time.RFC3339)

	// Step 1: Deactivate currently active account
	if _, err := tx.ExecContext(ctx, "UPDATE accounts SET is_active = 0, updated_at = ? WHERE is_active = 1", nowStr); err != nil {
		return fmt.Errorf("failed to deactivate current active account: %w", err)
	}

	// Step 2: Activate target account and mark status as active
	res, err := tx.ExecContext(ctx, "UPDATE accounts SET is_active = 1, status = 'active', updated_at = ? WHERE id = ?", nowStr, id)
	if err != nil {
		return fmt.Errorf("failed to activate target account %s: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return domain.ErrAccountNotFound
	}

	return tx.Commit()
}

// UpdateStatus modifies the operational status of an account.
func (r *AccountRepository) UpdateStatus(ctx context.Context, id string, status domain.AccountStatus) error {
	nowStr := time.Now().UTC().Format(time.RFC3339)
	res, err := r.db.ExecContext(ctx, "UPDATE accounts SET status = ?, updated_at = ? WHERE id = ?", string(status), nowStr, id)
	if err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return domain.ErrAccountNotFound
	}

	return nil
}

// UpdateToken updates the access token and expiry time for an account.
func (r *AccountRepository) UpdateToken(ctx context.Context, id string, accessToken string, expiry time.Time) error {
	nowStr := time.Now().UTC().Format(time.RFC3339)
	expiryStr := expiry.Format(time.RFC3339)
	res, err := r.db.ExecContext(ctx, `
		UPDATE accounts
		SET access_token = ?, token_expiry = ?, updated_at = ?
		WHERE id = ?
	`, accessToken, expiryStr, nowStr, id)
	if err != nil {
		return fmt.Errorf("failed to update token: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return domain.ErrAccountNotFound
	}

	return nil
}

// UpdateRefreshToken updates the stored OAuth2 refresh token for an account.
func (r *AccountRepository) UpdateRefreshToken(ctx context.Context, id string, refreshToken string) error {
	nowStr := time.Now().UTC().Format(time.RFC3339)
	res, err := r.db.ExecContext(ctx, "UPDATE accounts SET refresh_token = ?, updated_at = ? WHERE id = ?", refreshToken, nowStr, id)
	if err != nil {
		return fmt.Errorf("failed to update refresh token: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return domain.ErrAccountNotFound
	}

	return nil
}

// Delete removes an account and cascades deletions to related buckets and metrics.
func (r *AccountRepository) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, "DELETE FROM accounts WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("failed to delete account: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return domain.ErrAccountNotFound
	}

	return nil
}

// GetNextAvailable returns the least-recently updated account in 'active' status,
// excluding the given account ID (for failover rotation).
func (r *AccountRepository) GetNextAvailable(ctx context.Context, excludeID string) (*domain.Account, error) {
	query := `
		SELECT id, email, refresh_token, access_token, token_expiry, is_active, status, created_at, updated_at
		FROM accounts
		WHERE status = 'active'
		  AND (? = '' OR id != ?)
		ORDER BY updated_at ASC
		LIMIT 1
	`
	row := r.db.QueryRowContext(ctx, query, excludeID, excludeID)
	acc, err := r.scanAccount(row)
	if errors.Is(err, domain.ErrAccountNotFound) {
		return nil, domain.ErrNoAvailableAccount
	}
	return acc, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (r *AccountRepository) scanAccount(scanner rowScanner) (*domain.Account, error) {
	var acc domain.Account
	var statusStr, expiryStr, createdStr, updatedStr string
	var isActiveInt int

	err := scanner.Scan(
		&acc.ID,
		&acc.Email,
		&acc.RefreshToken,
		&acc.AccessToken,
		&expiryStr,
		&isActiveInt,
		&statusStr,
		&createdStr,
		&updatedStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrAccountNotFound
		}
		return nil, fmt.Errorf("failed to scan account: %w", err)
	}

	acc.IsActive = isActiveInt == 1
	acc.Status = domain.AccountStatus(statusStr)
	acc.TokenExpiry, _ = parseDBTime(expiryStr)
	acc.CreatedAt, _ = parseDBTime(createdStr)
	acc.UpdatedAt, _ = parseDBTime(updatedStr)

	return &acc, nil
}
