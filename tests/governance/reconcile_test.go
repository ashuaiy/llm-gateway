package governance

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/warm3snow/llm-gateway/internal/billing"
	"github.com/warm3snow/llm-gateway/internal/config"
	"github.com/warm3snow/llm-gateway/internal/handler"
	"github.com/warm3snow/llm-gateway/internal/middleware"
	"github.com/warm3snow/llm-gateway/tests/testutil"
)

func TestReconciliationAPIRequiresAdminTenantAndEvidence(t *testing.T) {
	addr := os.Getenv("LLM_GATEWAY_TEST_REDIS")
	if addr == "" {
		t.Skip("requires Redis")
	}
	db := testutil.SetupSQLiteDB(t)
	require.NoError(t, db.AutoMigrate(&billing.Ledger{}))
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()
	prefix := "reconcile:{" + uuid.NewString() + "}:"
	t.Cleanup(func() {
		keys, _ := rdb.Keys(context.Background(), prefix+"*").Result()
		if len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
	})
	store := billing.NewStore(rdb, prefix)
	ctx := context.Background()
	require.NoError(t, store.Reserve(ctx, billing.Reservation{ID: "review", KeyID: 123, TenantID: 1, Amount: 100, Limit: 100, InputRate: 2, OutputRate: 3}))
	require.NoError(t, store.Unknown(ctx, "review"))
	cfg := &config.Config{Security: config.SecurityConfig{JWTSecret: "test-governance-signing"}}
	router := gin.New()
	h := handler.BillingHandler{Store: store, DB: db}
	h.RegisterRoutesWithAuth(router, middleware.JWTAuth(cfg))
	call := func(role string, tenant int, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if role != "" {
			token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"role": role, "tenant_id": tenant, "user_id": 99, "exp": time.Now().Add(time.Hour).Unix()})
			s, err := token.SignedString([]byte(cfg.Security.JWTSecret))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+s)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	path := "/api/v1/billing/reservations/review"
	require.Equal(t, 401, call("", 1, "GET", path, "").Code)
	require.Equal(t, 403, call("tenant_user", 1, "GET", path, "").Code)
	require.Equal(t, 404, call("tenant_admin", 2, "GET", path, "").Code)
	require.Equal(t, 200, call("tenant_admin", 1, "GET", path, "").Code)
	require.Equal(t, 400, call("tenant_admin", 1, "POST", path+"/reconcile", `{"resolution_id":"r1","action":"settle","input_tokens":5}`).Code)
	body := `{"resolution_id":"r1","action":"settle","input_tokens":5,"output_tokens":3,"reason":"invoice verified","evidence":"invoice/123"}`
	require.Equal(t, 200, call("tenant_admin", 1, "POST", path+"/reconcile", body).Code)
	w := &billing.Worker{Store: store, DB: db}
	require.NoError(t, w.Flush(ctx))
	var row billing.Ledger
	require.NoError(t, db.First(&row, "reservation_id = ?", "review").Error)
	require.Equal(t, int64(19), row.ActualUnits)
	require.Contains(t, row.ResolutionJSON, "user:99")
	// A retry after Redis retention expiry must consult the durable ledger.
	require.NoError(t, rdb.Del(ctx, prefix+"reservation:review").Err())
	require.Equal(t, 200, call("tenant_admin", 1, "POST", path+"/reconcile", body).Code)
	require.Equal(t, 409, call("tenant_admin", 1, "POST", path+"/reconcile", strings.Replace(body, `"output_tokens":3`, `"output_tokens":4`, 1)).Code)
}
