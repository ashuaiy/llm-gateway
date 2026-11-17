package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/warm3snow/llm-gateway/internal/types"
	"github.com/warm3snow/llm-gateway/pkg/guardrail"
)

func TestGuardrailMiddlewareForwardsRedactedBody(t *testing.T) {
	m, err := guardrail.NewManagerFromConfig(true, []types.GuardrailConfig{{Type: "request_policy", Parameters: map[string]interface{}{"piiAction": "redact"}}})
	require.NoError(t, err)
	r := gin.New()
	r.Use(GuardrailMiddleware(m))
	var forwarded string
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		forwarded = string(b)
		require.Equal(t, int64(len(b)), c.Request.ContentLength)
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"call 13800138000"}]}`)))
	require.Equal(t, 200, w.Code)
	require.NotContains(t, forwarded, "13800138000")
	require.Contains(t, forwarded, "REDACTED")
}

func TestGuardrailDuplicateFieldsCannotPreserveUnscannedContent(t *testing.T) {
	m, err := guardrail.NewManagerFromConfig(true, []types.GuardrailConfig{{Type: "request_policy"}})
	require.NoError(t, err)
	r := gin.New()
	r.Use(GuardrailMiddleware(m))
	var parsed types.ChatCompletionRequest
	r.POST("/v1/chat/completions", func(c *gin.Context) { _ = c.ShouldBindJSON(&parsed); c.Status(200) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"a@example.com"}],"messages":[{"role":"user"}]}`)))
	for _, m := range parsed.Messages {
		require.NotEqual(t, "a@example.com", m.Content)
	}
}
