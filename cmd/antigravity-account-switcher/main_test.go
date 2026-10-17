package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
)

func captureStdout(f func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	f()

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

func TestCLI_PrintUsage(t *testing.T) {
	out := captureStdout(func() {
		printUsage()
	})

	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage output to contain 'Usage:', got: %s", out)
	}
	if !strings.Contains(out, "serve") || !strings.Contains(out, "wrap") {
		t.Errorf("expected usage output to describe subcommands, got: %s", out)
	}
}

func TestCLI_RunVersion(t *testing.T) {
	out := captureStdout(func() {
		runVersion()
	})

	if !strings.Contains(out, "antigravity-account-switcher") {
		t.Errorf("expected version output, got: %s", out)
	}
}

func TestCLI_DefaultDBPath(t *testing.T) {
	orig := os.Getenv("ANTIGRAVITY_DB_PATH")
	defer os.Setenv("ANTIGRAVITY_DB_PATH", orig)

	os.Setenv("ANTIGRAVITY_DB_PATH", "/tmp/custom-test.db")
	if defaultDBPath() != "/tmp/custom-test.db" {
		t.Errorf("expected /tmp/custom-test.db, got %s", defaultDBPath())
	}
}

func TestCLI_RunStatus_EmptyDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "status_test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	_ = db.Close()

	out := captureStdout(func() {
		runStatus([]string{"-db", dbPath})
	})

	if !strings.Contains(out, "Switcher Status: OK") {
		t.Errorf("expected status OK, got: %s", out)
	}
	if !strings.Contains(out, "None selected") {
		t.Errorf("expected no active account message, got: %s", out)
	}
}

func TestCLI_RunListAccounts_Empty(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "list_test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	_ = db.Close()

	out := captureStdout(func() {
		runListAccounts([]string{"-db", dbPath})
	})

	if !strings.Contains(out, "No accounts registered yet") {
		t.Errorf("expected no accounts message, got: %s", out)
	}
}

func TestCLI_RunConfig(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)

	// Test config list
	outList := captureStdout(func() {
		runConfig([]string{"list"})
	})
	if !strings.Contains(outList, "Configuration file:") {
		t.Errorf("expected config list output, got: %s", outList)
	}

	// Test config set
	outSet := captureStdout(func() {
		runConfig([]string{"set", "port", "9099"})
	})
	if !strings.Contains(outSet, "Updated 'port' to '9099'") {
		t.Errorf("expected config set confirmation, got: %s", outSet)
	}

	// Test config get
	outGet := captureStdout(func() {
		runConfig([]string{"get", "port"})
	})
	if !strings.Contains(outGet, "9099") {
		t.Errorf("expected 9099 from get port, got: %s", outGet)
	}
}

func TestCLI_RunInstallDesktop(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	fakeBin := filepath.Join(tmpDir, "fake-antigravity")
	_ = os.WriteFile(fakeBin, []byte("#!/bin/sh\n"), 0o755)

	out := captureStdout(func() {
		runInstallDesktop([]string{"-bin", fakeBin})
	})

	if !strings.Contains(out, "Success! Antigravity desktop application installed") {
		t.Errorf("expected installation success, got: %s", out)
	}
}

func TestExtractCanonicalFamilyAndWindow(t *testing.T) {
	tests := []struct {
		name       string
		bucket     *domain.QuotaBucket
		wantFamily string
		wantWindow string
	}{
		{
			name: "gemini weekly with parentheses",
			bucket: &domain.QuotaBucket{
				AccountID:   "acc-1",
				BucketID:    "gemini-weekly",
				DisplayName: "Gemini Models (weekly)",
				Window:      "weekly",
			},
			wantFamily: "Gemini Models",
			wantWindow: "weekly",
		},
		{
			name: "claude 5h with parentheses",
			bucket: &domain.QuotaBucket{
				AccountID:   "acc-1",
				BucketID:    "acc-1-3p-5h",
				DisplayName: "Claude and GPT models (5h)",
				Window:      "5h",
			},
			wantFamily: "Claude and GPT models",
			wantWindow: "5h",
		},
		{
			name: "brackets window suffix",
			bucket: &domain.QuotaBucket{
				AccountID:   "acc-1",
				BucketID:    "custom-1",
				DisplayName: "DeepSeek Coder [10m]",
				Window:      "10m",
			},
			wantFamily: "DeepSeek Coder",
			wantWindow: "10m",
		},
		{
			name: "hyphen window suffix",
			bucket: &domain.QuotaBucket{
				AccountID:   "acc-1",
				BucketID:    "llama-daily",
				DisplayName: "Llama Models - daily",
				Window:      "daily",
			},
			wantFamily: "Llama Models",
			wantWindow: "daily",
		},
		{
			name: "empty display name strips account ID and window from bucketID",
			bucket: &domain.QuotaBucket{
				AccountID: "acc-1",
				BucketID:  "acc-1-custom_model-weekly",
				Window:    "weekly",
			},
			wantFamily: "custom_model",
			wantWindow: "weekly",
		},
		{
			name: "no window suffix in display name",
			bucket: &domain.QuotaBucket{
				AccountID:   "acc-1",
				BucketID:    "gemini-pro",
				DisplayName: "Gemini 2.5 Pro",
				Window:      "5h",
			},
			wantFamily: "Gemini 2.5 Pro",
			wantWindow: "5h",
		},
		{
			name: "window extracted from parenthesized display name if window empty",
			bucket: &domain.QuotaBucket{
				AccountID:   "acc-1",
				DisplayName: "Mistral Large (12h)",
			},
			wantFamily: "Mistral Large",
			wantWindow: "12h",
		},
		{
			name: "preserves non-window parenthetical model name when window matches suffix",
			bucket: &domain.QuotaBucket{
				AccountID:   "acc-1",
				DisplayName: "Gemini 1.5 Flash (Experimental) (5h)",
				Window:      "5h",
			},
			wantFamily: "Gemini 1.5 Flash (Experimental)",
			wantWindow: "5h",
		},
		{
			name: "preserves parenthetical in model name when no window suffix is present",
			bucket: &domain.QuotaBucket{
				AccountID:   "acc-1",
				DisplayName: "Claude (Beta)",
				Window:      "5h",
			},
			wantFamily: "Claude (Beta)",
			wantWindow: "5h",
		},
		{
			name:       "nil bucket fallback",
			bucket:     nil,
			wantFamily: "Default",
			wantWindow: "quota",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotFamily, gotWindow := extractCanonicalFamilyAndWindow(tc.bucket)
			if gotFamily != tc.wantFamily {
				t.Errorf("extractCanonicalFamilyAndWindow() family = %q, want %q", gotFamily, tc.wantFamily)
			}
			if gotWindow != tc.wantWindow {
				t.Errorf("extractCanonicalFamilyAndWindow() window = %q, want %q", gotWindow, tc.wantWindow)
			}
		})
	}
}

func TestCLI_RunListAccounts_DynamicRendering(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "dynamic_list_test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	accRepo := sqlite.NewAccountRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)

	acc1 := &domain.Account{
		ID:        "acc-001-aaaa",
		Email:     "user1@example.com",
		Status:    domain.AccountStatusActive,
		IsActive:  true,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	acc2 := &domain.Account{
		ID:        "acc-002-bbbb",
		Email:     "user2@example.com",
		Status:    domain.AccountStatusExhausted,
		IsActive:  false,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	acc3 := &domain.Account{
		ID:        "acc-003-cccc",
		Email:     "user3@example.com",
		Status:    domain.AccountStatusActive,
		IsActive:  false,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	if err := accRepo.Create(ctx, acc1); err != nil {
		t.Fatalf("accRepo.Create acc1 failed: %v", err)
	}
	if err := accRepo.Create(ctx, acc2); err != nil {
		t.Fatalf("accRepo.Create acc2 failed: %v", err)
	}
	if err := accRepo.Create(ctx, acc3); err != nil {
		t.Fatalf("accRepo.Create acc3 failed: %v", err)
	}

	buckets := []*domain.QuotaBucket{
		// acc1: Full quota in Claude and Gemini
		{
			AccountID:         acc1.ID,
			BucketID:          "claude-5h",
			DisplayName:       "Claude and GPT models (5h)",
			Window:            "5h",
			RemainingFraction: 1.0,
			UpdatedAt:         time.Now().UTC(),
		},
		{
			AccountID:         acc1.ID,
			BucketID:          "claude-weekly",
			DisplayName:       "Claude and GPT models (weekly)",
			Window:            "weekly",
			RemainingFraction: 1.0,
			UpdatedAt:         time.Now().UTC(),
		},
		{
			AccountID:         acc1.ID,
			BucketID:          "gemini-5h",
			DisplayName:       "Gemini Models (5h)",
			Window:            "5h",
			RemainingFraction: 0.85,
			UpdatedAt:         time.Now().UTC(),
		},
		{
			AccountID:         acc1.ID,
			BucketID:          "gemini-weekly",
			DisplayName:       "Gemini Models (weekly)",
			Window:            "weekly",
			RemainingFraction: 0.90,
			UpdatedAt:         time.Now().UTC(),
		},

		// acc2: Exhausted Claude weekly, but healthy Gemini
		{
			AccountID:         acc2.ID,
			BucketID:          "claude-5h-2",
			DisplayName:       "Claude and GPT models (5h)",
			Window:            "5h",
			RemainingFraction: 1.0,
			UpdatedAt:         time.Now().UTC(),
		},
		{
			AccountID:         acc2.ID,
			BucketID:          "claude-weekly-2",
			DisplayName:       "Claude and GPT models (weekly)",
			Window:            "weekly",
			RemainingFraction: 0.0,
			UpdatedAt:         time.Now().UTC(),
		},
		{
			AccountID:         acc2.ID,
			BucketID:          "gemini-5h-2",
			DisplayName:       "Gemini Models (5h)",
			Window:            "5h",
			RemainingFraction: 1.0,
			UpdatedAt:         time.Now().UTC(),
		},
		{
			AccountID:         acc2.ID,
			BucketID:          "gemini-weekly-2",
			DisplayName:       "Gemini Models (weekly)",
			Window:            "weekly",
			RemainingFraction: 1.0,
			UpdatedAt:         time.Now().UTC(),
		},

		// acc3: Only has a custom third-party model family (e.g. DeepSeek Coder)
		{
			AccountID:         acc3.ID,
			BucketID:          "deepseek-hourly",
			DisplayName:       "DeepSeek Coder (hourly)",
			Window:            "hourly",
			RemainingFraction: 0.50,
			UpdatedAt:         time.Now().UTC(),
		},
	}

	if err := quotaRepo.UpsertBuckets(ctx, buckets); err != nil {
		t.Fatalf("quotaRepo.UpsertBuckets failed: %v", err)
	}

	out := captureStdout(func() {
		runListAccounts([]string{"-db", dbPath})
	})

	// Check table headers
	if !strings.Contains(out, "CLAUDE AND GPT MODELS") {
		t.Errorf("expected header to contain 'CLAUDE AND GPT MODELS', got:\n%s", out)
	}
	if !strings.Contains(out, "GEMINI MODELS") {
		t.Errorf("expected header to contain 'GEMINI MODELS', got:\n%s", out)
	}
	if !strings.Contains(out, "DEEPSEEK CODER") {
		t.Errorf("expected header to contain dynamically discovered 'DEEPSEEK CODER', got:\n%s", out)
	}

	// Check acc1 values
	if !strings.Contains(out, "5h: 100%, weekly: 100%") {
		t.Errorf("expected acc1 Claude quota '5h: 100%%, weekly: 100%%', got:\n%s", out)
	}
	if !strings.Contains(out, "5h: 85%, weekly: 90%") {
		t.Errorf("expected acc1 Gemini quota '5h: 85%%, weekly: 90%%', got:\n%s", out)
	}

	// Check acc2: status should be partial because Claude is exhausted but Gemini has healthy quota
	lines := strings.Split(out, "\n")
	var acc2Line string
	for _, l := range lines {
		if strings.Contains(l, "user2@example.com") {
			acc2Line = l
			break
		}
	}
	if acc2Line == "" {
		t.Fatalf("could not find user2 line in output:\n%s", out)
	}
	if !strings.Contains(acc2Line, "partial") {
		t.Errorf("expected acc2 status to be 'partial', got line: %s", acc2Line)
	}
	if !strings.Contains(acc2Line, "5h: 100%, weekly: 0%") {
		t.Errorf("expected acc2 Claude quota '5h: 100%%, weekly: 0%%', got line: %s", acc2Line)
	}

	// Check acc3: DeepSeek hourly quota present, other families show N/A
	var acc3Line string
	for _, l := range lines {
		if strings.Contains(l, "user3@example.com") {
			acc3Line = l
			break
		}
	}
	if acc3Line == "" {
		t.Fatalf("could not find user3 line in output:\n%s", out)
	}
	if !strings.Contains(acc3Line, "hourly: 50%") {
		t.Errorf("expected acc3 to display 'hourly: 50%%', got line: %s", acc3Line)
	}
	if !strings.Contains(acc3Line, "N/A") {
		t.Errorf("expected acc3 to display 'N/A' for missing families, got line: %s", acc3Line)
	}
}

func TestCLI_RunListAccounts_StatusExhaustedWhenAllFamiliesExhausted(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "exhausted_list_test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	accRepo := sqlite.NewAccountRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)

	acc := &domain.Account{
		ID:        "acc-fully-exhausted",
		Email:     "dead@example.com",
		Status:    domain.AccountStatusExhausted,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := accRepo.Create(ctx, acc); err != nil {
		t.Fatalf("accRepo.Create failed: %v", err)
	}

	buckets := []*domain.QuotaBucket{
		{
			AccountID:         acc.ID,
			BucketID:          "claude-5h",
			DisplayName:       "Claude and GPT models (5h)",
			Window:            "5h",
			RemainingFraction: 0.0,
			UpdatedAt:         time.Now().UTC(),
		},
		{
			AccountID:         acc.ID,
			BucketID:          "gemini-5h",
			DisplayName:       "Gemini Models (5h)",
			Window:            "5h",
			RemainingFraction: 0.0,
			UpdatedAt:         time.Now().UTC(),
		},
	}
	if err := quotaRepo.UpsertBuckets(ctx, buckets); err != nil {
		t.Fatalf("quotaRepo.UpsertBuckets failed: %v", err)
	}

	out := captureStdout(func() {
		runListAccounts([]string{"-db", dbPath})
	})

	if !strings.Contains(out, "exhausted") {
		t.Errorf("expected account status to remain 'exhausted' when all families exhausted, got:\n%s", out)
	}
	if strings.Contains(out, "partial") {
		t.Errorf("did not expect 'partial' when all families are 0.0, got:\n%s", out)
	}
}

func TestCLI_RunListAccounts_FamilyCasingNormalization(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "casing_test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	accRepo := sqlite.NewAccountRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)

	acc1 := &domain.Account{
		ID:        "acc-case-1",
		Email:     "user1@example.com",
		Status:    domain.AccountStatusActive,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	acc2 := &domain.Account{
		ID:        "acc-case-2",
		Email:     "user2@example.com",
		Status:    domain.AccountStatusActive,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := accRepo.Create(ctx, acc1); err != nil {
		t.Fatalf("accRepo.Create 1 failed: %v", err)
	}
	if err := accRepo.Create(ctx, acc2); err != nil {
		t.Fatalf("accRepo.Create 2 failed: %v", err)
	}

	// acc1 has "Claude and GPT models" (lowercase m), acc2 has "Claude and GPT Models" (uppercase M)
	buckets := []*domain.QuotaBucket{
		{
			AccountID:         acc1.ID,
			BucketID:          "claude-5h",
			DisplayName:       "Claude and GPT models (5h)",
			Window:            "5h",
			RemainingFraction: 1.0,
			UpdatedAt:         time.Now().UTC(),
		},
		{
			AccountID:         acc2.ID,
			BucketID:          "claude-weekly",
			DisplayName:       "Claude and GPT Models (weekly)",
			Window:            "weekly",
			RemainingFraction: 0.8,
			UpdatedAt:         time.Now().UTC(),
		},
	}
	if err := quotaRepo.UpsertBuckets(ctx, buckets); err != nil {
		t.Fatalf("quotaRepo.UpsertBuckets failed: %v", err)
	}

	out := captureStdout(func() {
		runListAccounts([]string{"-db", dbPath})
	})

	// Header line must contain "CLAUDE AND GPT MODELS" exactly once, not twice
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines of output, got:\n%s", out)
	}
	headerLine := lines[0]
	if count := strings.Count(headerLine, "CLAUDE AND GPT MODELS"); count != 1 {
		t.Errorf("expected 'CLAUDE AND GPT MODELS' header to appear exactly once, appeared %d times in header: %q", count, headerLine)
	}

	if !strings.Contains(out, "5h: 100%") {
		t.Errorf("expected acc1 quota '5h: 100%%', got:\n%s", out)
	}
	if !strings.Contains(out, "weekly: 80%") {
		t.Errorf("expected acc2 quota 'weekly: 80%%', got:\n%s", out)
	}
}

func TestCLI_RunListAccounts_StatusExhaustedWhenSingleFamilyHealthy(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "single_fam_test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	accRepo := sqlite.NewAccountRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)

	acc := &domain.Account{
		ID:        "acc-single-family",
		Email:     "single@example.com",
		Status:    domain.AccountStatusExhausted,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := accRepo.Create(ctx, acc); err != nil {
		t.Fatalf("accRepo.Create failed: %v", err)
	}

	// Only 1 model family exists for this account
	buckets := []*domain.QuotaBucket{
		{
			AccountID:         acc.ID,
			BucketID:          "gemini-5h",
			DisplayName:       "Gemini Models (5h)",
			Window:            "5h",
			RemainingFraction: 1.0,
			UpdatedAt:         time.Now().UTC(),
		},
		{
			AccountID:         acc.ID,
			BucketID:          "gemini-weekly",
			DisplayName:       "Gemini Models (weekly)",
			Window:            "weekly",
			RemainingFraction: 1.0,
			UpdatedAt:         time.Now().UTC(),
		},
	}
	if err := quotaRepo.UpsertBuckets(ctx, buckets); err != nil {
		t.Fatalf("quotaRepo.UpsertBuckets failed: %v", err)
	}

	out := captureStdout(func() {
		runListAccounts([]string{"-db", dbPath})
	})

	// Since there is NO other model family, it cannot be 'partial'
	if strings.Contains(out, "partial") {
		t.Errorf("did not expect 'partial' when account only has one model family, got:\n%s", out)
	}
	if !strings.Contains(out, "exhausted") {
		t.Errorf("expected status 'exhausted', got:\n%s", out)
	}
}

func TestCLI_RunListAccounts_UTF8Alignment(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "utf8_align_test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	accRepo := sqlite.NewAccountRepository(db)
	quotaRepo := sqlite.NewQuotaRepository(db)

	acc := &domain.Account{
		ID:        "acc-utf8",
		Email:     "üser-tëst@example.com",
		Status:    domain.AccountStatusActive,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := accRepo.Create(ctx, acc); err != nil {
		t.Fatalf("accRepo.Create failed: %v", err)
	}

	buckets := []*domain.QuotaBucket{
		{
			AccountID:         acc.ID,
			BucketID:          "glm-5h",
			DisplayName:       "智谱清言 GLM (5h)",
			Window:            "5h",
			RemainingFraction: 1.0,
			UpdatedAt:         time.Now().UTC(),
		},
	}
	if err := quotaRepo.UpsertBuckets(ctx, buckets); err != nil {
		t.Fatalf("quotaRepo.UpsertBuckets failed: %v", err)
	}

	out := captureStdout(func() {
		runListAccounts([]string{"-db", dbPath})
	})

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 lines of output, got:\n%s", out)
	}
	headerLine := lines[0]
	separatorLine := lines[1]
	rowLine := lines[2]

	if utf8.RuneCountInString(headerLine) != utf8.RuneCountInString(separatorLine) {
		t.Errorf("header rune length (%d) != separator rune length (%d)",
			utf8.RuneCountInString(headerLine), utf8.RuneCountInString(separatorLine))
	}
	if !strings.Contains(headerLine, "智谱清言 GLM") {
		t.Errorf("expected header to contain '智谱清言 GLM', got: %s", headerLine)
	}
	if !strings.Contains(rowLine, "üser-tëst@example.com") {
		t.Errorf("expected row to contain 'üser-tëst@example.com', got: %s", rowLine)
	}
}
