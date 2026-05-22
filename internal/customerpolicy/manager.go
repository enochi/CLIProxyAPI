package customerpolicy

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

type keyIdentity struct {
	ID         string
	MaskedKey  string
	Configured bool
}

// Manager owns policy evaluation and local file-backed state.
type Manager struct {
	mu sync.Mutex

	cfg        config.CustomerKeyPolicyConfig
	store      *store
	storePath  string
	keys       map[string]keyIdentity
	policies   map[string]KeyPolicy
	usage      UsageFile
	prices     PriceCatalog
	priceSync  PriceSyncState
	inFlight   map[string]int
	maxRecords int
}

// Reservation tracks one allowed request until completion.
type Reservation struct {
	manager *Manager
	info    RequestInfo
	keyID   string
	masked  string
	model   string
	started time.Time

	released  atomic.Bool
	finalized atomic.Bool
}

// NewManager initializes the isolated policy subsystem.
func NewManager(opts Options) (*Manager, error) {
	st, err := newStore(opts.ConfigFilePath, opts.Config.StorePath)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		cfg:       opts.Config,
		store:     st,
		storePath: st.dir,
		keys:      keyIdentityMap(opts.APIKeys),
		policies:  make(map[string]KeyPolicy),
		usage:     UsageFile{Version: 1, Counters: make(map[string]*CounterState)},
		prices:    PriceCatalog{Version: 1, Prices: make(map[string]ModelPrice)},
		inFlight:  make(map[string]int),
	}
	m.applyDefaultsLocked()
	if err = m.loadStateLocked(); err != nil {
		return nil, err
	}
	m.startInitialPriceSyncIfNeeded()
	return m, nil
}

func keyIdentityMap(keys []string) map[string]keyIdentity {
	out := make(map[string]keyIdentity, len(keys))
	for _, raw := range keys {
		id := KeyID(raw)
		if id == "" {
			continue
		}
		out[id] = keyIdentity{ID: id, MaskedKey: MaskKey(raw), Configured: true}
	}
	return out
}

func (m *Manager) applyDefaultsLocked() {
	if m.cfg.MaxAccessRecords <= 0 {
		m.cfg.MaxAccessRecords = 10000
	}
	m.cfg.PriceSync.SourceURL = strings.TrimSpace(m.cfg.PriceSync.SourceURL)
	if m.cfg.PriceSync.SourceURL == "" {
		m.cfg.PriceSync.SourceURL = config.DefaultLiteLLMPriceSourceURL
	}
	m.maxRecords = m.cfg.MaxAccessRecords
	m.priceSync.Enabled = m.cfg.PriceSync.Enabled
	m.priceSync.SourceURL = m.cfg.PriceSync.SourceURL
}

func (m *Manager) loadStateLocked() error {
	policies, err := m.store.loadPolicies()
	if err != nil {
		return err
	}
	m.policies = make(map[string]KeyPolicy, len(policies.Policies))
	for _, policy := range policies.Policies {
		policy = sanitizePolicy(policy)
		if policy.KeyID == "" {
			continue
		}
		m.policies[policy.KeyID] = policy
	}
	usageFile, err := m.store.loadUsage()
	if err != nil {
		return err
	}
	m.usage = usageFile
	prices, err := m.store.loadPrices()
	if err != nil {
		return err
	}
	m.prices = prices
	m.priceSync.LastSync = prices.SyncedAt
	m.priceSync.Models = len(prices.Prices)
	return nil
}

// Reload updates config-derived state after hot reload.
func (m *Manager) Reload(cfg config.CustomerKeyPolicyConfig, apiKeys []string, configFilePath string) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	newStore, err := newStore(configFilePath, cfg.StorePath)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	m.cfg = cfg
	m.store = newStore
	m.storePath = newStore.dir
	m.keys = keyIdentityMap(apiKeys)
	m.applyDefaultsLocked()
	err = m.loadStateLocked()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	m.startInitialPriceSyncIfNeeded()
	return nil
}

func (m *Manager) startInitialPriceSyncIfNeeded() {
	if m == nil {
		return
	}
	m.mu.Lock()
	shouldSync := m.cfg.PriceSync.Enabled &&
		strings.TrimSpace(m.cfg.PriceSync.SourceURL) != "" &&
		len(m.prices.Prices) == 0
	m.mu.Unlock()
	if !shouldSync {
		return
	}
	go func() {
		if _, err := m.SyncPrices(context.Background()); err != nil {
			log.WithError(err).Warn("customer policy: initial LiteLLM price sync failed")
		}
	}()
}

// Enabled returns whether policy evaluation is enabled.
func (m *Manager) Enabled() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.Enabled
}

// Begin reserves one request and returns a block decision when policy denies it.
func (m *Manager) Begin(ctx context.Context, info RequestInfo) (*Reservation, *Decision) {
	if m == nil {
		return nil, nil
	}
	info.APIKey = strings.TrimSpace(info.APIKey)
	info.Model = strings.TrimSpace(info.Model)
	if info.APIKey == "" || info.Model == "" {
		return nil, nil
	}
	if info.RequestID == "" {
		info.RequestID = logging.GetRequestID(ctx)
	}
	now := time.Now().UTC()
	keyID := KeyID(info.APIKey)
	masked := MaskKey(info.APIKey)

	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.cfg.Enabled {
		return nil, nil
	}
	if identity, ok := m.keys[keyID]; ok && identity.MaskedKey != "" {
		masked = identity.MaskedKey
	}
	policy, hasPolicy := m.policies[keyID]
	if !hasPolicy {
		policy = KeyPolicy{KeyID: keyID}
	}
	counter := m.counterLocked(keyID)
	m.ensureQuotaWindowLocked(counter, &policy, now)
	m.ensureRateWindowLocked(counter, &policy, now)

	if policyEnabled(&policy) {
		if denied := m.evaluatePolicyLocked(&policy, counter, info, keyID, masked, now); denied != nil {
			m.appendBlockedRecordLocked(info, keyID, masked, *denied, now)
			return nil, denied
		}
	}

	counter.Requests++
	if policy.RateLimit.Requests > 0 {
		counter.RateRequests++
	}
	if policy.MaxConcurrentRequests > 0 {
		m.inFlight[keyID]++
	}
	if err := m.saveUsageLocked(); err != nil {
		log.WithError(err).Warn("customer policy: failed to persist usage reservation")
	}
	return &Reservation{
		manager: m,
		info:    info,
		keyID:   keyID,
		masked:  masked,
		model:   info.Model,
		started: now,
	}, nil
}

func (m *Manager) evaluatePolicyLocked(policy *KeyPolicy, counter *CounterState, info RequestInfo, keyID, masked string, now time.Time) *Decision {
	model := strings.TrimSpace(info.Model)
	if anyPatternMatch(policy.DeniedModels, model) {
		return deny(http.StatusForbidden, ReasonModelDenied, "model is denied for this customer API key", keyID, masked, model, counter.WindowEndsAt)
	}
	if len(policy.AllowedModels) > 0 && !anyPatternMatch(policy.AllowedModels, model) {
		return deny(http.StatusForbidden, ReasonModelNotAllowed, "model is not allowed for this customer API key", keyID, masked, model, counter.WindowEndsAt)
	}
	if policy.Quota.Requests > 0 && counter.Requests >= policy.Quota.Requests {
		return deny(http.StatusTooManyRequests, ReasonRequestQuota, "request quota exceeded for this customer API key", keyID, masked, model, counter.WindowEndsAt)
	}
	if policy.Quota.Tokens > 0 && counter.Tokens >= policy.Quota.Tokens {
		return deny(http.StatusTooManyRequests, ReasonTokenQuota, "token quota exceeded for this customer API key", keyID, masked, model, counter.WindowEndsAt)
	}
	if policy.Quota.CostUSD > 0 && counter.CostUSD >= policy.Quota.CostUSD {
		return deny(http.StatusTooManyRequests, ReasonCostQuota, "cost quota exceeded for this customer API key", keyID, masked, model, counter.WindowEndsAt)
	}
	if policy.Quota.CostUSD > 0 && m.failClosedOnMissingPriceLocked(policy) {
		if _, ok := m.priceForModelLocked(model); !ok {
			addMissingPrice(counter, model)
			return deny(http.StatusTooManyRequests, ReasonMissingPrice, "model price is missing for a cost-limited customer API key", keyID, masked, model, counter.WindowEndsAt)
		}
	}
	if policy.RateLimit.Requests > 0 && counter.RateRequests >= policy.RateLimit.Requests {
		decision := deny(http.StatusTooManyRequests, ReasonRateLimit, "rate limit exceeded for this customer API key", keyID, masked, model, counter.RateWindowEnd)
		if !counter.RateWindowEnd.IsZero() {
			retry := int(time.Until(counter.RateWindowEnd).Seconds())
			if retry < 1 && now.Before(counter.RateWindowEnd) {
				retry = 1
			}
			decision.RetryAfter = retry
		}
		return decision
	}
	if policy.MaxConcurrentRequests > 0 && m.inFlight[keyID] >= policy.MaxConcurrentRequests {
		return deny(http.StatusTooManyRequests, ReasonConcurrencyLimit, "concurrency limit exceeded for this customer API key", keyID, masked, model, time.Time{})
	}
	return nil
}

func deny(status int, reason, message, keyID, masked, model string, resetAt time.Time) *Decision {
	return &Decision{
		Allowed:    false,
		StatusCode: status,
		Reason:     reason,
		Message:    message,
		KeyID:      keyID,
		MaskedKey:  masked,
		Model:      model,
		ResetAt:    resetAt,
	}
}

func (m *Manager) counterLocked(keyID string) *CounterState {
	if m.usage.Counters == nil {
		m.usage.Counters = make(map[string]*CounterState)
	}
	counter := m.usage.Counters[keyID]
	if counter == nil {
		counter = &CounterState{}
		m.usage.Counters[keyID] = counter
	}
	return counter
}

func (m *Manager) ensureQuotaWindowLocked(counter *CounterState, policy *KeyPolicy, now time.Time) {
	key, start, end := quotaWindow(quotaPeriod(policy), now)
	if counter.WindowKey != key {
		counter.WindowKey = key
		counter.WindowStartsAt = start
		counter.WindowEndsAt = end
		counter.Requests = 0
		counter.Tokens = 0
		counter.CostUSD = 0
		counter.MissingPrices = nil
	}
}

func (m *Manager) ensureRateWindowLocked(counter *CounterState, policy *KeyPolicy, now time.Time) {
	key, start, end := quotaWindow(ratePeriod(policy), now)
	if counter.RateWindowKey != key {
		counter.RateWindowKey = key
		counter.RateWindowStart = start
		counter.RateWindowEnd = end
		counter.RateRequests = 0
	}
}

func quotaWindow(period string, now time.Time) (string, time.Time, time.Time) {
	period = strings.ToLower(strings.TrimSpace(period))
	if period == "" {
		period = DefaultQuotaPeriod
	}
	now = now.UTC()
	switch period {
	case "none", "never", "no-reset", "no_reset":
		return "none", time.Time{}, time.Time{}
	case "day", "daily", "1d":
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		return start.Format("2006-01-02"), start, start.AddDate(0, 0, 1)
	case "week", "weekly", "1w":
		weekday := int(now.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(weekday - 1))
		return start.Format("2006-W01"), start, start.AddDate(0, 0, 7)
	case "month", "monthly", "1mo":
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		return start.Format("2006-01"), start, start.AddDate(0, 1, 0)
	}
	if duration, err := time.ParseDuration(period); err == nil && duration > 0 {
		start := now.Truncate(duration)
		return fmt.Sprintf("%s:%d", period, start.Unix()), start, start.Add(duration)
	}
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start.Format("2006-01"), start, start.AddDate(0, 1, 0)
}

func (m *Manager) saveUsageLocked() error {
	return m.store.saveUsage(m.usage)
}

func (r *Reservation) Finish(httpStatus int, failed bool) {
	if r == nil || r.manager == nil {
		return
	}
	if r.released.CompareAndSwap(false, true) {
		r.manager.mu.Lock()
		if r.keyID != "" && r.manager.inFlight[r.keyID] > 0 {
			r.manager.inFlight[r.keyID]--
		}
		r.manager.mu.Unlock()
	}
	if r.finalized.CompareAndSwap(false, true) {
		status := "allowed"
		if failed || httpStatus >= 400 {
			status = "failed"
		}
		record := AccessRecord{
			ID:         stableRecordID(r.info.RequestID, r.keyID, time.Now().UTC()),
			Timestamp:  time.Now().UTC(),
			KeyID:      r.keyID,
			MaskedKey:  r.masked,
			RequestID:  r.info.RequestID,
			Endpoint:   r.info.Endpoint,
			Model:      r.model,
			Status:     status,
			HTTPStatus: httpStatus,
			Failed:     status == "failed",
			LatencyMS:  time.Since(r.started).Milliseconds(),
			Source:     r.info.Source,
		}
		r.manager.appendRecord(record)
	}
}

// FinalizeUsage synchronously records tokens/cost for a completed usage record.
func (m *Manager) FinalizeUsage(ctx context.Context, record usage.Record) {
	if m == nil || !m.Enabled() {
		return
	}
	if res := ReservationFromContext(ctx); res != nil && res.manager == m {
		res.finalizeUsageRecord(ctx, record)
		return
	}
	m.recordUsageWithoutReservation(ctx, record)
}

func (r *Reservation) finalizeUsageRecord(ctx context.Context, record usage.Record) {
	if r == nil || r.manager == nil {
		return
	}
	if !r.finalized.CompareAndSwap(false, true) {
		return
	}
	r.manager.recordUsageForReservation(ctx, r, record)
}

func (m *Manager) recordUsageForReservation(ctx context.Context, r *Reservation, record usage.Record) {
	now := time.Now().UTC()
	model := strings.TrimSpace(record.Alias)
	if model == "" {
		model = strings.TrimSpace(record.Model)
	}
	if model == "" {
		model = r.model
	}
	detail := normalizeDetail(record.Detail)
	cost := m.applyUsageCounters(r.keyID, model, detail)
	status := "allowed"
	if record.Failed {
		status = "failed"
	}
	access := accessRecordFromUsage(ctx, record, r.keyID, r.masked, r.info, model, status, cost, detail, now, r.started)
	m.appendRecord(access)
}

func (m *Manager) recordUsageWithoutReservation(ctx context.Context, record usage.Record) {
	apiKey := strings.TrimSpace(record.APIKey)
	if apiKey == "" {
		return
	}
	keyID := KeyID(apiKey)
	masked := MaskKey(apiKey)
	model := strings.TrimSpace(record.Alias)
	if model == "" {
		model = strings.TrimSpace(record.Model)
	}
	now := time.Now().UTC()
	info := RequestInfo{
		APIKey:    apiKey,
		Model:     model,
		RequestID: logging.GetRequestID(ctx),
		Endpoint:  logging.GetEndpoint(ctx),
	}
	detail := normalizeDetail(record.Detail)
	cost := 0.0
	m.mu.Lock()
	policy, ok := m.policies[keyID]
	if !ok {
		policy = KeyPolicy{KeyID: keyID}
	}
	counter := m.counterLocked(keyID)
	m.ensureQuotaWindowLocked(counter, &policy, now)
	counter.Requests++
	cost = m.applyUsageCountersLocked(counter, model, detail)
	if err := m.saveUsageLocked(); err != nil {
		log.WithError(err).Warn("customer policy: failed to persist usage")
	}
	if identity, ok := m.keys[keyID]; ok && identity.MaskedKey != "" {
		masked = identity.MaskedKey
	}
	m.mu.Unlock()
	status := "allowed"
	if record.Failed {
		status = "failed"
	}
	m.appendRecord(accessRecordFromUsage(ctx, record, keyID, masked, info, model, status, cost, detail, now, now))
}

func (m *Manager) applyUsageCounters(keyID, model string, detail usage.Detail) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	counter := m.counterLocked(keyID)
	cost := m.applyUsageCountersLocked(counter, model, detail)
	if err := m.saveUsageLocked(); err != nil {
		log.WithError(err).Warn("customer policy: failed to persist usage finalization")
	}
	return cost
}

func (m *Manager) applyUsageCountersLocked(counter *CounterState, model string, detail usage.Detail) float64 {
	counter.Tokens += detail.TotalTokens
	cost := 0.0
	if price, ok := m.priceForModelLocked(model); ok {
		cost = usageCostUSD(price, detail)
		counter.CostUSD += cost
	} else if detail.TotalTokens > 0 {
		addMissingPrice(counter, model)
	}
	return cost
}

func normalizeDetail(detail usage.Detail) usage.Detail {
	if detail.TotalTokens == 0 {
		total := detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
		if total > 0 {
			detail.TotalTokens = total
		}
	}
	return detail
}

func accessRecordFromUsage(ctx context.Context, record usage.Record, keyID, masked string, info RequestInfo, model, status string, cost float64, detail usage.Detail, now, started time.Time) AccessRecord {
	requestID := strings.TrimSpace(info.RequestID)
	if requestID == "" {
		requestID = logging.GetRequestID(ctx)
	}
	endpoint := strings.TrimSpace(info.Endpoint)
	if endpoint == "" {
		endpoint = logging.GetEndpoint(ctx)
	}
	httpStatus := 0
	if record.Failed {
		httpStatus = record.Fail.StatusCode
	}
	return AccessRecord{
		ID:          stableRecordID(requestID, keyID, now),
		Timestamp:   now,
		KeyID:       keyID,
		MaskedKey:   masked,
		RequestID:   requestID,
		Endpoint:    endpoint,
		Model:       strings.TrimSpace(record.Model),
		Alias:       model,
		Provider:    record.Provider,
		AuthID:      record.AuthID,
		Status:      status,
		HTTPStatus:  httpStatus,
		Failed:      record.Failed,
		FailureBody: truncate(record.Fail.Body, 2048),
		LatencyMS:   time.Since(started).Milliseconds(),
		Detail:      detail,
		CostUSD:     cost,
		Source:      info.Source,
	}
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit]
}

func (m *Manager) appendBlockedRecordLocked(info RequestInfo, keyID, masked string, decision Decision, now time.Time) {
	record := AccessRecord{
		ID:          stableRecordID(info.RequestID, keyID, now),
		Timestamp:   now,
		KeyID:       keyID,
		MaskedKey:   masked,
		RequestID:   info.RequestID,
		Endpoint:    info.Endpoint,
		Model:       info.Model,
		Status:      "blocked",
		HTTPStatus:  decision.StatusCode,
		BlockReason: decision.Reason,
		Source:      info.Source,
	}
	if err := m.store.appendRecord(record, m.maxRecords); err != nil {
		log.WithError(err).Warn("customer policy: failed to append blocked access record")
	}
	if err := m.saveUsageLocked(); err != nil {
		log.WithError(err).Warn("customer policy: failed to persist blocked usage state")
	}
}

func (m *Manager) appendRecord(record AccessRecord) {
	if m == nil {
		return
	}
	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now().UTC()
	}
	if err := m.store.appendRecord(record, m.maxRecords); err != nil {
		log.WithError(err).Warn("customer policy: failed to append access record")
	}
}

func addMissingPrice(counter *CounterState, model string) {
	model = normalizeModel(model)
	if model == "" || counter == nil {
		return
	}
	for _, existing := range counter.MissingPrices {
		if existing == model {
			return
		}
	}
	counter.MissingPrices = append(counter.MissingPrices, model)
}

func (m *Manager) failClosedOnMissingPriceLocked(policy *KeyPolicy) bool {
	if policy != nil && policy.FailClosedOnMissingPrice != nil {
		return *policy.FailClosedOnMissingPrice
	}
	return m.cfg.PriceSync.FailClosedOnMissingPrice
}

func (m *Manager) priceSourceURL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.PriceSync.SourceURL
}

func (m *Manager) priceSyncState() PriceSyncState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.priceSyncStateLocked()
}

func (m *Manager) priceSyncStateLocked() PriceSyncState {
	m.priceSync.Enabled = m.cfg.PriceSync.Enabled
	m.priceSync.SourceURL = m.cfg.PriceSync.SourceURL
	m.priceSync.Models = len(m.prices.Prices)
	return m.priceSync
}

// ListKeys returns configured keys plus policies for keys no longer configured.
func (m *Manager) ListKeys() ([]KeySummary, error) {
	if m == nil {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	seen := make(map[string]struct{}, len(m.keys)+len(m.policies))
	out := make([]KeySummary, 0, len(m.keys)+len(m.policies))
	ids := make([]string, 0, len(m.keys))
	for id := range m.keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		identity := m.keys[id]
		out = append(out, m.summaryLocked(id, identity.MaskedKey, true, now))
		seen[id] = struct{}{}
	}
	for id := range m.policies {
		if _, ok := seen[id]; ok {
			continue
		}
		out = append(out, m.summaryLocked(id, "", false, now))
	}
	return out, nil
}

func (m *Manager) summaryLocked(keyID, masked string, configured bool, now time.Time) KeySummary {
	policy, ok := m.policies[keyID]
	counter := m.counterLocked(keyID)
	if !ok {
		policy = KeyPolicy{KeyID: keyID}
	}
	m.ensureQuotaWindowLocked(counter, &policy, now)
	m.ensureRateWindowLocked(counter, &policy, now)
	status := m.statusLocked(keyID, masked, &policy, counter)
	var policyPtr *KeyPolicy
	if ok {
		policyCopy := policy
		policyPtr = &policyCopy
	}
	return KeySummary{KeyID: keyID, MaskedKey: masked, Configured: configured, Policy: policyPtr, Status: status}
}

// Status returns current key counters.
func (m *Manager) Status(keyID string) (KeyStatus, bool) {
	if m == nil {
		return KeyStatus{}, false
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return KeyStatus{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	identity, configured := m.keys[keyID]
	policy, ok := m.policies[keyID]
	if !ok {
		policy = KeyPolicy{KeyID: keyID}
	}
	counter := m.counterLocked(keyID)
	now := time.Now().UTC()
	m.ensureQuotaWindowLocked(counter, &policy, now)
	m.ensureRateWindowLocked(counter, &policy, now)
	if err := m.saveUsageLocked(); err != nil {
		log.WithError(err).Warn("customer policy: failed to persist status window rollover")
	}
	return m.statusLocked(keyID, identity.MaskedKey, &policy, counter), configured || ok
}

func (m *Manager) statusLocked(keyID, masked string, policy *KeyPolicy, counter *CounterState) KeyStatus {
	remaining := Remaining{}
	if policy != nil {
		if policy.Quota.Requests > 0 {
			value := clampNonNegativeInt64(policy.Quota.Requests - counter.Requests)
			remaining.Requests = &value
		}
		if policy.Quota.Tokens > 0 {
			value := clampNonNegativeInt64(policy.Quota.Tokens - counter.Tokens)
			remaining.Tokens = &value
		}
		if policy.Quota.CostUSD > 0 {
			value := clampNonNegativeFloat(policy.Quota.CostUSD - counter.CostUSD)
			remaining.CostUSD = &value
		}
	}
	missing := append([]string(nil), counter.MissingPrices...)
	return KeyStatus{
		KeyID:     keyID,
		MaskedKey: masked,
		Window: WindowStatus{
			Key:      counter.WindowKey,
			StartsAt: counter.WindowStartsAt,
			EndsAt:   counter.WindowEndsAt,
			Requests: counter.Requests,
			Tokens:   counter.Tokens,
			CostUSD:  counter.CostUSD,
		},
		RateWindow: WindowStatus{
			Key:      counter.RateWindowKey,
			StartsAt: counter.RateWindowStart,
			EndsAt:   counter.RateWindowEnd,
			Requests: counter.RateRequests,
		},
		InFlight:     m.inFlight[keyID],
		Remaining:    remaining,
		MissingPrice: missing,
		PriceSync:    m.priceSyncStateLocked(),
	}
}

// UpsertPolicy creates or updates a key policy.
func (m *Manager) UpsertPolicy(keyID string, policy KeyPolicy) (KeyPolicy, error) {
	if m == nil {
		return KeyPolicy{}, fmt.Errorf("customer policy manager is not available")
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		keyID = strings.TrimSpace(policy.KeyID)
	}
	if keyID == "" {
		return KeyPolicy{}, fmt.Errorf("key_id is required")
	}
	policy.KeyID = keyID
	policy = sanitizePolicy(policy)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.policies[keyID] = policy
	return policy, m.savePoliciesLocked()
}

// ReplacePolicies replaces all static policies.
func (m *Manager) ReplacePolicies(policies []KeyPolicy) error {
	if m == nil {
		return fmt.Errorf("customer policy manager is not available")
	}
	next := make(map[string]KeyPolicy, len(policies))
	for _, policy := range policies {
		policy = sanitizePolicy(policy)
		if policy.KeyID == "" {
			return fmt.Errorf("key_id is required for every policy")
		}
		next[policy.KeyID] = policy
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.policies = next
	return m.savePoliciesLocked()
}

// DeletePolicy removes a static policy without deleting usage history.
func (m *Manager) DeletePolicy(keyID string) error {
	if m == nil {
		return fmt.Errorf("customer policy manager is not available")
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return fmt.Errorf("key_id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.policies, keyID)
	return m.savePoliciesLocked()
}

func (m *Manager) savePoliciesLocked() error {
	policies := make([]KeyPolicy, 0, len(m.policies))
	for _, policy := range m.policies {
		policies = append(policies, policy)
	}
	sort.Slice(policies, func(i, j int) bool { return policies[i].KeyID < policies[j].KeyID })
	return m.store.savePolicies(PolicyFile{Version: 1, Policies: policies})
}

func sanitizePolicy(policy KeyPolicy) KeyPolicy {
	policy.KeyID = strings.TrimSpace(policy.KeyID)
	policy.Label = strings.TrimSpace(policy.Label)
	policy.AllowedModels = sanitizePatterns(policy.AllowedModels)
	policy.DeniedModels = sanitizePatterns(policy.DeniedModels)
	policy.Quota.Period = strings.TrimSpace(policy.Quota.Period)
	if policy.Quota.Period == "" && (policy.Quota.Requests > 0 || policy.Quota.Tokens > 0 || policy.Quota.CostUSD > 0) {
		policy.Quota.Period = DefaultQuotaPeriod
	}
	if policy.Quota.Requests < 0 {
		policy.Quota.Requests = 0
	}
	if policy.Quota.Tokens < 0 {
		policy.Quota.Tokens = 0
	}
	if policy.Quota.CostUSD < 0 {
		policy.Quota.CostUSD = 0
	}
	policy.RateLimit.Period = strings.TrimSpace(policy.RateLimit.Period)
	if policy.RateLimit.Period == "" && policy.RateLimit.Requests > 0 {
		policy.RateLimit.Period = DefaultRatePeriod
	}
	if policy.RateLimit.Requests < 0 {
		policy.RateLimit.Requests = 0
	}
	if policy.MaxConcurrentRequests < 0 {
		policy.MaxConcurrentRequests = 0
	}
	return policy
}

func sanitizePatterns(patterns []string) []string {
	out := make([]string, 0, len(patterns))
	seen := make(map[string]struct{}, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		key := strings.ToLower(pattern)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, pattern)
	}
	return out
}

// ListRecords returns recent access history.
func (m *Manager) ListRecords(filter RecordsFilter) (RecordsPage, error) {
	if m == nil {
		return RecordsPage{}, fmt.Errorf("customer policy manager is not available")
	}
	records, err := m.store.listRecords(filter)
	if err != nil {
		return RecordsPage{}, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > defaultMaxManagementRecord {
		limit = defaultMaxManagementRecord
	}
	return RecordsPage{Records: records, Limit: limit}, nil
}

// Prices returns current price sync status and prices.
func (m *Manager) Prices() (PriceSyncState, map[string]ModelPrice) {
	if m == nil {
		return PriceSyncState{}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	prices := make(map[string]ModelPrice, len(m.prices.Prices))
	for model, price := range m.prices.Prices {
		prices[model] = price
	}
	return m.priceSyncStateLocked(), prices
}
