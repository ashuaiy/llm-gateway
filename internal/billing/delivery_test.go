package billing

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDeliveryClaimsPartitionAndRecoverExpiredOwner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		id := fmt.Sprint(i)
		require.NoError(t, s.Reserve(ctx, Reservation{ID: id, KeyID: 1, Amount: 1, Limit: 100}))
		require.NoError(t, s.Settle(ctx, id, Settlement{Actual: 1, Source: "provider"}))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids, _, err := s.claim(ctx, 10, time.Minute)
			require.NoError(t, err)
			mu.Lock()
			defer mu.Unlock()
			for _, id := range ids {
				require.False(t, seen[id])
				seen[id] = true
			}
		}()
	}
	wg.Wait()
	require.Len(t, seen, 30)
	// Expire a lease deterministically, then fence the previous owner.
	id := "0"
	oldToken, err := s.client.HGet(ctx, s.prefix+"deliveries", id).Result()
	require.NoError(t, err)
	require.NoError(t, s.client.ZAdd(ctx, s.prefix+"pending", redisZ(id)).Err())
	ids, newToken, err := s.claim(ctx, 1, time.Minute)
	require.NoError(t, err)
	require.Equal(t, []string{id}, ids)
	require.Error(t, s.acknowledge(ctx, id, oldToken, time.Hour))
	require.NoError(t, s.acknowledge(ctx, id, newToken, time.Hour))
	require.NoError(t, s.retry(ctx, id, oldToken, time.Second))
	_, err = s.client.ZScore(ctx, s.prefix+"pending", id).Result()
	require.Error(t, err)
}
