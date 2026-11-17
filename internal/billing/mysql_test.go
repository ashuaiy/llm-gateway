package billing

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/warm3snow/llm-gateway/internal/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func mysqlTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LLM_GATEWAY_TEST_MYSQL")
	if dsn == "" {
		t.Skip("requires disposable MySQL")
	}
	if !strings.Contains(dsn, "clientFoundRows") {
		dsn += "&clientFoundRows=true"
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&models.VirtualKey{}, &Ledger{}))
	return db
}
func TestMySQLDuplicateDeliveryDoesNotChargeWithClientFoundRows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	db := mysqlTestDB(t)
	id := uuid.NewString()
	key := models.VirtualKey{Name: "duplicate-test", KeyHash: uuid.NewString(), BudgetTotal: 1}
	require.NoError(t, db.Create(&key).Error)
	t.Cleanup(func() { db.Delete(&Ledger{}, "reservation_id = ?", id); db.Delete(&models.VirtualKey{}, key.ID) })
	require.NoError(t, s.Reserve(ctx, Reservation{ID: id, KeyID: key.ID, Amount: 1000, Limit: 1000}))
	require.NoError(t, s.Settle(ctx, id, Settlement{Actual: 100, Source: "provider"}))
	w := &Worker{Store: s, DB: db}
	require.NoError(t, w.Flush(ctx))
	require.NoError(t, s.client.ZAdd(ctx, s.prefix+"pending", redisZ(id)).Err())
	require.NoError(t, w.Flush(ctx))
	var actual models.VirtualKey
	require.NoError(t, db.First(&actual, key.ID).Error)
	require.InDelta(t, float64(100)/float64(NanoUSD), actual.BudgetUsed, 1e-10)
}
