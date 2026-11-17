package billing

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	addr := os.Getenv("LLM_GATEWAY_TEST_REDIS")
	if addr == "" {
		t.Skip("set LLM_GATEWAY_TEST_REDIS to run real Redis billing tests")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	require.NoError(t, rdb.Ping(context.Background()).Err())
	prefix := "test-budget:{" + uuid.NewString() + "}:"
	t.Cleanup(func() {
		keys, _ := rdb.Keys(context.Background(), prefix+"*").Result()
		if len(keys) > 0 {
			_ = rdb.Del(context.Background(), keys...).Err()
		}
		_ = rdb.Close()
	})
	return NewStore(rdb, prefix)
}

func TestReserveAcrossInstancesNeverOversubscribes(t *testing.T) {
	s := testStore(t)
	other := NewStore(s.client, s.prefix)
	var accepted atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			store := s
			if i%2 == 0 {
				store = other
			}
			err := store.Reserve(context.Background(), Reservation{ID: fmt.Sprint(i), KeyID: 1, Limit: 1000, Amount: 100})
			if err == nil {
				accepted.Add(1)
			} else if err != ErrInsufficient {
				t.Errorf("unexpected reserve error: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	require.Equal(t, int64(10), accepted.Load())
}

func TestSettlementIdempotencyAndUnknownHold(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := Reservation{ID: "first", KeyID: 1, Limit: 1000, Amount: 800}
	require.NoError(t, s.Reserve(ctx, r))
	require.NoError(t, s.Reserve(ctx, r))
	require.NoError(t, s.Unknown(ctx, r.ID))
	require.ErrorIs(t, s.Reserve(ctx, Reservation{ID: "second", KeyID: 1, Limit: 1000, Amount: 300}), ErrInsufficient)
	require.NoError(t, s.Settle(ctx, r.ID, Settlement{Actual: 300, InputTokens: 10, OutputTokens: 20, Source: "provider"}))
	require.NoError(t, s.Settle(ctx, r.ID, Settlement{Actual: 300, InputTokens: 10, OutputTokens: 20, Source: "provider"}))
	require.Error(t, s.Settle(ctx, r.ID, Settlement{Actual: 100, Source: "provider"}))
	require.NoError(t, s.Reserve(ctx, Reservation{ID: "third", KeyID: 1, Limit: 1000, Amount: 700}))
	require.ErrorIs(t, s.Reserve(ctx, Reservation{ID: "fourth", KeyID: 1, Limit: 1000, Amount: 1}), ErrInsufficient)
	jobs, err := s.Pending(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, []string{"first"}, jobs)
	job, err := s.Get(ctx, "first")
	require.NoError(t, err)
	require.Equal(t, int64(300), job.Settlement.Actual)
}

func TestMissingAccountAndOutboxFailureDoNotReleaseMoney(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	require.NoError(t, s.Reserve(ctx, Reservation{ID: "a", KeyID: 1, Limit: 1000, Amount: 800}))
	require.NoError(t, s.client.Set(ctx, s.prefix+"pending", "wrong-type", 0).Err())
	require.Error(t, s.Settle(ctx, "a", Settlement{Actual: 200, Source: "provider"}))
	job, err := s.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, "reserved", job.State)
	require.NoError(t, s.client.Del(ctx, s.accountKey(1)).Err())
	require.Error(t, s.Reserve(ctx, Reservation{ID: "b", KeyID: 1, Limit: 1000, Amount: 100}))
}

func TestBoundsAndRelease(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	require.Error(t, s.Reserve(ctx, Reservation{ID: "bad", KeyID: 1, Limit: 100, Amount: -1}))
	require.NoError(t, s.Reserve(ctx, Reservation{ID: "a", KeyID: 1, Limit: 100, Amount: 80}))
	require.Error(t, s.Settle(ctx, "a", Settlement{Actual: 81, Source: "provider"}))
	require.Error(t, s.Release(ctx, "a"))
	fields, err := s.client.HGetAll(ctx, s.prefix+"reservation:a").Result()
	require.NoError(t, err)
	require.Equal(t, "actual_exceeds_reserved", fields["unresolved_reason"])
	require.Contains(t, fields["reported_settlement"], `"actual":81`)
	require.NoError(t, s.Reserve(ctx, Reservation{ID: "releasable", KeyID: 2, Limit: 100, Amount: 80}))
	require.NoError(t, s.Release(ctx, "releasable"))
	require.NoError(t, s.Release(ctx, "releasable"))
	require.NoError(t, s.Reserve(ctx, Reservation{ID: "b", KeyID: 2, Limit: 100, Amount: 100}))
}
