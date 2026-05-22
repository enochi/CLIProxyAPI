package customerpolicy

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func newTestManager(t *testing.T, apiKeys []string) *Manager {
	t.Helper()
	cfg := config.CustomerKeyPolicyConfig{
		Enabled:          true,
		StorePath:        filepath.Join(t.TempDir(), "policy"),
		MaxAccessRecords: 100,
		PriceSync: config.CustomerKeyPolicyPriceSyncConfig{
			Enabled:                  true,
			SourceURL:                config.DefaultLiteLLMPriceSourceURL,
			FailClosedOnMissingPrice: true,
		},
	}
	manager, err := NewManager(Options{Config: cfg, APIKeys: apiKeys})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	return manager
}

func TestBeginAllowsMissingPolicyAndRecordsUsageWithoutRawKey(t *testing.T) {
	apiKey := "sk-customer-secret"
	manager := newTestManager(t, []string{apiKey})
	ctx := logging.WithRequestID(context.Background(), "req-1")

	res, decision := manager.Begin(ctx, RequestInfo{
		APIKey:    apiKey,
		Model:     "gpt-test",
		Endpoint:  "POST /v1/chat/completions",
		RequestID: "req-1",
	})
	if decision != nil {
		t.Fatalf("Begin() decision = %+v, want allowed", decision)
	}
	if res == nil {
		t.Fatal("Begin() reservation is nil")
	}
	res.finalizeUsageRecord(ctx, usage.Record{
		Model: "gpt-test",
		Detail: usage.Detail{
			InputTokens:  1,
			OutputTokens: 2,
			TotalTokens:  3,
		},
	})
	res.Finish(200, false)

	status, ok := manager.Status(KeyID(apiKey))
	if !ok {
		t.Fatal("Status() did not find configured key")
	}
	if status.Window.Requests != 1 {
		t.Fatalf("requests = %d, want 1", status.Window.Requests)
	}
	if status.Window.Tokens != 3 {
		t.Fatalf("tokens = %d, want 3", status.Window.Tokens)
	}

	page, err := manager.ListRecords(RecordsFilter{KeyID: KeyID(apiKey), Limit: 10})
	if err != nil {
		t.Fatalf("ListRecords() error = %v", err)
	}
	if len(page.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(page.Records))
	}
	encoded := page.Records[0].MaskedKey + page.Records[0].KeyID + page.Records[0].Model + page.Records[0].RequestID
	if strings.Contains(encoded, apiKey) {
		t.Fatalf("record leaked raw API key: %+v", page.Records[0])
	}
}

func TestModelPolicyAndRequestQuotaBlockBeforeUpstream(t *testing.T) {
	apiKey := "sk-policy"
	manager := newTestManager(t, []string{apiKey})
	policy := KeyPolicy{
		KeyID:         KeyID(apiKey),
		AllowedModels: []string{"gpt-4*"},
		DeniedModels:  []string{"gpt-4-denied"},
		Quota:         QuotaLimits{Period: "none", Requests: 1},
	}
	if _, err := manager.UpsertPolicy(policy.KeyID, policy); err != nil {
		t.Fatalf("UpsertPolicy() error = %v", err)
	}

	if _, decision := manager.Begin(context.Background(), RequestInfo{APIKey: apiKey, Model: "gpt-3"}); decision == nil || decision.Reason != ReasonModelNotAllowed {
		t.Fatalf("decision = %+v, want %s", decision, ReasonModelNotAllowed)
	}
	if _, decision := manager.Begin(context.Background(), RequestInfo{APIKey: apiKey, Model: "gpt-4-denied"}); decision == nil || decision.Reason != ReasonModelDenied {
		t.Fatalf("decision = %+v, want %s", decision, ReasonModelDenied)
	}
	res, decision := manager.Begin(context.Background(), RequestInfo{APIKey: apiKey, Model: "gpt-4o"})
	if decision != nil || res == nil {
		t.Fatalf("first allowed request = (%v, %+v), want reservation", res, decision)
	}
	res.Finish(200, false)
	if _, decision := manager.Begin(context.Background(), RequestInfo{APIKey: apiKey, Model: "gpt-4o"}); decision == nil || decision.Reason != ReasonRequestQuota {
		t.Fatalf("decision = %+v, want %s", decision, ReasonRequestQuota)
	}
}

func TestCostQuotaMissingPriceFailsClosed(t *testing.T) {
	apiKey := "sk-cost"
	manager := newTestManager(t, []string{apiKey})
	policy := KeyPolicy{
		KeyID: KeyID(apiKey),
		Quota: QuotaLimits{
			Period:  "daily",
			CostUSD: 1,
		},
	}
	if _, err := manager.UpsertPolicy(policy.KeyID, policy); err != nil {
		t.Fatalf("UpsertPolicy() error = %v", err)
	}

	_, decision := manager.Begin(context.Background(), RequestInfo{APIKey: apiKey, Model: "unknown-model"})
	if decision == nil || decision.Reason != ReasonMissingPrice {
		t.Fatalf("decision = %+v, want %s", decision, ReasonMissingPrice)
	}
	status, ok := manager.Status(KeyID(apiKey))
	if !ok {
		t.Fatal("Status() did not find configured key")
	}
	if len(status.MissingPrice) != 1 || status.MissingPrice[0] != "unknown-model" {
		t.Fatalf("missing prices = %#v, want unknown-model", status.MissingPrice)
	}
}

func TestLiteLLMPriceParserAndCostFormula(t *testing.T) {
	catalog, err := parseLiteLLMPriceCatalog("test", []byte(`{
		"gpt-test": {
			"input_cost_per_token": 0.000001,
			"output_cost_per_token": 0.000002,
			"cache_read_input_token_cost": 0.00000025
		}
	}`))
	if err != nil {
		t.Fatalf("parseLiteLLMPriceCatalog() error = %v", err)
	}
	price, ok := catalog.Prices["gpt-test"]
	if !ok {
		t.Fatal("gpt-test price missing")
	}
	cost := usageCostUSD(price, usage.Detail{
		InputTokens:     10,
		CachedTokens:    4,
		OutputTokens:    2,
		ReasoningTokens: 1,
	})
	want := 6*0.000001 + 4*0.00000025 + 3*0.000002
	if math.Abs(cost-want) > 0.000000000001 {
		t.Fatalf("cost = %.12f, want %.12f", cost, want)
	}
}
