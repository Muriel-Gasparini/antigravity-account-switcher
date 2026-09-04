package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/config"
	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

// FailoverAction specifies the failover decision reached by the FailoverEngine.
type FailoverAction int

const (
	// ActionNone indicates no failover action was taken (e.g. invalid account or no-op).
	ActionNone FailoverAction = iota

	// ActionFallbackSecondary indicates that the request should be rewritten to the secondary
	// model tier and replayed on the SAME active account.
	ActionFallbackSecondary

	// ActionRotateAccount indicates that intra-account tiers are exhausted (or fallback is disabled),
	// and the proxy must switch to the next available account in the pool, resetting the target
	// model back to the primary model tier.
	ActionRotateAccount
)

// String returns a canonical string representation of FailoverAction.
func (a FailoverAction) String() string {
	switch a {
	case ActionNone:
		return "ActionNone"
	case ActionFallbackSecondary:
		return "ActionFallbackSecondary"
	case ActionRotateAccount:
		return "ActionRotateAccount"
	default:
		return fmt.Sprintf("FailoverAction(%d)", a)
	}
}

// IsExhaustionResponse checks whether an HTTP status code and response body
// indicate quota exhaustion (HTTP 429 Too Many Requests or HTTP 403 RESOURCE_EXHAUSTED).
func IsExhaustionResponse(statusCode int, body []byte) bool {
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	if statusCode == http.StatusForbidden && len(body) > 0 {
		return bytes.Contains(body, []byte("RESOURCE_EXHAUSTED")) ||
			bytes.Contains(body, []byte("RATE_LIMIT_EXCEEDED")) ||
			bytes.Contains(body, []byte("QuotaFailure"))
	}
	return false
}

// cachedQuotaBucket represents a normalized in-memory quota bucket snapshot.
type cachedQuotaBucket struct {
	category          ModelCategory
	bucketIDLower     string
	displayNameLower  string
	modelNameLower    string
	hasPro            bool
	hasFlash          bool
	remainingFraction float64
	remainingAmount   int64
	resetTime         time.Time
}

func (c *cachedQuotaBucket) isExhausted() bool {
	return c.remainingFraction <= 0.0 || (c.remainingAmount == 0 && c.remainingFraction < 0.05)
}

func (c *cachedQuotaBucket) hasReset(now time.Time) bool {
	return !c.resetTime.IsZero() && (now.After(c.resetTime) || now.Equal(c.resetTime))
}

// accountFallbackState tracks the intra-account model exhaustion state for an account.
type accountFallbackState struct {
	primaryExhausted   bool
	primaryResetTime   time.Time
	secondaryExhausted bool
	lastFallback       time.Time
}

// FailoverOption provides functional options to configure FailoverEngine.
type FailoverOption func(*FailoverEngine)

// WithQuotaRepository configures the QuotaRepository for bucket inspection.
func WithQuotaRepository(repo domain.QuotaRepository) FailoverOption {
	return func(f *FailoverEngine) {
		f.quotaRepo = repo
	}
}

// WithModelFallback configures the model fallback primary, secondary, and enabled flag.
func WithModelFallback(primary, secondary string, enabled bool) FailoverOption {
	return func(f *FailoverEngine) {
		f.updateFallbackFieldsLocked(primary, secondary, enabled)
	}
}

// WithModelFallbackConfig is an alias for WithModelFallback.
func WithModelFallbackConfig(primary, secondary string, enabled bool) FailoverOption {
	return WithModelFallback(primary, secondary, enabled)
}

// WithFallbackConfig is an alias for WithModelFallback.
func WithFallbackConfig(primary, secondary string, enabled bool) FailoverOption {
	return WithModelFallback(primary, secondary, enabled)
}

// FailoverEngine coordinates quota exhaustion detection, intra-account model fallback,
// and atomic active account failover rotation with anti-stampede concurrency protection.
type FailoverEngine struct {
	accountRepo              domain.AccountRepository
	quotaRepo                domain.QuotaRepository
	eventBroadcaster         domain.EventBroadcaster
	eventRepo                domain.EventRepository
	modelPrimary             string
	modelSecondary           string
	fallbackSecondaryEnabled bool

	// Pre-computed normalized fields to ensure 0 allocations on hot path
	normPrimary       string
	normSecondary     string
	primaryLower      string
	secondaryLower    string
	primaryCat        ModelCategory
	secondaryCat      ModelCategory
	primaryHasPro     bool
	primaryHasFlash   bool
	secondaryHasPro   bool
	secondaryHasFlash bool

	mu            sync.RWMutex
	accountStates map[string]*accountFallbackState
	quotaCache    map[string][]cachedQuotaBucket
}

// NewFailoverEngine constructs a new FailoverEngine with optional functional configuration options.
func NewFailoverEngine(
	accountRepo domain.AccountRepository,
	eventBroadcaster domain.EventBroadcaster,
	eventRepo domain.EventRepository,
	opts ...FailoverOption,
) *FailoverEngine {
	f := &FailoverEngine{
		accountRepo:              accountRepo,
		eventBroadcaster:         eventBroadcaster,
		eventRepo:                eventRepo,
		modelPrimary:             config.DefaultModelPrimary,
		modelSecondary:           config.DefaultModelSecondary,
		fallbackSecondaryEnabled: false,
		accountStates:            make(map[string]*accountFallbackState),
		quotaCache:               make(map[string][]cachedQuotaBucket),
	}
	f.updateFallbackFieldsLocked(config.DefaultModelPrimary, config.DefaultModelSecondary, false)

	for _, opt := range opts {
		opt(f)
	}
	return f
}

func (f *FailoverEngine) updateFallbackFieldsLocked(primary, secondary string, enabled bool) {
	if p := strings.TrimSpace(primary); p != "" {
		f.modelPrimary = p
	}
	if s := strings.TrimSpace(secondary); s != "" {
		f.modelSecondary = s
	}
	f.fallbackSecondaryEnabled = enabled

	f.normPrimary = NormalizeModelName(f.modelPrimary)
	f.normSecondary = NormalizeModelName(f.modelSecondary)
	f.primaryLower = strings.ToLower(f.normPrimary)
	f.secondaryLower = strings.ToLower(f.normSecondary)
	f.primaryCat = CategorizeModel(f.normPrimary)
	f.secondaryCat = CategorizeModel(f.normSecondary)
	f.primaryHasPro = strings.Contains(f.primaryLower, "pro")
	f.primaryHasFlash = strings.Contains(f.primaryLower, "flash")
	f.secondaryHasPro = strings.Contains(f.secondaryLower, "pro")
	f.secondaryHasFlash = strings.Contains(f.secondaryLower, "flash")
}

// SetFallbackConfig dynamically updates the fallback configuration in a thread-safe manner.
func (f *FailoverEngine) SetFallbackConfig(primary, secondary string, enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateFallbackFieldsLocked(primary, secondary, enabled)
}

// SetQuotaRepository dynamically updates the quota repository.
func (f *FailoverEngine) SetQuotaRepository(repo domain.QuotaRepository) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quotaRepo = repo
}

// ResetAccountState clears cached exhaustion flags and fallback state for the given account.
func (f *FailoverEngine) ResetAccountState(accountID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.accountStates, accountID)
	delete(f.quotaCache, accountID)
}

// ResetAccountFallback is an alias for ResetAccountState.
func (f *FailoverEngine) ResetAccountFallback(accountID string) {
	f.ResetAccountState(accountID)
}

// ResetAllStates clears all cached account fallback and exhaustion states.
func (f *FailoverEngine) ResetAllStates() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accountStates = make(map[string]*accountFallbackState)
	f.quotaCache = make(map[string][]cachedQuotaBucket)
}

// UpdateQuotaCache populates or updates in-memory quota buckets for an account.
// Accepts either []*domain.QuotaBucket or []domain.QuotaBucket.
// If fresh buckets show that primary or secondary model quota is restored,
// corresponding in-memory exhaustion flags are automatically cleared.
func (f *FailoverEngine) UpdateQuotaCache(accountID string, buckets any) {
	if accountID == "" || buckets == nil {
		return
	}
	var ptrs []*domain.QuotaBucket
	switch b := buckets.(type) {
	case []*domain.QuotaBucket:
		ptrs = b
	case []domain.QuotaBucket:
		ptrs = make([]*domain.QuotaBucket, len(b))
		for i := range b {
			ptrs[i] = &b[i]
		}
	default:
		return
	}

	cached := make([]cachedQuotaBucket, 0, len(ptrs))
	for _, b := range ptrs {
		if b == nil {
			continue
		}
		cat := CategorizeModel(b.BucketID)
		if cat == CategoryUnknown {
			cat = CategorizeModel(b.DisplayName)
		}
		if cat == CategoryUnknown {
			if MatchesBucket(b, CategoryClaudeGPT) {
				cat = CategoryClaudeGPT
			} else if MatchesBucket(b, CategoryGemini) {
				cat = CategoryGemini
			}
		}

		bidLower := strings.ToLower(b.BucketID)
		dnameLower := strings.ToLower(b.DisplayName)
		cleanModel := strings.ToLower(NormalizeModelName(b.BucketID))
		hasPro := strings.Contains(bidLower, "pro") || strings.Contains(dnameLower, "pro")
		hasFlash := strings.Contains(bidLower, "flash") || strings.Contains(dnameLower, "flash")

		cached = append(cached, cachedQuotaBucket{
			category:          cat,
			bucketIDLower:     bidLower,
			displayNameLower:  dnameLower,
			modelNameLower:    cleanModel,
			hasPro:            hasPro,
			hasFlash:          hasFlash,
			remainingFraction: b.RemainingFraction,
			remainingAmount:   b.RemainingAmount,
			resetTime:         b.ResetTime,
		})
	}

	f.mu.Lock()
	if f.quotaCache == nil {
		f.quotaCache = make(map[string][]cachedQuotaBucket)
	}
	f.quotaCache[accountID] = cached

	if state, exists := f.accountStates[accountID]; exists {
		now := time.Now().UTC()
		if state.primaryExhausted {
			for i := range cached {
				b := &cached[i]
				if f.matchesCachedBucket(b, f.primaryLower, f.primaryCat, f.primaryHasPro, f.primaryHasFlash, f.secondaryLower, f.secondaryCat, f.secondaryHasPro, f.secondaryHasFlash) {
					if !b.isExhausted() || b.hasReset(now) || b.remainingFraction > 0.05 {
						state.primaryExhausted = false
						break
					}
				}
			}
		}
		if state.secondaryExhausted {
			for i := range cached {
				b := &cached[i]
				if f.matchesCachedBucket(b, f.secondaryLower, f.secondaryCat, f.secondaryHasPro, f.secondaryHasFlash, f.primaryLower, f.primaryCat, f.primaryHasPro, f.primaryHasFlash) {
					if !b.isExhausted() || b.hasReset(now) || b.remainingFraction > 0.05 {
						state.secondaryExhausted = false
						break
					}
				}
			}
		}
	}
	f.mu.Unlock()
}

// UpdateQuotaBuckets populates or updates in-memory quota buckets from a slice of values.
func (f *FailoverEngine) UpdateQuotaBuckets(accountID string, buckets []domain.QuotaBucket) {
	f.UpdateQuotaCache(accountID, buckets)
}

// ClearQuotaCache empties the in-memory quota cache.
func (f *FailoverEngine) ClearQuotaCache() {
	f.mu.Lock()
	f.quotaCache = make(map[string][]cachedQuotaBucket)
	f.mu.Unlock()
}

// MarkCategoryExhausted records an immediate exhaustion for a category on an account.
func (f *FailoverEngine) MarkCategoryExhausted(accountID string, cat ModelCategory, resetTime time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markCategoryExhaustedLocked(accountID, cat, resetTime)
}

func (f *FailoverEngine) markCategoryExhaustedLocked(accountID string, cat ModelCategory, resetTime time.Time) {
	if accountID == "" || cat == CategoryUnknown {
		return
	}
	if resetTime.IsZero() {
		resetTime = time.Now().UTC().Add(5 * time.Hour)
	}
	if f.quotaCache == nil {
		f.quotaCache = make(map[string][]cachedQuotaBucket)
	}
	buckets := f.quotaCache[accountID]
	updated := false
	for i := range buckets {
		if buckets[i].category == cat {
			buckets[i].remainingFraction = 0.0
			buckets[i].remainingAmount = 0
			buckets[i].resetTime = resetTime
			updated = true
		}
	}
	if !updated {
		f.quotaCache[accountID] = append(buckets, cachedQuotaBucket{
			category:          cat,
			remainingFraction: 0.0,
			remainingAmount:   0,
			resetTime:         resetTime,
		})
	}
}

func (f *FailoverEngine) ensureQuotaBucketsPrefetched(ctx context.Context, accountID string) {
	if accountID == "" || f.quotaRepo == nil {
		return
	}
	f.mu.RLock()
	cached := f.quotaCache != nil && len(f.quotaCache[accountID]) > 0
	f.mu.RUnlock()
	if cached {
		return
	}

	qCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	dbBuckets, err := f.quotaRepo.GetByAccountID(qCtx, accountID)
	if err != nil || len(dbBuckets) == 0 {
		return
	}
	f.UpdateQuotaCache(accountID, dbBuckets)
}

func (f *FailoverEngine) markModelExhaustedLocked(accountID string, model string, resetTime time.Time) {
	if accountID == "" || model == "" {
		return
	}
	if resetTime.IsZero() {
		resetTime = time.Now().UTC().Add(5 * time.Hour)
	}
	if f.quotaCache == nil {
		f.quotaCache = make(map[string][]cachedQuotaBucket)
	}
	buckets := f.quotaCache[accountID]
	norm := NormalizeModelName(model)
	lower := strings.ToLower(norm)
	cat := CategorizeModel(norm)
	hasPro := strings.Contains(lower, "pro")
	hasFlash := strings.Contains(lower, "flash")

	var otherLower string
	var otherCat ModelCategory
	var otherHasPro, otherHasFlash bool
	if f.isPrimaryModelLocked(model) {
		otherLower = f.secondaryLower
		otherCat = f.secondaryCat
		otherHasPro = f.secondaryHasPro
		otherHasFlash = f.secondaryHasFlash
	} else if f.isSecondaryModelLocked(model) {
		otherLower = f.primaryLower
		otherCat = f.primaryCat
		otherHasPro = f.primaryHasPro
		otherHasFlash = f.primaryHasFlash
	}

	updated := false
	for i := range buckets {
		b := &buckets[i]
		if f.matchesCachedBucket(b, lower, cat, hasPro, hasFlash, otherLower, otherCat, otherHasPro, otherHasFlash) {
			b.remainingFraction = 0.0
			b.remainingAmount = 0
			b.resetTime = resetTime
			updated = true
		}
	}
	if !updated {
		f.quotaCache[accountID] = append(buckets, cachedQuotaBucket{
			category:          cat,
			bucketIDLower:     lower,
			displayNameLower:  lower,
			modelNameLower:    lower,
			hasPro:            hasPro,
			hasFlash:          hasFlash,
			remainingFraction: 0.0,
			remainingAmount:   0,
			resetTime:         resetTime,
		})
	}
}

func (f *FailoverEngine) effectivePrimaryModel() string {
	if f.modelPrimary != "" {
		return f.modelPrimary
	}
	return config.DefaultModelPrimary
}

func (f *FailoverEngine) effectiveSecondaryModel() string {
	if f.modelSecondary != "" {
		return f.modelSecondary
	}
	return config.DefaultModelSecondary
}

func (f *FailoverEngine) isPrimaryModelLocked(model string) bool {
	norm := NormalizeModelName(model)
	if norm == "" || f.normPrimary == "" {
		return false
	}
	if strings.EqualFold(norm, f.normPrimary) {
		return true
	}
	lowerNorm := strings.ToLower(norm)
	if f.primaryHasFlash && strings.Contains(lowerNorm, "flash") && !strings.Contains(lowerNorm, "pro") {
		return true
	}
	if f.primaryHasPro && strings.Contains(lowerNorm, "pro") && !strings.Contains(lowerNorm, "flash") {
		return true
	}

	cat := CategorizeModel(norm)
	return cat != CategoryUnknown && cat == f.primaryCat && cat != f.secondaryCat
}

func (f *FailoverEngine) isSecondaryModelLocked(model string) bool {
	norm := NormalizeModelName(model)
	if norm == "" || f.normSecondary == "" {
		return false
	}
	if strings.EqualFold(norm, f.normSecondary) {
		return true
	}
	lowerNorm := strings.ToLower(norm)
	if f.secondaryHasFlash && strings.Contains(lowerNorm, "flash") && !strings.Contains(lowerNorm, "pro") {
		return true
	}
	if f.secondaryHasPro && strings.Contains(lowerNorm, "pro") && !strings.Contains(lowerNorm, "flash") {
		return true
	}

	cat := CategorizeModel(norm)
	return cat != CategoryUnknown && cat == f.secondaryCat && cat != f.primaryCat
}

func (f *FailoverEngine) matchesCachedBucket(
	b *cachedQuotaBucket,
	targetLower string,
	targetCat ModelCategory,
	targetHasPro bool,
	targetHasFlash bool,
	otherLower string,
	otherCat ModelCategory,
	otherHasPro bool,
	otherHasFlash bool,
) bool {
	if b == nil {
		return false
	}

	targetInBucket := targetLower != "" && (strings.Contains(b.bucketIDLower, targetLower) || strings.Contains(b.displayNameLower, targetLower))
	otherInBucket := otherLower != "" && (strings.Contains(b.bucketIDLower, otherLower) || strings.Contains(b.displayNameLower, otherLower))

	if targetInBucket && !otherInBucket {
		return true
	}
	if otherInBucket && !targetInBucket {
		return false
	}

	if targetHasPro && b.hasPro && !b.hasFlash {
		return true
	}
	if targetHasFlash && b.hasFlash && !b.hasPro {
		return true
	}

	if targetCat != CategoryUnknown && targetCat != otherCat {
		if b.category == targetCat {
			return true
		}
	}

	if b.modelNameLower != "" && targetLower != "" && strings.Contains(b.modelNameLower, targetLower) {
		return true
	}

	return false
}

func (f *FailoverEngine) getAccountBucketsLocked(ctx context.Context, accountID string) []cachedQuotaBucket {
	if accountID == "" {
		return nil
	}
	if f.quotaCache == nil {
		f.quotaCache = make(map[string][]cachedQuotaBucket)
	}
	if buckets, ok := f.quotaCache[accountID]; ok {
		return buckets
	}
	if f.quotaRepo == nil {
		return nil
	}

	qCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	dbBuckets, err := f.quotaRepo.GetByAccountID(qCtx, accountID)
	if err != nil || len(dbBuckets) == 0 {
		f.quotaCache[accountID] = []cachedQuotaBucket{}
		return nil
	}

	cached := make([]cachedQuotaBucket, 0, len(dbBuckets))
	for _, b := range dbBuckets {
		if b == nil {
			continue
		}
		cat := CategorizeModel(b.BucketID)
		if cat == CategoryUnknown {
			cat = CategorizeModel(b.DisplayName)
		}
		if cat == CategoryUnknown {
			if MatchesBucket(b, CategoryClaudeGPT) {
				cat = CategoryClaudeGPT
			} else if MatchesBucket(b, CategoryGemini) {
				cat = CategoryGemini
			}
		}

		bidLower := strings.ToLower(b.BucketID)
		dnameLower := strings.ToLower(b.DisplayName)
		cleanModel := strings.ToLower(NormalizeModelName(b.BucketID))
		hasPro := strings.Contains(bidLower, "pro") || strings.Contains(dnameLower, "pro")
		hasFlash := strings.Contains(bidLower, "flash") || strings.Contains(dnameLower, "flash")

		cached = append(cached, cachedQuotaBucket{
			category:          cat,
			bucketIDLower:     bidLower,
			displayNameLower:  dnameLower,
			modelNameLower:    cleanModel,
			hasPro:            hasPro,
			hasFlash:          hasFlash,
			remainingFraction: b.RemainingFraction,
			remainingAmount:   b.RemainingAmount,
			resetTime:         b.ResetTime,
		})
	}
	f.quotaCache[accountID] = cached
	return cached
}

func (f *FailoverEngine) hasSecondaryQuotaLocked(ctx context.Context, acc *domain.Account, state *accountFallbackState) bool {
	if state != nil && state.secondaryExhausted {
		return false
	}
	buckets := f.getAccountBucketsLocked(ctx, acc.ID)
	if len(buckets) == 0 {
		return true
	}

	now := time.Now().UTC()
	for i := range buckets {
		b := &buckets[i]
		if f.matchesCachedBucket(b, f.secondaryLower, f.secondaryCat, f.secondaryHasPro, f.secondaryHasFlash, f.primaryLower, f.primaryCat, f.primaryHasPro, f.primaryHasFlash) {
			if b.isExhausted() && !b.hasReset(now) {
				return false
			}
		}
	}

	return true
}

// PredictiveCheck evaluates whether an in-flight request should be proactively rewritten
// from the primary model tier to the secondary model tier before dispatching upstream.
// Employs a non-blocking shared read lock for sub-microsecond latency and 0 allocs on cache hit.
func (f *FailoverEngine) PredictiveCheck(
	ctx context.Context,
	acc *domain.Account,
	requestedModel string,
) (shouldRewrite bool, targetModel string, err error) {
	if f == nil {
		return false, requestedModel, nil
	}
	if ctx != nil && ctx.Err() != nil {
		return false, requestedModel, ctx.Err()
	}
	if acc == nil || requestedModel == "" {
		return false, requestedModel, nil
	}

	f.mu.RLock()
	enabled := f.fallbackSecondaryEnabled
	if !enabled {
		f.mu.RUnlock()
		return false, requestedModel, nil
	}

	normPri := f.normPrimary
	normSec := f.normSecondary
	priLower := f.primaryLower
	secLower := f.secondaryLower
	priCat := f.primaryCat
	secCat := f.secondaryCat
	priHasPro := f.primaryHasPro
	priHasFlash := f.primaryHasFlash
	secHasPro := f.secondaryHasPro
	secHasFlash := f.secondaryHasFlash
	modelSec := f.modelSecondary

	normReq := NormalizeModelName(requestedModel)

	// If request already targets secondary, do not rewrite
	if strings.EqualFold(normReq, normSec) {
		f.mu.RUnlock()
		return false, requestedModel, nil
	}

	catReq := CategorizeModel(requestedModel)

	// Fast check if matches primary
	lowerNormReq := strings.ToLower(normReq)
	matchesPrimary := strings.EqualFold(normReq, normPri) ||
		(priCat != CategoryUnknown && priCat != secCat && catReq == priCat) ||
		(priHasPro && strings.Contains(lowerNormReq, "pro") && !strings.Contains(lowerNormReq, "flash")) ||
		(priHasFlash && strings.Contains(lowerNormReq, "flash") && !strings.Contains(lowerNormReq, "pro"))

	if !matchesPrimary {
		f.mu.RUnlock()
		return false, requestedModel, nil
	}

	// Check cache under RLock
	buckets, ok := f.quotaCache[acc.ID]
	state := f.accountStates[acc.ID]
	now := time.Now().UTC()

	cachedPrimaryExhausted := false
	if ok && len(buckets) > 0 {
		for i := range buckets {
			b := &buckets[i]
			if f.matchesCachedBucket(b, priLower, priCat, priHasPro, priHasFlash, secLower, secCat, secHasPro, secHasFlash) {
				if b.isExhausted() && !b.hasReset(now) {
					cachedPrimaryExhausted = true
					break
				}
			}
		}
	}

	// If cache is missing, or primary was previously exhausted, or in-memory bucket indicates exhaustion,
	// re-query quota buckets from persistence to verify whether upstream quota has been restored.
	needDBQuery := (!ok || cachedPrimaryExhausted || (state != nil && state.primaryExhausted)) && f.quotaRepo != nil
	if needDBQuery {
		f.mu.RUnlock()
		dbBuckets, qErr := f.quotaRepo.GetByAccountID(ctx, acc.ID)
		if qErr == nil {
			f.UpdateQuotaCache(acc.ID, dbBuckets)
		} else {
			f.mu.Lock()
			if f.quotaCache == nil {
				f.quotaCache = make(map[string][]cachedQuotaBucket)
			}
			f.quotaCache[acc.ID] = []cachedQuotaBucket{}
			f.mu.Unlock()
		}
		f.mu.RLock()
		buckets = f.quotaCache[acc.ID]
		state = f.accountStates[acc.ID]
		now = time.Now().UTC()
	}

	// 1. Check if primary model is exhausted
	primaryExhausted := false
	if state != nil && state.primaryExhausted {
		// If primary reset time has elapsed, clear the exhaustion state
		if !state.primaryResetTime.IsZero() && (now.After(state.primaryResetTime) || now.Equal(state.primaryResetTime)) {
			f.mu.RUnlock()
			f.mu.Lock()
			if st := f.accountStates[acc.ID]; st != nil {
				st.primaryExhausted = false
			}
			f.mu.Unlock()
			f.mu.RLock()
			state = f.accountStates[acc.ID]
		} else {
			primaryExhausted = true
		}
	}

	// Check if in-memory buckets show primary quota is restored or has reset
	if primaryExhausted && len(buckets) > 0 {
		for i := range buckets {
			b := &buckets[i]
			if f.matchesCachedBucket(b, priLower, priCat, priHasPro, priHasFlash, secLower, secCat, secHasPro, secHasFlash) {
				if !b.isExhausted() || b.hasReset(now) || b.remainingFraction > 0.05 {
					f.mu.RUnlock()
					f.mu.Lock()
					if st := f.accountStates[acc.ID]; st != nil {
						st.primaryExhausted = false
					}
					f.mu.Unlock()
					f.mu.RLock()
					state = f.accountStates[acc.ID]
					primaryExhausted = false
					break
				}
			}
		}
	}

	if !primaryExhausted && len(buckets) > 0 {
		for i := range buckets {
			b := &buckets[i]
			if f.matchesCachedBucket(b, priLower, priCat, priHasPro, priHasFlash, secLower, secCat, secHasPro, secHasFlash) {
				if b.isExhausted() && !b.hasReset(now) {
					primaryExhausted = true
					break
				}
			}
		}
	}

	if !primaryExhausted {
		f.mu.RUnlock()
		return false, requestedModel, nil
	}

	// 2. Check if secondary model has available quota
	secondaryAvailable := true
	if state != nil && state.secondaryExhausted {
		secondaryAvailable = false
	}
	if secondaryAvailable && len(buckets) > 0 {
		for i := range buckets {
			b := &buckets[i]
			if f.matchesCachedBucket(b, secLower, secCat, secHasPro, secHasFlash, priLower, priCat, priHasPro, priHasFlash) {
				if b.isExhausted() && !b.hasReset(now) {
					secondaryAvailable = false
					break
				}
			}
		}
	}

	f.mu.RUnlock()

	if !secondaryAvailable {
		return false, requestedModel, nil
	}

	// Secondary is available: emit event if broadcaster/repo present and return rewrite
	if f.eventBroadcaster != nil || f.eventRepo != nil {
		f.emitEvent(&domain.ProxyEvent{
			Type:      domain.EventTypeModelFallback,
			AccountID: acc.ID,
			Message:   fmt.Sprintf("Predictive fallback from %s to %s for account %s", requestedModel, modelSec, acc.Email),
			Details: map[string]any{
				"account_id":      acc.ID,
				"email":           acc.Email,
				"mode":            "predictive",
				"reason":          "predictive_quota",
				"requested_model": requestedModel,
				"target_model":    modelSec,
			},
			Timestamp: time.Now().UTC(),
		})
	}

	return true, modelSec, nil
}

// HandleExhaustion processes reactive HTTP 429 / RESOURCE_EXHAUSTED errors.
// Under mutex serialization and anti-stampede protection, it determines whether to:
// 1. Fall back to secondary tier on the SAME account (ActionFallbackSecondary).
// 2. Rotate to the next available account in the pool (ActionRotateAccount).
// Parallel in-flight requests that were queued behind rotation immediately adopt the new active account.
func (f *FailoverEngine) HandleExhaustion(
	ctx context.Context,
	acc *domain.Account,
	failedModel string,
) (action FailoverAction, targetModel string, nextAcc *domain.Account, err error) {
	if acc == nil {
		return ActionNone, "", nil, domain.ErrAccountNotFound
	}
	if err := ctx.Err(); err != nil {
		return ActionNone, "", nil, err
	}

	// Prefetch quota outside lock so slow I/O doesn't block critical section
	f.ensureQuotaBucketsPrefetched(ctx, acc.ID)

	var eventsToEmit []*domain.ProxyEvent
	defer func() {
		for _, ev := range eventsToEmit {
			f.emitEvent(ev)
		}
	}()

	f.mu.Lock()
	defer f.mu.Unlock()

	primary := f.effectivePrimaryModel()
	secondary := f.effectiveSecondaryModel()

	// 1. Anti-stampede check: Did another concurrent request already rotate away from acc?
	currentActive, err := f.accountRepo.GetActive(ctx)
	if err == nil && currentActive != nil && currentActive.ID != acc.ID && currentActive.IsAvailable() {
		return ActionRotateAccount, primary, currentActive, nil
	}

	// 2. If fallback is disabled or no model was requested, rotate immediately to next account
	if !f.fallbackSecondaryEnabled || strings.TrimSpace(failedModel) == "" {
		next, rotErr := f.rotateAccountLocked(ctx, acc, &eventsToEmit)
		if rotErr != nil {
			return ActionRotateAccount, primary, nil, rotErr
		}
		return ActionRotateAccount, primary, next, nil
	}

	// 3. Fallback is enabled: retrieve or initialize account state
	state, exists := f.accountStates[acc.ID]
	if !exists {
		state = &accountFallbackState{}
		f.accountStates[acc.ID] = state
	}

	isSecondary := f.isSecondaryModelLocked(failedModel)
	hasSecondaryQuota := f.hasSecondaryQuotaLocked(ctx, acc, state)

	// 4. If failedModel was primary and secondary has available quota: intra-account fallback!
	if !isSecondary && hasSecondaryQuota {
		wasPrimaryExhausted := state.primaryExhausted
		state.primaryExhausted = true
		state.lastFallback = time.Now().UTC()

		// Record primary reset time from cached/db buckets or default to 5 minutes
		var resetTime time.Time
		buckets := f.getAccountBucketsLocked(ctx, acc.ID)
		for i := range buckets {
			b := &buckets[i]
			if f.matchesCachedBucket(b, f.primaryLower, f.primaryCat, f.primaryHasPro, f.primaryHasFlash, f.secondaryLower, f.secondaryCat, f.secondaryHasPro, f.secondaryHasFlash) {
				if !b.resetTime.IsZero() && b.resetTime.After(time.Now().UTC()) {
					resetTime = b.resetTime
					break
				}
			}
		}
		if resetTime.IsZero() {
			resetTime = time.Now().UTC().Add(5 * time.Minute)
		}
		state.primaryResetTime = resetTime

		f.markModelExhaustedLocked(acc.ID, primary, resetTime)

		// Emit EventTypeModelFallback once per transition
		if !wasPrimaryExhausted {
			eventsToEmit = append(eventsToEmit, &domain.ProxyEvent{
				Type:      domain.EventTypeModelFallback,
				AccountID: acc.ID,
				Message:   fmt.Sprintf("Falling back to secondary model %s for account %s after 429 on %s", secondary, acc.Email, failedModel),
				Details: map[string]any{
					"account_id":      acc.ID,
					"email":           acc.Email,
					"failed_model":    failedModel,
					"target_model":    secondary,
					"mode":            "reactive_429",
					"fallback_reason": "reactive_429",
				},
				Timestamp: time.Now().UTC(),
			})
		}

		return ActionFallbackSecondary, secondary, acc, nil
	}

	// 5. If failedModel was secondary OR secondary is also exhausted: total account exhaustion
	state.primaryExhausted = true
	state.secondaryExhausted = true
	if state.primaryResetTime.IsZero() || time.Now().UTC().After(state.primaryResetTime) {
		state.primaryResetTime = time.Now().UTC().Add(5 * time.Minute)
	}

	next, rotErr := f.rotateAccountLocked(ctx, acc, &eventsToEmit)
	if rotErr != nil {
		return ActionRotateAccount, primary, nil, rotErr
	}

	return ActionRotateAccount, primary, next, nil
}

// RotateAccount handles failover for an account that received HTTP 429 / RESOURCE_EXHAUSTED.
// Retained for backward compatibility and direct rotation invocations.
func (f *FailoverEngine) RotateAccount(ctx context.Context, exhaustedAcc *domain.Account) (*domain.Account, error) {
	if exhaustedAcc == nil {
		return nil, domain.ErrAccountNotFound
	}

	var eventsToEmit []*domain.ProxyEvent
	defer func() {
		for _, ev := range eventsToEmit {
			f.emitEvent(ev)
		}
	}()

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.rotateAccountLocked(ctx, exhaustedAcc, &eventsToEmit)
}

func (f *FailoverEngine) rotateAccountLocked(
	ctx context.Context,
	exhaustedAcc *domain.Account,
	eventsToEmit *[]*domain.ProxyEvent,
) (*domain.Account, error) {
	emit := func(ev *domain.ProxyEvent) {
		if eventsToEmit != nil {
			*eventsToEmit = append(*eventsToEmit, ev)
		} else {
			f.emitEvent(ev)
		}
	}

	// 1. Anti-stampede check: Did another concurrent request already rotate away from exhaustedAcc?
	currentActive, err := f.accountRepo.GetActive(ctx)
	if err == nil && currentActive != nil && currentActive.ID != exhaustedAcc.ID && currentActive.IsAvailable() {
		delete(f.accountStates, currentActive.ID)
		return currentActive, nil
	}

	// 2. Mark the exhausted account in the repository
	if err := f.accountRepo.UpdateStatus(ctx, exhaustedAcc.ID, domain.AccountStatusExhausted); err != nil {
		emit(&domain.ProxyEvent{
			Type:      domain.EventTypeError,
			AccountID: exhaustedAcc.ID,
			Message:   fmt.Sprintf("Failed to update status for exhausted account %s: %v", exhaustedAcc.Email, err),
			Timestamp: time.Now().UTC(),
		})
	}

	// Broadcast EventTypeFailover429
	emit(&domain.ProxyEvent{
		Type:      domain.EventTypeFailover429,
		AccountID: exhaustedAcc.ID,
		Message:   fmt.Sprintf("Account %s (%s) marked exhausted due to HTTP 429 / RESOURCE_EXHAUSTED", exhaustedAcc.Email, exhaustedAcc.ID),
		Details: map[string]any{
			"account_id": exhaustedAcc.ID,
			"email":      exhaustedAcc.Email,
			"reason":     "RESOURCE_EXHAUSTED",
		},
		Timestamp: time.Now().UTC(),
	})

	// 3. Find next available account
	nextAcc, err := f.accountRepo.GetNextAvailable(ctx, exhaustedAcc.ID)
	if err != nil || nextAcc == nil {
		// Pool is completely exhausted
		emit(&domain.ProxyEvent{
			Type:      domain.EventTypeQuotaExhausted,
			AccountID: exhaustedAcc.ID,
			Message:   "All accounts in the pool are exhausted",
			Details: map[string]any{
				"last_account_id": exhaustedAcc.ID,
				"last_email":      exhaustedAcc.Email,
			},
			Timestamp: time.Now().UTC(),
		})
		return nil, domain.ErrNoAvailableAccount
	}

	// 4. Set next account active atomically
	if err := f.accountRepo.SetActive(ctx, nextAcc.ID); err != nil {
		return nil, fmt.Errorf("failed to set active account to %s: %w", nextAcc.ID, err)
	}

	// Reset fallback state on the newly activated account
	delete(f.accountStates, nextAcc.ID)

	// Broadcast EventTypeAccountSwitched
	emit(&domain.ProxyEvent{
		Type:      domain.EventTypeAccountSwitched,
		AccountID: nextAcc.ID,
		Message:   fmt.Sprintf("Rotated active account from %s to %s", exhaustedAcc.Email, nextAcc.Email),
		Details: map[string]any{
			"from_account_id": exhaustedAcc.ID,
			"from_email":      exhaustedAcc.Email,
			"to_account_id":   nextAcc.ID,
			"to_email":        nextAcc.Email,
		},
		Timestamp: time.Now().UTC(),
	})

	return nextAcc, nil
}

func (f *FailoverEngine) emitEvent(event *domain.ProxyEvent) {
	if f.eventBroadcaster == nil && f.eventRepo == nil {
		return
	}
	if f.eventBroadcaster != nil {
		f.eventBroadcaster.Broadcast(event)
	}
	if f.eventRepo != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = f.eventRepo.Record(ctx, event)
	}
}
