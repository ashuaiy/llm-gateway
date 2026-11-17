package billing

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/warm3snow/llm-gateway/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestOutboxSurvivesWorkerRestartAndDuplicateDelivery(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "billing.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.VirtualKey{}, &Ledger{}))
	require.NoError(t, db.Create(&models.VirtualKey{ID: 1, Name: "test", KeyHash: "one", BudgetTotal: 1}).Error)
	require.NoError(t, s.Reserve(ctx, Reservation{ID: "survives", KeyID: 1, Limit: NanoUSD, Amount: NanoUSD / 2}))
	require.NoError(t, s.Settle(ctx, "survives", Settlement{Actual: NanoUSD / 10, Source: "provider"}))
	w := &Worker{Store: s, DB: db}
	require.NoError(t, w.Flush(ctx))
	// Simulate a lost acknowledgement after the SQL transaction committed.
	require.NoError(t, s.client.ZAdd(ctx, s.prefix+"pending", redisZ("survives")).Err())
	w = &Worker{Store: NewStore(s.client, s.prefix), DB: db}
	require.NoError(t, w.Flush(ctx))
	var count int64
	require.NoError(t, db.Model(&Ledger{}).Count(&count).Error)
	require.Equal(t, int64(1), count)
	var key models.VirtualKey
	require.NoError(t, db.First(&key, 1).Error)
	require.InDelta(t, 0.1, key.BudgetUsed, 1e-9)
	jobs, err := s.Pending(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, jobs)
}

func TestAcknowledgedReservationsExpireButUnfinishedOnesDoNot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "retention.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.VirtualKey{}, &Ledger{}))
	for _, id := range []string{"finished", "unknown", "pending"} {
		require.NoError(t, s.Reserve(ctx, Reservation{ID: id, KeyID: 1, Amount: 10, Limit: 100}))
	}
	require.NoError(t, s.Unknown(ctx, "unknown"))
	require.NoError(t, s.Settle(ctx, "finished", Settlement{Actual: 5, Source: "provider"}))
	w := &Worker{Store: s, DB: db}
	require.NoError(t, w.Flush(ctx))
	ttl, err := s.client.PTTL(ctx, s.prefix+"reservation:finished").Result()
	require.NoError(t, err)
	require.Positive(t, ttl, "only durable terminal records should get retention TTL")
	for _, id := range []string{"unknown", "pending"} {
		ttl, err = s.client.PTTL(ctx, s.prefix+"reservation:"+id).Result()
		require.NoError(t, err)
		require.Negative(t, ttl)
	}
}

func TestWorkerDrainsMoreThanOneBatchWithoutOneSecondSleeps(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&models.VirtualKey{}, &Ledger{}))
	for i := 0; i < 210; i++ {
		id := fmt.Sprint(i)
		require.NoError(t, s.Reserve(ctx, Reservation{ID: id, KeyID: 1, Amount: 1, Limit: 1000}))
		require.NoError(t, s.Settle(ctx, id, Settlement{Actual: 1, Source: "provider"}))
	}
	done := make(chan struct{})
	go func() { defer close(done); (&Worker{Store: s, DB: db}).Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	require.Eventually(t, func() bool { ids, err := s.Pending(ctx, 1); return err == nil && len(ids) == 0 }, 950*time.Millisecond, 10*time.Millisecond)
}

func TestSQLFailureRetainsOutbox(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	require.NoError(t, s.Reserve(ctx, Reservation{ID: "sql-down", KeyID: 1, Amount: 100, Limit: 100}))
	require.NoError(t, s.Settle(ctx, "sql-down", Settlement{Actual: 50, Source: "provider"}))
	w := &Worker{Store: s}
	require.Error(t, w.Flush(ctx))
	jobs, err := s.Pending(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, []string{"sql-down"}, jobs)
}

func TestDeletedKeyDoesNotLoseHistoricalBill(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "deleted.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.VirtualKey{}, &Ledger{}))
	require.NoError(t, s.Reserve(ctx, Reservation{ID: "deleted", KeyID: 88, Limit: 100, Amount: 100}))
	require.NoError(t, s.Settle(ctx, "deleted", Settlement{Actual: 50, Source: "provider"}))
	w := &Worker{Store: s, DB: db}
	require.NoError(t, w.Flush(ctx))
	var row Ledger
	require.NoError(t, db.First(&row, "reservation_id = ?", "deleted").Error)
	require.Equal(t, int64(50), row.ActualUnits)
}

func TestSweepOnlyExpiresLegacyHashesWithDurableLedger(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "sweep.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.VirtualKey{}, &Ledger{}))
	w := &Worker{Store: s, DB: db}
	for _, id := range []string{"legacy", "not-durable"} {
		require.NoError(t, s.Reserve(ctx, Reservation{ID: id, KeyID: 1, Amount: 10, Limit: 100}))
		require.NoError(t, s.Settle(ctx, id, Settlement{Actual: 5, Source: "provider"}))
		require.NoError(t, s.client.ZRem(ctx, s.prefix+"pending", id).Err())
	}
	job, err := s.Get(ctx, "legacy")
	require.NoError(t, err)
	require.NoError(t, w.persist(ctx, job))
	var cursor uint64
	for {
		cursor, err = w.Sweep(ctx, cursor)
		require.NoError(t, err)
		if cursor == 0 {
			break
		}
	}
	ttl, err := s.client.PTTL(ctx, s.prefix+"reservation:legacy").Result()
	require.NoError(t, err)
	require.Positive(t, ttl)
	ttl, err = s.client.PTTL(ctx, s.prefix+"reservation:not-durable").Result()
	require.NoError(t, err)
	require.Negative(t, ttl)
}
