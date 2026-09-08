package quota

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

type lsQuotaResponse struct {
	Response struct {
		Groups []struct {
			DisplayName string `json:"displayName"`
			Description string `json:"description"`
			Buckets     []struct {
				BucketID          string  `json:"bucketId"`
				DisplayName       string  `json:"displayName"`
				Description       string  `json:"description"`
				Window            string  `json:"window"`
				RemainingFraction float64 `json:"remainingFraction"`
				ResetTime         string  `json:"resetTime"`
			} `json:"buckets"`
		} `json:"groups"`
		Description string `json:"description"`
	} `json:"response"`
}

type modelsPayload struct {
	Models map[string]struct {
		DisplayName      string `json:"displayName"`
		Recommended      bool   `json:"recommended"`
		SupportsThinking bool   `json:"supportsThinking"`
	} `json:"models"`
	DefaultAgentModelID string `json:"defaultAgentModelId"`
	AgentModelSorts     []struct {
		DisplayName string `json:"displayName"`
		Groups      []struct {
			ModelIDs []string `json:"modelIds"`
		} `json:"groups"`
	} `json:"agentModelSorts"`
}

type lsAvailableModelsResponse struct {
	Response *modelsPayload `json:"response"`
	modelsPayload
}

// FindLocalLanguageServer discovers the CSRF token and candidate listening ports
// of any running Antigravity language_server process on Linux.
func FindLocalLanguageServer() (csrf string, ports []int, err error) {
	procEntries, readErr := os.ReadDir("/proc")
	if readErr != nil {
		return "", nil, readErr
	}

	var targetPid string
	csrfRegex := regexp.MustCompile(`--csrf_token[\x00\s]+([^\x00\s]+)`)
	for _, entry := range procEntries {
		if !entry.IsDir() {
			continue
		}
		pid := entry.Name()
		if _, convErr := strconv.Atoi(pid); convErr != nil {
			continue
		}

		cmdlinePath := filepath.Join("/proc", pid, "cmdline")
		cmdBytes, cmdErr := os.ReadFile(cmdlinePath)
		if cmdErr != nil {
			continue
		}

		cmdStr := string(cmdBytes)
		if strings.Contains(cmdStr, "language_server") && strings.Contains(cmdStr, "--csrf_token") {
			match := csrfRegex.FindStringSubmatch(cmdStr)
			if len(match) > 1 {
				csrf = match[1]
				targetPid = pid
				break
			}
		}
	}

	if csrf == "" {
		return "", nil, fmt.Errorf("language_server process with --csrf_token not found")
	}

	// Read socket inodes specifically owned by the language_server process
	pidInodes := make(map[string]bool)
	if targetPid != "" {
		fdDir := filepath.Join("/proc", targetPid, "fd")
		if entries, readErr := os.ReadDir(fdDir); readErr == nil {
			for _, entry := range entries {
				link, linkErr := os.Readlink(filepath.Join(fdDir, entry.Name()))
				if linkErr == nil && strings.HasPrefix(link, "socket:[") {
					inode := strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")
					pidInodes[inode] = true
				}
			}
		}
	}

	// Read listening TCP ports from /proc/net/tcp
	tcpBytes, tcpErr := os.ReadFile("/proc/net/tcp")
	if tcpErr != nil {
		return csrf, nil, tcpErr
	}

	lines := strings.Split(string(tcpBytes), "\n")
	seenPorts := make(map[int]bool)
	var fallbackPorts []int

	for i, line := range lines {
		if i == 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		state := fields[3]
		if state != "0A" { // TCP_LISTEN
			continue
		}
		inode := fields[9]
		localAddr := fields[1]
		parts := strings.Split(localAddr, ":")
		if len(parts) == 2 {
			portHex, hexErr := strconv.ParseInt(parts[1], 16, 64)
			if hexErr == nil && portHex > 1024 && portHex < 65535 {
				p := int(portHex)
				if !seenPorts[p] {
					seenPorts[p] = true
					if len(pidInodes) > 0 && pidInodes[inode] {
						ports = append(ports, p)
					} else {
						fallbackPorts = append(fallbackPorts, p)
					}
				}
			}
		}
	}

	// If no ports matched process inodes, use listening ports
	if len(ports) == 0 {
		ports = fallbackPorts
	}

	return csrf, ports, nil
}

func doLSRequest(ctx context.Context, csrf string, ports []int, endpoint string, reqBody []byte) ([]byte, error) {
	client := &http.Client{
		Timeout: 2500 * time.Millisecond,
		Transport: &http.Transport{
			Proxy: nil, // Bypass ambient HTTP_PROXY
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // Localhost self-signed TLS cert
			},
		},
	}

	schemes := []string{"https", "http"}
	for _, port := range ports {
		for _, scheme := range schemes {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			url := fmt.Sprintf("%s://127.0.0.1:%d%s", scheme, port, endpoint)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
			if err != nil {
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-codeium-csrf-token", csrf)

			resp, err := client.Do(req)
			if err != nil {
				continue
			}

			if resp.StatusCode == http.StatusOK {
				body, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr == nil && len(body) > 0 {
					return body, nil
				}
			} else {
				_ = resp.Body.Close()
			}
		}
	}
	return nil, fmt.Errorf("no language_server listening port responded successfully to %s", endpoint)
}

// QueryLocalLanguageServer queries the running language_server process on localhost
// for real model quota groups and buckets.
func QueryLocalLanguageServer(ctx context.Context, accountID string) ([]*domain.QuotaBucket, error) {
	csrf, ports, err := FindLocalLanguageServer()
	if err != nil {
		return nil, err
	}

	rawResp, err := doLSRequest(ctx, csrf, ports, "/exa.language_server_pb.LanguageServerService/RetrieveUserQuotaSummary", []byte(`{"forceRefresh":true}`))
	if err != nil {
		return nil, err
	}

	var parsed lsQuotaResponse
	if unmarshalErr := json.Unmarshal(rawResp, &parsed); unmarshalErr != nil {
		return nil, fmt.Errorf("failed to parse language_server quota response: %w", unmarshalErr)
	}

	now := time.Now().UTC()
	var buckets []*domain.QuotaBucket

	for _, g := range parsed.Response.Groups {
		groupName := g.DisplayName
		if strings.Contains(strings.ToLower(groupName), "claude") {
			groupName = "Claude & GPT"
		} else if strings.Contains(strings.ToLower(groupName), "gemini") {
			groupName = "Gemini"
		}

		for _, b := range g.Buckets {
			var resetTime time.Time
			if b.ResetTime != "" {
				t, tErr := time.Parse(time.RFC3339, b.ResetTime)
				if tErr == nil {
					resetTime = t
				}
			}

			windowName := b.Window
			if strings.EqualFold(windowName, "weekly") {
				windowName = "WEEKLY"
			} else if strings.EqualFold(windowName, "5h") {
				windowName = "5H"
			}

			displayName := fmt.Sprintf("%s (%s)", groupName, b.Window)
			bucket := &domain.QuotaBucket{
				AccountID:         accountID,
				BucketID:          fmt.Sprintf("%s-%s", accountID, b.BucketID),
				DisplayName:       displayName,
				Window:            domain.QuotaWindow(windowName),
				RemainingFraction: b.RemainingFraction,
				ResetTime:         resetTime,
				UpdatedAt:         now,
			}
			buckets = append(buckets, bucket)
		}
	}

	return buckets, nil
}

// QueryAvailableModels queries the running language_server for active AI models and tiers.
func QueryAvailableModels(ctx context.Context) ([]*domain.ModelInfo, error) {
	csrf, ports, err := FindLocalLanguageServer()
	if err != nil {
		return nil, err
	}

	rawResp, err := doLSRequest(ctx, csrf, ports, "/exa.language_server_pb.LanguageServerService/GetAvailableModels", []byte(`{}`))
	if err != nil {
		return nil, err
	}

	var parsed lsAvailableModelsResponse
	if unmarshalErr := json.Unmarshal(rawResp, &parsed); unmarshalErr != nil {
		return nil, fmt.Errorf("failed to parse available models response: %w", unmarshalErr)
	}

	payload := &parsed.modelsPayload
	if parsed.Response != nil && len(parsed.Response.Models) > 0 {
		payload = parsed.Response
	}

	return extractModels(payload)
}

func extractModels(p *modelsPayload) ([]*domain.ModelInfo, error) {
	if p == nil || len(p.Models) == 0 {
		return nil, fmt.Errorf("empty models map")
	}

	seen := make(map[string]bool)
	var result []*domain.ModelInfo

	// 1. First add sorted/recommended models in order from agentModelSorts
	for _, sortGrp := range p.AgentModelSorts {
		isRecGroup := strings.EqualFold(sortGrp.DisplayName, "Recommended")
		for _, grp := range sortGrp.Groups {
			for _, mid := range grp.ModelIDs {
				if seen[mid] {
					continue
				}
				modelMeta, ok := p.Models[mid]
				if !ok {
					continue
				}
				seen[mid] = true
				dName := modelMeta.DisplayName
				if dName == "" {
					dName = mid
				}
				result = append(result, &domain.ModelInfo{
					ID:          mid,
					DisplayName: dName,
					Category:    categorizeModelID(mid),
					Recommended: isRecGroup || modelMeta.Recommended,
				})
			}
		}
	}

	// 2. Add remaining models alphabetically
	var remainingIDs []string
	for mid := range p.Models {
		if !seen[mid] {
			remainingIDs = append(remainingIDs, mid)
		}
	}
	sort.Strings(remainingIDs)
	for _, mid := range remainingIDs {
		modelMeta := p.Models[mid]
		dName := modelMeta.DisplayName
		if dName == "" {
			dName = mid
		}
		result = append(result, &domain.ModelInfo{
			ID:          mid,
			DisplayName: dName,
			Category:    categorizeModelID(mid),
			Recommended: modelMeta.Recommended,
		})
	}

	return result, nil
}

// FetchAvailableModelsFromCloudCode queries Cloud Code PA at /v1internal:fetchAvailableModels using a Bearer token.
func FetchAvailableModelsFromCloudCode(ctx context.Context, token string) ([]*domain.ModelInfo, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("token is required")
	}

	reqBody := []byte(`{"project":"aicode-consumers"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "antigravity/cli/1.1.26 (aidev_client; os_type=linux; arch=amd64; cl=976013059; auth_method=consumer)")

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var parsed lsAvailableModelsResponse
	if unmarshalErr := json.Unmarshal(body, &parsed); unmarshalErr != nil {
		return nil, fmt.Errorf("failed to parse available models response: %w", unmarshalErr)
	}

	payload := &parsed.modelsPayload
	if parsed.Response != nil && len(parsed.Response.Models) > 0 {
		payload = parsed.Response
	}

	return extractModels(payload)
}

// DiscoverAvailableModels attempts to query available models in priority order:
// 1. Local language_server on localhost
// 2. Cloud Code PA via active token (if provided)
// 3. Fallback to DefaultModelCatalog()
func DiscoverAvailableModels(ctx context.Context, token string) []*domain.ModelInfo {
	if models, err := QueryAvailableModels(ctx); err == nil && len(models) > 0 {
		return models
	}
	if token != "" {
		if models, err := FetchAvailableModelsFromCloudCode(ctx, token); err == nil && len(models) > 0 {
			return models
		}
	}
	return DefaultModelCatalog()
}

func categorizeModelID(id string) string {
	lower := strings.ToLower(id)
	if strings.Contains(lower, "claude") || strings.Contains(lower, "gpt") || strings.Contains(lower, "sonnet") || strings.Contains(lower, "opus") || strings.Contains(lower, "haiku") || strings.Contains(lower, "3p") {
		return "claude_gpt"
	}
	if strings.Contains(lower, "gemini") || strings.Contains(lower, "gemma") || strings.Contains(lower, "flash") || strings.Contains(lower, "pro") {
		return "gemini"
	}
	return "unknown"
}

// DefaultModelCatalog provides the standard catalog of Antigravity 2.0 models.
func DefaultModelCatalog() []*domain.ModelInfo {
	return []*domain.ModelInfo{
		{ID: "gemini-3.8-flash-high", DisplayName: "Gemini 3.8 Flash (High)", Category: "gemini", Recommended: true},
		{ID: "gemini-3.8-flash-medium", DisplayName: "Gemini 3.8 Flash (Medium)", Category: "gemini", Recommended: true},
		{ID: "gemini-3.7-flash-high", DisplayName: "Gemini 3.7 Flash (High)", Category: "gemini", Recommended: true},
		{ID: "gemini-3.7-flash-medium", DisplayName: "Gemini 3.7 Flash (Medium)", Category: "gemini", Recommended: true},
		{ID: "gemini-3.6-flash-medium", DisplayName: "Gemini 3.6 Flash (Medium)", Category: "gemini", Recommended: true},
		{ID: "gemini-pro-agent", DisplayName: "Gemini 3.1 Pro (High)", Category: "gemini", Recommended: true},
		{ID: "gemini-3.1-pro-low", DisplayName: "Gemini 3.1 Pro (Low)", Category: "gemini", Recommended: true},
		{ID: "gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro", Category: "gemini", Recommended: false},
		{ID: "gemini-2.5-flash", DisplayName: "Gemini 2.5 Flash", Category: "gemini", Recommended: false},
		{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6 (Thinking)", Category: "claude_gpt", Recommended: true},
		{ID: "claude-opus-4-6-thinking", DisplayName: "Claude Opus 4.6 (Thinking)", Category: "claude_gpt", Recommended: true},
		{ID: "gpt-oss-120b-medium", DisplayName: "GPT-OSS 120B (Medium)", Category: "claude_gpt", Recommended: true},
	}
}
