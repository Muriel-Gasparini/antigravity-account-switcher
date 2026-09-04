package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEventTypeModelFallback(t *testing.T) {
	t.Parallel()

	if EventTypeModelFallback != "model_fallback" {
		t.Errorf("expected EventTypeModelFallback to be %q, got %q", "model_fallback", EventTypeModelFallback)
	}
}

func TestProxyEvent_ModelFallback_Serialization(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	event := &ProxyEvent{
		ID:        42,
		Type:      EventTypeModelFallback,
		AccountID: "acc-123",
		Message:   "Rewrote request from gemini-2.5-pro to gemini-2.5-flash due to predictive quota exhaustion",
		Details: map[string]any{
			"account_id": "acc-123",
			"email":      "user@example.com",
			"from_model": "gemini-2.5-pro",
			"to_model":   "gemini-2.5-flash",
			"trigger":    "predictive_quota_exhausted",
		},
		Timestamp: now,
	}

	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("failed to marshal ProxyEvent: %v", err)
	}

	var unmarshaled ProxyEvent
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("failed to unmarshal ProxyEvent: %v", err)
	}

	if unmarshaled.Type != EventTypeModelFallback {
		t.Errorf("expected event type %s, got %s", EventTypeModelFallback, unmarshaled.Type)
	}
	if unmarshaled.AccountID != "acc-123" {
		t.Errorf("expected account ID acc-123, got %s", unmarshaled.AccountID)
	}
	if unmarshaled.Details["from_model"] != "gemini-2.5-pro" {
		t.Errorf("expected from_model gemini-2.5-pro, got %v", unmarshaled.Details["from_model"])
	}
	if unmarshaled.Details["to_model"] != "gemini-2.5-flash" {
		t.Errorf("expected to_model gemini-2.5-flash, got %v", unmarshaled.Details["to_model"])
	}
	if unmarshaled.Details["trigger"] != "predictive_quota_exhausted" {
		t.Errorf("expected trigger predictive_quota_exhausted, got %v", unmarshaled.Details["trigger"])
	}
}
