package limits

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func testLimiter(t *testing.T) *Limiter {
	t.Helper()
	addr := os.Getenv("LLM_GATEWAY_TEST_REDIS")
	if addr == "" {
		t.Skip("requires Redis")
	}
	r := redis.NewClient(&redis.Options{Addr: addr})
	prefix := "limits-test:" + uuid.NewString() + ":"
	t.Cleanup(func() {
		keys, _ := r.Keys(context.Background(), prefix+"*").Result()
		if len(keys) > 0 {
			r.Del(context.Background(), keys...)
		}
		r.Close()
	})
	return &Limiter{Client: r, Prefix: prefix, LeaseTTL: time.Second}
}
func TestKeyLimitsAreAtomicAcrossInstances(t *testing.T) {
	l := testLimiter(t)
	ctx := context.Background()
	other := *l
	var wg sync.WaitGroup
	var accepted atomic.Int64
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lim := l
			if i%2 == 0 {
				lim = &other
			}
			_, err := lim.Acquire(ctx, 1, fmt.Sprint(i), Policy{Requests: 10, WindowSeconds: 60}, 0, false)
			if err == nil {
				accepted.Add(1)
			} else {
				require.ErrorIs(t, err, ErrLimited)
			}
		}(i)
	}
	wg.Wait()
	require.Equal(t, int64(10), accepted.Load())
}
func TestTokenBudgetAndStreamLeaseRecoverSeparately(t *testing.T) {
	l := testLimiter(t)
	ctx := context.Background()
	p := Policy{Tokens: 100, Streams: 1}
	a, err := l.Acquire(ctx, 1, "a", p, 80, true)
	require.NoError(t, err)
	_, err = l.Acquire(ctx, 1, "b", p, 10, true)
	require.ErrorIs(t, err, ErrLimited)
	require.NoError(t, a.Finish(ctx, 20, true))
	require.NoError(t, a.Finish(ctx, 20, true))
	b, err := l.Acquire(ctx, 1, "b", p, 80, true)
	require.NoError(t, err)
	require.NoError(t, b.Finish(ctx, 0, false))
	_, err = l.Acquire(ctx, 1, "c", p, 1, true)
	require.ErrorIs(t, err, ErrLimited, "unknown usage must not refund token reservation")
	p.Tokens = 0
	l.LeaseTTL = 80 * time.Millisecond
	_, err = l.Acquire(ctx, 2, "dead", p, 0, true)
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, err = l.Acquire(ctx, 2, "new", p, 0, true); return err == nil }, time.Second, 20*time.Millisecond)
}
