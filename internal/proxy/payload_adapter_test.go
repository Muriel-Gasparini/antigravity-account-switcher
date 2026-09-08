package proxy_test

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/proxy"
)

// ---------------------------------------------------------------------------
// 1. MutateParts & Accessors Unit Tests
// ---------------------------------------------------------------------------

func TestGetRequestMap(t *testing.T) {
	// Nil document
	if req, ok := proxy.GetRequestMap(nil); ok || req != nil {
		t.Errorf("expected false and nil for nil doc, got ok=%v, req=%v", ok, req)
	}

	// Doc without request key
	doc := map[string]any{"model": "gemini-2.5-pro"}
	if req, ok := proxy.GetRequestMap(doc); ok || req != nil {
		t.Errorf("expected false for missing request, got ok=%v", ok)
	}

	// Doc with non-map request
	doc["request"] = "not-a-map"
	if req, ok := proxy.GetRequestMap(doc); ok || req != nil {
		t.Errorf("expected false for non-map request, got ok=%v", ok)
	}

	// Doc with valid map request
	inner := map[string]any{"contents": []any{}}
	doc["request"] = inner
	req, ok := proxy.GetRequestMap(doc)
	if !ok || req == nil {
		t.Fatalf("expected true and valid map, got ok=%v", ok)
	}
	if _, hasContents := req["contents"]; !hasContents {
		t.Errorf("expected contents in retrieved request map")
	}
}

func TestGetGenerationConfig(t *testing.T) {
	// Nil input
	if cfg, ok := proxy.GetGenerationConfig(nil); ok || cfg != nil {
		t.Errorf("expected false for nil map, got ok=%v", ok)
	}

	// Direct generationConfig
	req := map[string]any{
		"generationConfig": map[string]any{"maxOutputTokens": 64000},
	}
	cfg, ok := proxy.GetGenerationConfig(req)
	if !ok || cfg == nil || cfg["maxOutputTokens"] != 64000 {
		t.Errorf("failed to retrieve direct generationConfig: %v", cfg)
	}

	// Nested inside request
	doc := map[string]any{
		"request": req,
	}
	cfgDoc, ok := proxy.GetGenerationConfig(doc)
	if !ok || cfgDoc == nil || cfgDoc["maxOutputTokens"] != 64000 {
		t.Errorf("failed to retrieve nested generationConfig: %v", cfgDoc)
	}

	// Missing generationConfig
	empty := map[string]any{"other": 123}
	if _, ok := proxy.GetGenerationConfig(empty); ok {
		t.Errorf("expected false for missing generationConfig")
	}
}

func TestGetLabels(t *testing.T) {
	// Nil input
	if labels, ok := proxy.GetLabels(nil); ok || labels != nil {
		t.Errorf("expected false for nil map, got ok=%v", ok)
	}

	// Direct labels
	req := map[string]any{
		"labels": map[string]any{"used_claude": "true"},
	}
	labels, ok := proxy.GetLabels(req)
	if !ok || labels == nil || labels["used_claude"] != "true" {
		t.Errorf("failed to retrieve direct labels: %v", labels)
	}

	// Nested inside request
	doc := map[string]any{
		"request": req,
	}
	labelsDoc, ok := proxy.GetLabels(doc)
	if !ok || labelsDoc == nil || labelsDoc["used_claude"] != "true" {
		t.Errorf("failed to retrieve nested labels: %v", labelsDoc)
	}

	// Missing labels
	empty := map[string]any{"other": 123}
	if _, ok := proxy.GetLabels(empty); ok {
		t.Errorf("expected false for missing labels")
	}
}

func TestMutateParts(t *testing.T) {
	// 1. Nil inputs
	if proxy.MutateParts(nil, nil) {
		t.Errorf("expected false for nil doc and fn")
	}
	if proxy.MutateParts(map[string]any{}, nil) {
		t.Errorf("expected false for nil fn")
	}
	if proxy.MutateParts(nil, func(role string, part map[string]any) (bool, bool) { return true, false }) {
		t.Errorf("expected false for nil doc")
	}

	// 2. Doc without contents
	emptyDoc := map[string]any{"model": "gemini-pro"}
	if proxy.MutateParts(emptyDoc, func(role string, part map[string]any) (bool, bool) { return true, false }) {
		t.Errorf("expected false for doc without contents")
	}

	// 3. Fallback when all parts are dropped
	allThoughtsDoc := map[string]any{
		"contents": []any{
			map[string]any{
				"role": "model",
				"parts": []any{
					map[string]any{"thought": true, "text": "thought 1"},
					map[string]any{"thought": true, "text": "thought 2"},
				},
			},
		},
	}
	changed := proxy.MutateParts(allThoughtsDoc, func(role string, part map[string]any) (keep bool, modified bool) {
		if isThought, _ := part["thought"].(bool); isThought {
			return false, true
		}
		return true, false
	})
	if !changed {
		t.Errorf("expected MutateParts to report change")
	}
	contents := allThoughtsDoc["contents"].([]any)
	parts := contents[0].(map[string]any)["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("expected 1 fallback part, got %d", len(parts))
	}
	fb := parts[0].(map[string]any)
	if fb["text"] != "" {
		t.Errorf("expected fallback text '', got %v", fb["text"])
	}

	// 4. Role validation
	rolesSeen := make(map[string]int)
	multiRoleDoc := map[string]any{
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}},
			map[string]any{"role": "model", "parts": []any{map[string]any{"text": "hello"}}},
		},
	}
	proxy.MutateParts(multiRoleDoc, func(role string, part map[string]any) (bool, bool) {
		rolesSeen[role]++
		return true, false
	})
	if rolesSeen["user"] != 1 || rolesSeen["model"] != 1 {
		t.Errorf("unexpected roles seen: %v", rolesSeen)
	}

	// 5. Non-map parts preserved without panic
	mixedPartsDoc := map[string]any{
		"contents": []any{
			map[string]any{
				"role": "user",
				"parts": []any{
					"raw-string-part",
					map[string]any{"text": "valid"},
				},
			},
		},
	}
	changed = proxy.MutateParts(mixedPartsDoc, func(role string, part map[string]any) (bool, bool) {
		return true, false
	})
	if changed {
		t.Errorf("expected no modification for untouched parts")
	}
	mixedParts := mixedPartsDoc["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	if len(mixedParts) != 2 || mixedParts[0] != "raw-string-part" {
		t.Errorf("expected raw string part to be preserved: %v", mixedParts)
	}
}

// ---------------------------------------------------------------------------
// 2. Payload Adapters Unit Tests
// ---------------------------------------------------------------------------

func TestClaudePayloadAdapter_DirectReq(t *testing.T) {
	adapter := &proxy.ClaudePayloadAdapter{}

	// Direct request map (no root "request" wrapper)
	req := map[string]any{
		"contents": []any{
			map[string]any{
				"role": "model",
				"parts": []any{
					map[string]any{"thought": true, "text": "secret"},
					map[string]any{"text": "visible", "thoughtSignature": "sig123"},
				},
			},
		},
		"generationConfig": map[string]any{
			"maxOutputTokens": float64(100000),
			"thinkingConfig": map[string]any{
				"thinkingBudget": float64(500),
			},
		},
		"labels": map[string]any{
			"used_claude": "false",
		},
	}

	changed, err := adapter.Adapt(req, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true")
	}

	// Verify maxOutputTokens clamped to 64000
	genCfg := req["generationConfig"].(map[string]any)
	if genCfg["maxOutputTokens"] != 64000 {
		t.Errorf("expected maxOutputTokens 64000, got %v", genCfg["maxOutputTokens"])
	}

	// Verify thinkingBudget raised to 1024
	thkCfg := genCfg["thinkingConfig"].(map[string]any)
	if thkCfg["thinkingBudget"] != 1024 && thkCfg["thinkingBudget"] != float64(1024) {
		t.Errorf("expected thinkingBudget 1024, got %v", thkCfg["thinkingBudget"])
	}

	// Verify parts
	parts := req["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("expected 1 part remaining, got %d", len(parts))
	}
	part := parts[0].(map[string]any)
	if part["text"] != "visible" {
		t.Errorf("expected text 'visible', got %v", part["text"])
	}
	if _, hasSig := part["thoughtSignature"]; hasSig {
		t.Errorf("thoughtSignature should have been deleted")
	}

	// Verify labels
	labels := req["labels"].(map[string]any)
	if labels["used_claude"] != "true" || labels["used_non_gemini_model"] != "true" {
		t.Errorf("expected claude labels true, got %v", labels)
	}
	if labels["model_enum"] != "MODEL_PLACEHOLDER_M35" {
		t.Errorf("expected placeholder MODEL_PLACEHOLDER_M35, got %v", labels["model_enum"])
	}

	// Re-adapting already compliant doc should return changed=false
	changed2, err2 := adapter.Adapt(req, "claude-sonnet-4-6")
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if changed2 {
		t.Errorf("expected changed=false on second adapt of already compliant doc")
	}
}

func TestGeminiPayloadAdapter_DirectReq(t *testing.T) {
	adapter := &proxy.GeminiPayloadAdapter{}

	req := map[string]any{
		"contents": []any{
			map[string]any{
				"role": "model",
				"parts": []any{
					map[string]any{"thought": true, "text": "secret"},
					map[string]any{"functionCall": map[string]any{"name": "test"}},
				},
			},
		},
		"generationConfig": map[string]any{
			"maxOutputTokens": float64(100000),
			"thinkingConfig": map[string]any{
				"thinkingBudget": float64(-1),
			},
		},
		"labels": map[string]any{
			"used_claude": "true",
		},
	}

	changed, err := adapter.Adapt(req, "gemini-3.7-flash-high")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true")
	}

	// Verify maxOutputTokens clamped to 65536
	genCfg := req["generationConfig"].(map[string]any)
	if genCfg["maxOutputTokens"] != 65536 {
		t.Errorf("expected maxOutputTokens 65536, got %v", genCfg["maxOutputTokens"])
	}

	// Verify thinkingBudget is NOT forced to 1024 for Gemini (allows -1)
	thkCfg := genCfg["thinkingConfig"].(map[string]any)
	if thkCfg["thinkingBudget"] != float64(-1) {
		t.Errorf("expected thinkingBudget to remain -1, got %v", thkCfg["thinkingBudget"])
	}

	// Verify parts: thought stripped, skip_thought_signature_validator injected
	parts := req["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("expected 1 part remaining, got %d", len(parts))
	}
	fnPart := parts[0].(map[string]any)
	if fnPart["thoughtSignature"] != "skip_thought_signature_validator" {
		t.Errorf("expected skip_thought_signature_validator, got %v", fnPart["thoughtSignature"])
	}

	// Verify labels
	labels := req["labels"].(map[string]any)
	if labels["used_claude"] != "false" || labels["used_non_gemini_model"] != "false" {
		t.Errorf("expected claude labels false, got %v", labels)
	}
	if labels["model_enum"] != "MODEL_PLACEHOLDER_M298" {
		t.Errorf("expected placeholder MODEL_PLACEHOLDER_M298, got %v", labels["model_enum"])
	}
}

func TestGetPayloadAdapter_NullObject(t *testing.T) {
	// Unknown category
	unknownAdapter := proxy.GetPayloadAdapter(proxy.CategoryUnknown)
	if unknownAdapter == nil {
		t.Fatalf("expected non-nil adapter for CategoryUnknown")
	}

	doc := map[string]any{"model": "custom-model"}
	changed, err := unknownAdapter.Adapt(doc, "custom-model")
	if err != nil || changed {
		t.Errorf("expected (false, nil) from null adapter, got changed=%v, err=%v", changed, err)
	}

	// CategoryClaudeGPT
	claudeAdapter := proxy.GetPayloadAdapter(proxy.CategoryClaudeGPT)
	if _, ok := claudeAdapter.(*proxy.ClaudePayloadAdapter); !ok {
		t.Errorf("expected *ClaudePayloadAdapter, got %T", claudeAdapter)
	}

	// CategoryGemini
	geminiAdapter := proxy.GetPayloadAdapter(proxy.CategoryGemini)
	if _, ok := geminiAdapter.(*proxy.GeminiPayloadAdapter); !ok {
		t.Errorf("expected *GeminiPayloadAdapter, got %T", geminiAdapter)
	}
}

// ---------------------------------------------------------------------------
// 3. Concurrency / Thread-Safety Tests
// ---------------------------------------------------------------------------

func TestPayloadAdapter_Concurrency(t *testing.T) {
	const goroutines = 50
	const iterations = 20

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Concurrent Claude adaptations
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			adapter := proxy.GetPayloadAdapter(proxy.CategoryClaudeGPT)
			for j := 0; j < iterations; j++ {
				doc := map[string]any{
					"request": map[string]any{
						"contents": []any{
							map[string]any{
								"role": "model",
								"parts": []any{
									map[string]any{"thought": true, "text": "internal thought"},
									map[string]any{"text": "output", "thoughtSignature": "sig"},
								},
							},
						},
						"generationConfig": map[string]any{
							"maxOutputTokens": float64(70000),
							"thinkingConfig": map[string]any{
								"thinkingBudget": float64(512),
							},
						},
						"labels": map[string]any{"used_claude": "false"},
					},
				}
				changed, err := adapter.Adapt(doc, "claude-opus-4-6-thinking")
				if err != nil || !changed {
					t.Errorf("concurrent Claude adapt failed: changed=%v, err=%v", changed, err)
				}
			}
		}()
	}

	// Concurrent Gemini adaptations
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			adapter := proxy.GetPayloadAdapter(proxy.CategoryGemini)
			for j := 0; j < iterations; j++ {
				doc := map[string]any{
					"request": map[string]any{
						"contents": []any{
							map[string]any{
								"role": "model",
								"parts": []any{
									map[string]any{"thought": true, "text": "internal thought"},
									map[string]any{"functionCall": map[string]any{"name": "bash"}},
								},
							},
						},
						"generationConfig": map[string]any{
							"maxOutputTokens": float64(80000),
						},
						"labels": map[string]any{"used_claude": "true"},
					},
				}
				changed, err := adapter.Adapt(doc, "gemini-3.8-flash-high")
				if err != nil || !changed {
					t.Errorf("concurrent Gemini adapt failed: changed=%v, err=%v", changed, err)
				}
			}
		}()
	}

	wg.Wait()
}

func TestPayloadAdapter_NonRequestDocumentSafeguard(t *testing.T) {
	// A document that contains "request" only as a string value in a prompt or field,
	// but is NOT an Antigravity request structure.
	raw := `{"model":"gemini-pro","message":"this is a request for help"}`
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	adapter := proxy.GetPayloadAdapter(proxy.CategoryClaudeGPT)
	changed, err := adapter.Adapt(doc, "claude-3-7-sonnet")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Errorf("adapter should not modify a document without request structure")
	}
}

// ---------------------------------------------------------------------------
// 4. ResolveRequestMap & Numeric Clamping Tests
// ---------------------------------------------------------------------------

func TestResolveRequestMap(t *testing.T) {
	// Nil
	if req, ok := proxy.ResolveRequestMap(nil); ok || req != nil {
		t.Errorf("expected false, nil for nil doc")
	}

	// Nested map
	inner := map[string]any{"contents": []any{}}
	docWithReq := map[string]any{"request": inner}
	if req, ok := proxy.ResolveRequestMap(docWithReq); !ok || req == nil {
		t.Errorf("expected true, inner map for nested request")
	}

	// Non-map request
	docBadReq := map[string]any{"request": "bad-string", "contents": []any{}}
	if _, ok := proxy.ResolveRequestMap(docBadReq); ok {
		t.Errorf("expected false when 'request' key exists but is not a map")
	}

	// Direct request via contents
	docContents := map[string]any{"contents": []any{}}
	if req, ok := proxy.ResolveRequestMap(docContents); !ok || req == nil {
		t.Errorf("expected true for direct contents map")
	}

	// Direct request via generationConfig
	docGenCfg := map[string]any{"generationConfig": map[string]any{}}
	if req, ok := proxy.ResolveRequestMap(docGenCfg); !ok || req == nil {
		t.Errorf("expected true for direct generationConfig map")
	}

	// Direct request via labels
	docLabels := map[string]any{"labels": map[string]any{}}
	if req, ok := proxy.ResolveRequestMap(docLabels); !ok || req == nil {
		t.Errorf("expected true for direct labels map")
	}

	// Completely unrelated map
	docUnrelated := map[string]any{"foo": "bar"}
	if _, ok := proxy.ResolveRequestMap(docUnrelated); ok {
		t.Errorf("expected false for unrelated map")
	}
}

func TestClampMaxOutputTokens_NumericTypes(t *testing.T) {
	cases := []struct {
		name     string
		input    any
		maxOut   int
		expected int
		changed  bool
	}{
		{"float64_exceeded", float64(100000), 64000, 64000, true},
		{"float64_within", float64(30000), 64000, 30000, false},
		{"float32_exceeded", float32(100000), 64000, 64000, true},
		{"int_exceeded", int(100000), 64000, 64000, true},
		{"int_within", int(50000), 64000, 50000, false},
		{"int32_exceeded", int32(100000), 64000, 64000, true},
		{"int64_exceeded", int64(100000), 64000, 64000, true},
		{"jsonNumber_exceeded", json.Number("100000"), 64000, 64000, true},
		{"jsonNumber_within", json.Number("20000"), 64000, 20000, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := map[string]any{
				"generationConfig": map[string]any{
					"maxOutputTokens": tc.input,
				},
			}
			changed := proxy.ClampMaxOutputTokens(req, tc.maxOut)
			if changed != tc.changed {
				t.Errorf("changed=%v, expected=%v", changed, tc.changed)
			}
			if tc.changed {
				genCfg := req["generationConfig"].(map[string]any)
				if genCfg["maxOutputTokens"] != tc.expected {
					t.Errorf("maxOutputTokens=%v, expected=%v", genCfg["maxOutputTokens"], tc.expected)
				}
			}
		})
	}
}

func TestClaudePayloadAdapter_ThinkingBudgetNumericTypes(t *testing.T) {
	cases := []struct {
		name     string
		budget   any
		expected float64
		changed  bool
	}{
		{"int32_below_min", int32(500), 1024, true},
		{"int64_below_min", int64(-1), 1024, true},
		{"float32_below_min", float32(800), 1024, true},
		{"jsonNumber_below_min", json.Number("100"), 1024, true},
		{"jsonNumber_above_min", json.Number("2048"), 2048, false},
		{"int_above_min", 4096, 4096, false},
	}

	adapter := &proxy.ClaudePayloadAdapter{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := map[string]any{
				"contents": []any{},
				"generationConfig": map[string]any{
					"thinkingConfig": map[string]any{
						"thinkingBudget": tc.budget,
					},
				},
			}
			changed, err := adapter.Adapt(req, "claude-3-7-sonnet")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if changed != tc.changed {
				t.Errorf("changed=%v, expected=%v", changed, tc.changed)
			}
			thkCfg := req["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
			if tc.changed && thkCfg["thinkingBudget"] != 1024 {
				t.Errorf("thinkingBudget=%v, expected 1024", thkCfg["thinkingBudget"])
			}
		})
	}
}

func TestClaudePayloadAdapter_LabelsOnlyDoc(t *testing.T) {
	adapter := &proxy.ClaudePayloadAdapter{}
	req := map[string]any{
		"labels": map[string]any{
			"used_claude": "false",
		},
	}
	changed, err := adapter.Adapt(req, "claude-3-7-sonnet")
	if err != nil || !changed {
		t.Fatalf("expected changed=true for labels-only doc: changed=%v, err=%v", changed, err)
	}
	labels := req["labels"].(map[string]any)
	if labels["used_claude"] != "true" || labels["used_non_gemini_model"] != "true" {
		t.Errorf("expected claude labels set to true, got: %v", labels)
	}
}

func TestGeminiPayloadAdapter_LabelsOnlyDoc(t *testing.T) {
	adapter := &proxy.GeminiPayloadAdapter{}
	req := map[string]any{
		"labels": map[string]any{
			"used_claude": "true",
		},
	}
	changed, err := adapter.Adapt(req, "gemini-2.5-pro")
	if err != nil || !changed {
		t.Fatalf("expected changed=true for labels-only doc: changed=%v, err=%v", changed, err)
	}
	labels := req["labels"].(map[string]any)
	if labels["used_claude"] != "false" || labels["used_non_gemini_model"] != "false" {
		t.Errorf("expected claude labels set to false, got: %v", labels)
	}
}

func TestGetPayloadAdapterForModel(t *testing.T) {
	if _, ok := proxy.GetPayloadAdapterForModel("claude-3-7-sonnet").(*proxy.ClaudePayloadAdapter); !ok {
		t.Errorf("expected ClaudePayloadAdapter for claude-3-7-sonnet")
	}
	if _, ok := proxy.GetPayloadAdapterForModel("gemini-2.5-pro").(*proxy.GeminiPayloadAdapter); !ok {
		t.Errorf("expected GeminiPayloadAdapter for gemini-2.5-pro")
	}
	unknownAdapter := proxy.GetPayloadAdapterForModel("unknown-vendor-model")
	changed, err := unknownAdapter.Adapt(map[string]any{"model": "unknown"}, "unknown")
	if err != nil || changed {
		t.Errorf("expected noop adapter behavior for unknown model")
	}
}

// ---------------------------------------------------------------------------
// 5. SanitizeGeminiSignatures Edge Cases
// ---------------------------------------------------------------------------

func TestSanitizeGeminiSignatures_DirectContents(t *testing.T) {
	// A payload without the "request" wrapper (direct "contents")
	rawPayload := `{
		"model": "gemini-3.8-flash-high",
		"contents": [
			{"role": "user", "parts": [{"text": "Hello"}]},
			{"role": "model", "parts": [
				{"thought": true, "text": "secret thought"},
				{"functionCall": {"name": "run_command"}, "thoughtSignature": "expired-sig"}
			]}
		]
	}`

	sanitized, err := proxy.SanitizeGeminiSignatures([]byte(rawPayload))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(sanitized, &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	contents := parsed["contents"].([]any)
	modelParts := contents[1].(map[string]any)["parts"].([]any)
	if len(modelParts) != 1 {
		t.Fatalf("expected 1 part after thought removal, got %d", len(modelParts))
	}
	part := modelParts[0].(map[string]any)
	if part["thoughtSignature"] != "skip_thought_signature_validator" {
		t.Errorf("expected skip_thought_signature_validator, got %v", part["thoughtSignature"])
	}
}

func TestSanitizeGeminiSignatures_AlreadySanitized(t *testing.T) {
	rawPayload := []byte(`{"model":"gemini-2.5-pro","request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`)
	sanitized, err := proxy.SanitizeGeminiSignatures(rawPayload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should return exact original slice without re-marshaling
	if string(sanitized) != string(rawPayload) {
		t.Errorf("expected identical bytes when no modification needed, got: %s", string(sanitized))
	}
}

func TestSanitizeGeminiSignatures_AllThoughtsFallback(t *testing.T) {
	rawPayload := `{
		"request": {
			"contents": [
				{"role": "model", "parts": [
					{"thought": true, "text": "thought only"}
				]}
			]
		}
	}`

	sanitized, err := proxy.SanitizeGeminiSignatures([]byte(rawPayload))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(sanitized, &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	contents := parsed["request"].(map[string]any)["contents"].([]any)
	parts := contents[0].(map[string]any)["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("expected 1 fallback part, got %d", len(parts))
	}
	if parts[0].(map[string]any)["text"] != "" {
		t.Errorf("expected empty text fallback, got %v", parts[0])
	}
}

func TestRewriteModelInBody_WordRequestInUserPromptNotFalselyAdapted(t *testing.T) {
	// A payload without a nested "request" object, but containing the word "request" in prompt text.
	// Must not be remarshaled or reordered.
	raw := `{"model":"gemini-2.5-flash","contents":[{"role":"user","parts":[{"text":"please handle this request carefully"}]}]}`
	rewritten, err := proxy.RewriteModelInBody([]byte(raw), "claude-3-7-sonnet")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := `{"model":"claude-3-7-sonnet","contents":[{"role":"user","parts":[{"text":"please handle this request carefully"}]}]}`
	if string(rewritten) != expected {
		t.Errorf("expected verbatim single-allocation byte replacement:\nwant: %s\ngot:  %s", expected, string(rewritten))
	}
}
