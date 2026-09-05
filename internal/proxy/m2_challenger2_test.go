package proxy_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/proxy"
)

// ---------------------------------------------------------------------------
// Empirical Challenge 1: 60+ Model Categorization & Anti-Collision Suite
// ---------------------------------------------------------------------------

func TestChallenger2_CategorizeModel_AntiCollisionMatrix(t *testing.T) {
	t.Parallel()

	testMatrix := []struct {
		model    string
		expected proxy.ModelCategory
		reason   string
	}{
		// Claude family
		{"claude-3-5-sonnet", proxy.CategoryClaudeGPT, "Standard Claude 3.5 Sonnet"},
		{"claude-3-7-sonnet", proxy.CategoryClaudeGPT, "Standard Claude 3.7 Sonnet"},
		{"claude-3-opus", proxy.CategoryClaudeGPT, "Claude 3 Opus"},
		{"claude-3-haiku", proxy.CategoryClaudeGPT, "Claude 3 Haiku"},
		{"claude-2.1", proxy.CategoryClaudeGPT, "Claude 2.1 legacy"},
		{"claude-instant-1.2", proxy.CategoryClaudeGPT, "Claude Instant"},
		{"claude-3-5-sonnet@20241022", proxy.CategoryClaudeGPT, "Claude with date suffix"},
		{"models/claude-3-7-sonnet", proxy.CategoryClaudeGPT, "Claude with models/ prefix"},
		{"anthropic/claude-3-5-sonnet", proxy.CategoryClaudeGPT, "Claude with provider prefix"},
		{"CLAUDE-3-7-SONNET", proxy.CategoryClaudeGPT, "Uppercase Claude"},
		{"Claude-3-Haiku", proxy.CategoryClaudeGPT, "Titlecase Claude"},
		{"anthropic.claude-v1", proxy.CategoryClaudeGPT, "Anthropic dot notation"},
		{"sonnet-3.7", proxy.CategoryClaudeGPT, "Shorthand Sonnet"},
		{"opus-4", proxy.CategoryClaudeGPT, "Shorthand Opus"},
		{"haiku-2024", proxy.CategoryClaudeGPT, "Shorthand Haiku"},

		// Claude anti-collision priority: "pro" token inside Claude model name
		{"claude-pro", proxy.CategoryClaudeGPT, "Precedence check: claude-pro must be Claude, not Gemini"},
		{"claude-3-5-pro", proxy.CategoryClaudeGPT, "Precedence check: claude-3-5-pro must be Claude, not Gemini"},
		{"claude_pro_custom", proxy.CategoryClaudeGPT, "Precedence check: claude_pro must be Claude, not Gemini"},

		// GPT family
		{"gpt-4o", proxy.CategoryClaudeGPT, "GPT-4o"},
		{"gpt-4o-mini", proxy.CategoryClaudeGPT, "GPT-4o mini"},
		{"gpt-4-turbo", proxy.CategoryClaudeGPT, "GPT-4 turbo"},
		{"gpt-4", proxy.CategoryClaudeGPT, "GPT-4 base"},
		{"gpt-3.5-turbo", proxy.CategoryClaudeGPT, "GPT-3.5 turbo"},
		{"chatgpt-4o-latest", proxy.CategoryClaudeGPT, "ChatGPT latest"},
		{"openai/gpt-4o", proxy.CategoryClaudeGPT, "OpenAI provider prefix"},
		{"GPT-4O-MINI", proxy.CategoryClaudeGPT, "Uppercase GPT"},
		{"models/gpt-4o", proxy.CategoryClaudeGPT, "GPT with models/ prefix"},
		{"custom-3p-claude", proxy.CategoryClaudeGPT, "3P provider marker"},
		{"external-3p-model", proxy.CategoryClaudeGPT, "3P external marker"},
		{"3p-agent", proxy.CategoryClaudeGPT, "3P prefix marker"},
		{"openai-codex", proxy.CategoryClaudeGPT, "OpenAI marker"},

		// GPT anti-collision priority: "pro" token inside GPT model name
		{"gpt-4o-pro", proxy.CategoryClaudeGPT, "Precedence check: gpt-4o-pro must be Claude/GPT, not Gemini"},
		{"chatgpt-pro", proxy.CategoryClaudeGPT, "Precedence check: chatgpt-pro must be Claude/GPT, not Gemini"},
		{"sonnet-pro", proxy.CategoryClaudeGPT, "Precedence check: sonnet-pro must be Claude/GPT, not Gemini"},

		// OpenAI Reasoning models: o1, o3, o4
		{"o1", proxy.CategoryClaudeGPT, "Exact o1"},
		{"o1-preview", proxy.CategoryClaudeGPT, "o1 with hyphen delimiter"},
		{"o1-mini", proxy.CategoryClaudeGPT, "o1-mini with hyphen delimiter"},
		{"o1-2024-12-17", proxy.CategoryClaudeGPT, "o1 with date delimiter"},
		{"o1_preview", proxy.CategoryClaudeGPT, "o1 with underscore delimiter"},
		{"o1_mini", proxy.CategoryClaudeGPT, "o1_mini with underscore delimiter"},
		{"models/o1", proxy.CategoryClaudeGPT, "o1 with models/ prefix"},
		{"models/o1-mini", proxy.CategoryClaudeGPT, "o1-mini with models/ prefix"},
		{"o3", proxy.CategoryClaudeGPT, "Exact o3"},
		{"o3-mini", proxy.CategoryClaudeGPT, "o3 with hyphen delimiter"},
		{"o3-mini-high", proxy.CategoryClaudeGPT, "o3-mini-high with hyphen delimiter"},
		{"o3_mini", proxy.CategoryClaudeGPT, "o3 with underscore delimiter"},
		{"o4", proxy.CategoryClaudeGPT, "Exact o4"},
		{"o4-preview", proxy.CategoryClaudeGPT, "o4 with hyphen delimiter"},
		{"o4_mini", proxy.CategoryClaudeGPT, "o4 with underscore delimiter"},

		// OpenAI Reasoning anti-collision: false-positive resistance
		{"auto1-agent", proxy.CategoryUnknown, "Substring o1 in auto1 must NOT trigger reasoning match"},
		{"mono1", proxy.CategoryUnknown, "Substring o1 in mono1 must NOT trigger reasoning match"},
		{"stereo1", proxy.CategoryUnknown, "Substring o1 in stereo1 must NOT trigger reasoning match"},
		{"apollo11", proxy.CategoryUnknown, "Substring o1 in apollo11 must NOT trigger reasoning match"},
		{"chrono3", proxy.CategoryUnknown, "Substring o3 in chrono3 must NOT trigger reasoning match"},
		{"torpedo3", proxy.CategoryUnknown, "Substring o3 in torpedo3 must NOT trigger reasoning match"},
		{"macro1", proxy.CategoryUnknown, "Substring o1 in macro1 must NOT trigger reasoning match"},
		{"micro1-model", proxy.CategoryUnknown, "Substring o1 in micro1 must NOT trigger reasoning match"},
		{"colorado4", proxy.CategoryUnknown, "Substring o4 in colorado4 must NOT trigger reasoning match"},
		{"solo1", proxy.CategoryUnknown, "Substring o1 in solo1 must NOT trigger reasoning match"},
		{"kilo1", proxy.CategoryUnknown, "Substring o1 in kilo1 must NOT trigger reasoning match"},
		{"turbo1", proxy.CategoryUnknown, "Substring o1 in turbo1 must NOT trigger reasoning match"},
		{"dynamo1", proxy.CategoryUnknown, "Substring o1 in dynamo1 must NOT trigger reasoning match"},
		{"o10", proxy.CategoryUnknown, "o10 is not o1 or o1- or o1_"},
		{"o30", proxy.CategoryUnknown, "o30 is not o3 or o3- or o3_"},
		{"o40", proxy.CategoryUnknown, "o40 is not o4 or o4- or o4_"},
		{"o1something", proxy.CategoryUnknown, "o1 without delimiter must NOT match"},
		{"o3something", proxy.CategoryUnknown, "o3 without delimiter must NOT match"},

		// Gemini family
		{"gemini-2.5-pro", proxy.CategoryGemini, "Gemini 2.5 Pro"},
		{"gemini-2.5-flash", proxy.CategoryGemini, "Gemini 2.5 Flash"},
		{"gemini-1.5-pro", proxy.CategoryGemini, "Gemini 1.5 Pro"},
		{"gemini-1.5-flash", proxy.CategoryGemini, "Gemini 1.5 Flash"},
		{"gemini-2.0-flash", proxy.CategoryGemini, "Gemini 2.0 Flash"},
		{"gemini-2.0-flash-exp", proxy.CategoryGemini, "Gemini 2.0 Flash Exp"},
		{"gemini-2.0-flash-thinking-exp", proxy.CategoryGemini, "Gemini 2.0 Flash Thinking Exp"},
		{"gemini-ultra", proxy.CategoryGemini, "Gemini Ultra"},
		{"gemini-experimental", proxy.CategoryGemini, "Gemini Experimental"},
		{"gemini-1.0-pro", proxy.CategoryGemini, "Gemini 1.0 Pro"},
		{"models/gemini-2.5-pro", proxy.CategoryGemini, "Gemini with models/ prefix"},
		{"models/gemini-2.5-flash", proxy.CategoryGemini, "Gemini Flash with models/ prefix"},
		{"GEMINI-2.5-PRO", proxy.CategoryGemini, "Uppercase Gemini Pro"},
		{"Gemini-2.5-Flash", proxy.CategoryGemini, "Mixed-case Gemini Flash"},
		{"gemma-2-9b", proxy.CategoryGemini, "Gemma 2 9b"},
		{"gemma-2-27b", proxy.CategoryGemini, "Gemma 2 27b"},
		{"gemma-7b-it", proxy.CategoryGemini, "Gemma 7b IT"},
		{"models/gemma-7b", proxy.CategoryGemini, "Gemma with models/ prefix"},
		{"flash", proxy.CategoryGemini, "Shorthand flash"},
		{"pro", proxy.CategoryGemini, "Shorthand pro"},
		{"ultra", proxy.CategoryGemini, "Shorthand ultra"},
		{"ultra-2.0", proxy.CategoryGemini, "Ultra 2.0"},

		// Other models (Unknown category)
		{"deepseek-r1", proxy.CategoryUnknown, "DeepSeek R1 is not Gemini or Claude/GPT"},
		{"deepseek-v3", proxy.CategoryUnknown, "DeepSeek V3 is not Gemini or Claude/GPT"},
		{"llama-3.3-70b", proxy.CategoryUnknown, "Llama 3.3 70b is not Gemini or Claude/GPT"},
		{"llama-3.1-8b", proxy.CategoryUnknown, "Llama 3.1 8b is not Gemini or Claude/GPT"},
		{"mistral-large", proxy.CategoryUnknown, "Mistral Large is not Gemini or Claude/GPT"},
		{"mistral-small", proxy.CategoryUnknown, "Mistral Small is not Gemini or Claude/GPT"},
		{"qwen-2.5-72b", proxy.CategoryUnknown, "Qwen 2.5 72b is not Gemini or Claude/GPT"},
		{"command-r-plus", proxy.CategoryUnknown, "Command R+ is not Gemini or Claude/GPT"},
		{"phi-4", proxy.CategoryUnknown, "Phi-4 is not Gemini or Claude/GPT"},
		{"starcoder2", proxy.CategoryUnknown, "StarCoder2 is not Gemini or Claude/GPT"},
		{"bert-large", proxy.CategoryUnknown, "BERT Large is not Gemini or Claude/GPT"},
		{"", proxy.CategoryUnknown, "Empty string"},
		{"   ", proxy.CategoryUnknown, "Whitespace string"},
	}

	if len(testMatrix) < 50 {
		t.Fatalf("Mission invariant violated: test matrix has only %d models, need 50+", len(testMatrix))
	}

	t.Logf("Running anti-collision challenge on %d model strings...", len(testMatrix))

	for _, tc := range testMatrix {
		tc := tc
		t.Run(tc.model, func(t *testing.T) {
			got := proxy.CategorizeModel(tc.model)
			if got != tc.expected {
				t.Errorf("CategorizeModel(%q) = %v (%s), want %v (%s) [Reason: %s]",
					tc.model, got, got.String(), tc.expected, tc.expected.String(), tc.reason)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Empirical Challenge 2: Wire Transport & http.Client Synchronization
// ---------------------------------------------------------------------------

func TestChallenger2_TransportSynchronization_WireWireFidelity(t *testing.T) {
	// Start an actual TCP HTTP test server
	var (
		mu              sync.Mutex
		receivedHeaders []http.Header
		receivedBodies  [][]byte
		receivedPaths   []string
		receivedQueries []string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, fmt.Sprintf("read error: %v", err), http.StatusInternalServerError)
			return
		}
		_ = r.Body.Close()

		mu.Lock()
		receivedHeaders = append(receivedHeaders, r.Header.Clone())
		receivedBodies = append(receivedBodies, bodyBytes)
		receivedPaths = append(receivedPaths, r.URL.Path)
		receivedQueries = append(receivedQueries, r.URL.RawQuery)
		mu.Unlock()

		// Echo back the received content length in response header
		w.Header().Set("X-Received-Content-Length", r.Header.Get("Content-Length"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	client := server.Client()

	t.Run("Body Expansion (50B -> 256KB)", func(t *testing.T) {
		origBody := []byte(`{"model":"gemini-2.5-flash","prompt":"hello"}`)
		req, err := http.NewRequest("POST", server.URL+"/v1internal/models/gemini-2.5-flash:streamGenerateContent", bytes.NewReader(origBody))
		if err != nil {
			t.Fatalf("failed to create request: %v", err)
		}

		// Expand body to 256KB
		largePayload := fmt.Sprintf(`{"model":"claude-3-7-sonnet","prompt":%q}`, strings.Repeat("X", 256*1024))
		newBody := []byte(largePayload)
		newPath := "/v1internal/models/claude-3-7-sonnet:streamGenerateContent"

		proxy.ApplyRewrittenBody(req, newBody, newPath)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("client.Do failed: %v", err)
		}
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
		}

		wireCL := resp.Header.Get("X-Received-Content-Length")
		expectedCL := strconv.Itoa(len(newBody))
		if wireCL != expectedCL {
			t.Errorf("wire Content-Length mismatch: got %s, want %s", wireCL, expectedCL)
		}

		mu.Lock()
		lastBody := receivedBodies[len(receivedBodies)-1]
		lastPath := receivedPaths[len(receivedPaths)-1]
		mu.Unlock()

		if !bytes.Equal(lastBody, newBody) {
			t.Errorf("received body mismatch: got %d bytes, want %d bytes", len(lastBody), len(newBody))
		}
		if lastPath != newPath {
			t.Errorf("received path mismatch: got %q, want %q", lastPath, newPath)
		}
	})

	t.Run("Body Contraction (512KB -> 48B)", func(t *testing.T) {
		largePayload := fmt.Sprintf(`{"model":"gemini-2.5-pro","prompt":%q}`, strings.Repeat("Y", 512*1024))
		origBody := []byte(largePayload)
		req, err := http.NewRequest("POST", server.URL+"/v1internal/models/gemini-2.5-pro:streamGenerateContent", bytes.NewReader(origBody))
		if err != nil {
			t.Fatalf("failed to create request: %v", err)
		}

		smallBody := []byte(`{"model":"gpt-4o","prompt":"short"}`)
		newPath := "/v1internal/models/gpt-4o:streamGenerateContent"

		proxy.ApplyRewrittenBody(req, smallBody, newPath)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("client.Do failed (possible hang on contraction): %v", err)
		}
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
		}

		wireCL := resp.Header.Get("X-Received-Content-Length")
		expectedCL := strconv.Itoa(len(smallBody))
		if wireCL != expectedCL {
			t.Errorf("wire Content-Length mismatch: got %s, want %s", wireCL, expectedCL)
		}

		mu.Lock()
		lastBody := receivedBodies[len(receivedBodies)-1]
		mu.Unlock()

		if !bytes.Equal(lastBody, smallBody) {
			t.Errorf("received body mismatch: got %d bytes, want %d bytes", len(lastBody), len(smallBody))
		}
	})

	t.Run("SynchronizeRequest with Query and Path Rewriting", func(t *testing.T) {
		origBody := []byte(`{"model":"gemini-2.5-pro"}`)
		req, _ := http.NewRequest("POST", server.URL+"/models/gemini-2.5-pro:generateContent?model=gemini-2.5-pro&alt=sse", bytes.NewReader(origBody))

		targetModel := "claude-3-7-sonnet"
		newBody := []byte(`{"model":"claude-3-7-sonnet"}`)
		newPath := proxy.RewriteModelInPath(req.URL.Path, targetModel)
		newQuery := proxy.RewriteModelInQuery(req.URL.RawQuery, targetModel)

		proxy.SynchronizeRequest(req, newBody, newPath, newQuery)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("client.Do failed: %v", err)
		}
		_ = resp.Body.Close()

		mu.Lock()
		lastPath := receivedPaths[len(receivedPaths)-1]
		lastQuery := receivedQueries[len(receivedQueries)-1]
		lastBody := receivedBodies[len(receivedBodies)-1]
		mu.Unlock()

		expectedPath := "/models/claude-3-7-sonnet:generateContent"
		expectedQuery := "model=claude-3-7-sonnet&alt=sse"

		if lastPath != expectedPath {
			t.Errorf("path mismatch: got %q, want %q", lastPath, expectedPath)
		}
		if lastQuery != expectedQuery {
			t.Errorf("query mismatch: got %q, want %q", lastQuery, expectedQuery)
		}
		if !bytes.Equal(lastBody, newBody) {
			t.Errorf("body mismatch")
		}
	})

	t.Run("Empty Body Handling", func(t *testing.T) {
		req, _ := http.NewRequest("POST", server.URL+"/ping", bytes.NewReader([]byte("not-empty")))
		proxy.ApplyRewrittenBody(req, []byte{}, "/empty")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("client.Do failed on empty body: %v", err)
		}
		_ = resp.Body.Close()

		mu.Lock()
		lastBody := receivedBodies[len(receivedBodies)-1]
		mu.Unlock()

		if len(lastBody) != 0 {
			t.Errorf("expected 0 bytes received, got %d", len(lastBody))
		}
	})
}

// ---------------------------------------------------------------------------
// Empirical Challenge 3: GetBody Reconstitution on Transport Retries / Redirects
// ---------------------------------------------------------------------------

func TestChallenger2_GetBody_TransportRetryAndRedirect(t *testing.T) {
	var attempts int
	var receivedBodies [][]byte
	var mu sync.Mutex

	// Server redirects 307 on first attempt, then accepts 200 on second attempt
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_ = r.Body.Close()

		mu.Lock()
		attempts++
		currentAttempt := attempts
		receivedBodies = append(receivedBodies, bodyBytes)
		mu.Unlock()

		if currentAttempt == 1 {
			// 307 Temporary Redirect forces http.Transport to replay body using req.GetBody
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"redirect_accepted"}`))
	}))
	defer server.Close()

	client := server.Client()

	origBody := []byte(`{"model":"gemini-2.5-pro","data":"initial"}`)
	req, err := http.NewRequest("POST", server.URL+"/initial", bytes.NewReader(origBody))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	newBody := []byte(`{"model":"claude-3-7-sonnet","data":"rewritten_content"}`)
	proxy.ApplyRewrittenBody(req, newBody, "/initial")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do failed during 307 redirect: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK after redirect, got %d", resp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()

	if attempts != 2 {
		t.Fatalf("expected 2 attempts (initial + redirect), got %d", attempts)
	}

	for i, b := range receivedBodies {
		if !bytes.Equal(b, newBody) {
			t.Errorf("attempt %d: body corrupted during replay: got %s, want %s", i+1, string(b), string(newBody))
		}
	}
}

// ---------------------------------------------------------------------------
// Empirical Challenge 4: Keep-Alive Connection Stream Pipelining (50 Requests)
// ---------------------------------------------------------------------------

func TestChallenger2_KeepAlivePipelining_NoTruncationOrHang(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_ = r.Body.Close()

		clHeader := r.Header.Get("Content-Length")
		cl, _ := strconv.Atoi(clHeader)
		if cl != len(body) {
			http.Error(w, fmt.Sprintf("CL header mismatch: header=%d, read=%d", cl, len(body)), 400)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fmt.Sprintf("ok:%d", len(body))))
	}))
	defer server.Close()

	client := server.Client()

	// Alternating payload sizes to rigorously stress connection re-use
	for i := 0; i < 50; i++ {
		var targetSize int
		if i%2 == 0 {
			targetSize = 100 + (i * 50) // growing
		} else {
			targetSize = 10000 - (i * 100) // shrinking
		}

		origBody := []byte(`{"model":"gemini-2.5-pro","content":"orig"}`)
		req, _ := http.NewRequest("POST", server.URL+"/stream", bytes.NewReader(origBody))

		rewrittenBody := []byte(fmt.Sprintf(`{"model":"claude-3-7-sonnet","content":%q}`, strings.Repeat("Z", targetSize)))
		proxy.ApplyRewrittenBody(req, rewrittenBody, "/stream")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d failed on keep-alive connection: %v", i, err)
		}

		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d returned status %d: %s", i, resp.StatusCode, string(respBody))
		}
	}
}

// ---------------------------------------------------------------------------
// Empirical Challenge 5: High Concurrency Stress Test (100 Goroutines)
// ---------------------------------------------------------------------------

func TestChallenger2_ConcurrentRewritingAndTransportStress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_ = r.Body.Close()

		expectedModel := r.Header.Get("X-Expected-Model")
		if expectedModel != "" {
			if !bytes.Contains(body, []byte(expectedModel)) {
				http.Error(w, "model missing from body", 400)
				return
			}
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	const concurrency = 100
	var wg sync.WaitGroup
	errCh := make(chan error, concurrency)

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        50,
			MaxIdleConnsPerHost: 50,
		},
	}

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			var targetModel string
			if id%2 == 0 {
				targetModel = "claude-3-7-sonnet"
			} else {
				targetModel = "gemini-2.5-flash"
			}

			origJSON := fmt.Sprintf(`{"model":"gemini-2.5-pro","id":%d,"data":%q}`, id, strings.Repeat("D", 512*(id%10+1)))
			origBody := []byte(origJSON)

			rewritten, err := proxy.RewriteModelInBody(origBody, targetModel)
			if err != nil {
				errCh <- fmt.Errorf("id %d: RewriteModelInBody failed: %w", id, err)
				return
			}

			req, err := http.NewRequest("POST", server.URL+"/v1internal/models/gemini-2.5-pro:streamGenerateContent", bytes.NewReader(origBody))
			if err != nil {
				errCh <- fmt.Errorf("id %d: NewRequest failed: %w", id, err)
				return
			}

			newPath := proxy.RewriteModelInPath(req.URL.Path, targetModel)
			proxy.ApplyRewrittenBody(req, rewritten, newPath)
			req.Header.Set("X-Expected-Model", targetModel)

			resp, err := client.Do(req)
			if err != nil {
				errCh <- fmt.Errorf("id %d: client.Do failed: %w", id, err)
				return
			}
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				errCh <- fmt.Errorf("id %d: expected status 200, got %d", id, resp.StatusCode)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrency failure: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Empirical Challenge 6: "pro" Substring Collision Analysis & Safeguards
// ---------------------------------------------------------------------------

func TestChallenger2_ProSubstringCollisions_Analysis(t *testing.T) {
	// Verify that Claude/GPT models with "pro" in the name are properly protected:
	protectedClaudeGPT := []string{
		"claude-pro",
		"claude-3-5-pro",
		"claude_pro",
		"gpt-4-pro",
		"gpt-4o-pro",
		"chatgpt-pro",
		"sonnet-pro",
		"anthropic-pro",
		"3p-pro",
	}

	for _, m := range protectedClaudeGPT {
		cat := proxy.CategorizeModel(m)
		if cat != proxy.CategoryClaudeGPT {
			t.Errorf("CRITICAL BUG: %q should be CategoryClaudeGPT, got %v", m, cat)
		}
	}

	// Verify that genuine Gemini models with "pro" are categorized as Gemini:
	genuineGemini := []string{
		"gemini-2.5-pro",
		"gemini-1.5-pro",
		"gemini-pro",
		"pro",
	}

	for _, m := range genuineGemini {
		cat := proxy.CategorizeModel(m)
		if cat != proxy.CategoryGemini {
			t.Errorf("CRITICAL BUG: %q should be CategoryGemini, got %v", m, cat)
		}
	}

	// Document edge-case behavior for non-provider models containing "pro" as a substring:
	// Because CategorizeModel checks strings.Contains(lower, "pro"), arbitrary names containing "pro"
	// (not covered by Claude/GPT rules) are classified as CategoryGemini rather than CategoryUnknown.
	proSubstrings := []string{
		"protocol-1",
		"proxy-engine",
		"prompt-gen",
		"compromise-model",
	}

	for _, m := range proSubstrings {
		cat := proxy.CategorizeModel(m)
		// This empirically confirms the behavior: strings.Contains(lower, "pro") matches these as CategoryGemini.
		if cat != proxy.CategoryGemini {
			t.Logf("Note: %q categorized as %v", m, cat)
		} else {
			t.Logf("Empirical Observation: %q contains 'pro' and is categorized as %v", m, cat)
		}
	}
}
