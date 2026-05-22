package customerpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// KeyID returns a deterministic non-secret identifier for a customer API key.
func KeyID(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(sum[:])
}

// MaskKey returns a display-safe API key mask.
func MaskKey(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) <= 8 {
		return strings.Repeat("*", len(trimmed))
	}
	return trimmed[:4] + strings.Repeat("*", 4) + trimmed[len(trimmed)-4:]
}

func stableRecordID(requestID, keyID string, now time.Time) string {
	base := fmt.Sprintf("%s:%s:%d", strings.TrimSpace(requestID), strings.TrimSpace(keyID), now.UnixNano())
	sum := sha256.Sum256([]byte(base))
	return hex.EncodeToString(sum[:12])
}

func normalizeModel(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func matchesPattern(pattern, model string) bool {
	pattern = normalizeModel(pattern)
	model = normalizeModel(model)
	if pattern == "" || model == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	if ok, err := filepath.Match(pattern, model); err == nil && ok {
		return true
	}
	return pattern == model
}

func anyPatternMatch(patterns []string, model string) bool {
	for _, pattern := range patterns {
		if matchesPattern(pattern, model) {
			return true
		}
	}
	return false
}

func clampNonNegativeInt64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func clampNonNegativeFloat(value float64) float64 {
	if value < 0 {
		return 0
	}
	return value
}
