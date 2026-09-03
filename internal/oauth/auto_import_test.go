package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
)

func TestAutoImportExistingAccount(t *testing.T) {
	// Mock Google Token & UserInfo endpoints
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "imported-test-token",
				"expires_in":   3600,
				"token_type":   "Bearer",
			})
		case "/userinfo":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"email": "autoimported@example.com",
				"id":    "12345",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	acpDir := filepath.Join(tmpHome, ".gemini", "antigravity-acp")
	if err := os.MkdirAll(acpDir, 0o755); err != nil {
		t.Fatal(err)
	}

	acpFile := filepath.Join(acpDir, "acp_token.json")
	tokenContent := `{
		"client_id": "test-client",
		"client_secret": "test-secret",
		"refresh_token": "test-refresh-token"
	}`
	if err := os.WriteFile(acpFile, []byte(tokenContent), 0o600); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(tmpHome, "test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	repo := sqlite.NewAccountRepository(db)
	oauthService := NewOAuthService(
		repo,
		WithTokenURL(server.URL+"/token"),
		WithUserInfoURL(server.URL+"/userinfo"),
	)

	acc, err := AutoImportExistingAccount(context.Background(), repo, oauthService)
	if err != nil {
		t.Fatalf("expected auto import to succeed, got: %v", err)
	}

	if acc == nil || acc.Email != "autoimported@example.com" {
		t.Errorf("expected auto-imported account with email autoimported@example.com, got %v", acc)
	}

	// Second run should return nil because accounts already exist
	acc2, err := AutoImportExistingAccount(context.Background(), repo, oauthService)
	if err != nil || acc2 != nil {
		t.Errorf("expected second run to be no-op, got %v, err %v", acc2, err)
	}
}
