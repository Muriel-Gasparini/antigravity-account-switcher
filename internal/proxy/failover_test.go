package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

type mockAccountRepo struct {
	mu       sync.Mutex
	accounts map[string]*domain.Account
}

func newMockAccountRepo() *mockAccountRepo {
	return &mockAccountRepo{
		accounts: make(map[string]*domain.Account),
	}
}

func (m *mockAccountRepo) addAccount(acc *domain.Account) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accounts[acc.ID] = acc
}

func (m *mockAccountRepo) Create(ctx context.Context, acc *domain.Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accounts[acc.ID] = acc
	return nil
}

func (m *mockAccountRepo) GetByID(ctx context.Context, id string) (*domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return nil, domain.ErrAccountNotFound
	}
	cp := *acc
	return &cp, nil
}

func (m *mockAccountRepo) GetByEmail(ctx context.Context, email string) (*domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, acc := range m.accounts {
		if acc.Email == email {
			cp := *acc
			return &cp, nil
		}
	}
	return nil, domain.ErrAccountNotFound
}

func (m *mockAccountRepo) GetActive(ctx context.Context) (*domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, acc := range m.accounts {
		if acc.IsActive {
			cp := *acc
			return &cp, nil
		}
	}
	return nil, domain.ErrNoActiveAccount
}

func (m *mockAccountRepo) List(ctx context.Context) ([]*domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var list []*domain.Account
	for _, acc := range m.accounts {
		cp := *acc
		list = append(list, &cp)
	}
	return list, nil
}

func (m *mockAccountRepo) SetActive(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	target, ok := m.accounts[id]
	if !ok {
		return domain.ErrAccountNotFound
	}
	for _, acc := range m.accounts {
		acc.IsActive = false
	}
	target.IsActive = true
	target.Status = domain.AccountStatusActive
	target.UpdatedAt = time.Now().UTC()
	return nil
}

func (m *mockAccountRepo) UpdateStatus(ctx context.Context, id string, status domain.AccountStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return domain.ErrAccountNotFound
	}
	acc.Status = status
	acc.UpdatedAt = time.Now().UTC()
	return nil
}

func (m *mockAccountRepo) UpdateToken(ctx context.Context, id string, accessToken string, expiry time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return domain.ErrAccountNotFound
	}
	acc.AccessToken = accessToken
	acc.TokenExpiry = expiry
	return nil
}

func (m *mockAccountRepo) UpdateRefreshToken(ctx context.Context, id string, refreshToken string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return domain.ErrAccountNotFound
	}
	acc.RefreshToken = refreshToken
	return nil
}

func (m *mockAccountRepo) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.accounts, id)
	return nil
}

func (m *mockAccountRepo) GetNextAvailable(ctx context.Context, excludeID string) (*domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *domain.Account
	for _, acc := range m.accounts {
		if acc.ID == excludeID {
			continue
		}
		if acc.Status == domain.AccountStatusActive {
			if best == nil || acc.UpdatedAt.Before(best.UpdatedAt) {
				best = acc
			}
		}
	}
	if best == nil {
		return nil, domain.ErrNoAvailableAccount
	}
	cp := *best
	return &cp, nil
}

type mockEventRepo struct {
	mu     sync.Mutex
	events []*domain.ProxyEvent
}

func (m *mockEventRepo) Record(ctx context.Context, event *domain.ProxyEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, event)
	return nil
}

func (m *mockEventRepo) ListRecent(ctx context.Context, limit int) ([]*domain.ProxyEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.events, nil
}

type mockQuotaRepo struct {
	mu      sync.Mutex
	buckets map[string][]*domain.QuotaBucket
}

func newMockQuotaRepo() *mockQuotaRepo {
	return &mockQuotaRepo{
		buckets: make(map[string][]*domain.QuotaBucket),
	}
}

func (m *mockQuotaRepo) setBuckets(accountID string, buckets []*domain.QuotaBucket) {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make([]*domain.QuotaBucket, len(buckets))
	for i, b := range buckets {
		cp := *b
		res[i] = &cp
	}
	m.buckets[accountID] = res
}

func (m *mockQuotaRepo) UpsertBuckets(ctx context.Context, buckets []*domain.QuotaBucket) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range buckets {
		cp := *b
		existing := m.buckets[b.AccountID]
		found := false
		for i, eb := range existing {
			if eb.BucketID == b.BucketID {
				existing[i] = &cp
				found = true
				break
			}
		}
		if !found {
			m.buckets[b.AccountID] = append(m.buckets[b.AccountID], &cp)
		}
	}
	return nil
}

func (m *mockQuotaRepo) GetByAccountID(ctx context.Context, accountID string) ([]*domain.QuotaBucket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	list, ok := m.buckets[accountID]
	if !ok {
		return nil, nil
	}
	res := make([]*domain.QuotaBucket, len(list))
	for i, b := range list {
		cp := *b
		res[i] = &cp
	}
	return res, nil
}

func (m *mockQuotaRepo) ListAll(ctx context.Context) (map[string][]*domain.QuotaBucket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make(map[string][]*domain.QuotaBucket)
	for k, v := range m.buckets {
		res[k] = append([]*domain.QuotaBucket(nil), v...)
	}
	return res, nil
}

func (m *mockQuotaRepo) DeleteByAccountID(ctx context.Context, accountID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.buckets, accountID)
	return nil
}

func (m *mockQuotaRepo) GetExhaustedAccountsPastReset(ctx context.Context, now time.Time) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return nil, nil
}

func TestFailoverAction_String(t *testing.T) {
	cases := []struct {
		action   FailoverAction
		expected string
	}{
		{ActionNone, "ActionNone"},
		{ActionFallbackSecondary, "ActionFallbackSecondary"},
		{ActionRotateAccount, "ActionRotateAccount"},
		{FailoverAction(999), "FailoverAction(999)"},
	}
	for _, c := range cases {
		if got := c.action.String(); got != c.expected {
			t.Errorf("action.String() = %q; want %q", got, c.expected)
		}
	}
}

func TestIsExhaustionResponse(t *testing.T) {
	cases := []struct {
		statusCode int
		body       string
		expected   bool
	}{
		{http.StatusTooManyRequests, "", true},
		{http.StatusTooManyRequests, `{"error":{"status":"RESOURCE_EXHAUSTED"}}`, true},
		{http.StatusForbidden, `{"error":{"status":"RESOURCE_EXHAUSTED"}}`, true},
		{http.StatusForbidden, `{"error":{"reason":"RATE_LIMIT_EXCEEDED"}}`, true},
		{http.StatusForbidden, `{"error":{"details":[{"@type":"...QuotaFailure"}]}}`, true},
		{http.StatusForbidden, `{"error":{"status":"PERMISSION_DENIED"}}`, false},
		{http.StatusOK, "", false},
		{http.StatusBadRequest, `{"error":{"code":400}}`, false},
		{http.StatusInternalServerError, `{"error":{"code":500}}`, false},
		{http.StatusServiceUnavailable, `{"error":{"code":503}}`, false},
	}

	for _, c := range cases {
		got := IsExhaustionResponse(c.statusCode, []byte(c.body))
		if got != c.expected {
			t.Errorf("IsExhaustionResponse(%d, %q) = %v; want %v", c.statusCode, c.body, got, c.expected)
		}
	}
}

func TestFailoverEngine_PredictiveCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("PrimaryExhausted_RewritesToSecondary", func(t *testing.T) {
		accRepo := newMockAccountRepo()
		quotaRepo := newMockQuotaRepo()
		broadcaster := NewBroadcaster(10)
		ch, unsub := broadcaster.Subscribe()
		defer unsub()

		accA := &domain.Account{
			ID:       "acc-A",
			Email:    "a@example.com",
			IsActive: true,
			Status:   domain.AccountStatusActive,
		}
		accRepo.addAccount(accA)

		now := time.Now().UTC()
		quotaRepo.setBuckets("acc-A", []*domain.QuotaBucket{
			{
				AccountID:         "acc-A",
				BucketID:          "acc-A-gemini-pro-5h",
				DisplayName:       "Gemini 2.5 Pro (5h)",
				RemainingFraction: 0.0,
				ResetTime:         now.Add(2 * time.Hour),
			},
			{
				AccountID:         "acc-A",
				BucketID:          "acc-A-gemini-flash-5h",
				DisplayName:       "Gemini 2.5 Flash (5h)",
				RemainingFraction: 0.8,
				ResetTime:         now.Add(2 * time.Hour),
			},
		})

		engine := NewFailoverEngine(
			accRepo, broadcaster, nil,
			WithQuotaRepository(quotaRepo),
			WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", true),
		)

		shouldRewrite, targetModel, err := engine.PredictiveCheck(ctx, accA, "gemini-2.5-pro")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !shouldRewrite {
			t.Fatalf("expected shouldRewrite == true")
		}
		if targetModel != "gemini-2.5-flash" {
			t.Fatalf("expected targetModel == gemini-2.5-flash, got %s", targetModel)
		}

		select {
		case ev := <-ch:
			if ev.Type != domain.EventTypeModelFallback {
				t.Fatalf("expected event %s, got %s", domain.EventTypeModelFallback, ev.Type)
			}
			if ev.Details["mode"] != "predictive" {
				t.Fatalf("expected mode predictive, got %v", ev.Details["mode"])
			}
		case <-time.After(1 * time.Second):
			t.Fatal("timed out waiting for EventTypeModelFallback")
		}
	})

	t.Run("PrimaryNonZero_DoesNotRewrite", func(t *testing.T) {
		accRepo := newMockAccountRepo()
		quotaRepo := newMockQuotaRepo()
		accA := &domain.Account{ID: "acc-A", Email: "a@example.com", Status: domain.AccountStatusActive}
		accRepo.addAccount(accA)

		quotaRepo.setBuckets("acc-A", []*domain.QuotaBucket{
			{
				AccountID:         "acc-A",
				BucketID:          "acc-A-gemini-pro-5h",
				DisplayName:       "Gemini 2.5 Pro (5h)",
				RemainingFraction: 0.4,
			},
		})

		engine := NewFailoverEngine(
			accRepo, nil, nil,
			WithQuotaRepository(quotaRepo),
			WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", true),
		)

		shouldRewrite, targetModel, err := engine.PredictiveCheck(ctx, accA, "gemini-2.5-pro")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if shouldRewrite {
			t.Fatalf("expected shouldRewrite == false for nonzero primary quota")
		}
		if targetModel != "gemini-2.5-pro" {
			t.Fatalf("expected targetModel == gemini-2.5-pro, got %s", targetModel)
		}
	})

	t.Run("FallbackDisabled_DoesNotRewrite", func(t *testing.T) {
		accRepo := newMockAccountRepo()
		quotaRepo := newMockQuotaRepo()
		accA := &domain.Account{ID: "acc-A", Email: "a@example.com", Status: domain.AccountStatusActive}
		accRepo.addAccount(accA)

		quotaRepo.setBuckets("acc-A", []*domain.QuotaBucket{
			{
				AccountID:         "acc-A",
				BucketID:          "acc-A-gemini-pro-5h",
				DisplayName:       "Gemini 2.5 Pro (5h)",
				RemainingFraction: 0.0,
			},
		})

		engine := NewFailoverEngine(
			accRepo, nil, nil,
			WithQuotaRepository(quotaRepo),
			WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", false),
		)

		shouldRewrite, _, err := engine.PredictiveCheck(ctx, accA, "gemini-2.5-pro")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if shouldRewrite {
			t.Fatalf("expected shouldRewrite == false when fallback disabled")
		}
	})

	t.Run("BothExhausted_DoesNotRewrite", func(t *testing.T) {
		accRepo := newMockAccountRepo()
		quotaRepo := newMockQuotaRepo()
		accA := &domain.Account{ID: "acc-A", Email: "a@example.com", Status: domain.AccountStatusActive}
		accRepo.addAccount(accA)

		quotaRepo.setBuckets("acc-A", []*domain.QuotaBucket{
			{
				AccountID:         "acc-A",
				BucketID:          "acc-A-gemini-pro-5h",
				DisplayName:       "Gemini 2.5 Pro (5h)",
				RemainingFraction: 0.0,
			},
			{
				AccountID:         "acc-A",
				BucketID:          "acc-A-gemini-flash-5h",
				DisplayName:       "Gemini 2.5 Flash (5h)",
				RemainingFraction: 0.0,
			},
		})

		engine := NewFailoverEngine(
			accRepo, nil, nil,
			WithQuotaRepository(quotaRepo),
			WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", true),
		)

		shouldRewrite, _, err := engine.PredictiveCheck(ctx, accA, "gemini-2.5-pro")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if shouldRewrite {
			t.Fatalf("expected shouldRewrite == false when both models are exhausted")
		}
	})

	t.Run("SecondaryRequestedDirectly_DoesNotRewrite", func(t *testing.T) {
		accRepo := newMockAccountRepo()
		accA := &domain.Account{ID: "acc-A", Email: "a@example.com", Status: domain.AccountStatusActive}
		accRepo.addAccount(accA)

		engine := NewFailoverEngine(
			accRepo, nil, nil,
			WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", true),
		)

		shouldRewrite, targetModel, err := engine.PredictiveCheck(ctx, accA, "gemini-2.5-flash")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if shouldRewrite {
			t.Fatalf("expected shouldRewrite == false when secondary is explicitly requested")
		}
		if targetModel != "gemini-2.5-flash" {
			t.Fatalf("expected targetModel == gemini-2.5-flash, got %s", targetModel)
		}
	})

	t.Run("ResetTimeInPast_TreatedAsAvailable", func(t *testing.T) {
		accRepo := newMockAccountRepo()
		quotaRepo := newMockQuotaRepo()
		accA := &domain.Account{ID: "acc-A", Email: "a@example.com", Status: domain.AccountStatusActive}
		accRepo.addAccount(accA)

		// Primary 0.0 fraction, but reset was 10 minutes ago
		now := time.Now().UTC()
		quotaRepo.setBuckets("acc-A", []*domain.QuotaBucket{
			{
				AccountID:         "acc-A",
				BucketID:          "acc-A-gemini-pro-5h",
				DisplayName:       "Gemini 2.5 Pro (5h)",
				RemainingFraction: 0.0,
				ResetTime:         now.Add(-10 * time.Minute),
			},
			{
				AccountID:         "acc-A",
				BucketID:          "acc-A-gemini-flash-5h",
				DisplayName:       "Gemini 2.5 Flash (5h)",
				RemainingFraction: 0.8,
				ResetTime:         now.Add(2 * time.Hour),
			},
		})

		engine := NewFailoverEngine(
			accRepo, nil, nil,
			WithQuotaRepository(quotaRepo),
			WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", true),
		)

		shouldRewrite, targetModel, err := engine.PredictiveCheck(ctx, accA, "gemini-2.5-pro")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if shouldRewrite {
			t.Fatalf("expected shouldRewrite == false when primary reset time has passed")
		}
		if targetModel != "gemini-2.5-pro" {
			t.Fatalf("expected targetModel == gemini-2.5-pro, got %s", targetModel)
		}
	})

	t.Run("UnmanagedModel_DoesNotRewrite", func(t *testing.T) {
		accRepo := newMockAccountRepo()
		accA := &domain.Account{ID: "acc-A", Email: "a@example.com", Status: domain.AccountStatusActive}
		accRepo.addAccount(accA)

		engine := NewFailoverEngine(
			accRepo, nil, nil,
			WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", true),
		)

		shouldRewrite, targetModel, err := engine.PredictiveCheck(ctx, accA, "llama-3.3-70b")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if shouldRewrite {
			t.Fatalf("expected shouldRewrite == false for unmanaged model")
		}
		if targetModel != "llama-3.3-70b" {
			t.Fatalf("expected targetModel == llama-3.3-70b, got %s", targetModel)
		}
	})
}

func TestFailoverEngine_HandleExhaustion_PrimaryToSecondary(t *testing.T) {
	ctx := context.Background()
	accRepo := newMockAccountRepo()
	quotaRepo := newMockQuotaRepo()
	broadcaster := NewBroadcaster(10)
	ch, unsub := broadcaster.Subscribe()
	defer unsub()

	accA := &domain.Account{
		ID:        "acc-A",
		Email:     "a@example.com",
		IsActive:  true,
		Status:    domain.AccountStatusActive,
		UpdatedAt: time.Now().UTC().Add(-10 * time.Minute),
	}
	accB := &domain.Account{
		ID:        "acc-B",
		Email:     "b@example.com",
		IsActive:  false,
		Status:    domain.AccountStatusActive,
		UpdatedAt: time.Now().UTC().Add(-5 * time.Minute),
	}
	accRepo.addAccount(accA)
	accRepo.addAccount(accB)

	engine := NewFailoverEngine(
		accRepo, broadcaster, nil,
		WithQuotaRepository(quotaRepo),
		WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", true),
	)

	action, targetModel, nextAcc, err := engine.HandleExhaustion(ctx, accA, "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if action != ActionFallbackSecondary {
		t.Fatalf("expected ActionFallbackSecondary, got %v", action)
	}
	if targetModel != "gemini-2.5-flash" {
		t.Fatalf("expected targetModel gemini-2.5-flash, got %s", targetModel)
	}
	if nextAcc.ID != "acc-A" {
		t.Fatalf("expected same account acc-A, got %s", nextAcc.ID)
	}

	// Invariant: Account A must STILL be active and healthy in repo
	currentA, _ := accRepo.GetByID(ctx, "acc-A")
	if currentA.Status != domain.AccountStatusActive {
		t.Fatalf("expected acc-A status active, got %s", currentA.Status)
	}
	activeAcc, _ := accRepo.GetActive(ctx)
	if activeAcc.ID != "acc-A" {
		t.Fatalf("expected active account acc-A, got %s", activeAcc.ID)
	}

	// Invariant: EventTypeModelFallback broadcast with mode reactive_429
	select {
	case ev := <-ch:
		if ev.Type != domain.EventTypeModelFallback {
			t.Fatalf("expected event %s, got %s", domain.EventTypeModelFallback, ev.Type)
		}
		if ev.Details["mode"] != "reactive_429" {
			t.Fatalf("expected mode reactive_429, got %v", ev.Details["mode"])
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for EventTypeModelFallback")
	}
}

func TestFailoverEngine_HandleExhaustion_DoubleExhaustionToRotation(t *testing.T) {
	ctx := context.Background()
	accRepo := newMockAccountRepo()
	broadcaster := NewBroadcaster(10)
	ch, unsub := broadcaster.Subscribe()
	defer unsub()

	accA := &domain.Account{
		ID:        "acc-A",
		Email:     "a@example.com",
		IsActive:  true,
		Status:    domain.AccountStatusActive,
		UpdatedAt: time.Now().UTC().Add(-10 * time.Minute),
	}
	accB := &domain.Account{
		ID:        "acc-B",
		Email:     "b@example.com",
		IsActive:  false,
		Status:    domain.AccountStatusActive,
		UpdatedAt: time.Now().UTC().Add(-5 * time.Minute),
	}
	accRepo.addAccount(accA)
	accRepo.addAccount(accB)

	engine := NewFailoverEngine(
		accRepo, broadcaster, nil,
		WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", true),
	)

	// Step 1: Primary fails -> fall back to secondary on Account A
	act1, target1, acc1, err1 := engine.HandleExhaustion(ctx, accA, "gemini-2.5-pro")
	if err1 != nil || act1 != ActionFallbackSecondary || acc1.ID != "acc-A" || target1 != "gemini-2.5-flash" {
		t.Fatalf("unexpected step 1 result: act=%v target=%s acc=%v err=%v", act1, target1, acc1, err1)
	}

	// Drain step 1 event
	<-ch

	// Step 2: Secondary ALSO fails with 429 on Account A -> DOUBLE EXHAUSTION
	act2, target2, acc2, err2 := engine.HandleExhaustion(ctx, accA, "gemini-2.5-flash")
	if err2 != nil {
		t.Fatalf("step 2 unexpected error: %v", err2)
	}
	if act2 != ActionRotateAccount {
		t.Fatalf("expected ActionRotateAccount on double exhaustion, got %v", act2)
	}
	if target2 != "gemini-2.5-pro" {
		t.Fatalf("expected targetModel reset to primary (gemini-2.5-pro), got %s", target2)
	}
	if acc2.ID != "acc-B" {
		t.Fatalf("expected nextAcc acc-B, got %s", acc2.ID)
	}

	// Account A must be marked exhausted
	updatedA, _ := accRepo.GetByID(ctx, "acc-A")
	if updatedA.Status != domain.AccountStatusExhausted {
		t.Fatalf("expected acc-A exhausted, got %s", updatedA.Status)
	}

	// Account B must be Active
	activeAcc, _ := accRepo.GetActive(ctx)
	if activeAcc.ID != "acc-B" {
		t.Fatalf("expected active account acc-B, got %s", activeAcc.ID)
	}

	// Verify events: Failover429 then AccountSwitched
	ev1 := <-ch
	if ev1.Type != domain.EventTypeFailover429 {
		t.Fatalf("expected event %s, got %s", domain.EventTypeFailover429, ev1.Type)
	}
	ev2 := <-ch
	if ev2.Type != domain.EventTypeAccountSwitched {
		t.Fatalf("expected event %s, got %s", domain.EventTypeAccountSwitched, ev2.Type)
	}

	// Step 3: Verify PredictiveCheck on newly active Account B starts fresh on primary
	shouldRewrite, _, err := engine.PredictiveCheck(ctx, acc2, "gemini-2.5-pro")
	if err != nil || shouldRewrite {
		t.Fatalf("Account B should start fresh with primary available (shouldRewrite=%v, err=%v)", shouldRewrite, err)
	}
}

func TestFailoverEngine_HandleExhaustion_FallbackDisabled_InstantRotation(t *testing.T) {
	ctx := context.Background()
	accRepo := newMockAccountRepo()

	accA := &domain.Account{
		ID:        "acc-A",
		Email:     "a@example.com",
		IsActive:  true,
		Status:    domain.AccountStatusActive,
		UpdatedAt: time.Now().UTC().Add(-10 * time.Minute),
	}
	accB := &domain.Account{
		ID:        "acc-B",
		Email:     "b@example.com",
		IsActive:  false,
		Status:    domain.AccountStatusActive,
		UpdatedAt: time.Now().UTC().Add(-5 * time.Minute),
	}
	accRepo.addAccount(accA)
	accRepo.addAccount(accB)

	engine := NewFailoverEngine(
		accRepo, NewBroadcaster(10), nil,
		WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", false),
	)

	action, targetModel, nextAcc, err := engine.HandleExhaustion(ctx, accA, "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if action != ActionRotateAccount {
		t.Fatalf("expected ActionRotateAccount, got %v", action)
	}
	if targetModel != "gemini-2.5-pro" {
		t.Fatalf("expected targetModel gemini-2.5-pro, got %s", targetModel)
	}
	if nextAcc.ID != "acc-B" {
		t.Fatalf("expected rotation to acc-B, got %s", nextAcc.ID)
	}

	// Verify Account A marked exhausted immediately
	updatedA, _ := accRepo.GetByID(ctx, "acc-A")
	if updatedA.Status != domain.AccountStatusExhausted {
		t.Fatalf("expected acc-A status exhausted, got %s", updatedA.Status)
	}
}

func TestFailoverEngine_HandleExhaustion_SecondaryAlreadyExhausted(t *testing.T) {
	ctx := context.Background()
	accRepo := newMockAccountRepo()
	quotaRepo := newMockQuotaRepo()

	accA := &domain.Account{
		ID:        "acc-A",
		Email:     "a@example.com",
		IsActive:  true,
		Status:    domain.AccountStatusActive,
		UpdatedAt: time.Now().UTC().Add(-10 * time.Minute),
	}
	accB := &domain.Account{
		ID:        "acc-B",
		Email:     "b@example.com",
		IsActive:  false,
		Status:    domain.AccountStatusActive,
		UpdatedAt: time.Now().UTC().Add(-5 * time.Minute),
	}
	accRepo.addAccount(accA)
	accRepo.addAccount(accB)

	// Secondary model (flash) is already 0% on Account A
	now := time.Now().UTC()
	quotaRepo.setBuckets("acc-A", []*domain.QuotaBucket{
		{
			AccountID:         "acc-A",
			BucketID:          "acc-A-gemini-flash-5h",
			DisplayName:       "Gemini 2.5 Flash (5h)",
			RemainingFraction: 0.0,
			ResetTime:         now.Add(2 * time.Hour),
		},
	})

	engine := NewFailoverEngine(
		accRepo, NewBroadcaster(10), nil,
		WithQuotaRepository(quotaRepo),
		WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", true),
	)

	// Primary fails, but secondary has no quota -> skips intra-account fallback, rotates to Account B!
	action, targetModel, nextAcc, err := engine.HandleExhaustion(ctx, accA, "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action != ActionRotateAccount {
		t.Fatalf("expected ActionRotateAccount, got %v", action)
	}
	if targetModel != "gemini-2.5-pro" {
		t.Fatalf("expected targetModel gemini-2.5-pro, got %s", targetModel)
	}
	if nextAcc.ID != "acc-B" {
		t.Fatalf("expected nextAcc acc-B, got %s", nextAcc.ID)
	}
}

func TestFailoverEngine_HandleExhaustion_PoolExhausted(t *testing.T) {
	ctx := context.Background()
	accRepo := newMockAccountRepo()
	accA := &domain.Account{
		ID:        "acc-A",
		Email:     "a@example.com",
		IsActive:  true,
		Status:    domain.AccountStatusActive,
		UpdatedAt: time.Now().UTC(),
	}
	accRepo.addAccount(accA)

	broadcaster := NewBroadcaster(10)
	eventsCh, unsub := broadcaster.Subscribe()
	defer unsub()

	engine := NewFailoverEngine(
		accRepo, broadcaster, nil,
		WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", false),
	)

	action, targetModel, nextAcc, err := engine.HandleExhaustion(ctx, accA, "gemini-2.5-pro")
	if !errors.Is(err, domain.ErrNoAvailableAccount) {
		t.Fatalf("expected ErrNoAvailableAccount, got %v", err)
	}
	if action != ActionRotateAccount {
		t.Fatalf("expected ActionRotateAccount, got %v", action)
	}
	if targetModel != "gemini-2.5-pro" {
		t.Fatalf("expected targetModel gemini-2.5-pro, got %s", targetModel)
	}
	if nextAcc != nil {
		t.Fatalf("expected nil nextAcc, got %+v", nextAcc)
	}

	// Verify EventTypeQuotaExhausted was broadcast
	var quotaExhaustedReceived bool
	for i := 0; i < 2; i++ {
		select {
		case ev := <-eventsCh:
			if ev.Type == domain.EventTypeQuotaExhausted {
				quotaExhaustedReceived = true
			}
		case <-time.After(1 * time.Second):
		}
	}
	if !quotaExhaustedReceived {
		t.Error("expected EventTypeQuotaExhausted event to be broadcast")
	}
}

func TestFailoverEngine_ResetAccountState(t *testing.T) {
	ctx := context.Background()
	accRepo := newMockAccountRepo()
	accA := &domain.Account{ID: "acc-A", Email: "a@example.com", Status: domain.AccountStatusActive}
	accB := &domain.Account{ID: "acc-B", Email: "b@example.com", Status: domain.AccountStatusActive}
	accRepo.addAccount(accA)
	accRepo.addAccount(accB)

	engine := NewFailoverEngine(
		accRepo, nil, nil,
		WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", true),
	)

	// Fall back on primary failure
	act, _, _, err := engine.HandleExhaustion(ctx, accA, "gemini-2.5-pro")
	if err != nil || act != ActionFallbackSecondary {
		t.Fatalf("unexpected failover: act=%v err=%v", act, err)
	}

	// Reset state
	engine.ResetAccountState("acc-A")

	// Now primary fails again -> should fall back to secondary again, not rotate
	act2, _, _, err2 := engine.HandleExhaustion(ctx, accA, "gemini-2.5-pro")
	if err2 != nil || act2 != ActionFallbackSecondary {
		t.Fatalf("expected fresh fallback after state reset, got act=%v err=%v", act2, err2)
	}
}

func TestFailoverEngine_AntiStampede_ConcurrentFailover(t *testing.T) {
	ctx := context.Background()

	t.Run("DirectRotation_20Goroutines", func(t *testing.T) {
		repo := newMockAccountRepo()
		now := time.Now().UTC()
		accA := &domain.Account{
			ID:          "acc-A",
			Email:       "a@example.com",
			AccessToken: "token-A",
			IsActive:    true,
			Status:      domain.AccountStatusActive,
			UpdatedAt:   now.Add(-10 * time.Minute),
		}
		accB := &domain.Account{
			ID:          "acc-B",
			Email:       "b@example.com",
			AccessToken: "token-B",
			IsActive:    false,
			Status:      domain.AccountStatusActive,
			UpdatedAt:   now.Add(-5 * time.Minute),
		}
		repo.addAccount(accA)
		repo.addAccount(accB)

		engine := NewFailoverEngine(repo, NewBroadcaster(100), nil)

		const concurrency = 20
		var wg sync.WaitGroup
		results := make([]*domain.Account, concurrency)
		errorsList := make([]error, concurrency)

		wg.Add(concurrency)
		for i := 0; i < concurrency; i++ {
			idx := i
			go func() {
				defer wg.Done()
				nextAcc, err := engine.RotateAccount(context.Background(), accA)
				results[idx] = nextAcc
				errorsList[idx] = err
			}()
		}
		wg.Wait()

		for i := 0; i < concurrency; i++ {
			if errorsList[i] != nil {
				t.Fatalf("goroutine %d failed with error: %v", i, errorsList[i])
			}
			if results[i] == nil || results[i].ID != "acc-B" {
				t.Fatalf("goroutine %d got unexpected account: %+v", i, results[i])
			}
		}

		accBUpdated, _ := repo.GetByID(context.Background(), "acc-B")
		if accBUpdated.Status != domain.AccountStatusActive {
			t.Fatalf("anti-stampede failed: acc-B was inappropriately marked %s", accBUpdated.Status)
		}
		if !accBUpdated.IsActive {
			t.Fatalf("anti-stampede failed: acc-B should be active")
		}
	})

	t.Run("ConcurrentPrimaryFailover_50Goroutines_AllFallbackToSecondary", func(t *testing.T) {
		accRepo := newMockAccountRepo()
		accA := &domain.Account{
			ID:        "acc-A",
			Email:     "a@example.com",
			IsActive:  true,
			Status:    domain.AccountStatusActive,
			UpdatedAt: time.Now().UTC().Add(-10 * time.Minute),
		}
		accB := &domain.Account{
			ID:        "acc-B",
			Email:     "b@example.com",
			IsActive:  false,
			Status:    domain.AccountStatusActive,
			UpdatedAt: time.Now().UTC().Add(-5 * time.Minute),
		}
		accRepo.addAccount(accA)
		accRepo.addAccount(accB)

		engine := NewFailoverEngine(
			accRepo, NewBroadcaster(100), nil,
			WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", true),
		)

		const concurrency = 50
		var wg sync.WaitGroup
		actions := make([]FailoverAction, concurrency)
		targets := make([]string, concurrency)
		accounts := make([]*domain.Account, concurrency)
		errorsList := make([]error, concurrency)

		wg.Add(concurrency)
		for i := 0; i < concurrency; i++ {
			idx := i
			go func() {
				defer wg.Done()
				act, tgt, acc, err := engine.HandleExhaustion(ctx, accA, "gemini-2.5-pro")
				actions[idx] = act
				targets[idx] = tgt
				accounts[idx] = acc
				errorsList[idx] = err
			}()
		}
		wg.Wait()

		// All 50 goroutines must successfully fall back to secondary on Account A
		for i := 0; i < concurrency; i++ {
			if errorsList[i] != nil {
				t.Fatalf("goroutine %d failed: %v", i, errorsList[i])
			}
			if actions[i] != ActionFallbackSecondary {
				t.Fatalf("goroutine %d action = %v; want ActionFallbackSecondary", i, actions[i])
			}
			if targets[i] != "gemini-2.5-flash" {
				t.Fatalf("goroutine %d target = %s; want gemini-2.5-flash", i, targets[i])
			}
			if accounts[i].ID != "acc-A" {
				t.Fatalf("goroutine %d account = %s; want acc-A", i, accounts[i].ID)
			}
		}

		// Account A must STILL be active; Account B must NOT have been activated
		curA, _ := accRepo.GetByID(ctx, "acc-A")
		if curA.Status != domain.AccountStatusActive {
			t.Fatalf("anti-stampede violation: acc-A status is %s; want active", curA.Status)
		}
		curB, _ := accRepo.GetByID(ctx, "acc-B")
		if curB.IsActive {
			t.Fatalf("anti-stampede violation: acc-B was prematurely activated")
		}
	})

	t.Run("ConcurrentDoubleExhaustion_50Goroutines_SingleRotationToAccountB", func(t *testing.T) {
		accRepo := newMockAccountRepo()
		accA := &domain.Account{
			ID:        "acc-A",
			Email:     "a@example.com",
			IsActive:  true,
			Status:    domain.AccountStatusActive,
			UpdatedAt: time.Now().UTC().Add(-15 * time.Minute),
		}
		accB := &domain.Account{
			ID:        "acc-B",
			Email:     "b@example.com",
			IsActive:  false,
			Status:    domain.AccountStatusActive,
			UpdatedAt: time.Now().UTC().Add(-10 * time.Minute),
		}
		accC := &domain.Account{
			ID:        "acc-C",
			Email:     "c@example.com",
			IsActive:  false,
			Status:    domain.AccountStatusActive,
			UpdatedAt: time.Now().UTC().Add(-5 * time.Minute),
		}
		accRepo.addAccount(accA)
		accRepo.addAccount(accB)
		accRepo.addAccount(accC)

		engine := NewFailoverEngine(
			accRepo, NewBroadcaster(100), nil,
			WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", true),
		)

		// 50 goroutines hit 429 on secondary model simultaneously
		const concurrency = 50
		var wg sync.WaitGroup
		actions := make([]FailoverAction, concurrency)
		targets := make([]string, concurrency)
		accounts := make([]*domain.Account, concurrency)
		errorsList := make([]error, concurrency)

		wg.Add(concurrency)
		for i := 0; i < concurrency; i++ {
			idx := i
			go func() {
				defer wg.Done()
				act, tgt, acc, err := engine.HandleExhaustion(ctx, accA, "gemini-2.5-flash")
				actions[idx] = act
				targets[idx] = tgt
				accounts[idx] = acc
				errorsList[idx] = err
			}()
		}
		wg.Wait()

		// All 50 goroutines must receive Account B targeting primary model
		for i := 0; i < concurrency; i++ {
			if errorsList[i] != nil {
				t.Fatalf("goroutine %d error: %v", i, errorsList[i])
			}
			if actions[i] != ActionRotateAccount {
				t.Fatalf("goroutine %d action = %v; want ActionRotateAccount", i, actions[i])
			}
			if targets[i] != "gemini-2.5-pro" {
				t.Fatalf("goroutine %d target = %s; want gemini-2.5-pro", i, targets[i])
			}
			if accounts[i].ID != "acc-B" {
				t.Fatalf("goroutine %d account = %s; want acc-B", i, accounts[i].ID)
			}
		}

		// Account A must be exhausted
		curA, _ := accRepo.GetByID(ctx, "acc-A")
		if curA.Status != domain.AccountStatusExhausted {
			t.Fatalf("acc-A should be exhausted, got %s", curA.Status)
		}

		// Account B must be active and healthy
		curB, _ := accRepo.GetByID(ctx, "acc-B")
		if curB.Status != domain.AccountStatusActive || !curB.IsActive {
			t.Fatalf("acc-B should be active, got status=%s active=%v", curB.Status, curB.IsActive)
		}

		// CRITICAL INVARIANT: Account C must NOT have been touched!
		curC, _ := accRepo.GetByID(ctx, "acc-C")
		if curC.IsActive || curC.Status != domain.AccountStatusActive {
			t.Fatalf("anti-stampede failed: cascading rotation touched acc-C (isActive=%v status=%s)", curC.IsActive, curC.Status)
		}
	})

	t.Run("ConcurrentPredictiveCheck_50Goroutines_ZeroRaces", func(t *testing.T) {
		accRepo := newMockAccountRepo()
		quotaRepo := newMockQuotaRepo()
		accA := &domain.Account{ID: "acc-A", Email: "a@example.com", Status: domain.AccountStatusActive}
		accRepo.addAccount(accA)

		quotaRepo.setBuckets("acc-A", []*domain.QuotaBucket{
			{
				AccountID:         "acc-A",
				BucketID:          "acc-A-gemini-pro-5h",
				DisplayName:       "Gemini 2.5 Pro (5h)",
				RemainingFraction: 0.0,
			},
			{
				AccountID:         "acc-A",
				BucketID:          "acc-A-gemini-flash-5h",
				DisplayName:       "Gemini 2.5 Flash (5h)",
				RemainingFraction: 0.9,
			},
		})

		engine := NewFailoverEngine(
			accRepo, nil, nil,
			WithQuotaRepository(quotaRepo),
			WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", true),
		)

		const concurrency = 50
		var wg sync.WaitGroup
		shouldRewrites := make([]bool, concurrency)
		targets := make([]string, concurrency)
		errorsList := make([]error, concurrency)

		wg.Add(concurrency)
		for i := 0; i < concurrency; i++ {
			idx := i
			go func() {
				defer wg.Done()
				sr, tgt, err := engine.PredictiveCheck(ctx, accA, "gemini-2.5-pro")
				shouldRewrites[idx] = sr
				targets[idx] = tgt
				errorsList[idx] = err
			}()
		}
		wg.Wait()

		for i := 0; i < concurrency; i++ {
			if errorsList[i] != nil {
				t.Fatalf("goroutine %d failed: %v", i, errorsList[i])
			}
			if !shouldRewrites[i] {
				t.Fatalf("goroutine %d shouldRewrite = false; want true", i)
			}
			if targets[i] != "gemini-2.5-flash" {
				t.Fatalf("goroutine %d target = %s; want gemini-2.5-flash", i, targets[i])
			}
		}
	})
}

func TestFailoverEngine_SuccessfulRotation(t *testing.T) {
	repo := newMockAccountRepo()
	now := time.Now().UTC()
	accA := &domain.Account{
		ID:          "acc-A",
		Email:       "a@example.com",
		AccessToken: "token-A",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		UpdatedAt:   now.Add(-10 * time.Minute),
	}
	accB := &domain.Account{
		ID:          "acc-B",
		Email:       "b@example.com",
		AccessToken: "token-B",
		IsActive:    false,
		Status:      domain.AccountStatusActive,
		UpdatedAt:   now.Add(-5 * time.Minute),
	}
	repo.addAccount(accA)
	repo.addAccount(accB)

	broadcaster := NewBroadcaster(10)
	eventsCh, unsubscribe := broadcaster.Subscribe()
	defer unsubscribe()

	eventRepo := &mockEventRepo{}
	engine := NewFailoverEngine(repo, broadcaster, eventRepo)

	nextAcc, err := engine.RotateAccount(context.Background(), accA)
	if err != nil {
		t.Fatalf("expected successful rotation, got error: %v", err)
	}

	if nextAcc.ID != "acc-B" {
		t.Errorf("expected rotated account acc-B, got %s", nextAcc.ID)
	}

	// Verify Account A is marked exhausted
	updatedA, _ := repo.GetByID(context.Background(), "acc-A")
	if updatedA.Status != domain.AccountStatusExhausted {
		t.Errorf("expected acc-A status exhausted, got %s", updatedA.Status)
	}

	// Verify Account B is active
	activeAcc, _ := repo.GetActive(context.Background())
	if activeAcc.ID != "acc-B" {
		t.Errorf("expected active account acc-B, got %s", activeAcc.ID)
	}

	// Verify events were broadcast
	var receivedEvents []*domain.ProxyEvent
	for i := 0; i < 2; i++ {
		select {
		case ev := <-eventsCh:
			receivedEvents = append(receivedEvents, ev)
		case <-time.After(1 * time.Second):
			t.Fatalf("timed out waiting for event %d", i+1)
		}
	}

	if receivedEvents[0].Type != domain.EventTypeFailover429 {
		t.Errorf("expected first event %s, got %s", domain.EventTypeFailover429, receivedEvents[0].Type)
	}
	if receivedEvents[1].Type != domain.EventTypeAccountSwitched {
		t.Errorf("expected second event %s, got %s", domain.EventTypeAccountSwitched, receivedEvents[1].Type)
	}
}

func TestFailoverEngine_PoolExhaustion(t *testing.T) {
	repo := newMockAccountRepo()
	accA := &domain.Account{
		ID:          "acc-sole",
		Email:       "sole@example.com",
		AccessToken: "token-sole",
		IsActive:    true,
		Status:      domain.AccountStatusActive,
		UpdatedAt:   time.Now().UTC(),
	}
	repo.addAccount(accA)

	broadcaster := NewBroadcaster(10)
	eventsCh, unsubscribe := broadcaster.Subscribe()
	defer unsubscribe()

	engine := NewFailoverEngine(repo, broadcaster, nil)

	nextAcc, err := engine.RotateAccount(context.Background(), accA)
	if !errors.Is(err, domain.ErrNoAvailableAccount) {
		t.Fatalf("expected ErrNoAvailableAccount, got %v (acc: %+v)", err, nextAcc)
	}

	// Verify accA marked exhausted
	updatedA, _ := repo.GetByID(context.Background(), "acc-sole")
	if updatedA.Status != domain.AccountStatusExhausted {
		t.Errorf("expected exhausted status, got %s", updatedA.Status)
	}

	// Verify EventTypeQuotaExhausted was broadcast
	var quotaExhaustedReceived bool
	for i := 0; i < 2; i++ {
		select {
		case ev := <-eventsCh:
			if ev.Type == domain.EventTypeQuotaExhausted {
				quotaExhaustedReceived = true
			}
		case <-time.After(1 * time.Second):
		}
	}
	if !quotaExhaustedReceived {
		t.Error("expected EventTypeQuotaExhausted event to be broadcast")
	}
}

func TestFailoverEngine_NilAccount(t *testing.T) {
	repo := newMockAccountRepo()
	engine := NewFailoverEngine(repo, nil, nil)

	_, err := engine.RotateAccount(context.Background(), nil)
	if !errors.Is(err, domain.ErrAccountNotFound) {
		t.Errorf("expected ErrAccountNotFound, got %v", err)
	}

	act, _, _, err2 := engine.HandleExhaustion(context.Background(), nil, "gemini-2.5-pro")
	if !errors.Is(err2, domain.ErrAccountNotFound) {
		t.Errorf("expected ErrAccountNotFound from HandleExhaustion, got %v", err2)
	}
	if act != ActionNone {
		t.Errorf("expected ActionNone for nil account, got %v", act)
	}
}

func BenchmarkPredictiveCheck_CacheHit(b *testing.B) {
	acc := &domain.Account{ID: "acc-bench", Email: "bench@example.com"}
	engine := NewFailoverEngine(nil, nil, nil,
		WithModelFallback("claude-3-5-sonnet", "gemini-2.5-pro", true),
	)
	engine.UpdateQuotaCache(acc.ID, []*domain.QuotaBucket{
		{
			AccountID:         acc.ID,
			BucketID:          "claude-5h",
			DisplayName:       "Claude and GPT models (5h)",
			RemainingFraction: 0.0,
			ResetTime:         time.Now().Add(3 * time.Hour),
		},
		{
			AccountID:         acc.ID,
			BucketID:          "gemini-5h",
			DisplayName:       "Gemini Models (5h)",
			RemainingFraction: 0.8,
			ResetTime:         time.Now().Add(3 * time.Hour),
		},
	})

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		rewrite, target, err := engine.PredictiveCheck(ctx, acc, "claude-3-5-sonnet")
		if !rewrite || target != "gemini-2.5-pro" || err != nil {
			b.Fatalf("unexpected result: rewrite=%v, target=%s, err=%v", rewrite, target, err)
		}
	}
}

func TestFailoverEngine_SameCategory_SecondaryQuotaPreservedAfterPrimaryExhaustion(t *testing.T) {
	acc := &domain.Account{ID: "acc-same-cat", Email: "samecat@example.com", Status: domain.AccountStatusActive}
	mockRepo := newMockAccountRepo()
	mockRepo.addAccount(acc)
	engine := NewFailoverEngine(mockRepo, nil, nil,
		WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", true),
	)

	// Seed cache with Pro exhausted (0%) and Flash available (85%)
	engine.UpdateQuotaCache(acc.ID, []*domain.QuotaBucket{
		{
			AccountID:         acc.ID,
			BucketID:          "gemini-2.5-pro",
			DisplayName:       "Gemini 2.5 Pro",
			RemainingFraction: 1.0,
			RemainingAmount:   100,
			ResetTime:         time.Now().Add(5 * time.Hour),
		},
		{
			AccountID:         acc.ID,
			BucketID:          "gemini-2.5-flash",
			DisplayName:       "Gemini 2.5 Flash",
			RemainingFraction: 0.85,
			RemainingAmount:   850,
			ResetTime:         time.Now().Add(5 * time.Hour),
		},
	})

	ctx := context.Background()

	// Reactive 429 on primary model (gemini-2.5-pro)
	action, targetModel, nextAcc, err := engine.HandleExhaustion(ctx, acc, "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("HandleExhaustion unexpected error: %v", err)
	}
	if action != ActionFallbackSecondary {
		t.Fatalf("expected ActionFallbackSecondary, got %v", action)
	}
	if targetModel != "gemini-2.5-flash" {
		t.Errorf("expected targetModel 'gemini-2.5-flash', got %s", targetModel)
	}
	if nextAcc.ID != acc.ID {
		t.Errorf("expected same account %s, got %s", acc.ID, nextAcc.ID)
	}

	// Verify that flash quota was NOT zeroed out in quotaCache
	buckets := engine.quotaCache[acc.ID]
	for _, b := range buckets {
		if strings.Contains(b.bucketIDLower, "flash") {
			if b.remainingFraction <= 0.0 {
				t.Fatalf("flash bucket was incorrectly zeroed out by primary exhaustion!")
			}
		}
	}

	// Subsequent predictive check must still find secondary available and rewrite!
	rewrite, target, err := engine.PredictiveCheck(ctx, acc, "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("PredictiveCheck unexpected error: %v", err)
	}
	if !rewrite {
		t.Fatalf("expected predictive rewrite to succeed because flash still has quota")
	}
	if target != "gemini-2.5-flash" {
		t.Errorf("expected predictive target 'gemini-2.5-flash', got %s", target)
	}
}

func TestFailoverEngine_PredictiveCheck_MixedCase(t *testing.T) {
	acc := &domain.Account{ID: "acc-case", Email: "case@example.com", Status: domain.AccountStatusActive}
	mockRepo := newMockAccountRepo()
	mockRepo.addAccount(acc)
	engine := NewFailoverEngine(mockRepo, nil, nil,
		WithModelFallback("gemini-2.5-pro", "gemini-2.5-flash", true),
	)

	engine.UpdateQuotaCache(acc.ID, []*domain.QuotaBucket{
		{
			AccountID:         acc.ID,
			BucketID:          "gemini-2.5-pro",
			DisplayName:       "Gemini 2.5 Pro",
			RemainingFraction: 0.0,
			ResetTime:         time.Now().Add(5 * time.Hour),
		},
		{
			AccountID:         acc.ID,
			BucketID:          "gemini-2.5-flash",
			DisplayName:       "Gemini 2.5 Flash",
			RemainingFraction: 0.9,
			ResetTime:         time.Now().Add(5 * time.Hour),
		},
	})

	ctx := context.Background()

	cases := []string{"Gemini-2.5-Pro", "GEMINI-2.5-PRO", "models/Gemini-2.5-Pro"}
	for _, c := range cases {
		rewrite, target, err := engine.PredictiveCheck(ctx, acc, c)
		if err != nil {
			t.Errorf("[%s] unexpected error: %v", c, err)
		}
		if !rewrite {
			t.Errorf("[%s] expected rewrite for mixed-case model name, got false", c)
		}
		if target != "gemini-2.5-flash" {
			t.Errorf("[%s] expected target 'gemini-2.5-flash', got %s", c, target)
		}
	}
}
