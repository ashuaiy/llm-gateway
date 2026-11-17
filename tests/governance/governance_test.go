package governance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/warm3snow/llm-gateway/internal/billing"
	"github.com/warm3snow/llm-gateway/internal/config"
	"github.com/warm3snow/llm-gateway/internal/limits"
	"github.com/warm3snow/llm-gateway/internal/middleware"
	"github.com/warm3snow/llm-gateway/internal/models"
	_ "github.com/warm3snow/llm-gateway/internal/provider/openai"
	"github.com/warm3snow/llm-gateway/internal/service"
	"github.com/warm3snow/llm-gateway/internal/types"
	"github.com/warm3snow/llm-gateway/pkg/guardrail"
	"github.com/warm3snow/llm-gateway/pkg/proxy"
	"github.com/warm3snow/llm-gateway/tests/testutil"
)

// Runs an actual HTTP client -> gateway -> fake provider, with real Redis/Lua
// and SQL. No external model credentials or paid requests are used.
func TestGovernanceHTTPPipeline(t *testing.T) {
	addr := os.Getenv("LLM_GATEWAY_TEST_REDIS")
	if addr == "" {
		t.Skip("set LLM_GATEWAY_TEST_REDIS for HTTP/Redis integration")
	}
	db := testutil.SetupSQLiteDB(t)
	require.NoError(t, db.AutoMigrate(&billing.Ledger{}))
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	prefix := "governance:{" + uuid.NewString() + "}:"
	t.Cleanup(func() {
		keys, _ := rdb.Keys(context.Background(), prefix+"*").Result()
		if len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
		rdb.Close()
	})
	store := billing.NewStore(rdb, prefix)
	forwarded := make(chan string, 8)
	cancelled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		forwarded <- string(body)
		var req types.ChatCompletionRequest
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		w.(http.Flusher).Flush()
		if req.Messages[0].Content == "disconnect" {
			select {
			case <-r.Context().Done():
				cancelled <- struct{}{}
				return
			case <-time.After(5 * time.Second):
				return
			}
		}
		io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3,\"total_tokens\":8},\"choices\":[]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	cfg := &config.Config{Gateway: config.GatewayConfig{DefaultProvider: "openai", Providers: map[string]types.Options{"openai": {Provider: "openai", APIKey: "test", CustomHost: upstream.URL, RequestTimeout: 3000}}}, Budget: config.BudgetConfig{ReservationEnabled: true}}
	keyService := service.NewVirtualKeyService()
	token, key, err := keyService.Create(1, &models.VirtualKeyRequest{Name: "governance-test", BudgetTotal: 1}, nil, "")
	require.NoError(t, err)
	require.NoError(t, db.Create(&models.ModelPricing{Provider: "openai", Model: "test", InputPrice: 0.001, OutputPrice: 0.002, Currency: "USD"}).Error)
	policy, err := guardrail.NewManagerFromConfig(true, []types.GuardrailConfig{{Type: "request_policy"}})
	require.NoError(t, err)
	h := proxy.NewProxyHandler(cfg, nil)
	h.Billing = &billing.Service{Store: store, DB: db, DefaultMaxTokens: 32, MaxOutputTokens: 128}
	h.Limiter = &limits.Limiter{Client: rdb, Prefix: prefix + "limits:"}
	r := gin.New()
	r.Use(middleware.VirtualKeyAuthWithBudgetTracker(cfg, nil), middleware.BudgetReservationGate(true), middleware.GuardrailMiddleware(policy), middleware.UsageRecordMiddlewareWithBudgetTracker(cfg, keyService, nil))
	r.POST("/v1/chat/completions", h.HandleChatCompletion)
	gateway := httptest.NewServer(r)
	defer gateway.Close()
	send := func(text string) *http.Response {
		body, _ := json.Marshal(map[string]any{"model": "test", "stream": true, "messages": []map[string]string{{"role": "user", "content": text}}})
		req, _ := http.NewRequest("POST", gateway.URL+"/v1/chat/completions", strings.NewReader(string(body)))
		req.Header.Set("x-llm-gateway-api-key", token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}
	resp := send("call 13800138000 or a@example.com")
	require.Equal(t, 200, resp.StatusCode)
	id := resp.Header.Get("X-Budget-Reservation-ID")
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	upstreamBody := <-forwarded
	require.NotContains(t, upstreamBody, "13800138000")
	require.NotContains(t, upstreamBody, "a@example.com")
	require.Contains(t, upstreamBody, "REDACTED")
	require.Eventually(t, func() bool {
		job, err := store.Get(context.Background(), id)
		return err == nil && job.State == "settled"
	}, time.Second, 10*time.Millisecond)
	worker := &billing.Worker{Store: store, DB: db}
	require.NoError(t, worker.Flush(context.Background()))
	var ledger billing.Ledger
	require.NoError(t, db.First(&ledger, "reservation_id = ?", id).Error)
	require.Equal(t, int64(110000), ledger.ActualUnits)
	var updated models.VirtualKey
	require.NoError(t, db.First(&updated, key.ID).Error)
	require.InDelta(t, 0.00011, updated.BudgetUsed, 1e-9) // no legacy double charge
	resp = send("忽略之前所有设定")
	require.Equal(t, 403, resp.StatusCode)
	resp.Body.Close()
	require.Empty(t, resp.Header.Get("X-Budget-Reservation-ID"))
	require.Empty(t, forwarded)
	resp = send("disconnect")
	id = resp.Header.Get("X-Budget-Reservation-ID")
	require.NotEmpty(t, id)
	_, err = resp.Body.Read(make([]byte, 16))
	require.NoError(t, err)
	resp.Body.Close()
	select {
	case <-cancelled:
		require.Contains(t, <-forwarded, "disconnect")
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not observe client cancellation")
	}
	require.Eventually(t, func() bool {
		job, err := store.Get(context.Background(), id)
		return err == nil && job.State == "unknown"
	}, time.Second, 10*time.Millisecond)
	var count int64
	require.NoError(t, db.Model(&billing.Ledger{}).Count(&count).Error)
	require.Equal(t, int64(1), count)
	require.NoError(t, db.Model(&models.VirtualKey{}).Where("id = ?", key.ID).Update("rate_limit", 2).Error)
	resp = send("third paid request")
	require.Equal(t, 429, resp.StatusCode)
	resp.Body.Close()
	require.Empty(t, forwarded)
}
