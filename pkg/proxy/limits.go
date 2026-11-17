package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/warm3snow/llm-gateway/internal/limits"
	"github.com/warm3snow/llm-gateway/internal/models"
	"github.com/warm3snow/llm-gateway/internal/types"
)

func (h *ProxyHandler) beginLimits(c *gin.Context, req *types.ChatCompletionRequest) (func(), bool) {
	if h.Limiter == nil {
		return func() {}, true
	}
	value, _ := c.Get("budget_virtual_key")
	key, ok := value.(models.VirtualKey)
	if !ok {
		h.abortWithError(c, 401, "key_missing", "Virtual key required")
		return nil, false
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = h.Config.Budget.ReservationDefaultMaxTokens
	}
	body, err := json.Marshal(req.Messages)
	if err != nil {
		h.abortWithError(c, 400, "invalid_request", "Cannot estimate tokens")
		return nil, false
	}
	tokens := len(body) + 256*len(req.Messages) + 256 + req.MaxTokens
	attempt, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	lease, err := h.Limiter.Acquire(attempt, key.ID, uuid.NewString(), limits.Policy{Requests: key.RateLimit, WindowSeconds: key.RateLimitWindow, Tokens: key.TokenLimitPerMinute, Streams: key.MaxConcurrentStreams}, tokens, req.Stream)
	cancel()
	if err != nil {
		status := 503
		if errors.Is(err, limits.ErrLimited) {
			status = 429
			c.Header("Retry-After", "1")
		}
		h.abortWithError(c, status, "key_limit", "Key rate or concurrency limit unavailable or exceeded")
		return nil, false
	}
	parent := c.Request.Context()
	ctx, stopUpstream := context.WithCancel(parent)
	c.Request = c.Request.WithContext(ctx)
	done := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				attempt, cancel := context.WithTimeout(ctx, 3*time.Second)
				err := lease.Renew(attempt)
				cancel()
				if err != nil {
					stopUpstream()
					return
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-done
		stopUpstream()
		actual := 0
		authoritative := !c.GetBool("budget_upstream_attempted")
		if value, ok := c.Get("stream_usage"); ok {
			if usage, ok := value.(types.TokenUsage); ok && usage.Source == "provider" {
				actual = usage.TotalTokens
				authoritative = true
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := lease.Finish(ctx, actual, authoritative); err != nil {
			slog.Error("key_limit_finish_failed", "virtual_key_id", key.ID)
		}
	}, true
}
