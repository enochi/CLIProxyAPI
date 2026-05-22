package management

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/customerpolicy"
	customerpolicyui "github.com/router-for-me/CLIProxyAPI/v7/internal/customerpolicy/ui"
)

type customerPolicyPayload struct {
	KeyID                    string                     `json:"key_id"`
	APIKey                   string                     `json:"api_key"`
	Label                    string                     `json:"label"`
	Enabled                  *bool                      `json:"enabled"`
	AllowedModels            []string                   `json:"allowed_models"`
	DeniedModels             []string                   `json:"denied_models"`
	Quota                    customerpolicy.QuotaLimits `json:"quota"`
	RateLimit                customerpolicy.RateLimit   `json:"rate_limit"`
	MaxConcurrentRequests    int                        `json:"max_concurrent_requests"`
	FailClosedOnMissingPrice *bool                      `json:"fail_closed_on_missing_price"`
}

func (h *Handler) customerPolicyManager(c *gin.Context) *customerpolicy.Manager {
	if h == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "customer policy manager unavailable"})
		return nil
	}
	h.mu.Lock()
	manager := h.customerPolicy
	h.mu.Unlock()
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "customer policy manager unavailable"})
		return nil
	}
	return manager
}

// ServeCustomerKeysPage serves the standalone policy UI under management auth.
func (h *Handler) ServeCustomerKeysPage(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	c.String(http.StatusOK, customerpolicyui.HTML)
}

// GetCustomerKeyPolicies lists configured customer keys and policy state.
func (h *Handler) GetCustomerKeyPolicies(c *gin.Context) {
	manager := h.customerPolicyManager(c)
	if manager == nil {
		return
	}
	items, err := manager.ListKeys()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// PutCustomerKeyPolicies replaces all static policies.
func (h *Handler) PutCustomerKeyPolicies(c *gin.Context) {
	manager := h.customerPolicyManager(c)
	if manager == nil {
		return
	}
	var body struct {
		Policies []customerPolicyPayload `json:"policies"`
	}
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}
	if err := json.Unmarshal(data, &body); err != nil {
		var arr []customerPolicyPayload
		if errArr := json.Unmarshal(data, &arr); errArr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		body.Policies = arr
	}
	policies := make([]customerpolicy.KeyPolicy, 0, len(body.Policies))
	for _, payload := range body.Policies {
		policy := payload.toPolicy()
		if policy.KeyID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key_id or api_key is required"})
			return
		}
		policies = append(policies, policy)
	}
	if err := manager.ReplacePolicies(policies); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.GetCustomerKeyPolicies(c)
}

// PatchCustomerKeyPolicy creates or updates one static policy.
func (h *Handler) PatchCustomerKeyPolicy(c *gin.Context) {
	manager := h.customerPolicyManager(c)
	if manager == nil {
		return
	}
	var payload customerPolicyPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	keyID := strings.TrimSpace(c.Param("key_id"))
	policy := payload.toPolicy()
	if keyID == "" {
		keyID = policy.KeyID
	}
	updated, err := manager.UpsertPolicy(keyID, policy)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"policy": updated})
}

// DeleteCustomerKeyPolicy removes one static policy.
func (h *Handler) DeleteCustomerKeyPolicy(c *gin.Context) {
	manager := h.customerPolicyManager(c)
	if manager == nil {
		return
	}
	if err := manager.DeletePolicy(c.Param("key_id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// GetCustomerKeyPolicyStatus returns quota and limiter state for one key.
func (h *Handler) GetCustomerKeyPolicyStatus(c *gin.Context) {
	manager := h.customerPolicyManager(c)
	if manager == nil {
		return
	}
	status, ok := manager.Status(c.Param("key_id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": status})
}

// GetCustomerKeyPolicyRecords returns recent access records for one key.
func (h *Handler) GetCustomerKeyPolicyRecords(c *gin.Context) {
	manager := h.customerPolicyManager(c)
	if manager == nil {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	page, err := manager.ListRecords(customerpolicy.RecordsFilter{
		KeyID:  strings.TrimSpace(c.Param("key_id")),
		Model:  c.Query("model"),
		Status: c.Query("status"),
		Limit:  limit,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, page)
}

// GetCustomerKeyPrices returns current LiteLLM price cache metadata.
func (h *Handler) GetCustomerKeyPrices(c *gin.Context) {
	manager := h.customerPolicyManager(c)
	if manager == nil {
		return
	}
	state, prices := manager.Prices()
	c.JSON(http.StatusOK, gin.H{"status": state, "prices": prices})
}

// SyncCustomerKeyPrices refreshes LiteLLM model prices.
func (h *Handler) SyncCustomerKeyPrices(c *gin.Context) {
	manager := h.customerPolicyManager(c)
	if manager == nil {
		return
	}
	state, err := manager.SyncPrices(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "status": state})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": state})
}

func (p customerPolicyPayload) toPolicy() customerpolicy.KeyPolicy {
	keyID := strings.TrimSpace(p.KeyID)
	if keyID == "" && strings.TrimSpace(p.APIKey) != "" {
		keyID = customerpolicy.KeyID(p.APIKey)
	}
	return customerpolicy.KeyPolicy{
		KeyID:                    keyID,
		Label:                    p.Label,
		Enabled:                  p.Enabled,
		AllowedModels:            p.AllowedModels,
		DeniedModels:             p.DeniedModels,
		Quota:                    p.Quota,
		RateLimit:                p.RateLimit,
		MaxConcurrentRequests:    p.MaxConcurrentRequests,
		FailClosedOnMissingPrice: p.FailClosedOnMissingPrice,
	}
}
