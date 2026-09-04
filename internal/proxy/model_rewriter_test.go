package proxy_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/proxy"
)

// generateTestPayload constructs a valid JSON payload containing model and filler text of targetSize.
func generateTestPayload(targetSize int, model string) []byte {
	prefix := fmt.Sprintf(`{"model":%q,"contents":[{"role":"user","parts":[{"text":"`, model)
	suffix := `"}]}]}`
	overhead := len(prefix) + len(suffix)
	if targetSize < overhead {
		targetSize = overhead
	}
	fillerLen := targetSize - overhead
	filler := bytes.Repeat([]byte("A"), fillerLen)

	buf := make([]byte, 0, targetSize)
	buf = append(buf, prefix...)
	buf = append(buf, filler...)
	buf = append(buf, suffix...)
	return buf
}

// ---------------------------------------------------------------------------
// 1. Path Extraction Unit Tests
// ---------------------------------------------------------------------------

func TestExtractModelFromPath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{"RPC Path with Model", "/v1internal/models/gemini-2.5-pro:streamGenerateContent", "gemini-2.5-pro"},
		{"RPC Path Unary", "/v1internal/models/claude-3-7-sonnet:generateContent", "claude-3-7-sonnet"},
		{"Standard Path", "/models/gpt-4o:streamGenerateContent", "gpt-4o"},
		{"Versioned Model Path", "/v1internal/models/claude-3-5-sonnet@20241022:predict", "claude-3-5-sonnet@20241022"},
		{"Sub-resource Path", "/v1internal/models/gemini-2.5-pro/metadata", "gemini-2.5-pro"},
		{"Path Without Method", "/models/gpt-4o", "gpt-4o"},
		{"Path With Query Delimiter", "/models/gemini-2.5-flash?alt=sse", "gemini-2.5-flash"},
		{"Path With Fragment Delimiter", "/models/gemini-2.5-flash#section", "gemini-2.5-flash"},
		{"Path Without Model", "/v1internal:streamGenerateContent", ""},
		{"Root Path", "/", ""},
		{"Empty Path", "", ""},
		{"Empty Model Substring", "/v1internal/models/", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := proxy.ExtractModelFromPath(tc.path)
			if got != tc.expected {
				t.Errorf("ExtractModelFromPath(%q) = %q; want %q", tc.path, got, tc.expected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2. JSON Extraction Unit Tests (including Adversarial & Edge Cases)
// ---------------------------------------------------------------------------

func TestExtractModelFromJSON(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		expected    string
		expectError bool
	}{
		{
			name:        "Standard Compact",
			body:        `{"model":"gemini-2.5-pro","contents":[]}`,
			expected:    "gemini-2.5-pro",
			expectError: false,
		},
		{
			name:        "Standard Pretty-Printed",
			body:        "{\n  \"model\"  :  \"claude-3-7-sonnet\"  ,\n  \"contents\": []\n}",
			expected:    "claude-3-7-sonnet",
			expectError: false,
		},
		{
			name:        "Model With Prefix",
			body:        `{"model":"models/gemini-2.5-flash","contents":[]}`,
			expected:    "models/gemini-2.5-flash",
			expectError: false,
		},
		{
			name:        "Empty String Model Value",
			body:        `{"model":"","contents":[]}`,
			expected:    "",
			expectError: false,
		},
		{
			name: "Adversarial Prompt Injection (Depth > 1)",
			body: `{
				"contents": [
					{"role": "user", "parts": [{"text": "Can you pretend to be model: \"gpt-4o\"?"}]}
				],
				"model": "gemini-2.5-pro"
			}`,
			expected:    "gemini-2.5-pro",
			expectError: false,
		},
		{
			name: "Nested Config Object Containing Model Key",
			body: `{
				"generationConfig": {"model": "inner-model-to-ignore"},
				"model": "gemini-2.5-pro"
			}`,
			expected:    "gemini-2.5-pro",
			expectError: false,
		},
		{
			name: "Model Key Preceded by Escaped Quotes",
			body: `{
				"systemInstruction": {"text": "Escaped quote \" inside prompt"},
				"model": "claude-3-5-sonnet"
			}`,
			expected:    "claude-3-5-sonnet",
			expectError: false,
		},
		{
			name: "Escaped Backslashes Before Closing Quote",
			body: `{
				"prompt": "C:\\Windows\\System32\\",
				"model": "gpt-4o"
			}`,
			expected:    "gpt-4o",
			expectError: false,
		},
		{
			name: "Model as Value of Other Key",
			body: `{
				"role": "model",
				"model": "gemini-2.5-flash"
			}`,
			expected:    "gemini-2.5-flash",
			expectError: false,
		},
		{
			name:        "Missing Model Key",
			body:        `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			expected:    "",
			expectError: true,
		},
		{
			name:        "Empty JSON Object",
			body:        `{}`,
			expected:    "",
			expectError: true,
		},
		{
			name:        "Truncated Key",
			body:        `{"mod`,
			expected:    "",
			expectError: true,
		},
		{
			name:        "Truncated After Colon",
			body:        `{"model":`,
			expected:    "",
			expectError: true,
		},
		{
			name:        "Truncated Value String",
			body:        `{"model":"gemini-2.5`,
			expected:    "",
			expectError: true,
		},
		{
			name:        "Non-String Value (Integer)",
			body:        `{"model":12345}`,
			expected:    "",
			expectError: true,
		},
		{
			name:        "Non-String Value (Null)",
			body:        `{"model":null}`,
			expected:    "",
			expectError: true,
		},
		{
			name:        "Root Array Not Object",
			body:        `[{"model":"gemini-2.5-pro"}]`,
			expected:    "",
			expectError: true,
		},
		{
			name:        "Empty Body",
			body:        ``,
			expected:    "",
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := proxy.ExtractModelFromJSON([]byte(tc.body))
			if tc.expectError {
				if err == nil {
					t.Errorf("expected error, got nil with result %q", got)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tc.expected {
					t.Errorf("got %q, want %q", got, tc.expected)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 3. Body Rewriting Unit Tests (Immutability & Prefix Preservation)
// ---------------------------------------------------------------------------

func TestRewriteModelInBody(t *testing.T) {
	tests := []struct {
		name        string
		origBody    string
		targetModel string
		expected    string
		expectError bool
	}{
		{
			name:        "Length Expansion (16b -> 17b)",
			origBody:    `{"model":"gemini-2.5-flash","contents":[]}`,
			targetModel: "claude-3-7-sonnet",
			expected:    `{"model":"claude-3-7-sonnet","contents":[]}`,
			expectError: false,
		},
		{
			name:        "Length Contraction (17b -> 6b)",
			origBody:    `{"model":"claude-3-7-sonnet","contents":[]}`,
			targetModel: "gpt-4o",
			expected:    `{"model":"gpt-4o","contents":[]}`,
			expectError: false,
		},
		{
			name:        "Equal Length (14b -> 14b)",
			origBody:    `{"model":"gemini-2.5-pro","contents":[]}`,
			targetModel: "gemini-1.5-pro",
			expected:    `{"model":"gemini-1.5-pro","contents":[]}`,
			expectError: false,
		},
		{
			name:        "Prefix Preservation (orig has models/)",
			origBody:    `{"model":"models/gemini-2.5-pro","contents":[]}`,
			targetModel: "claude-3-7-sonnet",
			expected:    `{"model":"models/claude-3-7-sonnet","contents":[]}`,
			expectError: false,
		},
		{
			name:        "Target already has models/ prefix (no duplication)",
			origBody:    `{"model":"models/gemini-2.5-pro","contents":[]}`,
			targetModel: "models/claude-3-7-sonnet",
			expected:    `{"model":"models/claude-3-7-sonnet","contents":[]}`,
			expectError: false,
		},
		{
			name:        "Orig lacks models/ prefix, target has it (normalize to no prefix)",
			origBody:    `{"model":"gemini-2.5-pro","contents":[]}`,
			targetModel: "models/claude-3-7-sonnet",
			expected:    `{"model":"claude-3-7-sonnet","contents":[]}`,
			expectError: false,
		},
		{
			name: "Nested prompt with model string is untouched",
			origBody: `{
				"contents": [{"text": "{\"model\": \"keep-this-unmodified\"}"}],
				"model": "gemini-2.5-pro"
			}`,
			targetModel: "claude-3-7-sonnet",
			expected: `{
				"contents": [{"text": "{\"model\": \"keep-this-unmodified\"}"}],
				"model": "claude-3-7-sonnet"
			}`,
			expectError: false,
		},
		{
			name:        "Missing Model in Original Body",
			origBody:    `{"contents":[]}`,
			targetModel: "claude-3-7-sonnet",
			expected:    "",
			expectError: true,
		},
		{
			name:        "Empty Target Model",
			origBody:    `{"model":"gemini-2.5-pro"}`,
			targetModel: "",
			expected:    "",
			expectError: true,
		},
		{
			name:        "Whitespace Target Model",
			origBody:    `{"model":"gemini-2.5-pro"}`,
			targetModel: "   ",
			expected:    "",
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origBytes := []byte(tc.origBody)
			origCopy := make([]byte, len(origBytes))
			copy(origCopy, origBytes)

			rewritten, err := proxy.RewriteModelInBody(origBytes, tc.targetModel)
			if tc.expectError {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(rewritten) != tc.expected {
				t.Errorf("got:\n%s\nwant:\n%s", string(rewritten), tc.expected)
			}

			// Mandatory Invariant Check: Original body MUST remain completely unmodified!
			if !bytes.Equal(origBytes, origCopy) {
				t.Fatalf("CRITICAL INVARIANT VIOLATION: origBody was mutated in place!")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4. Path & Query Rewriting Unit Tests
// ---------------------------------------------------------------------------

func TestRewriteModelInPath(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		targetModel string
		expected    string
	}{
		{
			name:        "Standard RPC Path",
			path:        "/v1internal/models/gemini-2.5-pro:streamGenerateContent",
			targetModel: "gemini-2.5-flash",
			expected:    "/v1internal/models/gemini-2.5-flash:streamGenerateContent",
		},
		{
			name:        "Target Model With models/ Prefix Stripped",
			path:        "/v1internal/models/gemini-2.5-pro:streamGenerateContent",
			targetModel: "models/claude-3-7-sonnet",
			expected:    "/v1internal/models/claude-3-7-sonnet:streamGenerateContent",
		},
		{
			name:        "Path Without Delimiter",
			path:        "/models/gpt-4o",
			targetModel: "gemini-2.5-pro",
			expected:    "/models/gemini-2.5-pro",
		},
		{
			name:        "Path With Subresource",
			path:        "/models/gemini-2.5-pro/status",
			targetModel: "claude-3-7-sonnet",
			expected:    "/models/claude-3-7-sonnet/status",
		},
		{
			name:        "Path Without /models/",
			path:        "/v1internal:streamGenerateContent",
			targetModel: "gemini-2.5-flash",
			expected:    "/v1internal:streamGenerateContent",
		},
		{
			name:        "Empty Path",
			path:        "",
			targetModel: "gemini-2.5-flash",
			expected:    "",
		},
		{
			name:        "Empty Target Model",
			path:        "/models/gemini-2.5-pro",
			targetModel: "",
			expected:    "/models/gemini-2.5-pro",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := proxy.RewriteModelInPath(tc.path, tc.targetModel)
			if got != tc.expected {
				t.Errorf("RewriteModelInPath(%q, %q) = %q; want %q", tc.path, tc.targetModel, got, tc.expected)
			}
		})
	}
}

func TestRewriteModelInQuery(t *testing.T) {
	tests := []struct {
		name        string
		rawQuery    string
		targetModel string
		expected    string
	}{
		{
			name:        "Query with model at start",
			rawQuery:    "model=gemini-2.5-pro&alt=sse",
			targetModel: "gemini-2.5-flash",
			expected:    "model=gemini-2.5-flash&alt=sse",
		},
		{
			name:        "Query with model in middle",
			rawQuery:    "alt=sse&model=gemini-2.5-pro&key=123",
			targetModel: "claude-3-7-sonnet",
			expected:    "alt=sse&model=claude-3-7-sonnet&key=123",
		},
		{
			name:        "Query with model at end",
			rawQuery:    "alt=sse&model=gemini-2.5-pro",
			targetModel: "models/gpt-4o",
			expected:    "alt=sse&model=gpt-4o",
		},
		{
			name:        "Query with similar param prefix",
			rawQuery:    "supermodel=test&model=gemini-2.5-pro",
			targetModel: "gemini-2.5-flash",
			expected:    "supermodel=test&model=gemini-2.5-flash",
		},
		{
			name:        "Query without model param",
			rawQuery:    "alt=sse&key=123",
			targetModel: "gemini-2.5-flash",
			expected:    "alt=sse&key=123",
		},
		{
			name:        "Empty Query",
			rawQuery:    "",
			targetModel: "gemini-2.5-flash",
			expected:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := proxy.RewriteModelInQuery(tc.rawQuery, tc.targetModel)
			if got != tc.expected {
				t.Errorf("RewriteModelInQuery(%q, %q) = %q; want %q", tc.rawQuery, tc.targetModel, got, tc.expected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 5. Model Categorization Unit Tests (35+ Catalog)
// ---------------------------------------------------------------------------

func TestCategorizeModel_Catalog(t *testing.T) {
	tests := []struct {
		input    string
		expected proxy.ModelCategory
	}{
		// Claude Family
		{"claude-3-5-sonnet", proxy.CategoryClaudeGPT},
		{"claude-3-7-sonnet", proxy.CategoryClaudeGPT},
		{"claude-3.5-sonnet", proxy.CategoryClaudeGPT},
		{"claude-3.7-sonnet", proxy.CategoryClaudeGPT},
		{"claude-3-opus", proxy.CategoryClaudeGPT},
		{"claude-3-haiku", proxy.CategoryClaudeGPT},
		{"claude-3-5-sonnet-v2@20241022", proxy.CategoryClaudeGPT},
		{"models/claude-3-7-sonnet", proxy.CategoryClaudeGPT},
		{"Claude 3.5 Sonnet", proxy.CategoryClaudeGPT},
		{"Claude 3.7 Sonnet (Hybrid)", proxy.CategoryClaudeGPT},
		{"anthropic.claude-v2", proxy.CategoryClaudeGPT},
		{"claude-pro", proxy.CategoryClaudeGPT}, // Claude precedence prevents "pro" collision!

		// GPT Family
		{"gpt-4o", proxy.CategoryClaudeGPT},
		{"gpt-4o-mini", proxy.CategoryClaudeGPT},
		{"gpt-4-turbo", proxy.CategoryClaudeGPT},
		{"GPT-4o", proxy.CategoryClaudeGPT},
		{"chatgpt-4o-latest", proxy.CategoryClaudeGPT},
		{"models/gpt-4o", proxy.CategoryClaudeGPT},
		{"openai/gpt-4", proxy.CategoryClaudeGPT},
		{"o1", proxy.CategoryClaudeGPT},
		{"o1-preview", proxy.CategoryClaudeGPT},
		{"o1-mini", proxy.CategoryClaudeGPT},
		{"o3", proxy.CategoryClaudeGPT},
		{"o3-mini", proxy.CategoryClaudeGPT},
		{"o4-preview", proxy.CategoryClaudeGPT},
		{"models/o1", proxy.CategoryClaudeGPT},
		{"custom-3p-model", proxy.CategoryClaudeGPT},

		// Gemini Family
		{"gemini-2.5-pro", proxy.CategoryGemini},
		{"gemini-2.5-flash", proxy.CategoryGemini},
		{"gemini-1.5-pro", proxy.CategoryGemini},
		{"gemini-1.5-flash", proxy.CategoryGemini},
		{"gemini-2.0-flash", proxy.CategoryGemini},
		{"gemini-2.0-flash-thinking-exp", proxy.CategoryGemini},
		{"gemini-ultra", proxy.CategoryGemini},
		{"gemini-experimental", proxy.CategoryGemini},
		{"models/gemini-2.5-pro", proxy.CategoryGemini},
		{"Gemini 2.5 Pro", proxy.CategoryGemini},
		{"Gemini 2.5 Flash", proxy.CategoryGemini},
		{"gemma-2-9b", proxy.CategoryGemini},
		{"flash", proxy.CategoryGemini},
		{"pro", proxy.CategoryGemini},

		// Unknown / Anti-collision Edge cases
		{"llama-3.3-70b", proxy.CategoryUnknown},
		{"deepseek-r1", proxy.CategoryUnknown},
		{"mistral-large", proxy.CategoryUnknown},
		{"custom-finetune", proxy.CategoryUnknown},
		{"auto1-agent", proxy.CategoryUnknown}, // "o1" substring must not false positive
		{"mono1", proxy.CategoryUnknown},       // "o1" substring must not false positive
		{"", proxy.CategoryUnknown},
		{"   ", proxy.CategoryUnknown},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := proxy.CategorizeModel(tc.input)
			if got != tc.expected {
				t.Errorf("CategorizeModel(%q) = %v; want %v", tc.input, got, tc.expected)
			}
		})
	}
}

func TestCategorizeModelWithDefault(t *testing.T) {
	if got := proxy.CategorizeModelWithDefault("gemini-2.5-pro", proxy.CategoryClaudeGPT); got != proxy.CategoryGemini {
		t.Errorf("expected CategoryGemini, got %v", got)
	}
	if got := proxy.CategorizeModelWithDefault("llama-3.3", proxy.CategoryGemini); got != proxy.CategoryGemini {
		t.Errorf("expected CategoryGemini fallback, got %v", got)
	}
}

func TestModelCategoryString(t *testing.T) {
	if proxy.CategoryClaudeGPT.String() != "claude_gpt" {
		t.Errorf("unexpected CategoryClaudeGPT string: %s", proxy.CategoryClaudeGPT.String())
	}
	if proxy.CategoryGemini.String() != "gemini" {
		t.Errorf("unexpected CategoryGemini string: %s", proxy.CategoryGemini.String())
	}
	if proxy.CategoryUnknown.String() != "unknown" {
		t.Errorf("unexpected CategoryUnknown string: %s", proxy.CategoryUnknown.String())
	}
}

// ---------------------------------------------------------------------------
// 6. Bucket Matching Unit Tests
// ---------------------------------------------------------------------------

func TestMatchesBucketCategory(t *testing.T) {
	tests := []struct {
		name        string
		category    proxy.ModelCategory
		displayName string
		bucketID    string
		expected    bool
	}{
		{
			name:        "CloudCode Poller Claude 5h",
			category:    proxy.CategoryClaudeGPT,
			displayName: "Claude and GPT models (5h)",
			bucketID:    "3p-5h",
			expected:    true,
		},
		{
			name:        "CloudCode Poller Claude Weekly",
			category:    proxy.CategoryClaudeGPT,
			displayName: "Claude and GPT models (weekly)",
			bucketID:    "3p-weekly",
			expected:    true,
		},
		{
			name:        "Local LangServer Claude 5h",
			category:    proxy.CategoryClaudeGPT,
			displayName: "Claude & GPT (5h)",
			bucketID:    "acc_1-3p_5h",
			expected:    true,
		},
		{
			name:        "Consumer Fallback Claude 5h",
			category:    proxy.CategoryClaudeGPT,
			displayName: "Claude and GPT models (5h)",
			bucketID:    "acc_1-3p-5h",
			expected:    true,
		},
		{
			name:        "CloudCode Poller Gemini 5h",
			category:    proxy.CategoryGemini,
			displayName: "Gemini Models (5h)",
			bucketID:    "gemini-5h",
			expected:    true,
		},
		{
			name:        "Local LangServer Gemini Weekly",
			category:    proxy.CategoryGemini,
			displayName: "Gemini (weekly)",
			bucketID:    "acc_1-gemini_weekly",
			expected:    true,
		},
		{
			name:        "Cross Check: Gemini bucket against Claude category",
			category:    proxy.CategoryClaudeGPT,
			displayName: "Gemini Models (5h)",
			bucketID:    "gemini-5h",
			expected:    false,
		},
		{
			name:        "Cross Check: Claude bucket against Gemini category",
			category:    proxy.CategoryGemini,
			displayName: "Claude and GPT models (5h)",
			bucketID:    "3p-5h",
			expected:    false,
		},
		{
			name:        "Unknown Category Check",
			category:    proxy.CategoryUnknown,
			displayName: "Gemini Models (5h)",
			bucketID:    "gemini-5h",
			expected:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := proxy.MatchesBucketCategory(tc.category, tc.displayName, tc.bucketID)
			if got != tc.expected {
				t.Errorf("MatchesBucketCategory() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestMatchesBucket(t *testing.T) {
	bucket := &domain.QuotaBucket{
		BucketID:    "gemini-5h",
		DisplayName: "Gemini Models (5h)",
	}
	if !proxy.MatchesBucket(bucket, proxy.CategoryGemini) {
		t.Errorf("expected MatchesBucket to return true for Gemini bucket")
	}
	if proxy.MatchesBucket(bucket, proxy.CategoryClaudeGPT) {
		t.Errorf("expected MatchesBucket to return false for Claude category")
	}
	if proxy.MatchesBucket(nil, proxy.CategoryGemini) {
		t.Errorf("expected MatchesBucket to return false for nil bucket")
	}
}

// ---------------------------------------------------------------------------
// 7. Request Header Synchronization Unit Tests
// ---------------------------------------------------------------------------

func TestApplyRewrittenBody(t *testing.T) {
	origURL, _ := url.Parse("https://daily-cloudcode-pa.googleapis.com/v1internal/models/gemini-2.5-pro:streamGenerateContent")
	origBody := []byte(`{"model":"gemini-2.5-pro","contents":[]}`)

	req, err := http.NewRequest("POST", origURL.String(), bytes.NewReader(origBody))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	newBody := []byte(`{"model":"claude-3-7-sonnet","contents":[]}`)
	newPath := "/v1internal/models/claude-3-7-sonnet:streamGenerateContent"

	proxy.ApplyRewrittenBody(req, newBody, newPath)

	if req.URL.Path != newPath {
		t.Errorf("expected URL.Path %q, got %q", newPath, req.URL.Path)
	}
	if req.ContentLength != int64(len(newBody)) {
		t.Errorf("expected ContentLength %d, got %d", len(newBody), req.ContentLength)
	}
	if req.Header.Get("Content-Length") != strconv.Itoa(len(newBody)) {
		t.Errorf("expected wire Content-Length %s, got %s", strconv.Itoa(len(newBody)), req.Header.Get("Content-Length"))
	}

	// Verify primary Body reading
	readBytes, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("failed to read req.Body: %v", err)
	}
	if !bytes.Equal(readBytes, newBody) {
		t.Errorf("req.Body content mismatch: got %s, want %s", readBytes, newBody)
	}

	// Verify GetBody reconstitution for transport retries
	if req.GetBody == nil {
		t.Fatalf("req.GetBody is nil")
	}
	r1, err := req.GetBody()
	if err != nil {
		t.Fatalf("req.GetBody() 1st attempt error: %v", err)
	}
	b1, _ := io.ReadAll(r1)
	if !bytes.Equal(b1, newBody) {
		t.Errorf("GetBody 1st read mismatch")
	}

	r2, err := req.GetBody()
	if err != nil {
		t.Fatalf("req.GetBody() 2nd attempt error: %v", err)
	}
	b2, _ := io.ReadAll(r2)
	if !bytes.Equal(b2, newBody) {
		t.Errorf("GetBody 2nd read mismatch (not replayable)")
	}

	// Test empty body handling
	proxy.ApplyRewrittenBody(req, []byte{}, "/empty")
	if req.ContentLength != 0 {
		t.Errorf("expected ContentLength 0 for empty body, got %d", req.ContentLength)
	}
	if req.Header.Get("Content-Length") != "" {
		t.Errorf("expected Content-Length header to be deleted for empty body")
	}

	// Test nil request safety
	proxy.ApplyRewrittenBody(nil, newBody, newPath)
}

func TestSynchronizeRequest(t *testing.T) {
	origURL, _ := url.Parse("https://daily-cloudcode-pa.googleapis.com/v1internal/models/gemini-2.5-pro:streamGenerateContent?alt=sse")
	req, _ := http.NewRequest("POST", origURL.String(), bytes.NewReader([]byte(`{}`)))

	newBody := []byte(`{"model":"gemini-2.5-flash"}`)
	newPath := "/v1internal/models/gemini-2.5-flash:streamGenerateContent"
	newQuery := "alt=sse&key=abc"

	proxy.SynchronizeRequest(req, newBody, newPath, newQuery)

	if req.URL.Path != newPath {
		t.Errorf("got path %q, want %q", req.URL.Path, newPath)
	}
	if req.URL.RawQuery != newQuery {
		t.Errorf("got query %q, want %q", req.URL.RawQuery, newQuery)
	}
	if req.ContentLength != int64(len(newBody)) {
		t.Errorf("got ContentLength %d, want %d", req.ContentLength, len(newBody))
	}
}

func TestExtractModelFromRequest(t *testing.T) {
	// From Path
	req1, _ := http.NewRequest("POST", "https://example.com/v1internal/models/gemini-2.5-pro:streamGenerateContent", nil)
	m1, cat1, src1 := proxy.ExtractModelFromRequest(req1, nil)
	if m1 != "gemini-2.5-pro" || cat1 != proxy.CategoryGemini || src1 != proxy.SourcePath {
		t.Errorf("path extraction failed: m=%s, cat=%v, src=%v", m1, cat1, src1)
	}

	// From Query
	req2, _ := http.NewRequest("POST", "https://example.com/v1internal:streamGenerateContent?model=claude-3-7-sonnet", nil)
	m2, cat2, src2 := proxy.ExtractModelFromRequest(req2, nil)
	if m2 != "claude-3-7-sonnet" || cat2 != proxy.CategoryClaudeGPT || src2 != proxy.SourceQuery {
		t.Errorf("query extraction failed: m=%s, cat=%v, src=%v", m2, cat2, src2)
	}

	// From JSON Body
	req3, _ := http.NewRequest("POST", "https://example.com/v1internal:streamGenerateContent", nil)
	body3 := []byte(`{"model":"gpt-4o","contents":[]}`)
	m3, cat3, src3 := proxy.ExtractModelFromRequest(req3, body3)
	if m3 != "gpt-4o" || cat3 != proxy.CategoryClaudeGPT || src3 != proxy.SourceJSON {
		t.Errorf("json body extraction failed: m=%s, cat=%v, src=%v", m3, cat3, src3)
	}

	// None
	req4, _ := http.NewRequest("POST", "https://example.com/v1internal:streamGenerateContent", nil)
	m4, cat4, src4 := proxy.ExtractModelFromRequest(req4, nil)
	if m4 != "" || cat4 != proxy.CategoryUnknown || src4 != proxy.SourceNone {
		t.Errorf("none extraction failed: m=%s, cat=%v, src=%v", m4, cat4, src4)
	}
}

// ---------------------------------------------------------------------------
// 8. Allocation Invariant Tests (testing.AllocsPerRun)
// ---------------------------------------------------------------------------

func TestExtractModelFromJSON_ZeroAllocations(t *testing.T) {
	sizes := []struct {
		name string
		size int
	}{
		{"1KB", 1024},
		{"1MB", 1024 * 1024},
		{"10MB", 10 * 1024 * 1024},
	}

	for _, tc := range sizes {
		t.Run(tc.name, func(t *testing.T) {
			payload := generateTestPayload(tc.size, "gemini-2.5-pro")
			var extracted string
			var err error

			allocs := testing.AllocsPerRun(100, func() {
				extracted, err = proxy.ExtractModelFromJSON(payload)
			})

			if err != nil {
				t.Fatalf("ExtractModelFromJSON error: %v", err)
			}
			if extracted != "gemini-2.5-pro" {
				t.Fatalf("expected 'gemini-2.5-pro', got %q", extracted)
			}
			if allocs != 0 {
				t.Fatalf("CRITICAL ZERO-ALLOCATION VIOLATION: expected 0 allocs/op, got %v allocs/op on %s payload", allocs, tc.name)
			}
			t.Logf("%s allocs/op: %v", tc.name, allocs)
		})
	}
}

func TestExtractModelFromPath_ZeroAllocations(t *testing.T) {
	path := "/v1internal/models/gemini-2.5-pro:streamGenerateContent"
	var extracted string

	allocs := testing.AllocsPerRun(100, func() {
		extracted = proxy.ExtractModelFromPath(path)
	})

	if extracted != "gemini-2.5-pro" {
		t.Fatalf("expected 'gemini-2.5-pro', got %q", extracted)
	}
	if allocs != 0 {
		t.Fatalf("CRITICAL ZERO-ALLOCATION VIOLATION: expected 0 allocs/op, got %v allocs/op on path extraction", allocs)
	}
}

func TestCategorizeModel_ZeroAllocations(t *testing.T) {
	models := []string{
		"gemini-2.5-pro",
		"claude-3-7-sonnet",
		"gpt-4o",
		"Claude 3.5 Sonnet",
		"models/gemini-2.5-flash",
		"o1-preview",
		"llama-3.3",
	}

	for _, m := range models {
		t.Run(m, func(t *testing.T) {
			allocs := testing.AllocsPerRun(100, func() {
				_ = proxy.CategorizeModel(m)
			})
			if allocs != 0 {
				t.Fatalf("CRITICAL ZERO-ALLOCATION VIOLATION: expected 0 allocs/op for model %q, got %v", m, allocs)
			}
		})
	}
}

func TestRewriteModelInBody_SingleAllocation(t *testing.T) {
	sizes := []struct {
		name string
		size int
	}{
		{"1KB", 1024},
		{"1MB", 1024 * 1024},
		{"10MB", 10 * 1024 * 1024},
	}

	for _, tc := range sizes {
		t.Run(tc.name, func(t *testing.T) {
			payload := generateTestPayload(tc.size, "gemini-2.5-pro")
			targetModel := "claude-3-7-sonnet"
			var rewritten []byte
			var err error

			allocs := testing.AllocsPerRun(100, func() {
				rewritten, err = proxy.RewriteModelInBody(payload, targetModel)
			})

			if err != nil {
				t.Fatalf("RewriteModelInBody error: %v", err)
			}
			if allocs > 1.0 {
				t.Fatalf("CRITICAL SINGLE-ALLOCATION VIOLATION: expected <= 1.0 alloc/op, got %v allocs/op on %s payload", allocs, tc.name)
			}
			t.Logf("%s allocs/op: %v", tc.name, allocs)
			if len(rewritten) == 0 {
				t.Fatalf("rewritten slice is empty")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 9. Go Microbenchmarks
// ---------------------------------------------------------------------------

func BenchmarkExtractModelFromPath(b *testing.B) {
	path := "/v1internal/models/gemini-2.5-pro:streamGenerateContent"
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		model := proxy.ExtractModelFromPath(path)
		if model != "gemini-2.5-pro" {
			b.Fatalf("unexpected model: %s", model)
		}
	}
}

func BenchmarkExtractModelFromJSON_1KB(b *testing.B) {
	payload := generateTestPayload(1024, "gemini-2.5-pro")
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		model, err := proxy.ExtractModelFromJSON(payload)
		if err != nil || model != "gemini-2.5-pro" {
			b.Fatalf("unexpected result: model=%s, err=%v", model, err)
		}
	}
}

func BenchmarkExtractModelFromJSON_1MB(b *testing.B) {
	payload := generateTestPayload(1024*1024, "gemini-2.5-pro")
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		model, err := proxy.ExtractModelFromJSON(payload)
		if err != nil || model != "gemini-2.5-pro" {
			b.Fatalf("unexpected result: model=%s, err=%v", model, err)
		}
	}
}

func BenchmarkExtractModelFromJSON_10MB(b *testing.B) {
	payload := generateTestPayload(10*1024*1024, "gemini-2.5-pro")
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		model, err := proxy.ExtractModelFromJSON(payload)
		if err != nil || model != "gemini-2.5-pro" {
			b.Fatalf("unexpected result: model=%s, err=%v", model, err)
		}
	}
}

func BenchmarkRewriteModelInBody_1KB(b *testing.B) {
	payload := generateTestPayload(1024, "gemini-2.5-pro")
	target := "claude-3-7-sonnet"
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		res, err := proxy.RewriteModelInBody(payload, target)
		if err != nil || len(res) == 0 {
			b.Fatalf("unexpected rewrite failure: %v", err)
		}
	}
}

func BenchmarkRewriteModelInBody_1MB(b *testing.B) {
	payload := generateTestPayload(1024*1024, "gemini-2.5-pro")
	target := "claude-3-7-sonnet"
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		res, err := proxy.RewriteModelInBody(payload, target)
		if err != nil || len(res) == 0 {
			b.Fatalf("unexpected rewrite failure: %v", err)
		}
	}
}

func BenchmarkRewriteModelInBody_10MB(b *testing.B) {
	payload := generateTestPayload(10*1024*1024, "gemini-2.5-pro")
	target := "claude-3-7-sonnet"
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		res, err := proxy.RewriteModelInBody(payload, target)
		if err != nil || len(res) == 0 {
			b.Fatalf("unexpected rewrite failure: %v", err)
		}
	}
}

func BenchmarkCategorizeModel(b *testing.B) {
	model := "gemini-2.5-pro"
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		cat := proxy.CategorizeModel(model)
		if cat != proxy.CategoryGemini {
			b.Fatalf("unexpected category: %v", cat)
		}
	}
}

func BenchmarkRewriteModelInPath(b *testing.B) {
	path := "/v1internal/models/gemini-2.5-pro:streamGenerateContent"
	target := "claude-3-7-sonnet"
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		res := proxy.RewriteModelInPath(path, target)
		if len(res) == 0 {
			b.Fatalf("empty rewritten path")
		}
	}
}

func BenchmarkRewriteModelInQuery(b *testing.B) {
	query := "alt=sse&model=gemini-2.5-pro&key=123"
	target := "claude-3-7-sonnet"
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		res := proxy.RewriteModelInQuery(query, target)
		if len(res) == 0 {
			b.Fatalf("empty rewritten query")
		}
	}
}
