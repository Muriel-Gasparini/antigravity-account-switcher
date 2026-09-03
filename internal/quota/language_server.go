package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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

// QueryLocalLanguageServer queries the running language_server process on localhost
// for real model quota groups and buckets.
func QueryLocalLanguageServer(ctx context.Context, accountID string) ([]*domain.QuotaBucket, error) {
	csrf, ports, err := FindLocalLanguageServer()
	if err != nil {
		return nil, err
	}

	client := &http.Client{
		Timeout: 2500 * time.Millisecond,
		Transport: &http.Transport{
			// Bypass any ambient HTTP_PROXY
			Proxy: nil,
		},
	}

	var rawResp []byte
	var successfulPort int

	for _, port := range ports {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		url := fmt.Sprintf("http://127.0.0.1:%d/exa.language_server_pb.LanguageServerService/RetrieveUserQuotaSummary", port)
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(`{"forceRefresh":true}`)))
		if reqErr != nil {
			continue
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-codeium-csrf-token", csrf)

		resp, doErr := client.Do(req)
		if doErr != nil {
			continue
		}

		if resp.StatusCode == http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if len(body) > 0 && strings.Contains(string(body), "groups") {
				rawResp = body
				successfulPort = port
				break
			}
		} else {
			resp.Body.Close()
		}
	}

	if len(rawResp) == 0 {
		return nil, fmt.Errorf("no language_server listening port responded to RetrieveUserQuotaSummary")
	}

	_ = successfulPort

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
