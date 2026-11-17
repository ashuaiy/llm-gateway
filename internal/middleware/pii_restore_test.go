package middleware

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/warm3snow/llm-gateway/internal/types"
	"github.com/warm3snow/llm-gateway/pkg/guardrail"
)

func TestTokenizedResponseRestoresAcrossSSEFramesWithoutChangingObservedBody(t *testing.T) {
	manager, err := guardrail.NewManagerFromConfig(true, []types.GuardrailConfig{{Type: "request_policy", Parameters: map[string]interface{}{"piiMode": "tokenize"}}})
	require.NoError(t, err)
	router := gin.New()
	router.Use(GuardrailMiddleware(manager))
	observed := ""
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		require.NotContains(t, string(body), "13800138000")
		payload := guardrailPayload(body).(map[string]any)
		text := payload["prompt"].(string)
		cut := len(text) / 2
		c.Header("Content-Type", "text/event-stream")
		a := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + text[:cut] + "\"}}]}\n\n"
		b := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + text[cut:] + "\"}}]}\n\ndata: [DONE]\n\n"
		observed = a + b
		c.Writer.Write([]byte(a))
		c.Writer.Flush()
		c.Writer.Write([]byte(b))
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"prompt":"13800138000"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	require.Contains(t, w.Body.String(), "13800138000")
	require.NotContains(t, w.Body.String(), "[PII_")
	require.NotContains(t, observed, "13800138000")
}

func TestRestoredPIIStillPassesOutputPolicy(t *testing.T) {
	manager, err := guardrail.NewManagerFromConfig(true, []types.GuardrailConfig{{Type: "request_policy", Parameters: map[string]interface{}{"piiMode": "tokenize"}}, {Type: "response_policy", Parameters: map[string]interface{}{"blockPII": true}}})
	require.NoError(t, err)
	for _, stream := range []bool{false, true} {
		r := gin.New()
		r.Use(GuardrailMiddleware(manager))
		r.POST("/v1/chat/completions", func(c *gin.Context) {
			body, _ := io.ReadAll(c.Request.Body)
			text := guardrailPayload(body).(map[string]any)["prompt"].(string)
			if stream {
				c.Data(200, "text/event-stream", []byte(`data: {"choices":[{"index":0,"delta":{"content":"`+text+`"}}]}`+"\n\ndata: [DONE]\n\n"))
			} else {
				c.JSON(200, gin.H{"content": text})
			}
		})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"prompt":"13800138000"}`)))
		require.Equal(t, 403, w.Code)
		require.NotContains(t, w.Body.String(), "13800138000")
	}
}

func TestRestoredOutputExpansionIsBounded(t *testing.T) {
	manager, err := guardrail.NewManagerFromConfig(true, []types.GuardrailConfig{{Type: "request_policy", Parameters: map[string]interface{}{"piiMode": "tokenize"}}})
	require.NoError(t, err)
	for _, stream := range []bool{false, true} {
		r := gin.New()
		r.Use(GuardrailMiddleware(manager))
		r.POST("/v1/chat/completions", func(c *gin.Context) {
			body, _ := io.ReadAll(c.Request.Body)
			token := guardrailPayload(body).(map[string]any)["prompt"].(string)
			expanded := strings.Repeat(token+" ", 500)
			if stream {
				c.Data(200, "text/event-stream", []byte(`data: {"choices":[{"index":0,"delta":{"content":"`+expanded+`"}}]}`+"\n\ndata: [DONE]\n\n"))
			} else {
				c.JSON(200, gin.H{"content": expanded})
			}
		})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"prompt":"`+strings.Repeat("a", 4096)+`@example.com"}`)))
		require.Equal(t, 502, w.Code)
		require.Less(t, w.Body.Len(), 1<<20)
	}
}
