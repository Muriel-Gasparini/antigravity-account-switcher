package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/oauth"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/test/mocks"
)

func setupTestStore(t *testing.T) (*sqlite.DB, *sqlite.AccountRepository) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), fmt.Sprintf("test_oauth_%d.db", time.Now().UnixNano()))
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	accRepo := sqlite.NewAccountRepository(db)
	return db, accRepo
}

// -----------------------------------------------------------------------------
// 1. Concurrent authorization flows on multiple ephemeral ports
// -----------------------------------------------------------------------------
func TestChallenger2_ConcurrentLoopbackFlows(t *testing.T) {
	// Custom mock server that generates unique emails per token
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		switch r.URL.Path {
		case "/token":
			w.WriteHeader(http.StatusOK)
			tok := fmt.Sprintf("tok_%d", time.Now().UnixNano())
			_ = json.NewEncoder(w).Encode(oauth.TokenResponse{
				AccessToken:  tok,
				TokenType:    "Bearer",
				ExpiresIn:    3600,
				RefreshToken: "ref_" + tok,
			})
		case "/oauth2/v3/userinfo":
			auth := r.Header.Get("Authorization")
			token := strings.TrimPrefix(auth, "Bearer ")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(oauth.UserInfoResponse{
				Email:         fmt.Sprintf("user_%s@example.com", token),
				VerifiedEmail: true,
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer mockServer.Close()

	db, accRepo := setupTestStore(t)
	ctx := context.Background()

	svc := oauth.NewOAuthService(accRepo,
		oauth.WithTokenURL(mockServer.URL+"/token"),
		oauth.WithUserInfoURL(mockServer.URL+"/oauth2/v3/userinfo"),
		oauth.WithAuthURL(mockServer.URL+"/o/oauth2/v2/auth"),
		oauth.WithFlowTimeout(10*time.Second),
	)

	const numFlows = 15
	var (
		wg           sync.WaitGroup
		usedPorts    sync.Map
		successCount atomic.Int32
		errorsList   = make([]error, numFlows)
	)

	wg.Add(numFlows)
	for i := 0; i < numFlows; i++ {
		go func(flowIdx int) {
			defer wg.Done()

			opener := func(authURL string) error {
				u, err := url.Parse(authURL)
				if err != nil {
					return fmt.Errorf("failed to parse authURL: %w", err)
				}

				redirectURI := u.Query().Get("redirect_uri")
				redirU, err := url.Parse(redirectURI)
				if err != nil {
					return fmt.Errorf("failed to parse redirectURI: %w", err)
				}
				portStr := redirU.Port()
				if portStr == "" {
					return errors.New("redirect_uri missing port")
				}
				if _, loaded := usedPorts.LoadOrStore(portStr, flowIdx); loaded {
					return fmt.Errorf("duplicate ephemeral port detected across flows: %s", portStr)
				}

				state := u.Query().Get("state")

				// Launch callback asynchronously as a real browser would
				go func() {
					time.Sleep(20 * time.Millisecond)
					callbackURL := fmt.Sprintf("%s?code=mock_code_%d&state=%s", redirectURI, flowIdx, state)
					resp, err := http.Get(callbackURL)
					if err != nil {
						return
					}
					defer resp.Body.Close()
				}()

				return nil
			}

			acc, err := svc.StartLoopbackFlow(ctx, opener, nil)
			if err != nil {
				errorsList[flowIdx] = err
				return
			}
			if acc == nil {
				errorsList[flowIdx] = errors.New("StartLoopbackFlow returned nil account")
				return
			}
			successCount.Add(1)
		}(i)
	}

	wg.Wait()

	for i, err := range errorsList {
		if err != nil {
			t.Errorf("flow %d failed: %v", i, err)
		}
	}

	if successCount.Load() != numFlows {
		t.Fatalf("expected %d successful flows, got %d", numFlows, successCount.Load())
	}

	// Invariant check: exactly one active account in SQLite
	var activeCount int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE is_active = 1").Scan(&activeCount)
	if err != nil {
		t.Fatalf("failed to query active account count: %v", err)
	}
	if activeCount != 1 {
		t.Fatalf("idx_accounts_single_active VIOLATED: found %d active accounts, expected exactly 1", activeCount)
	}

	// Verify all accounts exist in SQLite
	allAccounts, err := accRepo.List(ctx)
	if err != nil {
		t.Fatalf("List accounts failed: %v", err)
	}
	if len(allAccounts) != numFlows {
		t.Fatalf("expected %d stored accounts, got %d", numFlows, len(allAccounts))
	}

	// SQLite PRAGMA integrity_check
	var integrityResult string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrityResult); err != nil {
		t.Fatalf("PRAGMA integrity_check error: %v", err)
	}
	if integrityResult != "ok" {
		t.Fatalf("PRAGMA integrity_check failed: %s", integrityResult)
	}
}

// -----------------------------------------------------------------------------
// 2. State parameter tampering, missing, expiration, and replay protection
// -----------------------------------------------------------------------------
func TestChallenger2_StateTamperingAndExpiration(t *testing.T) {
	server := mocks.NewMockGoogleServer()
	defer server.Close()

	_, accRepo := setupTestStore(t)
	ctx := context.Background()

	t.Run("TamperedStateRejection", func(t *testing.T) {
		svc := oauth.NewOAuthService(accRepo,
			oauth.WithTokenURL(server.URL+"/token"),
			oauth.WithUserInfoURL(server.URL+"/oauth2/v3/userinfo"),
		)

		testCases := []struct {
			name     string
			rawQuery string
			wantSub  string
		}{
			{
				name:     "forged_state",
				rawQuery: "code=valid_code&state=completely_forged_state_12345",
				wantSub:  "state",
			},
			{
				name:     "missing_state",
				rawQuery: "code=valid_code",
				wantSub:  "missing state",
			},
			{
				name:     "empty_state",
				rawQuery: "code=valid_code&state=",
				wantSub:  "missing state",
			},
			{
				name:     "missing_code",
				rawQuery: "state=some_state",
				wantSub:  "missing authorization code",
			},
			{
				name:     "error_from_provider",
				rawQuery: "error=access_denied&error_description=user+denied+consent",
				wantSub:  "access_denied",
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:0/oauth/callback?"+tc.rawQuery, nil)
				_, err := svc.HandleCallbackRequest(req)
				if err == nil {
					t.Fatalf("expected error for case %s, got nil", tc.name)
				}
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantSub)) {
					t.Errorf("expected error containing %q, got %q", tc.wantSub, err.Error())
				}
			})
		}
	})

	t.Run("ReplayProtection_SingleUse", func(t *testing.T) {
		svc := oauth.NewOAuthService(accRepo,
			oauth.WithTokenURL(server.URL+"/token"),
			oauth.WithUserInfoURL(server.URL+"/oauth2/v3/userinfo"),
		)

		openerCalled := make(chan struct{})
		var callbackURL string
		opener := func(authURL string) error {
			u, _ := url.Parse(authURL)
			redirectURI := u.Query().Get("redirect_uri")
			st := u.Query().Get("state")
			callbackURL = fmt.Sprintf("%s?code=valid_code&state=%s", redirectURI, st)
			close(openerCalled)

			go func() {
				time.Sleep(10 * time.Millisecond)
				resp, err := http.Get(callbackURL)
				if err == nil {
					_ = resp.Body.Close()
				}
			}()
			return nil
		}

		acc, err := svc.StartLoopbackFlow(ctx, opener, nil)
		if err != nil {
			t.Fatalf("flow failed: %v", err)
		}
		if acc == nil {
			t.Fatal("expected account, got nil")
		}

		<-openerCalled

		// Attempt replay on HandleCallbackRequest with the same state: MUST FAIL
		req, _ := http.NewRequest(http.MethodGet, callbackURL, nil)
		_, replayErr := svc.HandleCallbackRequest(req)
		if replayErr == nil {
			t.Fatal("CRITICAL BUG: replay attack succeeded! Already consumed state was accepted!")
		}
		if !strings.Contains(replayErr.Error(), "already consumed") && !strings.Contains(replayErr.Error(), "invalid") {
			t.Errorf("unexpected error on replay: %v", replayErr)
		}
	})

	t.Run("ExpiredStateRejection", func(t *testing.T) {
		svc := oauth.NewOAuthService(accRepo,
			oauth.WithTokenURL(server.URL+"/token"),
			oauth.WithUserInfoURL(server.URL+"/oauth2/v3/userinfo"),
			oauth.WithStateTTL(50*time.Millisecond),
		)

		opener := func(authURL string) error {
			u, _ := url.Parse(authURL)
			state := u.Query().Get("state")
			redirectURI := u.Query().Get("redirect_uri")

			go func() {
				time.Sleep(100 * time.Millisecond)
				resp, err := http.Get(fmt.Sprintf("%s?code=code123&state=%s", redirectURI, state))
				if err != nil {
					return
				}
				defer resp.Body.Close()
			}()
			return nil
		}

		_, err := svc.StartLoopbackFlow(ctx, opener, nil)
		if err == nil {
			t.Fatal("expected error on expired state flow, got nil")
		}
		if !strings.Contains(err.Error(), "expired") && !strings.Contains(err.Error(), "invalid") {
			t.Errorf("unexpected error on expired state: %v", err)
		}
	})
}

// -----------------------------------------------------------------------------
// 3. PKCE code verifier mismatches (RFC 7636 & RFC 8252)
// -----------------------------------------------------------------------------
func TestChallenger2_PKCEMismatchRejection(t *testing.T) {
	_, accRepo := setupTestStore(t)
	ctx := context.Background()

	var expectedChallenge atomic.Pointer[string]

	pkceMockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			_ = r.ParseForm()
			codeVerifier := r.Form.Get("code_verifier")
			grantType := r.Form.Get("grant_type")

			if grantType != "authorization_code" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"unsupported_grant_type"}`))
				return
			}

			if codeVerifier == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_request","error_description":"missing code_verifier"}`))
				return
			}

			exp := expectedChallenge.Load()
			if exp != nil && *exp != "" {
				h := sha256.Sum256([]byte(codeVerifier))
				actualChallenge := base64.RawURLEncoding.EncodeToString(h[:])
				if actualChallenge != *exp {
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"error":             "invalid_grant",
						"error_description": fmt.Sprintf("PKCE verification failed: challenge mismatch (got %s, expected %s)", actualChallenge, *exp),
					})
					return
				}
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(oauth.TokenResponse{
				AccessToken:  "pkce_valid_access_token",
				TokenType:    "Bearer",
				ExpiresIn:    3600,
				RefreshToken: "pkce_valid_refresh_token",
			})

		case "/oauth2/v3/userinfo":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(oauth.UserInfoResponse{
				Email:         "pkce_user@example.com",
				VerifiedEmail: true,
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer pkceMockServer.Close()

	t.Run("PKCE_DirectExchange_Mismatch", func(t *testing.T) {
		svc := oauth.NewOAuthService(accRepo,
			oauth.WithTokenURL(pkceMockServer.URL+"/token"),
			oauth.WithUserInfoURL(pkceMockServer.URL+"/oauth2/v3/userinfo"),
		)

		pair, err := oauth.GeneratePKCE()
		if err != nil {
			t.Fatalf("GeneratePKCE: %v", err)
		}

		expectedChallenge.Store(&pair.Challenge)

		// 1. Legitimate verifier must succeed
		tok, err := svc.ExchangeCode(ctx, "mock_code", pair.Verifier, "http://127.0.0.1:1234/oauth/callback")
		if err != nil {
			t.Fatalf("legitimate verifier failed: %v", err)
		}
		if tok.AccessToken != "pkce_valid_access_token" {
			t.Errorf("unexpected access token: %s", tok.AccessToken)
		}

		// 2. Corrupted verifier must be rejected with 400 Bad Request
		tamperedVerifier := pair.Verifier[:len(pair.Verifier)-4] + "XXXX"
		_, err = svc.ExchangeCode(ctx, "mock_code", tamperedVerifier, "http://127.0.0.1:1234/oauth/callback")
		if err == nil {
			t.Fatal("expected error on tampered code_verifier, got nil")
		}
		if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "PKCE verification failed") {
			t.Errorf("expected 400 PKCE verification failed error, got: %v", err)
		}

		// 3. Empty verifier must be rejected
		_, err = svc.ExchangeCode(ctx, "mock_code", "", "http://127.0.0.1:1234/oauth/callback")
		if err == nil {
			t.Fatal("expected error on empty code_verifier, got nil")
		}
	})

	t.Run("PKCE_EndToEndFlow_WithTamperedVerifier", func(t *testing.T) {
		svc := oauth.NewOAuthService(accRepo,
			oauth.WithTokenURL(pkceMockServer.URL+"/token"),
			oauth.WithUserInfoURL(pkceMockServer.URL+"/oauth2/v3/userinfo"),
		)

		impossibleChallenge := "IMPOSSIBLE_CHALLENGE_THAT_WILL_NEVER_MATCH_RANDOM_PKCE"
		expectedChallenge.Store(&impossibleChallenge)

		opener := func(authURL string) error {
			u, _ := url.Parse(authURL)
			redirectURI := u.Query().Get("redirect_uri")
			state := u.Query().Get("state")

			go func() {
				time.Sleep(10 * time.Millisecond)
				resp, err := http.Get(fmt.Sprintf("%s?code=code123&state=%s", redirectURI, state))
				if err != nil {
					return
				}
				defer resp.Body.Close()
			}()
			return nil
		}

		_, err := svc.StartLoopbackFlow(ctx, opener, nil)
		if err == nil {
			t.Fatal("expected StartLoopbackFlow to fail due to PKCE mismatch, got nil")
		}
		if !strings.Contains(err.Error(), "token exchange failed") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("PKCE_MathematicalEntropyAndEncoding", func(t *testing.T) {
		const iterations = 500
		seenVerifiers := make(map[string]bool)

		for i := 0; i < iterations; i++ {
			pkce, err := oauth.GeneratePKCE()
			if err != nil {
				t.Fatalf("GeneratePKCE failed: %v", err)
			}

			if len(pkce.Verifier) < 43 || len(pkce.Verifier) > 128 {
				t.Fatalf("RFC 7636 violation: verifier length %d out of [43, 128]", len(pkce.Verifier))
			}

			if strings.ContainsAny(pkce.Verifier, "+/=") {
				t.Fatalf("verifier contains unencoded characters: %s", pkce.Verifier)
			}
			if strings.ContainsAny(pkce.Challenge, "+/=") {
				t.Fatalf("challenge contains unencoded characters: %s", pkce.Challenge)
			}

			h := sha256.Sum256([]byte(pkce.Verifier))
			calcChallenge := base64.RawURLEncoding.EncodeToString(h[:])
			if pkce.Challenge != calcChallenge {
				t.Fatalf("PKCE challenge mismatch: got %s, want %s", pkce.Challenge, calcChallenge)
			}

			if seenVerifiers[pkce.Verifier] {
				t.Fatalf("collision in code verifier generation: %s", pkce.Verifier)
			}
			seenVerifiers[pkce.Verifier] = true
		}
	})
}

// -----------------------------------------------------------------------------
// 4. Invariant idx_accounts_single_active in SQLite under heavy concurrency
// -----------------------------------------------------------------------------
func TestChallenger2_SingleActiveInvariant_Stress(t *testing.T) {
	db, accRepo := setupTestStore(t)
	ctx := context.Background()

	svc := oauth.NewOAuthService(accRepo)

	const numAccounts = 40
	var wg sync.WaitGroup
	wg.Add(numAccounts)

	for i := 0; i < numAccounts; i++ {
		go func(idx int) {
			defer wg.Done()
			email := fmt.Sprintf("concurrent_acc_%d@example.com", idx)
			accessToken := fmt.Sprintf("tok_%d", idx)
			refreshToken := fmt.Sprintf("ref_%d", idx)
			expiry := time.Now().Add(1 * time.Hour)

			_, err := svc.UpsertAccount(ctx, email, accessToken, refreshToken, expiry)
			if err != nil {
				t.Errorf("UpsertAccount failed for %s: %v", email, err)
			}
		}(i)
	}

	wg.Wait()

	var activeCount int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE is_active = 1").Scan(&activeCount)
	if err != nil {
		t.Fatalf("failed to query active account count: %v", err)
	}
	if activeCount != 1 {
		t.Fatalf("INVARIANT VIOLATION: expected exactly 1 active account after concurrent creation, got %d", activeCount)
	}

	activeAcc, err := accRepo.GetActive(ctx)
	if err != nil || activeAcc == nil {
		t.Fatalf("GetActive returned error or nil: %v", err)
	}

	const rotationWorkers = 30
	var rotWg sync.WaitGroup
	rotWg.Add(rotationWorkers)

	allAccounts, err := accRepo.List(ctx)
	if err != nil || len(allAccounts) != numAccounts {
		t.Fatalf("expected %d accounts, got %d (err: %v)", numAccounts, len(allAccounts), err)
	}

	for i := 0; i < rotationWorkers; i++ {
		go func(workerID int) {
			defer rotWg.Done()
			target := allAccounts[workerID%len(allAccounts)]

			if err := accRepo.SetActive(ctx, target.ID); err != nil {
				t.Errorf("SetActive failed: %v", err)
				return
			}

			var currentActive int
			rowErr := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE is_active = 1").Scan(&currentActive)
			if rowErr != nil {
				t.Errorf("query error: %v", rowErr)
				return
			}
			if currentActive != 1 {
				t.Errorf("INTERMITTENT INVARIANT VIOLATION during rotation: found %d active accounts", currentActive)
			}
		}(i)
	}

	rotWg.Wait()

	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE is_active = 1").Scan(&activeCount)
	if err != nil {
		t.Fatalf("failed to query active account count: %v", err)
	}
	if activeCount != 1 {
		t.Fatalf("FINAL INVARIANT VIOLATION: expected 1 active account, got %d", activeCount)
	}

	// Raw SQL insert test: must fail
	_, err = db.ExecContext(ctx, `
		INSERT INTO accounts (id, email, refresh_token, access_token, is_active, status)
		VALUES ('violator-1', 'violator@example.com', 'ref', 'acc', 1, 'active')
	`)
	if err == nil {
		t.Fatal("CRITICAL BUG: SQLite allowed inserting second account with is_active = 1! idx_accounts_single_active failed to enforce uniqueness!")
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("expected UNIQUE constraint violation, got: %v", err)
	}

	// Raw SQL update test without LIMIT: must fail with UNIQUE constraint
	_, err = db.ExecContext(ctx, `
		UPDATE accounts SET is_active = 1 WHERE id = (SELECT id FROM accounts WHERE is_active = 0 LIMIT 1)
	`)
	if err == nil {
		t.Fatal("CRITICAL BUG: SQLite allowed updating second account to is_active = 1! idx_accounts_single_active failed!")
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("expected UNIQUE constraint violation, got: %v", err)
	}

	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("PRAGMA integrity_check failed: %s (err: %v)", integrity, err)
	}
}

// -----------------------------------------------------------------------------
// 5. Empirical Challenge: Multiple/Duplicate Requests to the Ephemeral Callback Listener
// -----------------------------------------------------------------------------
func TestChallenger2_DuplicateCallbacks_ChannelSendVulnerability(t *testing.T) {
	server := mocks.NewMockGoogleServer()
	defer server.Close()

	_, accRepo := setupTestStore(t)
	ctx := context.Background()

	svc := oauth.NewOAuthService(accRepo,
		oauth.WithTokenURL(server.URL+"/token"),
		oauth.WithUserInfoURL(server.URL+"/oauth2/v3/userinfo"),
		oauth.WithFlowTimeout(3*time.Second),
	)

	// An opener where multiple callbacks are sent in parallel to the loopback port
	opener := func(authURL string) error {
		u, _ := url.Parse(authURL)
		redirectURI := u.Query().Get("redirect_uri")
		state := u.Query().Get("state")

		go func() {
			// Callback 1 (valid)
			time.Sleep(10 * time.Millisecond)
			cb1 := fmt.Sprintf("%s?code=code1&state=%s", redirectURI, state)
			resp1, err1 := http.Get(cb1)
			if err1 == nil {
				_ = resp1.Body.Close()
			}

			// Callback 2 (duplicate/retry or secondary request)
			time.Sleep(5 * time.Millisecond)
			cb2 := fmt.Sprintf("%s?code=code2&state=%s", redirectURI, state)
			client := &http.Client{Timeout: 1 * time.Second}
			resp2, err2 := client.Get(cb2)
			if err2 == nil {
				_ = resp2.Body.Close()
			}
		}()
		return nil
	}

	acc, err := svc.StartLoopbackFlow(ctx, opener, nil)
	if err != nil {
		t.Fatalf("StartLoopbackFlow failed: %v", err)
	}
	if acc == nil {
		t.Fatal("expected account, got nil")
	}
}

// -----------------------------------------------------------------------------
