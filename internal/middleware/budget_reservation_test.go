package middleware

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/warm3snow/llm-gateway/pkg/cache"
)

func TestReservationGateRejectsBypasses(t *testing.T) {
	r := gin.New()
	r.Use(BudgetReservationGate(true))
	r.Any("/*path", func(c *gin.Context) { c.Status(200) })
	for _, test := range []struct {
		path, body string
		status     int
	}{
		{"/v1/embeddings", `{"input":"hello"}`, 400},
		{"/v1/proxy/chat/completions", `{}`, 400},
		{"/v1/chat/completions", `{"max_completion_tokens":10}`, 400},
		{"/v1/chat/completions", `{"model":"test","stream":true}`, 200},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", test.path, strings.NewReader(test.body)))
		require.Equal(t, test.status, w.Code, test.path)
	}
}

func TestReservationModeBypassesResponseCache(t *testing.T) {
	cached, err := cache.NewCache(&cache.Config{Type: "memory", MaxEntries: 10, DefaultTTL: time.Minute})
	require.NoError(t, err)
	r := gin.New()
	r.Use(BudgetReservationGate(true))
	r.Use(CacheMiddleware(cached, time.Minute, "openai"))
	calls := 0
	r.POST("/v1/chat/completions", func(c *gin.Context) { calls++; c.JSON(200, gin.H{"id": "test"}) })
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"test","messages":[]}`)))
		require.Equal(t, 200, w.Code)
	}
	require.Equal(t, 2, calls)
}
