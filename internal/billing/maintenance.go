package billing

import (
	"context"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
)

var pendingGauge = promauto.NewGauge(prometheus.GaugeOpts{Name: "llmgw_billing_pending", Help: "Durable billing tasks including claimed deliveries."})
var unresolvedGauge = promauto.NewGauge(prometheus.GaugeOpts{Name: "llmgw_billing_unresolved", Help: "Reservations still holding budget."})
var unresolvedAge = promauto.NewGauge(prometheus.GaugeOpts{Name: "llmgw_billing_oldest_unresolved_seconds", Help: "Age of the oldest budget hold."})
var sweepScript = redis.NewScript(`
local state=redis.call('HGET',KEYS[1],'state')
if state~='settled' and state~='released' then return 0 end
if redis.call('ZSCORE',KEYS[2],ARGV[1]) or redis.call('PTTL',KEYS[1])>=0 then return 0 end
local now=redis.call('TIME');redis.call('HSET',KEYS[1],'persisted_at',now[1]);redis.call('PEXPIRE',KEYS[1],ARGV[2]);return 1`)

// Sweep upgrades terminal hashes acknowledged by the pre-retention worker.
// SCAN is incremental. A SQL ledger is mandatory before expiring any old hash.
func (w *Worker) Sweep(ctx context.Context, cursor uint64) (uint64, error) {
	keys, next, err := w.Store.client.Scan(ctx, cursor, w.Store.prefix+"reservation:*", 100).Result()
	if err != nil {
		return cursor, err
	}
	_, _, retention := w.options()
	for _, key := range keys {
		ttl, err := w.Store.client.PTTL(ctx, key).Result()
		if err != nil {
			return cursor, err
		}
		if ttl >= 0 {
			continue
		}
		id := strings.TrimPrefix(key, w.Store.prefix+"reservation:")
		job, err := w.Store.Get(ctx, id)
		if err != nil {
			continue
		}
		if job.State != "settled" && job.State != "released" {
			continue
		}
		var count int64
		if err = w.DB.WithContext(ctx).Model(&Ledger{}).Where("reservation_id = ?", id).Count(&count).Error; err != nil {
			return cursor, err
		}
		if count == 0 {
			continue
		}
		if err = sweepScript.Run(ctx, w.Store.client, []string{key, w.Store.prefix + "pending"}, id, retention.Milliseconds()).Err(); err != nil {
			return cursor, err
		}
	}
	return next, nil
}
func (w *Worker) maintain(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var cursor uint64
	for {
		if ctx.Err() != nil {
			return
		}
		attempt, cancel := context.WithTimeout(ctx, 3*time.Second)
		if w.DB != nil {
			next, err := w.Sweep(attempt, cursor)
			if err == nil {
				cursor = next
			}
		}
		if n, err := w.Store.client.ZCard(attempt, w.Store.prefix+"pending").Result(); err == nil {
			pendingGauge.Set(float64(n))
		}
		if n, err := w.Store.client.ZCard(attempt, w.Store.prefix+"active").Result(); err == nil {
			unresolvedGauge.Set(float64(n))
		}
		oldest, err := w.Store.client.ZRangeWithScores(attempt, w.Store.prefix+"active", 0, 0).Result()
		if err == nil {
			age := float64(0)
			if len(oldest) > 0 {
				now, e := w.Store.client.Time(attempt).Result()
				if e == nil {
					age = float64(now.UnixMilli())/1000 - oldest[0].Score/1000
				}
			}
			if age < 0 {
				age = 0
			}
			unresolvedAge.Set(age)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
