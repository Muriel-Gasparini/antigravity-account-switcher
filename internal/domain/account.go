package domain

import (
	"context"
	"time"
)

// AccountStatus represents the operational status of a Google account in the pool.
type AccountStatus string

const (
	// AccountStatusActive denotes an account healthy, authenticated, and holding quota.
	AccountStatusActive AccountStatus = "active"
	// AccountStatusExhausted denotes an account that received HTTP 429 / RESOURCE_EXHAUSTED.
	AccountStatusExhausted AccountStatus = "exhausted"
	// AccountStatusError denotes an account with authentication failure (e.g. invalid refresh token).
	AccountStatusError AccountStatus = "error"
	// AccountStatusDisabled denotes an account manually disabled by the user in the UI.
	AccountStatusDisabled AccountStatus = "disabled"
)

// ModelCategory represents the top-level quota and provider tier for a model.
type ModelCategory string

const (
	// CategoryUnknown represents unrecognized model identifiers.
	CategoryUnknown ModelCategory = "unknown"
	// CategoryClaudeGPT covers Anthropic Claude and OpenAI GPT third-party models.
	CategoryClaudeGPT ModelCategory = "claude_gpt"
	// CategoryGemini covers Google Gemini and Gemma first-party models.
	CategoryGemini ModelCategory = "gemini"
)

// String returns the string representation of ModelCategory.
func (c ModelCategory) String() string {
	return string(c)
}

// Account represents a Google user account managed by the switcher.
type Account struct {
	ID           string        `json:"id"`
	Email        string        `json:"email"`
	RefreshToken string        `json:"-"` // Omit credentials from JSON serialization for security
	AccessToken  string        `json:"-"` // Omit credentials from JSON serialization for security
	TokenExpiry  time.Time     `json:"token_expiry"`
	IsActive     bool          `json:"is_active"`
	Status       AccountStatus `json:"status"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

// IsTokenExpired checks if the access token has expired or is within the safety margin.
// A safety margin (e.g. 60 seconds) prevents mid-flight token expiration.
func (a *Account) IsTokenExpired(safetyMargin time.Duration) bool {
	if a.AccessToken == "" || a.TokenExpiry.IsZero() {
		return true
	}
	return time.Now().Add(safetyMargin).After(a.TokenExpiry)
}

// IsAvailable determines whether the account is eligible for proxy routing.
func (a *Account) IsAvailable() bool {
	return a.Status == AccountStatusActive
}

// AccountRepository defines persistence operations for Account entities.
type AccountRepository interface {
	// Create stores a new account in the database.
	Create(ctx context.Context, acc *Account) error

	// GetByID retrieves an account by its unique identifier.
	GetByID(ctx context.Context, id string) (*Account, error)

	// GetByEmail retrieves an account by its email address.
	GetByEmail(ctx context.Context, email string) (*Account, error)

	// GetActive retrieves the currently selected active account.
	GetActive(ctx context.Context) (*Account, error)

	// List returns all accounts, ordered by creation date.
	List(ctx context.Context) ([]*Account, error)

	// SetActive atomically marks the specified account as active (is_active = 1)
	// and unsets all other accounts (is_active = 0).
	SetActive(ctx context.Context, id string) error

	// UpdateStatus updates the operational status of an account.
	UpdateStatus(ctx context.Context, id string, status AccountStatus) error

	// UpdateToken updates the access token and expiration timestamp.
	UpdateToken(ctx context.Context, id string, accessToken string, expiry time.Time) error

	// UpdateRefreshToken updates the long-lived refresh token.
	UpdateRefreshToken(ctx context.Context, id string, refreshToken string) error

	// Delete removes an account and cascades deletion to buckets and metrics.
	Delete(ctx context.Context, id string) error

	// GetNextAvailable selects the next eligible account for failover rotation,
	// excluding the specified account ID (e.g. the one that encountered 429).
	// Accounts are ordered by least recently used (updated_at ASC).
	GetNextAvailable(ctx context.Context, excludeID string) (*Account, error)

	// GetActiveForCategory retrieves the active account designated for a specific model category.
	GetActiveForCategory(ctx context.Context, category ModelCategory) (*Account, error)

	// SetActiveForCategory sets the active account pointer for a specific model category.
	SetActiveForCategory(ctx context.Context, category ModelCategory, accountID string) error

	// GetNextAvailableForCategory selects the next eligible account for a model category,
	// excluding the specified account ID and accounts whose quota for that category is exhausted.
	GetNextAvailableForCategory(ctx context.Context, category ModelCategory, excludeID string) (*Account, error)
}
