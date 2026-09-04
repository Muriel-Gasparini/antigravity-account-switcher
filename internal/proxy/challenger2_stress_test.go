package proxy_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/proxy"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/store/sqlite"
)

// mockFlushRecorder is a mock http.ResponseWriter implementing http.Flusher.
type mockFlushRecorder struct {
	header     http.Header
	buf        bytes.Buffer
	statusCode int
	flushCount int
	writeErr   error
	onWrite    func(b []byte)
	mu         sync.Mutex
}

func newMockFlushRecorder() *mockFlushRecorder {
	return &mockFlushRecorder{
		header: make(http.Header),
	}
}

func (m *mockFlushRecorder) Header() http.Header {
	return m.header
}

func (m *mockFlushRecorder) Write(b []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.onWrite != nil {
		m.onWrite(b)
	}
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	return m.buf.Write(b)
}

func (m *mockFlushRecorder) WriteHeader(statusCode int) {
	m.statusCode = statusCode
}

func (m *mockFlushRecorder) Flush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushCount++
}

func (m *mockFlushRecorder) getFlushCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.flushCount
}

func (m *mockFlushRecorder) getBufLen() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buf.Len()
}

// nonFlusherResponseWriter hides http.Flusher interface.
type nonFlusherResponseWriter struct {
	http.ResponseWriter
}

// waitForRecords polls SQLite until the expected number of records appear or timeout expires.
func waitForRecords(t *testing.T, metricsRepo domain.MetricsRepository, accountID string, expectedCount int, timeout time.Duration) *domain.AggregatedMetrics {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		summary, err := metricsRepo.GetSummary(context.Background(), accountID, "lifetime")
		if err == nil && summary.TotalRequests >= int64(expectedCount) {
			return summary
		}
		time.Sleep(15 * time.Millisecond)
	}
	summary, _ := metricsRepo.GetSummary(context.Background(), accountID, "lifetime")
	return summary
}

// TestChallenger2_ClientDisconnect_DetachedContext_RealSQLite tests that when a client
// drops the connection mid-stream (context cancel or broken pipe), token metrics parsed
// before the drop are safely persisted to real SQLite via the detached context goroutine.
func TestChallenger2_ClientDisconnect_DetachedContext_RealSQLite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "client_disconnect_test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	accRepo := sqlite.NewAccountRepository(db)
	metricsRepo := sqlite.NewMetricsRepository(db)
	broadcaster := proxy.NewBroadcaster(50)
	ctx := context.Background()

	// 1. Setup account in SQLite
	accID := "acc-disconnect-oracle"
	if err := accRepo.Create(ctx, &domain.Account{
		ID:           accID,
		Email:        "oracle@example.com",
		RefreshToken: "rt-oracle",
		Status:       domain.AccountStatusActive,
	}); err != nil {
		t.Fatalf("failed to create account: %v", err)
	}

	t.Run("Client context cancellation mid-stream over real HTTP server", func(t *testing.T) {
		streamStarted := make(chan struct{})
		clientCanceled := make(chan struct{})

		// Mock upstream SSE server that yields a chunk with usageMetadata then hangs
		upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "flusher unsupported", http.StatusInternalServerError)
				return
			}

			// Send chunk with usageMetadata
			payload := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"Partial output"}]}}],"usageMetadata":{"promptTokenCount":150,"candidatesTokenCount":60,"totalTokenCount":210,"cachedContentTokenCount":20,"thoughtsTokenCount":12}}}` + "\n\n"
			_, _ = w.Write([]byte(payload))
			flusher.Flush()
			close(streamStarted)

			// Wait until client cancels before closing
			<-clientCanceled
		}))
		defer upstreamSrv.Close()

		// Proxy HTTP server wrapping StreamAndInterceptSSE
		proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resp, err := http.Get(upstreamSrv.URL)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()

			// Intercept stream with real SQLite repository
			_ = proxy.StreamAndInterceptSSE(r.Context(), w, resp.Body, accID, "/v1internal:stream", metricsRepo, broadcaster)
		}))
		defer proxySrv.Close()

		// Client connects with cancellable context
		reqCtx, cancelReq := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, proxySrv.URL, nil)
		if err != nil {
			t.Fatalf("failed to create client request: %v", err)
		}

		clientResp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("client request failed: %v", err)
		}
		defer clientResp.Body.Close()

		// Wait until upstream has emitted the chunk
		<-streamStarted

		// Read the first line to confirm downstream received it
		reader := bufio.NewReader(clientResp.Body)
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("failed to read first line: %v", err)
		}
		if !strings.Contains(line, "Partial output") {
			t.Errorf("unexpected line received: %s", line)
		}

		// CLIENT CANCELS CONTEXT MID-STREAM
		cancelReq()
		close(clientCanceled)

		// Wait for detached context write to commit to SQLite
		summary := waitForRecords(t, metricsRepo, accID, 1, 2*time.Second)

		if summary.TotalRequests != 1 {
			t.Fatalf("PERSISTENCE FAILURE: expected 1 recorded request in SQLite after mid-stream client disconnect, got %d", summary.TotalRequests)
		}
		if summary.TotalPromptTokens != 150 {
			t.Errorf("expected 150 prompt tokens, got %d", summary.TotalPromptTokens)
		}
		if summary.TotalCandidatesTokens != 60 {
			t.Errorf("expected 60 candidates tokens, got %d", summary.TotalCandidatesTokens)
		}
		if summary.TotalTokens != 210 {
			t.Errorf("expected 210 total tokens, got %d", summary.TotalTokens)
		}
		if summary.TotalCachedContentTokens != 20 {
			t.Errorf("expected 20 cached tokens, got %d", summary.TotalCachedContentTokens)
		}
		if summary.TotalThoughtsTokens != 12 {
			t.Errorf("expected 12 thoughts tokens, got %d", summary.TotalThoughtsTokens)
		}
	})

	t.Run("Downstream broken pipe write error immediately after parsing usageMetadata", func(t *testing.T) {
		accIDPipe := "acc-broken-pipe"
		if err := accRepo.Create(ctx, &domain.Account{
			ID:           accIDPipe,
			Email:        "pipe@example.com",
			RefreshToken: "rt-pipe",
			Status:       domain.AccountStatusActive,
		}); err != nil {
			t.Fatalf("failed to create account: %v", err)
		}

		w := newMockFlushRecorder()
		w.writeErr = syscall.EPIPE // Simulate broken pipe

		payload := `data: {"response":{"usageMetadata":{"promptTokenCount":300,"candidatesTokenCount":100,"totalTokenCount":400}}}` + "\n"

		err := proxy.StreamAndInterceptSSE(
			context.Background(),
			w,
			strings.NewReader(payload),
			accIDPipe,
			"/stream",
			metricsRepo,
			broadcaster,
		)

		if err == nil {
			t.Fatal("expected broken pipe error, got nil")
		}
		if !errors.Is(err, syscall.EPIPE) {
			t.Errorf("expected syscall.EPIPE, got %v", err)
		}

		// Verify that despite broken pipe on Write, SQLite STILL recorded the 400 tokens!
		summary := waitForRecords(t, metricsRepo, accIDPipe, 1, 2*time.Second)
		if summary.TotalRequests != 1 {
			t.Fatalf("RESILIENCE FAILURE: expected 1 request in SQLite after broken pipe, got %d", summary.TotalRequests)
		}
		if summary.TotalTokens != 400 {
			t.Errorf("expected 400 tokens in SQLite, got %d", summary.TotalTokens)
		}
	})

	t.Run("Concurrent client disconnect storm (30 parallel aborted streams)", func(t *testing.T) {
		const stormWorkers = 30
		var wg sync.WaitGroup
		startBarrier := make(chan struct{})

		for i := 0; i < stormWorkers; i++ {
			stormAccID := fmt.Sprintf("acc-storm-%d", i)
			if err := accRepo.Create(ctx, &domain.Account{
				ID:           stormAccID,
				Email:        fmt.Sprintf("storm%d@example.com", i),
				RefreshToken: "rt",
				Status:       domain.AccountStatusActive,
			}); err != nil {
				t.Fatalf("create account: %v", err)
			}

			wg.Add(1)
			go func(workerID int, targetAcc string) {
				defer wg.Done()
				<-startBarrier

				w := newMockFlushRecorder()
				streamCtx, cancel := context.WithCancel(context.Background())
				defer cancel()

				if workerID%2 == 0 {
					w.writeErr = errors.New("connection reset by peer")
				} else {
					// Deterministic mid-stream client cancellation: cancel context
					// immediately when downstream receives the usageMetadata chunk.
					w.onWrite = func(b []byte) {
						cancel()
					}
				}

				// Each stream receives usageMetadata with distinct token counts, followed by an additional line
				promptTokens := int64(100 + workerID)
				candTokens := int64(50 + workerID)
				totalTokens := promptTokens + candTokens
				payload := fmt.Sprintf(
					`data: {"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":%d,"totalTokenCount":%d}}`+"\n"+
						`data: {"candidates":[{"content":{"parts":[{"text":"post-usage"}]}}]}`+"\n",
					promptTokens, candTokens, totalTokens,
				)

				_ = proxy.StreamAndInterceptSSE(
					streamCtx,
					w,
					strings.NewReader(payload),
					targetAcc,
					"/stream",
					metricsRepo,
					broadcaster,
				)
			}(i, stormAccID)
		}

		close(startBarrier)
		wg.Wait()

		// Verify all 30 disconnected streams recorded metrics in SQLite
		for i := 0; i < stormWorkers; i++ {
			stormAccID := fmt.Sprintf("acc-storm-%d", i)
			summary := waitForRecords(t, metricsRepo, stormAccID, 1, 3*time.Second)
			if summary.TotalRequests != 1 {
				t.Errorf("storm worker %d: expected 1 record in SQLite, got %d", i, summary.TotalRequests)
			}
			expectedTotal := int64(100 + i + 50 + i)
			if summary.TotalTokens != expectedTotal {
				t.Errorf("storm worker %d: expected %d tokens, got %d", i, expectedTotal, summary.TotalTokens)
			}
		}
	})
}

// TestChallenger2_OversizedSSEPayloads_NoErrTooLong verifies that SSE payloads with line
// lengths exceeding bufio.Scanner's default 64KB limit (up to 4MB) are processed cleanly
// without bufio.ErrTooLong or data truncation, and metrics are accurately persisted.
func TestChallenger2_OversizedSSEPayloads_NoErrTooLong(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "oversized_payload_test.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	accRepo := sqlite.NewAccountRepository(db)
	metricsRepo := sqlite.NewMetricsRepository(db)
	broadcaster := proxy.NewBroadcaster(10)
	ctx := context.Background()

	accID := "acc-oversized"
	if err := accRepo.Create(ctx, &domain.Account{
		ID:           accID,
		Email:        "oversized@example.com",
		RefreshToken: "rt-oversized",
		Status:       domain.AccountStatusActive,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}

	testSizes := []struct {
		name        string
		contentSize int // bytes of text inside JSON candidate part
	}{
		{name: "65KB boundary (> 64KB bufio limit)", contentSize: 66 * 1024},
		{name: "128KB payload", contentSize: 128 * 1024},
		{name: "512KB payload", contentSize: 512 * 1024},
		{name: "1MB payload", contentSize: 1024 * 1024},
		{name: "4MB payload", contentSize: 4 * 1024 * 1024},
	}

	for idx, tc := range testSizes {
		t.Run(tc.name, func(t *testing.T) {
			largeText := strings.Repeat("X", tc.contentSize)
			promptTokens := int64(1000 + idx)
			candTokens := int64(2000 + idx)
			totalTokens := promptTokens + candTokens

			// Construct raw SSE line
			linePayload := fmt.Sprintf(
				`data: {"response":{"candidates":[{"content":{"parts":[{"text":%q}]}}],"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":%d,"totalTokenCount":%d}}}`+"\n",
				largeText, promptTokens, candTokens, totalTokens,
			)

			w := newMockFlushRecorder()

			err := proxy.StreamAndInterceptSSE(
				context.Background(),
				w,
				strings.NewReader(linePayload),
				accID,
				"/large-stream",
				metricsRepo,
				broadcaster,
			)

			// 1. Verify no error and specifically no bufio.ErrTooLong
			if err != nil {
				t.Fatalf("StreamAndInterceptSSE failed on %s: %v", tc.name, err)
			}
			if errors.Is(err, bufio.ErrTooLong) {
				t.Fatalf("CRITICAL BUG: bufio.ErrTooLong triggered on %s", tc.name)
			}

			// 2. Verify exact byte count was delivered downstream
			if w.getBufLen() != len(linePayload) {
				t.Errorf("byte length mismatch on %s: got %d, want %d", tc.name, w.getBufLen(), len(linePayload))
			}

			// 3. Verify flusher was called
			if w.getFlushCount() < 1 {
				t.Errorf("expected flusher to be called for oversized line on %s", tc.name)
			}
		})
	}

	// Verify all 5 oversized streams recorded their token metrics in SQLite
	summary := waitForRecords(t, metricsRepo, accID, len(testSizes), 3*time.Second)
	if summary.TotalRequests != int64(len(testSizes)) {
		t.Fatalf("expected %d requests recorded in SQLite, got %d", len(testSizes), summary.TotalRequests)
	}
}

// TestChallenger2_RealtimeFlushing_EveryLine verifies that http.Flusher.Flush() is triggered
// on every non-empty line received, ensuring zero latency for client real-time streaming,
// and confirms graceful behavior when ResponseWriter does not implement Flusher.
func TestChallenger2_RealtimeFlushing_EveryLine(t *testing.T) {
	broadcaster := proxy.NewBroadcaster(10)

	t.Run("Exact flush count oracle per line", func(t *testing.T) {
		lines := []string{
			": keep-alive comment line\n",
			"event: message\n",
			"id: 101\n",
			"data: {\"chunk\": 1}\n",
			"\n", // SSE empty line separator
			": another comment\n",
			"event: message\n",
			"id: 102\n",
			"data: {\"chunk\": 2, \"usageMetadata\": {\"promptTokenCount\": 10, \"candidatesTokenCount\": 20, \"totalTokenCount\": 30}}\n",
			"\n", // Final SSE separator
		}

		rawStream := strings.Join(lines, "")
		w := newMockFlushRecorder()

		err := proxy.StreamAndInterceptSSE(
			context.Background(),
			w,
			strings.NewReader(rawStream),
			"acc-flush",
			"/stream",
			nil, // Metrics repo nil for pure flusher test
			broadcaster,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Every line has len > 0 (even "\n" has len 1), so flushCount must equal len(lines)
		if w.getFlushCount() != len(lines) {
			t.Errorf("FLUSHER ORACLE MISMATCH: expected exactly %d flushes for %d lines, got %d",
				len(lines), len(lines), w.getFlushCount())
		}
	})

	t.Run("HTTP real-time timing oracle (chunks arrive before stream finishes)", func(t *testing.T) {
		chunkInterval := 60 * time.Millisecond

		// Upstream server emits 3 chunks separated by sleeps
		upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			flusher := w.(http.Flusher)
			for i := 1; i <= 3; i++ {
				_, _ = fmt.Fprintf(w, "data: chunk-%d\n\n", i)
				flusher.Flush()
				if i < 3 {
					time.Sleep(chunkInterval)
				}
			}
		}))
		defer upstreamSrv.Close()

		// Proxy handler using StreamAndInterceptSSE
		proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resp, err := http.Get(upstreamSrv.URL)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()

			_ = proxy.StreamAndInterceptSSE(r.Context(), w, resp.Body, "acc-timing", "/stream", nil, broadcaster)
		}))
		defer proxySrv.Close()

		start := time.Now()
		resp, err := http.Get(proxySrv.URL)
		if err != nil {
			t.Fatalf("http.Get failed: %v", err)
		}
		defer resp.Body.Close()

		reader := bufio.NewReader(resp.Body)
		var arrivalTimes []time.Duration

		for i := 1; i <= 3; i++ {
			line, rErr := reader.ReadString('\n')
			if rErr != nil {
				t.Fatalf("failed reading chunk %d: %v", i, rErr)
			}
			// Skip empty separator lines
			if strings.TrimSpace(line) == "" {
				continue
			}
			arrivalTimes = append(arrivalTimes, time.Since(start))
			// Read the trailing blank line
			_, _ = reader.ReadString('\n')
		}

		if len(arrivalTimes) != 3 {
			t.Fatalf("expected 3 chunk arrival timestamps, got %d", len(arrivalTimes))
		}

		t.Logf("Real-time chunk arrivals: chunk1=%v, chunk2=%v, chunk3=%v",
			arrivalTimes[0], arrivalTimes[1], arrivalTimes[2])

		// Oracle check:
		// chunk 1 must arrive almost immediately (< 40ms)
		if arrivalTimes[0] > 45*time.Millisecond {
			t.Errorf("LATENCY ANOMALY: chunk 1 arrived late (%v > 45ms), flusher buffering suspected", arrivalTimes[0])
		}
		// chunk 2 must arrive before chunk 3, around 60ms
		if arrivalTimes[1] < 45*time.Millisecond || arrivalTimes[1] > 110*time.Millisecond {
			t.Errorf("TIMING ANOMALY: chunk 2 arrived at %v, expected ~60ms", arrivalTimes[1])
		}
		// chunk 3 must arrive after ~120ms
		if arrivalTimes[2] < 100*time.Millisecond {
			t.Errorf("TIMING ANOMALY: chunk 3 arrived too fast at %v", arrivalTimes[2])
		}
	})

	t.Run("Non-flusher ResponseWriter graceful degradation without panic", func(t *testing.T) {
		rec := httptest.NewRecorder()
		nonFlusher := &nonFlusherResponseWriter{ResponseWriter: rec}

		payload := "data: hello\n\ndata: world\n\n"
		err := proxy.StreamAndInterceptSSE(
			context.Background(),
			nonFlusher,
			strings.NewReader(payload),
			"acc-noflush",
			"/stream",
			nil,
			broadcaster,
		)
		if err != nil {
			t.Fatalf("unexpected error with non-flusher: %v", err)
		}
		if rec.Body.String() != payload {
			t.Errorf("payload mismatch with non-flusher: got %q, want %q", rec.Body.String(), payload)
		}
	})
}

// TestChallenger2_ConcurrentStreams_RaceAndStress spins up 40 concurrent SSE streams
// of diverse varieties (normal, broken pipe, canceled context, oversized payloads, multi-chunk cumulative)
// targeting the same SQLite database and Broadcaster, running under -race.
func TestChallenger2_ConcurrentStreams_RaceAndStress(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "concurrent_race_stress.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	accRepo := sqlite.NewAccountRepository(db)
	metricsRepo := sqlite.NewMetricsRepository(db)
	broadcaster := proxy.NewBroadcaster(100)
	ctx := context.Background()

	const numAccounts = 4
	accountIDs := make([]string, numAccounts)
	for i := 0; i < numAccounts; i++ {
		id := fmt.Sprintf("acc-race-%d", i)
		accountIDs[i] = id
		if err := accRepo.Create(ctx, &domain.Account{
			ID:           id,
			Email:        fmt.Sprintf("race%d@example.com", i),
			RefreshToken: fmt.Sprintf("rt-%d", i),
			Status:       domain.AccountStatusActive,
		}); err != nil {
			t.Fatalf("create account: %v", err)
		}
	}

	// Active subscriber collecting telemetry events concurrently
	eventsCh, unsubscribe := broadcaster.Subscribe()
	defer unsubscribe()

	var eventsReceived atomic.Int64
	doneReceiving := make(chan struct{})
	go func() {
		for range eventsCh {
			eventsReceived.Add(1)
		}
		close(doneReceiving)
	}()

	const totalWorkers = 40
	var wg sync.WaitGroup
	startBarrier := make(chan struct{})

	for w := 0; w < totalWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			<-startBarrier

			targetAcc := accountIDs[workerID%numAccounts]
			mode := workerID % 4

			switch mode {
			case 0:
				// Normal stream
				payload := fmt.Sprintf(`data: {"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":%d,"totalTokenCount":%d}}`+"\n",
					10+workerID, 20+workerID, 30+2*workerID)
				w := newMockFlushRecorder()
				_ = proxy.StreamAndInterceptSSE(context.Background(), w, strings.NewReader(payload), targetAcc, "/normal", metricsRepo, broadcaster)

			case 1:
				// Broken pipe stream
				payload := fmt.Sprintf(`data: {"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":%d,"totalTokenCount":%d}}`+"\n",
					50+workerID, 50+workerID, 100+2*workerID)
				w := newMockFlushRecorder()
				w.writeErr = errors.New("client connection closed")
				_ = proxy.StreamAndInterceptSSE(context.Background(), w, strings.NewReader(payload), targetAcc, "/broken-pipe", metricsRepo, broadcaster)

			case 2:
				// Oversized payload stream (>70KB)
				bigText := strings.Repeat("Z", 70*1024)
				payload := fmt.Sprintf(`data: {"response":{"candidates":[{"content":{"parts":[{"text":%q}]}}],"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":%d,"totalTokenCount":%d}}}`+"\n",
					bigText, 100+workerID, 200+workerID, 300+2*workerID)
				w := newMockFlushRecorder()
				_ = proxy.StreamAndInterceptSSE(context.Background(), w, strings.NewReader(payload), targetAcc, "/oversized", metricsRepo, broadcaster)

			case 3:
				// Progressive multi-chunk stream: 3 chunks, each updating usageMetadata
				var multiBuf bytes.Buffer
				for step := 1; step <= 3; step++ {
					multiBuf.WriteString(fmt.Sprintf(
						`data: {"response":{"candidates":[{"content":{"parts":[{"text":"step"}]}}],"usageMetadata":{"promptTokenCount":%d,"candidatesTokenCount":%d,"totalTokenCount":%d}}}`+"\n\n",
						workerID*10, step*10, workerID*10+step*10,
					))
				}
				w := newMockFlushRecorder()
				_ = proxy.StreamAndInterceptSSE(context.Background(), w, &multiBuf, targetAcc, "/multi-step", metricsRepo, broadcaster)
			}
		}(w)
	}

	close(startBarrier)
	wg.Wait()

	// Wait for background persistence goroutines to complete
	deadline := time.Now().Add(5 * time.Second)
	var totalCommitted int64
	for time.Now().Before(deadline) {
		sum, err := metricsRepo.GetSummary(context.Background(), "", "lifetime")
		if err == nil && sum.TotalRequests >= int64(totalWorkers) {
			totalCommitted = sum.TotalRequests
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if totalCommitted < int64(totalWorkers) {
		sum, _ := metricsRepo.GetSummary(context.Background(), "", "lifetime")
		t.Fatalf("CONCURRENCY DATA LOSS: expected %d committed token records in SQLite, got %d", totalWorkers, sum.TotalRequests)
	}

	t.Logf("Concurrent stress completed successfully: %d total streams committed to SQLite, %d telemetry events broadcasted",
		totalCommitted, eventsReceived.Load())

	if eventsReceived.Load() == 0 {
		t.Errorf("expected telemetry events to be broadcasted during concurrent stress, got 0")
	}
}
