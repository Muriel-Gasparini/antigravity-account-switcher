package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/google/uuid"
)

// ACPTokenFile represents the JSON structure used by Antigravity in ~/.gemini/antigravity-acp/acp_token.json.
type ACPTokenFile struct {
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	RefreshToken string   `json:"refresh_token"`
	TokenURI     string   `json:"token_uri"`
	Scopes       []string `json:"scopes"`
	ProjectID    string   `json:"project_id"`
}

// FindExistingACPTokenFile locates any existing Antigravity ACP OAuth credentials on the machine.
func FindExistingACPTokenFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	candidates := []string{
		filepath.Join(home, ".gemini", "antigravity-acp", "acp_token.json"),
		filepath.Join(home, ".gemini", "antigravity-cli", "acp_token.json"),
	}

	for _, p := range candidates {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Size() > 0 {
			return p
		}
	}

	return ""
}

// AutoImportExistingAccount checks if accounts pool is empty and automatically imports
// credentials from an existing Antigravity installation (~/.gemini/antigravity-acp/acp_token.json).
func AutoImportExistingAccount(ctx context.Context, repo domain.AccountRepository, oauthService *OAuthService) (*domain.Account, error) {
	if repo == nil || oauthService == nil {
		return nil, errors.New("repository and oauth service cannot be nil")
	}

	existing, err := repo.List(ctx)
	if err == nil && len(existing) > 0 {
		// Already has accounts, no auto-import needed
		return nil, nil
	}

	tokenPath := FindExistingACPTokenFile()
	if tokenPath == "" {
		return nil, nil // No existing token file found
	}

	data, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", tokenPath, err)
	}

	var acp ACPTokenFile
	if err := json.Unmarshal(data, &acp); err != nil || acp.RefreshToken == "" {
		return nil, fmt.Errorf("invalid token file format: %w", err)
	}

	// Exchange refresh token for fresh access token
	resp, err := oauthService.RefreshToken(ctx, acp.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("failed to refresh imported token: %w", err)
	}

	// Fetch userinfo to discover email
	userInfo, err := oauthService.FetchUserInfo(ctx, resp.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch userinfo for imported token: %w", err)
	}

	acc := &domain.Account{
		ID:           uuid.New().String(),
		Email:        userInfo.Email,
		RefreshToken: acp.RefreshToken,
		AccessToken:  resp.AccessToken,
		TokenExpiry:  time.Now().UTC().Add(time.Duration(resp.ExpiresIn) * time.Second),
		IsActive:     true,
		Status:       domain.AccountStatusActive,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}

	if err := repo.Create(ctx, acc); err != nil {
		return nil, fmt.Errorf("failed to persist auto-imported account: %w", err)
	}

	_ = repo.SetActive(ctx, acc.ID)

	return acc, nil
}
