package web

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/config"
)

// With --bind 0.0.0.0 the startup banner prints http://0.0.0.0:<port>/, and a
// browser opened there sends that as Origin while the connection's local
// address is 127.0.0.1. The UI must keep working in that setup.
func TestIsOwnOrigin_AcceptsConfiguredBindHost(t *testing.T) {
	_, accRepo, quotaRepo, _, metricsSvc, broadcaster, eventRepo := setupTestWeb(t)
	server, err := NewServer(
		accRepo, quotaRepo, metricsSvc, broadcaster, eventRepo, nil,
		WithConfig(&config.Config{Port: 8080, ModelPrimary: "a", ModelSecondary: "b"}),
		WithBindAddr("0.0.0.0"),
	)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/config", nil)
	local := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080}
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, local))

	cases := []struct {
		origin string
		want   bool
	}{
		{"http://0.0.0.0:8080", true},
		{"http://127.0.0.1:8080", true},
		{"http://0.0.0.0:9999", false},
		{"http://evil.example:8080", false},
	}
	for _, tc := range cases {
		if got := server.isOwnOrigin(req, tc.origin); got != tc.want {
			t.Errorf("isOwnOrigin(%q) = %v, want %v", tc.origin, got, tc.want)
		}
	}
}
