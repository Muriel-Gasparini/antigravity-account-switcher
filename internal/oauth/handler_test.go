package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/test/mocks"
)

func setupTestStore(t *testing.T) (*sqlite.DB, *sqlite.AccountRepository) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test_oauth.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	accRepo := sqlite.NewAccountRepository(db)
	return db, accRepo
}

func TestPKCE_Generation(t *testing.T) {
	pkce, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE failed: %v", err)
	}

	if len(pkce.Verifier) < 43 || len(pkce.Verifier) > 128 {
		t.Errorf("verifier length %d out of bounds [43, 128]", len(pkce.Verifier))
	}
	if pkce.Method != "S256" {
		t.Errorf("expected method S256, got %s", pkce.Method)
	}

	// Verify SHA-256 calculation
	h := sha256.Sum256([]byte(pkce.Verifier))
	expectedChallenge := base64.RawURLEncoding.EncodeToString(h[:])
	if pkce.Challenge != expectedChallenge {
		t.Errorf("challenge mismatch: got %s, want %s", pkce.Challenge, expectedChallenge)
	}

	// State randomness test
	states := make(map[string]bool)
	for i := 0; i < 50; i++ {
		st, err := GenerateState()
		if err != nil {
			t.Fatalf("GenerateState failed: %v", err)
		}
		if states[st] {
			t.Fatalf("duplicate state generated: %s", st)
		}
		states[st] = true
	}
}

func TestStateStore_TTLAndSingleUse(t *testing.T) {
	store := NewStateStore(50 * time.Millisecond)

	auth := &PendingAuth{
		State:        "test-state-1",
		CodeVerifier: "verifier-1",
		RedirectURI:  "http://127.0.0.1:1234/oauth/callback",
		CreatedAt:    time.Now(),
	}

	store.Put(auth)

	// First fetch must succeed and evict
	retrieved, ok := store.GetAndRemove("test-state-1")
	if !ok || retrieved.CodeVerifier != "verifier-1" {
		t.Fatalf("expected to retrieve pending auth, got ok=%v, val=%+v", ok, retrieved)
	}

	// Second fetch must fail (single-use / replay protection)
	_, ok = store.GetAndRemove("test-state-1")
	if ok {
		t.Fatal("expected second GetAndRemove to return false (already evicted)")
	}

	// Test TTL expiration
	store.Put(&PendingAuth{
		State:        "test-state-expired",
		CodeVerifier: "verifier-2",
		CreatedAt:    time.Now().Add(-100 * time.Millisecond), // already expired
	})

	_, ok = store.GetAndRemove("test-state-expired")
	if ok {
		t.Fatal("expected expired state to be rejected")
	}

	// Concurrency test under -race
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			st := fmt.Sprintf("concurrent-state-%d", idx)
			store.Put(&PendingAuth{
				State:        st,
				CodeVerifier: "v",
				CreatedAt:    time.Now(),
			})
			store.GetAndRemove(st)
		}(i)
	}
	wg.Wait()
}

func TestOAuth_SuccessfulLoopbackFlow(t *testing.T) {
	server := mocks.NewMockGoogleServer()
	defer server.Close()

	_, accRepo := setupTestStore(t)
	ctx := context.Background()

	svc := NewOAuthService(accRepo,
		WithTokenURL(server.URL+"/token"),
		WithUserInfoURL(server.URL+"/oauth2/v3/userinfo"),
		WithAuthURL(server.URL+"/o/oauth2/v2/auth"),
	)

	// Simulated browser opener
	opener := func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		redirectURI := u.Query().Get("redirect_uri")
		state := u.Query().Get("state")

		// Simulate user completing login: browser follows redirect
		callbackURL := fmt.Sprintf("%s?code=mock_auth_code_123&state=%s", redirectURI, state)
		resp, err := http.Get(callbackURL)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("callback returned HTTP %d", resp.StatusCode)
		}

		body, _ := io.ReadAll(resp.Body)
		bodyStr := string(body)
		if !strings.Contains(bodyStr, "Account Connected!") || !strings.Contains(bodyStr, "developer@mockgoogle.com") {
			return fmt.Errorf("unexpected HTML body: %s", bodyStr)
		}

		return nil
	}

	var loggedURL string
	account, err := svc.StartLoopbackFlow(ctx, opener, func(u string) {
		loggedURL = u
	})

	if err != nil {
		t.Fatalf("StartLoopbackFlow failed: %v", err)
	}

	if loggedURL == "" {
		t.Error("expected auth URL to be logged")
	}

	if account == nil || account.Email != "developer@mockgoogle.com" {
		t.Fatalf("unexpected account: %+v", account)
	}
	if !account.IsActive {
		t.Error("expected first created account to be marked active")
	}
	if account.Status != domain.AccountStatusActive {
		t.Errorf("expected status active, got %s", account.Status)
	}

	// Verify account in SQLite
	stored, err := accRepo.GetByID(ctx, account.ID)
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}
	if stored.Email != "developer@mockgoogle.com" || !stored.IsActive {
		t.Errorf("stored account mismatch: %+v", stored)
	}
}

func TestOAuth_ReAuth_ExistingAccount(t *testing.T) {
	server := mocks.NewMockGoogleServer()
	defer server.Close()

	_, accRepo := setupTestStore(t)
	ctx := context.Background()

	// Pre-create account
	pre := &domain.Account{
		ID:           "acc-existing",
		Email:        "developer@mockgoogle.com",
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		TokenExpiry:  time.Now().Add(-1 * time.Hour),
		Status:       domain.AccountStatusExhausted, // was exhausted
		IsActive:     true,
	}
	if err := accRepo.Create(ctx, pre); err != nil {
		t.Fatalf("Create: %v", err)
	}

	svc := NewOAuthService(accRepo,
		WithTokenURL(server.URL+"/token"),
		WithUserInfoURL(server.URL+"/oauth2/v3/userinfo"),
	)

	opener := func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		callbackURL := fmt.Sprintf("%s?code=mock_code&state=%s", u.Query().Get("redirect_uri"), u.Query().Get("state"))
		resp, err := http.Get(callbackURL)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return nil
	}

	acc, err := svc.StartLoopbackFlow(ctx, opener, nil)
	if err != nil {
		t.Fatalf("StartLoopbackFlow failed: %v", err)
	}

	if acc.ID != "acc-existing" {
		t.Errorf("expected existing ID acc-existing, got %s", acc.ID)
	}
	if acc.Status != domain.AccountStatusActive {
		t.Errorf("expected status restored to active, got %s", acc.Status)
	}

	// Total accounts in DB must be 1
	accounts, err := accRepo.List(ctx)
	if err != nil || len(accounts) != 1 {
		t.Errorf("expected exactly 1 account in SQLite, got %d", len(accounts))
	}
}

func TestOAuth_MultiAccount_SingleActiveInvariant(t *testing.T) {
	server := mocks.NewMockGoogleServer()
	defer server.Close()

	_, accRepo := setupTestStore(t)
	ctx := context.Background()

	// Account 1 exists and is active
	acc1 := &domain.Account{
		ID:           "acc-1",
		Email:        "user1@example.com",
		AccessToken:  "token-1",
		RefreshToken: "refresh-1",
		IsActive:     true,
		Status:       domain.AccountStatusActive,
	}
	if err := accRepo.Create(ctx, acc1); err != nil {
		t.Fatalf("Create acc1: %v", err)
	}

	svc := NewOAuthService(accRepo,
		WithTokenURL(server.URL+"/token"),
		WithUserInfoURL(server.URL+"/oauth2/v3/userinfo"),
	)

	// Second account onboarded (developer@mockgoogle.com)
	opener := func(authURL string) error {
		u, _ := url.Parse(authURL)
		resp, err := http.Get(fmt.Sprintf("%s?code=mock_code&state=%s", u.Query().Get("redirect_uri"), u.Query().Get("state")))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return nil
	}

	acc2, err := svc.StartLoopbackFlow(ctx, opener, nil)
	if err != nil {
		t.Fatalf("StartLoopbackFlow acc2 failed: %v", err)
	}

	if acc2.IsActive {
		t.Error("expected new account acc2 to be inactive since acc1 is already active")
	}

	// Verify acc1 remains active
	active, err := accRepo.GetActive(ctx)
	if err != nil || active.ID != "acc-1" {
		t.Errorf("expected acc-1 to remain active, got %+v", active)
	}
}

func TestOAuth_RefreshToken_SuccessAndRevoked(t *testing.T) {
	server := mocks.NewMockGoogleServer()
	defer server.Close()

	_, accRepo := setupTestStore(t)
	svc := NewOAuthService(accRepo, WithTokenURL(server.URL+"/token"))

	ctx := context.Background()

	// 1. Valid refresh
	tokResp, err := svc.RefreshToken(ctx, "valid_refresh_token")
	if err != nil {
		t.Fatalf("RefreshToken failed: %v", err)
	}
	if !strings.HasPrefix(tokResp.AccessToken, "mock_access_token_") {
		t.Errorf("unexpected access token: %s", tokResp.AccessToken)
	}

	// 2. Revoked refresh token
	_, err = svc.RefreshToken(ctx, "revoked")
	if !errors.Is(err, domain.ErrInvalidRefreshToken) {
		t.Errorf("expected ErrInvalidRefreshToken, got %v", err)
	}
}

func TestOAuth_EnsureValidToken(t *testing.T) {
	server := mocks.NewMockGoogleServer()
	defer server.Close()

	_, accRepo := setupTestStore(t)
	svc := NewOAuthService(accRepo, WithTokenURL(server.URL+"/token"))
	ctx := context.Background()

	now := time.Now().UTC()

	// 1. Non-expired token should not make network calls
	validAcc := &domain.Account{
		ID:          "acc-valid",
		Email:       "valid@example.com",
		AccessToken: "still-good",
		TokenExpiry: now.Add(1 * time.Hour),
	}
	ensured, err := svc.EnsureValidToken(ctx, validAcc, 60*time.Second)
	if err != nil {
		t.Fatalf("EnsureValidToken failed: %v", err)
	}
	if ensured.AccessToken != "still-good" {
		t.Errorf("expected token untouched, got %s", ensured.AccessToken)
	}

	// 2. Expired token should be refreshed and updated in DB
	expiredAcc := &domain.Account{
		ID:           "acc-to-refresh",
		Email:        "refresh@example.com",
		AccessToken:  "old-token",
		RefreshToken: "good-refresh",
		TokenExpiry:  now.Add(-5 * time.Minute),
		Status:       domain.AccountStatusActive,
	}
	_ = accRepo.Create(ctx, expiredAcc)

	refreshed, err := svc.EnsureValidToken(ctx, expiredAcc, 60*time.Second)
	if err != nil {
		t.Fatalf("EnsureValidToken failed: %v", err)
	}
	if !strings.HasPrefix(refreshed.AccessToken, "mock_access_token_") {
		t.Errorf("expected refreshed token, got %s", refreshed.AccessToken)
	}

	dbAcc, _ := accRepo.GetByID(ctx, "acc-to-refresh")
	if dbAcc.AccessToken != refreshed.AccessToken {
		t.Errorf("database token not updated: got %s, want %s", dbAcc.AccessToken, refreshed.AccessToken)
	}

	// 3. Revoked token transitions account to error status
	revokedAcc := &domain.Account{
		ID:           "acc-revoked-ensure",
		Email:        "revoked@example.com",
		AccessToken:  "old-token",
		RefreshToken: "revoked",
		TokenExpiry:  now.Add(-5 * time.Minute),
		Status:       domain.AccountStatusActive,
	}
	_ = accRepo.Create(ctx, revokedAcc)

	_, err = svc.EnsureValidToken(ctx, revokedAcc, 60*time.Second)
	if err == nil {
		t.Fatal("expected error on revoked token refresh, got nil")
	}

	statusAcc, _ := accRepo.GetByID(ctx, "acc-revoked-ensure")
	if statusAcc.Status != domain.AccountStatusError {
		t.Errorf("expected account status error, got %s", statusAcc.Status)
	}
}

func TestOAuth_CSRFStateTampering(t *testing.T) {
	_, accRepo := setupTestStore(t)
	svc := NewOAuthService(accRepo)

	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:0/oauth/callback?code=abc&state=tampered_state", nil)
	_, err := svc.HandleCallbackRequest(req)
	if err == nil {
		t.Fatal("expected error for tampered state, got nil")
	}
	if !strings.Contains(err.Error(), "state parameter") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestOAuth_HeadlessFallback(t *testing.T) {
	server := mocks.NewMockGoogleServer()
	defer server.Close()

	_, accRepo := setupTestStore(t)
	ctx := context.Background()

	svc := NewOAuthService(accRepo,
		WithTokenURL(server.URL+"/token"),
		WithUserInfoURL(server.URL+"/oauth2/v3/userinfo"),
	)

	urlChan := make(chan string, 1)
	// Failing opener simulating headless environment
	failingOpener := func(authURL string) error {
		return errors.New("xdg-open: no display found")
	}

	// We trigger callback in a background goroutine once URL is logged
	go func() {
		select {
		case loggedURL := <-urlChan:
			u, err := url.Parse(loggedURL)
			if err == nil {
				redirectURI := u.Query().Get("redirect_uri")
				state := u.Query().Get("state")
				resp, err := http.Get(fmt.Sprintf("%s?code=mock_code&state=%s", redirectURI, state))
				if err == nil {
					_ = resp.Body.Close()
				}
			}
		case <-time.After(2 * time.Second):
		}
	}()

	acc, err := svc.StartLoopbackFlow(ctx, failingOpener, func(u string) {
		urlChan <- u
	})

	if err != nil {
		t.Fatalf("StartLoopbackFlow in headless mode failed: %v", err)
	}
	if acc == nil || acc.Email != "developer@mockgoogle.com" {
		t.Errorf("unexpected account returned in headless mode: %+v", acc)
	}
}
