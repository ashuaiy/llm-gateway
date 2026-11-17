package governance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/warm3snow/llm-gateway/internal/billing"
	"github.com/warm3snow/llm-gateway/internal/config"
	"github.com/warm3snow/llm-gateway/internal/database"
	"github.com/warm3snow/llm-gateway/internal/limits"
	"github.com/warm3snow/llm-gateway/internal/middleware"
	"github.com/warm3snow/llm-gateway/internal/models"
	"github.com/warm3snow/llm-gateway/internal/service"
	"github.com/warm3snow/llm-gateway/internal/types"
	"github.com/warm3snow/llm-gateway/pkg/guardrail"
	"github.com/warm3snow/llm-gateway/pkg/proxy"
	"gorm.io/gorm"
)

type loadResult struct {
	Mode        string      `json:"mode"`
	Requests    int         `json:"requests"`
	Concurrency int         `json:"concurrency"`
	Status      map[int]int `json:"http_status"`
	RPS         float64     `json:"rps"`
	P50         float64     `json:"p50_ms"`
	P95         float64     `json:"p95_ms"`
	P99         float64     `json:"p99_ms"`
	Budget      float64     `json:"budget_usd"`
	Spent       float64     `json:"spent_usd"`
	Overspend   float64     `json:"overspend_usd"`
	DrainMillis float64     `json:"outbox_drain_ms"`
	Ledgers     int64       `json:"ledgers"`
}

// Explicit opt-in: this exercises real TCP HTTP, Redis and MySQL with a fake
// provider, not an external paid LLM. Use only a disposable test database.
func TestGovernanceLoadEvidence(t *testing.T) {
	if os.Getenv("LLM_GATEWAY_RUN_LOAD") != "1" {
		t.Skip("set LLM_GATEWAY_RUN_LOAD=1 with disposable Redis/MySQL")
	}
	require.NotEmpty(t, os.Getenv("LLM_GATEWAY_TEST_REDIS"))
	require.NotEmpty(t, os.Getenv("LLM_GATEWAY_TEST_MYSQL"))
	require.NoError(t, database.Connect(&database.Config{Driver: "mysql", DSN: os.Getenv("LLM_GATEWAY_TEST_MYSQL"), LogLevel: "silent"}))
	db := database.GetDB()
	t.Cleanup(func() { database.Close(); database.DB = nil })
	require.NoError(t, db.AutoMigrate(&models.VirtualKey{}, &models.ModelPricing{}, &billing.Ledger{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(32)
	results := []loadResult{
		runHTTPLoad(t, db, false, 100, 100, true),
		runHTTPLoad(t, db, true, 100, 100, true),
		runHTTPLoad(t, db, true, 1000, 64, false),
	}
	require.Greater(t, results[0].Overspend, 0.0)
	require.Equal(t, 0.0, results[1].Overspend)
	require.Equal(t, 10, results[1].Status[200])
	require.Equal(t, 1000, results[2].Status[200])
	report := map[string]any{"recorded_at": time.Now().UTC().Format(time.RFC3339), "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "logical_cpus": runtime.NumCPU(), "gateway_instances": 2, "provider": "local fake SSE, 10 ms delay for capacity run; synchronized barrier for budget race", "results": results}
	b, err := json.MarshalIndent(report, "", "  ")
	require.NoError(t, err)
	t.Log(string(b))
	if path := os.Getenv("LLM_GATEWAY_BENCH_REPORT"); path != "" {
		require.NoError(t, os.WriteFile(path, append(b, '\n'), 0644))
	}
}

func runHTTPLoad(t *testing.T, db *gorm.DB, reservations bool, n, parallel int, barrier bool) loadResult {
	t.Helper()
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: os.Getenv("LLM_GATEWAY_TEST_REDIS")})
	prefix := "load:{" + uuid.NewString() + "}:"
	defer func() {
		keys, _ := rdb.Keys(ctx, prefix+"*").Result()
		if len(keys) > 0 {
			rdb.Del(ctx, keys...)
		}
		rdb.Close()
	}()
	store := billing.NewStore(rdb, prefix)
	arrived := make(chan struct{}, n*2)
	release := make(chan struct{})
	if !barrier {
		close(release)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		if barrier {
			arrived <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		} else {
			time.Sleep(10 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n")
		w.(http.Flusher).Flush()
		io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":10,\"total_tokens\":15},\"choices\":[]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	model := "load-" + uuid.NewString()
	price := models.ModelPricing{Provider: "openai", Model: model, InputPrice: 0, OutputPrice: 10, Currency: "USD"}
	require.NoError(t, db.Create(&price).Error)
	defer db.Delete(&price)
	budget := 10.0
	if !barrier {
		budget = 100000
	}
	ks := service.NewVirtualKeyService()
	token, key, err := ks.Create(1, &models.VirtualKeyRequest{Name: model, BudgetTotal: budget}, nil, "")
	require.NoError(t, err)
	require.NoError(t, db.Model(&models.VirtualKey{}).Where("id = ?", key.ID).Update("rate_limit", 0).Error)
	defer func() {
		db.Delete(&billing.Ledger{}, "virtual_key_id = ?", key.ID)
		db.Delete(&models.VirtualKey{}, key.ID)
	}()
	cfg := &config.Config{Gateway: config.GatewayConfig{DefaultProvider: "openai", MaxRequestTimeout: 30000, Providers: map[string]types.Options{"openai": {Provider: "openai", APIKey: "test", CustomHost: upstream.URL, RequestTimeout: 30000}}}, Budget: config.BudgetConfig{ReservationEnabled: reservations, ReservationDefaultMaxTokens: 10}}
	policy, err := guardrail.NewManagerFromConfig(true, []types.GuardrailConfig{{Type: "request_policy"}, {Type: "response_policy", Parameters: map[string]interface{}{"keywords": []string{"blocked example"}}}})
	require.NoError(t, err)
	var servers []*httptest.Server
	workerCtx, stopWorkers := context.WithCancel(ctx)
	var workers sync.WaitGroup
	for i := 0; i < 2; i++ {
		h := proxy.NewProxyHandler(cfg, nil)
		h.SetGuardrailManager(policy)
		if reservations {
			h.Billing = &billing.Service{Store: store, DB: db, DefaultMaxTokens: 10, MaxOutputTokens: 128}
			h.Limiter = &limits.Limiter{Client: rdb, Prefix: prefix + "limits:"}
			workers.Add(1)
			go func() { defer workers.Done(); (&billing.Worker{Store: store, DB: db, Concurrency: 4}).Run(workerCtx) }()
		}
		r := gin.New()
		r.Use(middleware.VirtualKeyAuthWithBudgetTracker(cfg, nil), middleware.BudgetReservationGate(reservations), middleware.GuardrailMiddleware(policy), middleware.UsageRecordMiddlewareWithBudgetTracker(cfg, ks, nil))
		r.POST("/v1/chat/completions", h.HandleChatCompletion)
		s := httptest.NewServer(r)
		servers = append(servers, s)
		defer s.Close()
	}
	defer func() { stopWorkers(); workers.Wait() }()
	client := &http.Client{Timeout: 40 * time.Second, Transport: &http.Transport{MaxIdleConns: 256, MaxIdleConnsPerHost: 128}}
	defer client.CloseIdleConnections()
	result := loadResult{Mode: fmt.Sprintf("reservations=%v,barrier=%v", reservations, barrier), Requests: n, Concurrency: parallel, Status: map[int]int{}, Budget: budget}
	latencies := make([]float64, 0, n)
	var mu sync.Mutex
	var wg sync.WaitGroup
	work := make(chan int, n)
	for i := 0; i < n; i++ {
		work <- i
	}
	close(work)
	start := time.Now()
	for j := 0; j < parallel; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				now := time.Now()
				payload := fmt.Sprintf(`{"model":%q,"stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`, model)
				req, _ := http.NewRequest("POST", servers[i%len(servers)].URL+"/v1/chat/completions", strings.NewReader(payload))
				req.Header.Set("x-llm-gateway-api-key", token)
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				status := 0
				if err == nil {
					status = resp.StatusCode
					_, readErr := io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if readErr != nil {
						status = 0
					}
				}
				if barrier && status != 200 {
					arrived <- struct{}{}
				}
				mu.Lock()
				result.Status[status]++
				latencies = append(latencies, float64(time.Since(now).Microseconds())/1000)
				mu.Unlock()
			}
		}()
	}
	if barrier {
		timeout := time.NewTimer(35 * time.Second)
		for i := 0; i < n; i++ {
			select {
			case <-arrived:
			case <-timeout.C:
				close(release)
				wg.Wait()
				t.Fatal("barrier timeout")
			}
		}
		timeout.Stop()
		close(release)
	}
	wg.Wait()
	result.RPS = float64(n) / time.Since(start).Seconds()
	sort.Float64s(latencies)
	result.P50 = latencies[(n-1)*50/100]
	result.P95 = latencies[(n-1)*95/100]
	result.P99 = latencies[(n-1)*99/100]
	drainStart := time.Now()
	if reservations {
		require.Eventually(t, func() bool { count, err := rdb.ZCard(ctx, prefix+"pending").Result(); return err == nil && count == 0 }, 30*time.Second, 10*time.Millisecond)
	}
	result.DrainMillis = float64(time.Since(drainStart).Microseconds()) / 1000
	require.NoError(t, db.Model(&billing.Ledger{}).Where("virtual_key_id = ?", key.ID).Count(&result.Ledgers).Error)
	var final models.VirtualKey
	require.NoError(t, db.First(&final, key.ID).Error)
	result.Spent = final.BudgetUsed
	result.Overspend = math.Max(0, result.Spent-budget)
	if reservations {
		require.Equal(t, int64(result.Status[200]), result.Ledgers)
	}
	return result
}
