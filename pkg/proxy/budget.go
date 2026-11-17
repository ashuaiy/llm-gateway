package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/warm3snow/llm-gateway/internal/billing"
	"github.com/warm3snow/llm-gateway/internal/models"
	"github.com/warm3snow/llm-gateway/internal/types"
)

func (h *ProxyHandler) beginBudget(c *gin.Context, req *types.ChatCompletionRequest, opts *types.Options) (*billing.Session, bool) {
	if h.Billing == nil {
		return nil, true
	}
	if opts.Retry != nil || len(opts.OverrideParams) > 0 {
		h.abortWithError(c, http.StatusBadRequest, "budget_unsupported", "Reservation mode requires retries and parameter overrides to be disabled")
		return nil, false
	}
	value, _ := c.Get("budget_virtual_key")
	key, ok := value.(models.VirtualKey)
	if !ok || key.ID == 0 {
		h.abortWithError(c, http.StatusUnauthorized, "budget_key_missing", "Virtual key required for reservations")
		return nil, false
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	session, err := h.Billing.Begin(ctx, key, opts.Provider, req)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, billing.ErrInsufficient) {
			status = http.StatusForbidden
		}
		// Do not expose provider payloads, database errors or Redis topology.
		slog.WarnContext(c.Request.Context(), "budget_reservation_rejected", "virtual_key_id", key.ID, "error", err)
		h.abortWithError(c, status, "budget_reservation_rejected", "Budget reservation unavailable or request outside supported billing policy")
		return nil, false
	}
	c.Set("budget_managed", true)
	c.Header("X-Budget-Reservation-ID", session.ID)
	return session, true
}

func (h *ProxyHandler) finishBudget(c *gin.Context, session *billing.Session) {
	if session == nil {
		return
	}
	// This context intentionally outlives a disconnected HTTP request, but is bounded.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var err error
	if !c.GetBool("budget_upstream_attempted") {
		err = session.Release(ctx)
	} else {
		var usage *types.TokenUsage
		if value, ok := c.Get("stream_usage"); ok {
			if u, ok := value.(types.TokenUsage); ok {
				usage = &u
			}
		}
		err = session.Complete(ctx, usage)
	}
	if err != nil {
		slog.Error("budget_settlement_pending", "reservation_id", session.ID, "error", err)
	}
}
