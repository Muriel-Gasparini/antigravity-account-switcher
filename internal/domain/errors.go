package domain

import "errors"

var (
	// ErrAccountNotFound indicates no account matches the given query criteria.
	ErrAccountNotFound = errors.New("account not found")

	// ErrAccountEmailExists indicates an account with the specified email already exists.
	ErrAccountEmailExists = errors.New("account with this email already exists")

	// ErrNoActiveAccount indicates no account is currently flagged as active.
	ErrNoActiveAccount = errors.New("no active account configured")

	// ErrNoAvailableAccount indicates all accounts in the pool are exhausted or in error state.
	ErrNoAvailableAccount = errors.New("no available account with remaining quota in pool")

	// ErrNoAvailableAccounts is an alias for ErrNoAvailableAccount.
	ErrNoAvailableAccounts = ErrNoAvailableAccount

	// ErrAccountExhausted indicates the requested account has depleted its quota.
	ErrAccountExhausted = errors.New("account quota is exhausted")

	// ErrTokenExpired indicates an account access token is expired and refresh failed.
	ErrTokenExpired = errors.New("account token has expired")

	// ErrInvalidRefreshToken indicates Google rejected the refresh token.
	ErrInvalidRefreshToken = errors.New("invalid or revoked refresh token")
)
