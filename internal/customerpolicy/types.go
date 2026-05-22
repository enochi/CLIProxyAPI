// Package customerpolicy enforces per-customer API key model policy, quotas,
// rate limits, and access records.
package customerpolicy

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const (
	ReasonModelDenied          = "model_denied"
	ReasonModelNotAllowed      = "model_not_allowed"
	ReasonRequestQuota         = "request_quota_exceeded"
	ReasonTokenQuota           = "token_quota_exceeded"
	ReasonCostQuota            = "cost_quota_exceeded"
	ReasonMissingPrice         = "missing_price"
	ReasonConcurrencyLimit     = "concurrency_limit"
	ReasonRateLimit            = "rate_limit"
	DefaultQuotaPeriod         = "monthly"
	DefaultRatePeriod          = "1m"
	defaultStateFileMode       = 0o600
	defaultStateDirectoryMode  = 0o700
	defaultMaxManagementRecord = 500
)

// Options configures a Manager instance.
type Options struct {
	Config         config.CustomerKeyPolicyConfig
	ConfigFilePath string
	APIKeys        []string
}

// PolicyFile is the persisted static policy state.
type PolicyFile struct {
	Version  int         `json:"version"`
	Policies []KeyPolicy `json:"policies"`
}

// KeyPolicy controls one customer API key. KeyID is the SHA-256 identifier of
// the configured customer API key; raw keys are never persisted here.
type KeyPolicy struct {
	KeyID                    string      `json:"key_id"`
	Label                    string      `json:"label,omitempty"`
	Enabled                  *bool       `json:"enabled,omitempty"`
	AllowedModels            []string    `json:"allowed_models,omitempty"`
	DeniedModels             []string    `json:"denied_models,omitempty"`
	Quota                    QuotaLimits `json:"quota,omitempty"`
	RateLimit                RateLimit   `json:"rate_limit,omitempty"`
	MaxConcurrentRequests    int         `json:"max_concurrent_requests,omitempty"`
	FailClosedOnMissingPrice *bool       `json:"fail_closed_on_missing_price,omitempty"`
}

// QuotaLimits holds hard per-window quota limits.
type QuotaLimits struct {
	Period   string  `json:"period,omitempty"`
	Requests int64   `json:"requests,omitempty"`
	Tokens   int64   `json:"tokens,omitempty"`
	CostUSD  float64 `json:"cost_usd,omitempty"`
}

// RateLimit limits request starts within a small window.
type RateLimit struct {
	Requests int64  `json:"requests,omitempty"`
	Period   string `json:"period,omitempty"`
}

// KeySummary is returned by management APIs.
type KeySummary struct {
	KeyID      string     `json:"key_id"`
	MaskedKey  string     `json:"masked_key"`
	Configured bool       `json:"configured"`
	Policy     *KeyPolicy `json:"policy,omitempty"`
	Status     KeyStatus  `json:"status"`
}

// KeyStatus describes current counters and limiter state.
type KeyStatus struct {
	KeyID        string         `json:"key_id"`
	MaskedKey    string         `json:"masked_key"`
	Window       WindowStatus   `json:"window"`
	RateWindow   WindowStatus   `json:"rate_window"`
	InFlight     int            `json:"in_flight"`
	Remaining    Remaining      `json:"remaining"`
	MissingPrice []string       `json:"missing_price,omitempty"`
	PriceSync    PriceSyncState `json:"price_sync"`
}

// WindowStatus describes a fixed enforcement window.
type WindowStatus struct {
	Key      string    `json:"key,omitempty"`
	StartsAt time.Time `json:"starts_at,omitempty"`
	EndsAt   time.Time `json:"ends_at,omitempty"`
	Requests int64     `json:"requests,omitempty"`
	Tokens   int64     `json:"tokens,omitempty"`
	CostUSD  float64   `json:"cost_usd,omitempty"`
}

// Remaining reports remaining quota where a limit exists.
type Remaining struct {
	Requests *int64   `json:"requests,omitempty"`
	Tokens   *int64   `json:"tokens,omitempty"`
	CostUSD  *float64 `json:"cost_usd,omitempty"`
}

// Decision is returned when a request should be blocked.
type Decision struct {
	Allowed    bool      `json:"allowed"`
	StatusCode int       `json:"status_code,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Message    string    `json:"message,omitempty"`
	KeyID      string    `json:"key_id,omitempty"`
	MaskedKey  string    `json:"masked_key,omitempty"`
	Model      string    `json:"model,omitempty"`
	RetryAfter int       `json:"retry_after,omitempty"`
	ResetAt    time.Time `json:"reset_at,omitempty"`
}

// RequestInfo contains data known before upstream execution begins.
type RequestInfo struct {
	APIKey    string
	Model     string
	Endpoint  string
	RequestID string
	Source    string
}

// AccessRecord is one per-key history entry.
type AccessRecord struct {
	ID          string       `json:"id"`
	Timestamp   time.Time    `json:"timestamp"`
	KeyID       string       `json:"key_id"`
	MaskedKey   string       `json:"masked_key"`
	RequestID   string       `json:"request_id,omitempty"`
	Endpoint    string       `json:"endpoint,omitempty"`
	Model       string       `json:"model,omitempty"`
	Alias       string       `json:"alias,omitempty"`
	Provider    string       `json:"provider,omitempty"`
	AuthID      string       `json:"auth_id,omitempty"`
	Status      string       `json:"status"`
	HTTPStatus  int          `json:"http_status,omitempty"`
	BlockReason string       `json:"block_reason,omitempty"`
	Failed      bool         `json:"failed,omitempty"`
	FailureBody string       `json:"failure_body,omitempty"`
	LatencyMS   int64        `json:"latency_ms,omitempty"`
	Detail      usage.Detail `json:"detail,omitempty"`
	CostUSD     float64      `json:"cost_usd,omitempty"`
	Source      string       `json:"source,omitempty"`
}

// RecordsFilter filters management record queries.
type RecordsFilter struct {
	KeyID  string
	Model  string
	Status string
	Limit  int
}

// RecordsPage is a bounded record query result.
type RecordsPage struct {
	Records []AccessRecord `json:"records"`
	Limit   int            `json:"limit"`
}

// PriceCatalog is the persisted LiteLLM price cache.
type PriceCatalog struct {
	Version  int                   `json:"version"`
	Source   string                `json:"source"`
	SyncedAt time.Time             `json:"synced_at,omitempty"`
	Prices   map[string]ModelPrice `json:"prices"`
}

// ModelPrice stores supported V1 token price fields per one million tokens.
type ModelPrice struct {
	InputCostPerMillion       float64 `json:"input_cost_per_million,omitempty"`
	OutputCostPerMillion      float64 `json:"output_cost_per_million,omitempty"`
	CachedInputCostPerMillion float64 `json:"cached_input_cost_per_million,omitempty"`
}

// PriceSyncState reports last sync metadata.
type PriceSyncState struct {
	Enabled   bool      `json:"enabled"`
	SourceURL string    `json:"source_url"`
	LastSync  time.Time `json:"last_sync,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Models    int       `json:"models"`
}

// UsageFile is the persisted mutable counter state.
type UsageFile struct {
	Version  int                      `json:"version"`
	Counters map[string]*CounterState `json:"counters"`
}

// CounterState holds the current quota and rate window for one key.
type CounterState struct {
	WindowKey       string    `json:"window_key,omitempty"`
	WindowStartsAt  time.Time `json:"window_starts_at,omitempty"`
	WindowEndsAt    time.Time `json:"window_ends_at,omitempty"`
	Requests        int64     `json:"requests,omitempty"`
	Tokens          int64     `json:"tokens,omitempty"`
	CostUSD         float64   `json:"cost_usd,omitempty"`
	RateWindowKey   string    `json:"rate_window_key,omitempty"`
	RateWindowStart time.Time `json:"rate_window_start,omitempty"`
	RateWindowEnd   time.Time `json:"rate_window_end,omitempty"`
	RateRequests    int64     `json:"rate_requests,omitempty"`
	MissingPrices   []string  `json:"missing_prices,omitempty"`
}

func policyEnabled(policy *KeyPolicy) bool {
	if policy == nil || policy.Enabled == nil {
		return true
	}
	return *policy.Enabled
}

func quotaPeriod(policy *KeyPolicy) string {
	if policy == nil {
		return DefaultQuotaPeriod
	}
	period := strings.TrimSpace(policy.Quota.Period)
	if period == "" {
		return DefaultQuotaPeriod
	}
	return period
}

func ratePeriod(policy *KeyPolicy) string {
	if policy == nil {
		return DefaultRatePeriod
	}
	period := strings.TrimSpace(policy.RateLimit.Period)
	if period == "" {
		return DefaultRatePeriod
	}
	return period
}
