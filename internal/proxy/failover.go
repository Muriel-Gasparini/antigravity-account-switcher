package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

// IsExhaustionResponse checks whether an HTTP status code and response body
// indicate quota exhaustion (HTTP 429 Too Many Requests or HTTP 403 RESOURCE_EXHAUSTED).
func IsExhaustionResponse(statusCode int, body []byte) bool {
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	if statusCode == http.StatusForbidden && len(body) > 0 {
		return bytes.Contains(body, []byte("RESOURCE_EXHAUSTED")) ||
			bytes.Contains(body, []byte("RATE_LIMIT_EXCEEDED")) ||
			bytes.Contains(body, []byte("QuotaFailure"))
	}
	return false
}

// FailoverEngine coordinates quota exhaustion detection, account status transitions,
// and atomic active account failover rotation with anti-stampede concurrency protection.
type FailoverEngine struct {
	accountRepo      domain.AccountRepository
	eventBroadcaster domain.EventBroadcaster
	eventRepo        domain.EventRepository
	mu               sync.Mutex
}

// NewFailoverEngine constructs a new FailoverEngine.
func NewFailoverEngine(
	accountRepo domain.AccountRepository,
	eventBroadcaster domain.EventBroadcaster,
	eventRepo domain.EventRepository,
) *FailoverEngine {
	return &FailoverEngine{
		accountRepo:      accountRepo,
		eventBroadcaster: eventBroadcaster,
		eventRepo:        eventRepo,
	}
}

// RotateAccount handles failover for an account that received HTTP 429 / RESOURCE_EXHAUSTED.
// It acquires an internal mutex to serialize failover operations, marks the failed account
// as exhausted, queries SQLite for the next available healthy account, and sets it as active.
// Under concurrent requests, if another goroutine already completed rotation away from exhaustedAcc,
// this method immediately returns the newly activated account without cascading re-rotations.
func (f *FailoverEngine) RotateAccount(ctx context.Context, exhaustedAcc *domain.Account) (*domain.Account, error) {
	if exhaustedAcc == nil {
		return nil, domain.ErrAccountNotFound
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	// 1. Anti-stampede check: Did another concurrent request already rotate away from exhaustedAcc?
	currentActive, err := f.accountRepo.GetActive(ctx)
	if err == nil && currentActive != nil && currentActive.ID != exhaustedAcc.ID && currentActive.IsAvailable() {
		return currentActive, nil
	}

	// 2. Mark the exhausted account in the repository
	if err := f.accountRepo.UpdateStatus(ctx, exhaustedAcc.ID, domain.AccountStatusExhausted); err != nil {
		// Log or record error event if needed, but continue rotation attempt
		f.emitEvent(&domain.ProxyEvent{
			Type:      domain.EventTypeError,
			AccountID: exhaustedAcc.ID,
			Message:   fmt.Sprintf("Failed to update status for exhausted account %s: %v", exhaustedAcc.Email, err),
			Timestamp: time.Now().UTC(),
		})
	}

	// Broadcast EventTypeFailover429
	f.emitEvent(&domain.ProxyEvent{
		Type:      domain.EventTypeFailover429,
		AccountID: exhaustedAcc.ID,
		Message:   fmt.Sprintf("Account %s (%s) marked exhausted due to HTTP 429 / RESOURCE_EXHAUSTED", exhaustedAcc.Email, exhaustedAcc.ID),
		Details: map[string]any{
			"account_id": exhaustedAcc.ID,
			"email":      exhaustedAcc.Email,
			"reason":     "RESOURCE_EXHAUSTED",
		},
		Timestamp: time.Now().UTC(),
	})

	// 3. Find next available account
	nextAcc, err := f.accountRepo.GetNextAvailable(ctx, exhaustedAcc.ID)
	if err != nil || nextAcc == nil {
		// Pool is completely exhausted
		f.emitEvent(&domain.ProxyEvent{
			Type:      domain.EventTypeQuotaExhausted,
			AccountID: exhaustedAcc.ID,
			Message:   "All accounts in the pool are exhausted",
			Details: map[string]any{
				"last_account_id": exhaustedAcc.ID,
				"last_email":      exhaustedAcc.Email,
			},
			Timestamp: time.Now().UTC(),
		})
		return nil, domain.ErrNoAvailableAccount
	}

	// 4. Set next account active atomically
	if err := f.accountRepo.SetActive(ctx, nextAcc.ID); err != nil {
		return nil, fmt.Errorf("failed to set active account to %s: %w", nextAcc.ID, err)
	}

	// Broadcast EventTypeAccountSwitched
	f.emitEvent(&domain.ProxyEvent{
		Type:      domain.EventTypeAccountSwitched,
		AccountID: nextAcc.ID,
		Message:   fmt.Sprintf("Rotated active account from %s to %s", exhaustedAcc.Email, nextAcc.Email),
		Details: map[string]any{
			"from_account_id": exhaustedAcc.ID,
			"from_email":      exhaustedAcc.Email,
			"to_account_id":   nextAcc.ID,
			"to_email":        nextAcc.Email,
		},
		Timestamp: time.Now().UTC(),
	})

	return nextAcc, nil
}

func (f *FailoverEngine) emitEvent(event *domain.ProxyEvent) {
	if f.eventBroadcaster != nil {
		f.eventBroadcaster.Broadcast(event)
	}
	if f.eventRepo != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = f.eventRepo.Record(ctx, event)
	}
}
