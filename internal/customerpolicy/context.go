package customerpolicy

import (
	"context"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	managerGinKey     = "__customer_policy_manager__"
	reservationGinKey = "__customer_policy_reservation__"
)

// ReservationFromContext returns the active policy reservation from a usage context.
func ReservationFromContext(ctx context.Context) *Reservation {
	if ctx == nil {
		return nil
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil {
		return nil
	}
	return ReservationFromGin(ginCtx)
}

// ReservationFromGin returns the active reservation on a Gin context.
func ReservationFromGin(c *gin.Context) *Reservation {
	if c == nil {
		return nil
	}
	value, ok := c.Get(reservationGinKey)
	if !ok {
		return nil
	}
	res, _ := value.(*Reservation)
	return res
}

func setReservation(c *gin.Context, res *Reservation) {
	if c == nil {
		return
	}
	c.Set(reservationGinKey, res)
}

func clearReservation(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(reservationGinKey, (*Reservation)(nil))
}

func setManager(c *gin.Context, manager *Manager) {
	if c == nil || manager == nil {
		return
	}
	c.Set(managerGinKey, manager)
}

func managerFromGin(c *gin.Context) *Manager {
	if c == nil {
		return nil
	}
	value, ok := c.Get(managerGinKey)
	if !ok {
		return nil
	}
	manager, _ := value.(*Manager)
	return manager
}

func apiKeyFromGin(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, ok := c.Get("userApiKey")
	if !ok || value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case interface{ String() string }:
		return strings.TrimSpace(v.String())
	default:
		return strings.TrimSpace(ginValueString(value))
	}
}

func ginValueString(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%v", value))
}
