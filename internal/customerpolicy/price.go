package customerpolicy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func (m *Manager) priceForModelLocked(model string) (ModelPrice, bool) {
	if m == nil || m.prices.Prices == nil {
		return ModelPrice{}, false
	}
	candidates := priceModelCandidates(model)
	for _, candidate := range candidates {
		if price, ok := m.prices.Prices[candidate]; ok {
			return price, true
		}
	}
	return ModelPrice{}, false
}

func priceModelCandidates(model string) []string {
	model = normalizeModel(model)
	if model == "" {
		return nil
	}
	out := []string{model}
	if idx := strings.LastIndex(model, "/"); idx >= 0 && idx+1 < len(model) {
		out = append(out, model[idx+1:])
	}
	return out
}

func usageCostUSD(price ModelPrice, detail usage.Detail) float64 {
	input := float64(detail.InputTokens)
	output := float64(detail.OutputTokens + detail.ReasoningTokens)
	cached := float64(detail.CachedTokens)
	if detail.CacheReadTokens > int64(cached) {
		cached = float64(detail.CacheReadTokens)
	}
	if cached > input {
		cached = input
	}
	regularInput := input - cached
	cachedPrice := price.CachedInputCostPerMillion
	if cachedPrice == 0 {
		cachedPrice = price.InputCostPerMillion
	}
	total := regularInput*price.InputCostPerMillion/1_000_000 +
		cached*cachedPrice/1_000_000 +
		output*price.OutputCostPerMillion/1_000_000
	if total < 0 || math.IsNaN(total) || math.IsInf(total, 0) {
		return 0
	}
	return total
}

// SyncPrices refreshes the LiteLLM price catalog from the configured source URL.
func (m *Manager) SyncPrices(ctx context.Context) (PriceSyncState, error) {
	if m == nil {
		return PriceSyncState{}, fmt.Errorf("customer policy manager is not available")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sourceURL := strings.TrimSpace(m.priceSourceURL())
	if sourceURL == "" {
		return m.priceSyncState(), fmt.Errorf("price source URL is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return m.recordPriceSyncError(fmt.Errorf("create price sync request: %w", err)), err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		wrapped := fmt.Errorf("sync LiteLLM prices: %w", err)
		return m.recordPriceSyncError(wrapped), wrapped
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		wrapped := fmt.Errorf("sync LiteLLM prices: status %d", resp.StatusCode)
		return m.recordPriceSyncError(wrapped), wrapped
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		wrapped := fmt.Errorf("read LiteLLM price response: %w", err)
		return m.recordPriceSyncError(wrapped), wrapped
	}
	catalog, err := parseLiteLLMPriceCatalog(sourceURL, body)
	if err != nil {
		wrapped := fmt.Errorf("parse LiteLLM price response: %w", err)
		return m.recordPriceSyncError(wrapped), wrapped
	}
	m.mu.Lock()
	m.prices = catalog
	m.priceSync.LastError = ""
	m.priceSync.LastSync = catalog.SyncedAt
	m.priceSync.Models = len(catalog.Prices)
	err = m.store.savePrices(catalog)
	state := m.priceSyncStateLocked()
	m.mu.Unlock()
	if err != nil {
		return state, err
	}
	return state, nil
}

func (m *Manager) recordPriceSyncError(err error) PriceSyncState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.priceSync.LastError = err.Error()
	}
	return m.priceSyncStateLocked()
}

func parseLiteLLMPriceCatalog(source string, data []byte) (PriceCatalog, error) {
	var raw map[string]map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return PriceCatalog{}, err
	}
	prices := make(map[string]ModelPrice, len(raw))
	for model, entry := range raw {
		model = normalizeModel(model)
		if model == "" {
			continue
		}
		price := ModelPrice{
			InputCostPerMillion:       perMillion(firstNumeric(entry, "input_cost_per_token", "prompt_cost_per_token")),
			OutputCostPerMillion:      perMillion(firstNumeric(entry, "output_cost_per_token", "completion_cost_per_token")),
			CachedInputCostPerMillion: perMillion(firstNumeric(entry, "cache_read_input_token_cost", "input_cost_per_token_cache_hit", "cached_input_cost_per_token")),
		}
		if price.InputCostPerMillion == 0 && price.OutputCostPerMillion == 0 && price.CachedInputCostPerMillion == 0 {
			continue
		}
		prices[model] = price
	}
	return PriceCatalog{
		Version:  1,
		Source:   source,
		SyncedAt: time.Now().UTC(),
		Prices:   prices,
	}, nil
}

func firstNumeric(entry map[string]any, keys ...string) float64 {
	for _, key := range keys {
		if value, ok := entry[key]; ok {
			if n := numeric(value); n > 0 {
				return n
			}
		}
	}
	return 0
}

func numeric(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f
	default:
		return 0
	}
}

func perMillion(costPerToken float64) float64 {
	if costPerToken <= 0 {
		return 0
	}
	return costPerToken * 1_000_000
}
