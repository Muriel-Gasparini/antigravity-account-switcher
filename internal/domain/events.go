package domain

import (
	"context"
	"time"
)

// EventType represents the category of operational proxy events.
type EventType string

const (
	// EventTypeAccountSwitched denotes an intentional or failover switch of the active account.
	EventTypeAccountSwitched EventType = "account_switched"
	// EventTypeFailover429 denotes a failover triggered by an HTTP 429 response.
	EventTypeFailover429 EventType = "failover_429"
	// EventTypeModelFallback denotes an intra-account request payload rewrite to a secondary model tier.
	EventTypeModelFallback EventType = "model_fallback"
	// EventTypeQuotaExhausted denotes an account whose quota was marked exhausted.
	EventTypeQuotaExhausted EventType = "quota_exhausted"
	// EventTypeQuotaRestored denotes an account whose quota was restored after reset.
	EventTypeQuotaRestored EventType = "quota_restored"
	// EventTypeTokenRefreshed denotes a successful OAuth2 access token refresh.
	EventTypeTokenRefreshed EventType = "token_refreshed"
	// EventTypeRequestSuccess denotes a successfully proxied request.
	EventTypeRequestSuccess EventType = "request_success"
	// EventTypeTokensCaptured denotes token usage metadata successfully captured from SSE stream.
	EventTypeTokensCaptured EventType = "tokens_captured"
	// EventTypeError denotes a proxy or system operational error.
	EventTypeError EventType = "error"
)

// ProxyEvent records an operational event emitted by the proxy or background daemons.
type ProxyEvent struct {
	ID        int64          `json:"id"`
	Type      EventType      `json:"type"`
	AccountID string         `json:"account_id,omitempty"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
}

// EventRepository defines persistence and retrieval for historical proxy events.
type EventRepository interface {
	// Record persists a proxy event.
	Record(ctx context.Context, event *ProxyEvent) error

	// ListRecent returns the most recent N events ordered by timestamp DESC.
	ListRecent(ctx context.Context, limit int) ([]*ProxyEvent, error)
}

// EventBroadcaster defines real-time event publishing and subscriber multiplexing.
type EventBroadcaster interface {
	// Broadcast delivers an event to all active subscribers.
	Broadcast(event *ProxyEvent)

	// Subscribe creates a new subscription channel and returns an unsubscribe cleanup func.
	Subscribe() (<-chan *ProxyEvent, func())
}
