package customerpolicy

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/tidwall/gjson"
)

// Middleware enforces customer API key policy after AuthMiddleware.
func (m *Manager) Middleware(source string) gin.HandlerFunc {
	return func(c *gin.Context) {
		setManager(c, m)
		if m == nil || !m.Enabled() {
			c.Next()
			return
		}
		model := extractRequestModel(c)
		if model == "" {
			c.Next()
			return
		}
		res, decision := m.Begin(c.Request.Context(), requestInfoFromGin(c, model, source))
		if decision != nil {
			writeDecision(c, decision)
			return
		}
		if res == nil {
			c.Next()
			return
		}
		setReservation(c, res)
		defer func() {
			if recovered := recover(); recovered != nil {
				res.Finish(http.StatusInternalServerError, true)
				clearReservation(c)
				panic(recovered)
			}
			status := c.Writer.Status()
			if status <= 0 {
				status = http.StatusOK
			}
			res.Finish(status, status >= http.StatusBadRequest)
			clearReservation(c)
		}()
		c.Next()
	}
}

// BeginWebsocketMessage enforces one normalized websocket message.
func BeginWebsocketMessage(c *gin.Context, model string) (*Reservation, *Decision) {
	manager := managerFromGin(c)
	if manager == nil || !manager.Enabled() {
		return nil, nil
	}
	res, decision := manager.Begin(c.Request.Context(), requestInfoFromGin(c, model, "websocket"))
	if decision != nil {
		return nil, decision
	}
	if res != nil {
		setReservation(c, res)
	}
	return res, nil
}

// FinishWebsocketMessage finalizes one websocket reservation.
func FinishWebsocketMessage(c *gin.Context, res *Reservation, status int, failed bool) {
	if res != nil {
		res.Finish(status, failed)
	}
	clearReservation(c)
}

func requestInfoFromGin(c *gin.Context, model, source string) RequestInfo {
	return RequestInfo{
		APIKey:    apiKeyFromGin(c),
		Model:     model,
		Endpoint:  requestEndpoint(c),
		RequestID: logging.GetGinRequestID(c),
		Source:    source,
	}
}

func requestEndpoint(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	path := strings.TrimSpace(c.FullPath())
	if path == "" && c.Request.URL != nil {
		path = strings.TrimSpace(c.Request.URL.Path)
	}
	method := strings.TrimSpace(c.Request.Method)
	if method != "" && path != "" {
		return method + " " + path
	}
	return path
}

func writeDecision(c *gin.Context, decision *Decision) {
	if c == nil || decision == nil {
		return
	}
	if decision.RetryAfter > 0 {
		c.Header("Retry-After", strconvItoa(decision.RetryAfter))
	}
	c.AbortWithStatusJSON(decision.StatusCode, gin.H{
		"error": gin.H{
			"code":        decision.Reason,
			"message":     decision.Message,
			"key_id":      decision.KeyID,
			"masked_key":  decision.MaskedKey,
			"model":       decision.Model,
			"reset_at":    decision.ResetAt,
			"retry_after": decision.RetryAfter,
		},
	})
}

func strconvItoa(value int) string {
	if value <= 0 {
		return ""
	}
	return strconv.FormatInt(int64(value), 10)
}

func extractRequestModel(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	if model := extractModelFromPath(c.Request.URL.Path); model != "" {
		return model
	}
	if model := strings.TrimSpace(c.Query("model")); model != "" {
		return model
	}
	contentType := strings.ToLower(c.Request.Header.Get("Content-Type"))
	if strings.HasPrefix(contentType, "multipart/form-data") || strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
		if strings.HasPrefix(contentType, "multipart/form-data") {
			_ = c.Request.ParseMultipartForm(32 << 20)
		} else {
			_ = c.Request.ParseForm()
		}
		if c.Request.Form != nil || c.Request.MultipartForm != nil {
			if model := strings.TrimSpace(c.Request.FormValue("model")); model != "" {
				return model
			}
		}
		return ""
	}
	if c.Request.Body == nil {
		return ""
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.Request.Body = io.NopCloser(bytes.NewReader(nil))
		return ""
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	return strings.TrimSpace(gjson.GetBytes(body, "model").String())
}

func extractModelFromPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	idx := strings.Index(path, "/models/")
	if idx < 0 {
		return ""
	}
	rest := strings.TrimPrefix(path[idx+len("/models/"):], "/")
	if rest == "" {
		return ""
	}
	if slash := strings.Index(rest, "/"); slash >= 0 {
		rest = rest[:slash]
	}
	if colon := strings.Index(rest, ":"); colon >= 0 {
		rest = rest[:colon]
	}
	return strings.TrimSpace(rest)
}
