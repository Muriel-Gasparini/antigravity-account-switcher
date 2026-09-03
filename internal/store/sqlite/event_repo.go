package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

// EventRepository implements domain.EventRepository backed by SQLite.
type EventRepository struct {
	db *DB
}

// NewEventRepository creates a new SQLite EventRepository.
func NewEventRepository(db *DB) *EventRepository {
	return &EventRepository{db: db}
}

// Record inserts a new proxy event into proxy_events.
func (r *EventRepository) Record(ctx context.Context, event *domain.ProxyEvent) error {
	now := time.Now().UTC()
	if event.Timestamp.IsZero() {
		event.Timestamp = now
	}

	detailsStr := ""
	if len(event.Details) > 0 {
		if b, err := json.Marshal(event.Details); err == nil {
			detailsStr = string(b)
		}
	}

	query := `
		INSERT INTO proxy_events (event_type, account_id, message, details, created_at)
		VALUES (?, ?, ?, ?, ?)
	`
	res, err := r.db.ExecContext(ctx, query,
		string(event.Type),
		event.AccountID,
		event.Message,
		detailsStr,
		event.Timestamp.Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("failed to record proxy event: %w", err)
	}

	id, err := res.LastInsertId()
	if err == nil {
		event.ID = id
	}

	return nil
}

// ListRecent retrieves the most recent events up to limit.
func (r *EventRepository) ListRecent(ctx context.Context, limit int) ([]*domain.ProxyEvent, error) {
	if limit <= 0 {
		limit = 50
	}

	query := `
		SELECT id, event_type, COALESCE(account_id, ''), message, details, created_at
		FROM proxy_events
		ORDER BY created_at DESC, id DESC
		LIMIT ?
	`
	rows, err := r.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list proxy events: %w", err)
	}
	defer rows.Close()

	var events []*domain.ProxyEvent
	for rows.Next() {
		var ev domain.ProxyEvent
		var typeStr, detailsStr, createdStr string
		if err := rows.Scan(&ev.ID, &typeStr, &ev.AccountID, &ev.Message, &detailsStr, &createdStr); err != nil {
			return nil, fmt.Errorf("failed to scan proxy event: %w", err)
		}
		ev.Type = domain.EventType(typeStr)
		ev.Timestamp, _ = parseDBTime(createdStr)
		if detailsStr != "" {
			var details map[string]any
			if err := json.Unmarshal([]byte(detailsStr), &details); err == nil {
				ev.Details = details
			}
		}
		events = append(events, &ev)
	}

	return events, rows.Err()
}
