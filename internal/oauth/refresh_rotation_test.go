package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

// newRotatingRefreshServer answers every refresh_token grant with the given
// refresh_token value (empty means the field is omitted from the response).
func newRotatingRefreshServer(t *testing.T, rotated string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := TokenResponse{AccessToken: "fresh-access", ExpiresIn: 3600, RefreshToken: rotated}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRefreshAccessToken_PersistsRotatedRefreshToken(t *testing.T) {
	srv := newRotatingRefreshServer(t, "rotated-refresh")
	_, accRepo := setupTestStore(t)
	svc := NewOAuthService(accRepo, WithTokenURL(srv.URL+"/token"))
	ctx := context.Background()

	if err := accRepo.Create(ctx, &domain.Account{
		ID: "acc-rot", Email: "rot@example.com", AccessToken: "old-access",
		RefreshToken: "old-refresh", TokenExpiry: time.Now().Add(-time.Minute),
		Status: domain.AccountStatusActive,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}

	access, expiry, err := svc.RefreshAccessToken(ctx, "old-refresh")
	if err != nil {
		t.Fatalf("RefreshAccessToken: %v", err)
	}
	if access != "fresh-access" {
		t.Errorf("access token = %q, want fresh-access", access)
	}
	if !expiry.After(time.Now()) {
		t.Errorf("expiry %v should be in the future", expiry)
	}

	got, err := accRepo.GetByID(ctx, "acc-rot")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.RefreshToken != "rotated-refresh" {
		t.Errorf("stored refresh token = %q, want rotated-refresh", got.RefreshToken)
	}
}

func TestRefreshAccessToken_EmptyRefreshTokenKeepsStored(t *testing.T) {
	srv := newRotatingRefreshServer(t, "")
	_, accRepo := setupTestStore(t)
	svc := NewOAuthService(accRepo, WithTokenURL(srv.URL+"/token"))
	ctx := context.Background()

	if err := accRepo.Create(ctx, &domain.Account{
		ID: "acc-keep", Email: "keep@example.com", AccessToken: "old-access",
		RefreshToken: "old-refresh", TokenExpiry: time.Now().Add(-time.Minute),
		Status: domain.AccountStatusActive,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}

	if _, _, err := svc.RefreshAccessToken(ctx, "old-refresh"); err != nil {
		t.Fatalf("RefreshAccessToken: %v", err)
	}

	got, err := accRepo.GetByID(ctx, "acc-keep")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.RefreshToken != "old-refresh" {
		t.Errorf("stored refresh token = %q, want old-refresh", got.RefreshToken)
	}
}

func TestRefreshAccessToken_UnknownTokenStillReturnsAccess(t *testing.T) {
	srv := newRotatingRefreshServer(t, "rotated-refresh")
	_, accRepo := setupTestStore(t)
	svc := NewOAuthService(accRepo, WithTokenURL(srv.URL+"/token"))

	access, _, err := svc.RefreshAccessToken(context.Background(), "not-stored")
	if err != nil {
		t.Fatalf("RefreshAccessToken: %v", err)
	}
	if access != "fresh-access" {
		t.Errorf("access token = %q, want fresh-access", access)
	}
}
