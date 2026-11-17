package billing

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// A pending score is its next eligible delivery time. Claiming moves it to a
// lease deadline; a dead worker cannot lose the task. Ownership fences old ACKs.
var claimScript = redis.NewScript(`
local now=redis.call('TIME'); now=now[1]*1000+math.floor(now[2]/1000)
local t=redis.call('TYPE',KEYS[2]).ok
if t~='none' and t~='hash' then return redis.error_reply('invalid delivery registry') end
local ids=redis.call('ZRANGEBYSCORE',KEYS[1],'-inf',now,'LIMIT',0,ARGV[1])
for _,id in ipairs(ids) do
 redis.call('ZADD',KEYS[1],now+tonumber(ARGV[2]),id)
 redis.call('HSET',KEYS[2],id,ARGV[3])
end
return ids`)

var ackScript = redis.NewScript(`
if redis.call('HGET',KEYS[2],ARGV[1])~=ARGV[2] then return 0 end
local state=redis.call('HGET',KEYS[3],'state')
if state~='settled' and state~='released' then return redis.error_reply('unfinished reservation') end
local now=redis.call('TIME')
if redis.call('HEXISTS',KEYS[3],'persisted_at')==0 then
 redis.call('HSET',KEYS[3],'persisted_at',now[1])
 redis.call('PEXPIRE',KEYS[3],ARGV[3])
end
redis.call('ZREM',KEYS[1],ARGV[1])
redis.call('HDEL',KEYS[2],ARGV[1])
return 1`)

var retryScript = redis.NewScript(`
if redis.call('HGET',KEYS[2],ARGV[1])~=ARGV[2] then return 0 end
local now=redis.call('TIME'); now=now[1]*1000+math.floor(now[2]/1000)
redis.call('ZADD',KEYS[1],now+tonumber(ARGV[3]),ARGV[1])
redis.call('HDEL',KEYS[2],ARGV[1])
return 1`)

func (s *Store) claim(ctx context.Context, batch int, lease time.Duration) ([]string, string, error) {
	token := uuid.NewString()
	ids, err := claimScript.Run(ctx, s.client, []string{s.prefix + "pending", s.prefix + "deliveries"}, batch, lease.Milliseconds(), token).StringSlice()
	return ids, token, err
}
func (s *Store) acknowledge(ctx context.Context, id, token string, retention time.Duration) error {
	n, err := ackScript.Run(ctx, s.client, []string{s.prefix + "pending", s.prefix + "deliveries", s.prefix + "reservation:" + id}, id, token, retention.Milliseconds()).Int()
	if err == nil && n != 1 {
		return errors.New("delivery lease lost")
	}
	return err
}
func (s *Store) retry(ctx context.Context, id, token string, delay time.Duration) error {
	return retryScript.Run(ctx, s.client, []string{s.prefix + "pending", s.prefix + "deliveries"}, id, token, delay.Milliseconds()).Err()
}
