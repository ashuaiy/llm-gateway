package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/warm3snow/llm-gateway/internal/types"
)

// Reservation mode is deliberately limited to text chat. Other billable routes
// must not continue through the legacy post-charge path against the same budget.
func BudgetReservationGate(enabled bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !enabled {
			c.Next()
			return
		}
		if c.Request.Method == http.MethodGet && c.Request.URL.Path == "/v1/models" {
			c.Next()
			return
		}
		reject := func(status int, message string) {
			c.AbortWithStatusJSON(status, types.ErrorResponse{Type: "budget_unsupported", Message: message})
		}
		if c.Request.Method != http.MethodPost || (c.Request.URL.Path != "/v1/chat/completions" && c.Request.URL.Path != "/v1/chat/completions/stream") {
			reject(400, "Reservation mode supports chat completions only")
			return
		}
		b, err := io.ReadAll(io.LimitReader(c.Request.Body, (1<<20)+1))
		if err != nil {
			reject(400, "Invalid request body")
			return
		}
		if len(b) > 1<<20 {
			reject(413, "Request exceeds 1 MiB")
			return
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(b, &fields) != nil || fields == nil {
			reject(400, "Expected JSON object")
			return
		}
		for _, key := range []string{"max_completion_tokens", "functions", "function_call"} {
			if _, ok := fields[key]; ok {
				reject(400, "Reservation mode requires max_tokens and text-only messages")
				return
			}
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(b))
		c.Set("budget_reservations_enabled", true)
		var streaming bool
		_ = json.Unmarshal(fields["stream"], &streaming)
		c.Set("request_is_stream", streaming || c.Request.URL.Path == "/v1/chat/completions/stream")
		c.Next()
	}
}
