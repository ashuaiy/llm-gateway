package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/warm3snow/llm-gateway/internal/billing"
	"github.com/warm3snow/llm-gateway/internal/config"
	"github.com/warm3snow/llm-gateway/internal/models"
	"github.com/warm3snow/llm-gateway/internal/provider"
	"github.com/warm3snow/llm-gateway/internal/provider/mockprovider"
	"github.com/warm3snow/llm-gateway/internal/types"
	"github.com/warm3snow/llm-gateway/pkg/guardrail"
	"go.uber.org/mock/gomock"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestStreamingOutputPolicyBlocksCrossChunkContent(t *testing.T) {
	cfg := &config.Config{}
	h := NewProxyHandler(cfg, nil)
	manager, err := guardrail.NewManagerFromConfig(true, []types.GuardrailConfig{{Type: "response_policy", Parameters: map[string]interface{}{"keywords": []string{"internal-password"}}}})
	require.NoError(t, err)
	h.SetGuardrailManager(manager)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"internal-\"}}]}\n\n" + "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"password\"}}]}\n\n"
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}
	h.streamResponse(c, resp, &types.ChatCompletionRequest{}, "openai", time.Now())
	require.NotContains(t, w.Body.String(), "internal-")
	require.Contains(t, w.Body.String(), "output_blocked")
}

func TestChatStreamUsesFinalUsageForReservationSettlement(t *testing.T) {
	addr := os.Getenv("LLM_GATEWAY_TEST_REDIS")
	if addr == "" {
		t.Skip("set LLM_GATEWAY_TEST_REDIS")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	prefix := "proxy-budget:{" + uuid.NewString() + "}:"
	t.Cleanup(func() {
		keys, _ := rdb.Keys(context.Background(), prefix+"*").Result()
		if len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
		rdb.Close()
	})
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "proxy.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.ModelPricing{}))
	require.NoError(t, db.Create(&models.ModelPricing{Provider: "openai", Model: "test", InputPrice: 0.001, OutputPrice: 0.002, Currency: "USD"}).Error)
	store := billing.NewStore(rdb, prefix)
	ctrl := gomock.NewController(t)
	p := mockprovider.NewMockProvider(ctrl)
	h := newProxyTestHandler(t, p)
	opts := h.Config.Gateway.Providers["mock"]
	opts.Provider = "openai"
	h.Config.Gateway.Providers["mock"] = opts
	h.ProviderFactory.Register("openai", func(*types.Options) (provider.Provider, error) { return p, nil })
	h.Billing = &billing.Service{Store: store, DB: db, DefaultMaxTokens: 32, MaxOutputTokens: 128}
	p.EXPECT().ChatCompletion(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, req *types.ChatCompletionRequest, _ *types.Options) (*http.Response, error) {
		require.True(t, req.Stream)
		require.NotNil(t, req.StreamOptions)
		require.True(t, req.StreamOptions.IncludeUsage)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3,\"total_tokens\":8},\"choices\":[]}\n\ndata: [DONE]\n\n"))}, nil
	})
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("budget_virtual_key", models.VirtualKey{ID: 1, BudgetTotal: 1}); c.Next() })
	r.POST("/v1/chat/completions", h.HandleChatCompletion)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"test","stream":true,"messages":[{"role":"user","content":"hello"}]}`)))
	require.Equal(t, 200, w.Code)
	id := w.Header().Get("X-Budget-Reservation-ID")
	require.NotEmpty(t, id)
	job, err := store.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, "settled", job.State)
	require.Equal(t, int64(110000), job.Settlement.Actual)
}

type disconnectedWriter struct{ *httptest.ResponseRecorder }

func (w disconnectedWriter) Write([]byte) (int, error) { return 0, errors.New("client disconnected") }

func TestStreamAccountsFinalUsageBeforeWritingToDisconnectedClient(t *testing.T) {
	w := disconnectedWriter{httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions/stream", nil)
	h := NewProxyHandler(nil, nil)
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("data: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9},\"choices\":[]}\n\n"))}
	h.streamResponse(c, resp, &types.ChatCompletionRequest{}, "mock", time.Now())
	u, ok := c.Get("stream_usage")
	require.True(t, ok)
	usage, ok := u.(types.TokenUsage)
	require.True(t, ok)
	require.Equal(t, 7, usage.PromptTokens)
	require.Equal(t, "provider", usage.Source)
}

func TestIncompleteUsageIsNotAuthoritative(t *testing.T) {
	for _, payload := range []string{`{"usage":{}}`, `{"usage":{"total_tokens":99}}`, `{"usage":{"input_tokens":7,"output_tokens":3}}`, `{"usage":{"prompt_tokens":-1,"completion_tokens":2,"total_tokens":1}}`, `{"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":0}}`} {
		require.Nil(t, extractUsageFromChunk([]byte(payload)), payload)
	}
}

func TestFinalUsageAcceptsWhitespaceEmptyChoices(t *testing.T) {
	require.NotNil(t, extractFinalStreamUsage([]byte(`{"choices": [ ], "usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)))
	for _, choices := range []string{"null", `[{}]`} {
		require.Nil(t, extractFinalStreamUsage([]byte(`{"choices":`+choices+`,"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)))
	}
}

func TestSSEScannerPreservesCRLFEvents(t *testing.T) {
	first := []byte("data: {\"choices\":[]}\r\n\r\n")
	data := append(append([]byte{}, first...), []byte("data: [DONE]\r\n\r\n")...)
	n, event, err := scanSSEEvents(data, false)
	require.NoError(t, err)
	require.Equal(t, len(first), n)
	require.Equal(t, first, event)
}

func TestRestorationCapsNonStreamingUpstreamRead(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	c.Set("pii_restore", true)
	body := strings.NewReader(strings.Repeat("x", 2<<20))
	h := NewProxyHandler(nil, nil)
	h.handleResponse(c, &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(body)})
	require.Equal(t, 502, w.Code)
	require.Greater(t, body.Len(), 0)
}

func TestIntermediateStreamUsageIsNotFinal(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	h := NewProxyHandler(nil, nil)
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\n"))}
	h.streamResponse(c, resp, &types.ChatCompletionRequest{}, "openai", time.Now())
	u, ok := c.Get("stream_usage")
	require.True(t, ok)
	require.NotEqual(t, "provider", u.(types.TokenUsage).Source)
}
