package proxy_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/proxy"
)

// buildAdversarial10MBPayload constructs a 10MB JSON payload containing hundreds of
// occurrences of `"model": "claude-3-opus"` across user prompts, code blocks, escaped JSON,
// nested objects, arrays, emojis, RTL Unicode, and backslashes, with the true root model
// placed at rootModelPos ("start", "middle", or "end").
func buildAdversarial10MBPayload(rootModelPos string, rootModel string) []byte {
	var sb strings.Builder
	// Pre-grow to ~10.5 MB to avoid re-allocations during build
	sb.Grow(10500000)

	sb.WriteString("{\n")

	if rootModelPos == "start" {
		sb.WriteString(fmt.Sprintf(`  "model": %q,`+"\n", rootModel))
	}

	sb.WriteString(`  "systemInstruction": {` + "\n")
	sb.WriteString(`    "parts": [` + "\n")
	sb.WriteString(`      {"text": "System instructions with backslashes C:\\\\Windows\\\\System32\\\\ and \"model\": \"claude-3-opus\" inside"},` + "\n")
	sb.WriteString(`      {"text": "Unicode emojis 🚀 🤖 🦀 🇧🇷 and multilingual: こんにちは世界, مرحبا بالعالم, שלום עולם"},` + "\n")
	sb.WriteString(`      {"text": "Escaped quotes: \\\"model\\\": \\\"claude-3-opus\\\" and double escaped \\\\\"end"}` + "\n")
	sb.WriteString(`    ]` + "\n")
	sb.WriteString(`  },` + "\n")

	if rootModelPos == "middle" {
		sb.WriteString(fmt.Sprintf(`  "model": %q,`+"\n", rootModel))
	}

	sb.WriteString(`  "contents": [` + "\n")

	// Generate ~400 repeated items, each ~25 KB, total ~10 MB
	const numItems = 400
	const fillerChunkSize = 25000

	filler := strings.Repeat("X", fillerChunkSize)

	for i := 0; i < numItems; i++ {
		sb.WriteString(`    {` + "\n")
		sb.WriteString(`      "role": "user",` + "\n")
		sb.WriteString(`      "parts": [` + "\n")
		// 1. Nested prompt injection attempt
		sb.WriteString(fmt.Sprintf(`        {"text": "Prompt item %d injection: \"model\": \"claude-3-opus\", and fake json: {\"model\": \"claude-3-opus\"}"},`+"\n", i))
		// 2. Code snippet with markdown and nested JSON
		sb.WriteString("        {\"text\": \"```json\\n{\\n  \\\"model\\\": \\\"claude-3-opus\\\",\\n  \\\"temperature\\\": 0.7\\n}\\n```\"},\n")
		// 3. Backslash variations
		sb.WriteString(`        {"text": "Backslashes: \\, \\\\, \\\\\\, \\\\\\\\ and path: D:\\\\projects\\\\ai\\\\models\\\\claude-3-opus\\\\"},` + "\n")
		// 4. Unicode & emojis
		sb.WriteString(`        {"text": "Emojis & symbols: 🔥 💡 🎯 ⚡ 🛡️ ✨ 🧪 🧬 and RTL: اللغة العربية, עברית"},` + "\n")
		// 5. Filler text to reach 10MB
		sb.WriteString(fmt.Sprintf(`        {"text": "item_%d_%s"}`+"\n", i, filler))
		sb.WriteString(`      ],` + "\n")
		// 6. Nested object containing model key
		sb.WriteString(`      "generationConfig": {` + "\n")
		sb.WriteString(`        "model": "claude-3-opus",` + "\n")
		sb.WriteString(`        "nested": {` + "\n")
		sb.WriteString(`          "model": "claude-3-opus",` + "\n")
		sb.WriteString(`          "deepArray": [` + "\n")
		sb.WriteString(`            {"model": "claude-3-opus"},` + "\n")
		sb.WriteString(`            {"model": "claude-3-opus"}` + "\n")
		sb.WriteString(`          ]` + "\n")
		sb.WriteString(`        }` + "\n")
		sb.WriteString(`      }` + "\n")

		if i < numItems-1 {
			sb.WriteString(`    },` + "\n")
		} else {
			sb.WriteString(`    }` + "\n")
		}
	}

	sb.WriteString(`  ]` + "\n")

	if rootModelPos == "end" {
		sb.WriteString(fmt.Sprintf(`,  "model": %q`+"\n", rootModel))
	}

	sb.WriteString("}\n")

	return []byte(sb.String())
}

// ---------------------------------------------------------------------------
// 1. Stress-test ExtractModelFromJSON with 10MB adversarial payloads
// ---------------------------------------------------------------------------

func TestChallenger1_ExtractModelFromJSON_10MB_Adversarial(t *testing.T) {
	positions := []string{"start", "middle", "end"}
	const expectedModel = "gemini-2.5-pro"

	for _, pos := range positions {
		t.Run("RootModelAt_"+pos, func(t *testing.T) {
			payload := buildAdversarial10MBPayload(pos, expectedModel)
			t.Logf("Generated adversarial payload for pos=%s: %d bytes (%.2f MB)", pos, len(payload), float64(len(payload))/(1024*1024))

			// Verify payload contains hundreds of "claude-3-opus"
			claudeCount := bytes.Count(payload, []byte("claude-3-opus"))
			if claudeCount < 1000 {
				t.Fatalf("Expected at least 1000 occurrences of 'claude-3-opus', got %d", claudeCount)
			}
			t.Logf("Verified payload contains %d occurrences of 'claude-3-opus' in prompts and nested objects", claudeCount)

			// Extract model
			model, err := proxy.ExtractModelFromJSON(payload)
			if err != nil {
				t.Fatalf("ExtractModelFromJSON failed: %v", err)
			}

			// MUST extract root model and NOT any of the inner claude-3-opus
			if model != expectedModel {
				t.Fatalf("Extracted wrong model! got %q, want %q", model, expectedModel)
			}

			// Programmatic zero-allocation verification
			var extracted string
			allocs := testing.AllocsPerRun(20, func() {
				extracted, err = proxy.ExtractModelFromJSON(payload)
			})
			if err != nil {
				t.Fatalf("AllocsPerRun ExtractModelFromJSON failed: %v", err)
			}
			if extracted != expectedModel {
				t.Fatalf("AllocsPerRun extracted wrong model: got %q, want %q", extracted, expectedModel)
			}
			if allocs != 0 {
				t.Fatalf("CRITICAL ZERO-ALLOCATION VIOLATION: expected 0 allocs/op, got %v allocs/op on 10MB adversarial payload", allocs)
			}
			t.Logf("Extraction on 10MB payload (%s) succeeded with strictly 0 allocs/op!", pos)
		})
	}
}

// ---------------------------------------------------------------------------
// 2. Stress-test RewriteModelInBody with 10MB adversarial payloads & Immutability
// ---------------------------------------------------------------------------

func TestChallenger1_RewriteModelInBody_10MB_Adversarial_And_Immutability(t *testing.T) {
	positions := []string{"start", "middle", "end"}
	const origModel = "gemini-2.5-pro"
	const targetModel = "claude-3-7-sonnet"

	for _, pos := range positions {
		t.Run("Rewrite_RootModelAt_"+pos, func(t *testing.T) {
			payload := buildAdversarial10MBPayload(pos, origModel)
			origLen := len(payload)

			// Compute SHA256 of original input before rewriting
			hBefore := sha256.Sum256(payload)
			hashBefore := hex.EncodeToString(hBefore[:])

			// Rewrite model in 10MB body
			rewritten, err := proxy.RewriteModelInBody(payload, targetModel)
			if err != nil {
				t.Fatalf("RewriteModelInBody failed: %v", err)
			}

			// 1. Immutability Check: verify original slice was NEVER modified
			hAfter := sha256.Sum256(payload)
			hashAfter := hex.EncodeToString(hAfter[:])
			if hashBefore != hashAfter {
				t.Fatalf("CRITICAL IMMUTABILITY VIOLATION: Original payload buffer was mutated! before=%s, after=%s", hashBefore, hashAfter)
			}
			if len(payload) != origLen {
				t.Fatalf("CRITICAL IMMUTABILITY VIOLATION: Original payload length changed! before=%d, after=%d", origLen, len(payload))
			}

			// 2. Length Delta Verification
			deltaExpected := len(targetModel) - len(origModel)
			if len(rewritten) != origLen+deltaExpected {
				t.Fatalf("Rewritten length mismatch: got %d, want %d (orig %d + delta %d)", len(rewritten), origLen+deltaExpected, origLen, deltaExpected)
			}

			// 3. Extract model from rewritten body: MUST be targetModel
			extracted, err := proxy.ExtractModelFromJSON(rewritten)
			if err != nil {
				t.Fatalf("Failed to extract model from rewritten payload: %v", err)
			}
			if extracted != targetModel {
				t.Fatalf("Rewritten payload model mismatch: got %q, want %q", extracted, targetModel)
			}

			// 4. Verify that internal prompt occurrences of "claude-3-opus" were NOT corrupted
			claudeCountBefore := bytes.Count(payload, []byte("claude-3-opus"))
			claudeCountAfter := bytes.Count(rewritten, []byte("claude-3-opus"))
			if claudeCountBefore != claudeCountAfter {
				t.Fatalf("Nested occurrences altered! countBefore=%d, countAfter=%d", claudeCountBefore, claudeCountAfter)
			}

			// 5. Verify valid JSON parse on a sample slice or structural check
			var parsed map[string]json.RawMessage
			if err := json.Unmarshal(rewritten, &parsed); err != nil {
				t.Fatalf("Rewritten 10MB payload is invalid JSON: %v", err)
			}

			var rootModelVal string
			if err := json.Unmarshal(parsed["model"], &rootModelVal); err != nil {
				t.Fatalf("Failed to parse root model from JSON: %v", err)
			}
			if rootModelVal != targetModel {
				t.Fatalf("Parsed root model mismatch: got %q, want %q", rootModelVal, targetModel)
			}

			// 6. Programmatic single-allocation verification
			var rewr []byte
			allocs := testing.AllocsPerRun(10, func() {
				rewr, err = proxy.RewriteModelInBody(payload, targetModel)
			})
			if err != nil {
				t.Fatalf("AllocsPerRun RewriteModelInBody failed: %v", err)
			}
			if len(rewr) == 0 {
				t.Fatalf("rewritten slice is empty")
			}
			if allocs > 1.0 {
				t.Fatalf("CRITICAL SINGLE-ALLOCATION VIOLATION: expected <= 1.0 alloc/op, got %v allocs/op on 10MB payload (%s)", allocs, pos)
			}
			t.Logf("Rewrite on 10MB payload (%s) succeeded with %.1f allocs/op and perfect immutability!", pos, allocs)
		})
	}
}

// ---------------------------------------------------------------------------
// 3. Adversarial Escape Sequences & Backslash Parity Suite
// ---------------------------------------------------------------------------

func TestChallenger1_AdversarialEscapeSequences(t *testing.T) {
	testCases := []struct {
		name        string
		jsonBody    string
		expectModel string
		expectError bool
	}{
		{
			name:        "Zero slashes before quote",
			jsonBody:    `{"prompt": "clean text", "model": "gemini-2.5-pro"}`,
			expectModel: "gemini-2.5-pro",
		},
		{
			name:        "1 slash (escaped quote)",
			jsonBody:    `{"prompt": "escaped \" quote", "model": "gemini-2.5-pro"}`,
			expectModel: "gemini-2.5-pro",
		},
		{
			name:        "2 slashes (literal slash + unescaped quote)",
			jsonBody:    `{"prompt": "Windows path C:\\", "model": "gemini-2.5-pro"}`,
			expectModel: "gemini-2.5-pro",
		},
		{
			name:        "3 slashes (literal slash + escaped quote)",
			jsonBody:    `{"prompt": "Path with quote C:\\\"something", "model": "gemini-2.5-pro"}`,
			expectModel: "gemini-2.5-pro",
		},
		{
			name:        "4 slashes (2 literal slashes + unescaped quote)",
			jsonBody:    `{"prompt": "Two slashes \\\\", "model": "gemini-2.5-pro"}`,
			expectModel: "gemini-2.5-pro",
		},
		{
			name:        "5 slashes (2 literal slashes + escaped quote)",
			jsonBody:    `{"prompt": "Two slashes and quote \\\\\"nested", "model": "gemini-2.5-pro"}`,
			expectModel: "gemini-2.5-pro",
		},
		{
			name:        "6 slashes (3 literal slashes + unescaped quote)",
			jsonBody:    `{"prompt": "Three slashes \\\\\\", "model": "gemini-2.5-pro"}`,
			expectModel: "gemini-2.5-pro",
		},
		{
			name:        "Trailing backslash at end of model value itself",
			jsonBody:    `{"model": "gemini-2.5-pro\\\\"}`,
			expectModel: `gemini-2.5-pro\\\\`,
		},
		{
			name:        "Nested escaped JSON containing root-like model string",
			jsonBody:    `{"text": "{\"model\": \"claude-3-opus\"}", "model": "gemini-2.5-flash"}`,
			expectModel: "gemini-2.5-flash",
		},
		{
			name:        "Prompt injection with fake braces and model key",
			jsonBody:    `{"prompt": "}\n,\n\"model\": \"fake-model\"\n{\n", "model": "gemini-2.5-pro"}`,
			expectModel: "gemini-2.5-pro",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			origBytes := []byte(tc.jsonBody)
			origCopy := make([]byte, len(origBytes))
			copy(origCopy, origBytes)

			model, err := proxy.ExtractModelFromJSON(origBytes)
			if tc.expectError {
				if err == nil {
					t.Fatalf("expected error, got nil (model=%q)", model)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if model != tc.expectModel {
				t.Fatalf("got model %q, want %q", model, tc.expectModel)
			}

			// Immutability check on extract
			if !bytes.Equal(origBytes, origCopy) {
				t.Fatalf("ExtractModelFromJSON mutated the input buffer!")
			}

			// Test rewriting
			rewritten, err := proxy.RewriteModelInBody(origBytes, "claude-3-7-sonnet")
			if err != nil {
				t.Fatalf("RewriteModelInBody failed: %v", err)
			}

			// Immutability check on rewrite
			if !bytes.Equal(origBytes, origCopy) {
				t.Fatalf("RewriteModelInBody mutated the input buffer!")
			}

			newModel, err := proxy.ExtractModelFromJSON(rewritten)
			if err != nil {
				t.Fatalf("ExtractModelFromJSON on rewritten failed: %v", err)
			}
			if newModel != "claude-3-7-sonnet" {
				t.Fatalf("Rewritten model mismatch: got %q, want %q", newModel, "claude-3-7-sonnet")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4. Concurrency Safety & Race Stress Harness
// ---------------------------------------------------------------------------

func TestChallenger1_ConcurrencySafety_Race(t *testing.T) {
	const goroutines = 100
	const iterations = 50

	// Use a 1MB adversarial payload shared across all goroutines
	payload := buildAdversarial10MBPayload("middle", "gemini-2.5-pro")[:1024*1024]
	origHash := sha256.Sum256(payload)

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		workerID := g
		go func() {
			defer wg.Done()

			for it := 0; it < iterations; it++ {
				if workerID%2 == 0 {
					// Reader goroutine: extract model
					model, err := proxy.ExtractModelFromJSON(payload)
					if err != nil {
						t.Errorf("worker %d extract error: %v", workerID, err)
						return
					}
					if model != "gemini-2.5-pro" {
						t.Errorf("worker %d extract wrong model: %s", workerID, model)
						return
					}
				} else {
					// Writer/Rewriter goroutine: rewrite model to target
					target := fmt.Sprintf("claude-target-%d", workerID%5)
					rewritten, err := proxy.RewriteModelInBody(payload, target)
					if err != nil {
						t.Errorf("worker %d rewrite error: %v", workerID, err)
						return
					}
					extracted, err := proxy.ExtractModelFromJSON(rewritten)
					if err != nil {
						t.Errorf("worker %d extract rewritten error: %v", workerID, err)
						return
					}
					if extracted != target {
						t.Errorf("worker %d extracted %q, want %q", workerID, extracted, target)
						return
					}
				}
			}
		}()
	}

	wg.Wait()

	// Verify shared buffer was never modified by any of the 100 concurrent goroutines
	afterHash := sha256.Sum256(payload)
	if origHash != afterHash {
		t.Fatalf("CRITICAL CONCURRENCY VIOLATION: Shared payload buffer was mutated by concurrent goroutines!")
	}
	t.Logf("Concurrency safety verified: 100 goroutines * 50 iterations = 5000 ops with 0 races and perfect immutability!")
}

// ---------------------------------------------------------------------------
// 5. Malformed & Truncated Fuzzing
// ---------------------------------------------------------------------------

func TestChallenger1_Fuzz_TruncatedInputs(t *testing.T) {
	baseJSON := []byte(`{"model":"gemini-2.5-pro","contents":[{"role":"user","parts":[{"text":"hello world"}]}]}`)

	// Test truncation at every single byte index from 0 to len(baseJSON)
	for i := 0; i <= len(baseJSON); i++ {
		truncated := baseJSON[:i]

		// Must never panic!
		model, err := proxy.ExtractModelFromJSON(truncated)
		if i < len(`{"model":"gemini-2.5-pro"`) {
			if err == nil && model == "gemini-2.5-pro" {
				t.Fatalf("unexpected success on truncated prefix of len %d: model=%q", i, model)
			}
		}

		// Rewrite on truncated input must never panic
		_, _ = proxy.RewriteModelInBody(truncated, "claude-3-7-sonnet")
	}
}
