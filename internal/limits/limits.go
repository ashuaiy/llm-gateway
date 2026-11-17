// Package limits implements distributed virtual-key admission, separate from money.
package limits

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrLimited = errors.New("key rate limit exceeded")

type Policy struct {
	Requests      int
	WindowSeconds int
	Tokens        int
	Streams       int
}
type Limiter struct {
	Client   *redis.Client
	Prefix   string
	LeaseTTL time.Duration
}
type Lease struct {
	limiter *Limiter
	key     uint
	id      string
}

var acquireScript = redis.NewScript(`
local expected={'hash','zset','hash'}
for i=1,3 do local t=redis.call('TYPE',KEYS[i]).ok;if t~='none' and t~=expected[i] then return -2 end end
if redis.call('EXISTS',KEYS[3])==1 then return -2 end
local ts=redis.call('TIME');local now=ts[1]*1000+math.floor(ts[2]/1000)
local rw=math.floor(now/(tonumber(ARGV[3])*1000));local tw=math.floor(now/60000)
local requests=tonumber(redis.call('HGET',KEYS[1],'requests') or '0')
local tokens=tonumber(redis.call('HGET',KEYS[1],'tokens') or '0')
if tonumber(redis.call('HGET',KEYS[1],'rw') or '-1')~=rw then requests=0 end
if tonumber(redis.call('HGET',KEYS[1],'tw') or '-1')~=tw then tokens=0 end
redis.call('ZREMRANGEBYSCORE',KEYS[2],'-inf',now)
if tonumber(ARGV[2])>0 and requests>=tonumber(ARGV[2]) then return 0 end
if tonumber(ARGV[4])>0 and tokens+tonumber(ARGV[6])>tonumber(ARGV[4]) then return 0 end
if ARGV[7]=='1' and tonumber(ARGV[5])>0 and redis.call('ZCARD',KEYS[2])>=tonumber(ARGV[5]) then return 0 end
redis.call('HSET',KEYS[1],'rw',rw,'tw',tw,'requests',requests+1,'tokens',tokens+tonumber(ARGV[6]))
redis.call('PEXPIRE',KEYS[1],math.max(120000,tonumber(ARGV[3])*2000))
if ARGV[7]=='1' then redis.call('ZADD',KEYS[2],now+tonumber(ARGV[8]),ARGV[1]);redis.call('PEXPIRE',KEYS[2],tonumber(ARGV[8])*2) end
redis.call('HSET',KEYS[3],'state','active','tokens',ARGV[6],'tw',tw,'stream',ARGV[7])
redis.call('PEXPIRE',KEYS[3],math.max(600000,tonumber(ARGV[8])*4))
return 1`)
var finishScript = redis.NewScript(`
local expected={'hash','zset','hash'}
for i=1,3 do local t=redis.call('TYPE',KEYS[i]).ok;if t~='none' and t~=expected[i] then return -2 end end
local state=redis.call('HGET',KEYS[3],'state')
if state=='finished' then return 1 end
if state~='active' then return -1 end
if ARGV[3]=='1' and redis.call('HGET',KEYS[1],'tw')==redis.call('HGET',KEYS[3],'tw') then
 local tokens=tonumber(redis.call('HGET',KEYS[1],'tokens') or '0')
 local reserved=tonumber(redis.call('HGET',KEYS[3],'tokens'))
 redis.call('HSET',KEYS[1],'tokens',math.max(0,tokens-reserved+tonumber(ARGV[2])))
end
redis.call('ZREM',KEYS[2],ARGV[1]);redis.call('HSET',KEYS[3],'state','finished')
return 1`)
var renewScript = redis.NewScript(`
local ts=redis.call('TIME');local now=ts[1]*1000+math.floor(ts[2]/1000)
if redis.call('HGET',KEYS[3],'state')~='active' then return 0 end
if redis.call('HGET',KEYS[3],'stream')=='1' then
 local deadline=tonumber(redis.call('ZSCORE',KEYS[2],ARGV[1]) or '0')
 if deadline<=now then return 0 end
 redis.call('ZADD',KEYS[2],now+tonumber(ARGV[2]),ARGV[1]);redis.call('PEXPIRE',KEYS[2],tonumber(ARGV[2])*2)
end
redis.call('PEXPIRE',KEYS[3],math.max(600000,tonumber(ARGV[2])*4));return 1`)

func (l *Limiter) ttl() time.Duration {
	if l.LeaseTTL <= 0 {
		return 30 * time.Second
	}
	return l.LeaseTTL
}
func (l *Limiter) keys(key uint, id string) []string {
	base := fmt.Sprintf("%s{%d}:", l.Prefix, key)
	return []string{base + "quota", base + "streams", base + "request:" + id}
}
func (l *Limiter) Acquire(ctx context.Context, key uint, id string, p Policy, tokens int, stream bool) (*Lease, error) {
	if key == 0 || id == "" || len(id) > 128 || tokens < 0 || tokens > 1_000_000_000 || p.Requests < 0 || p.Tokens < 0 || p.Streams < 0 || p.Requests > 1_000_000_000 || p.Tokens > 1_000_000_000 || p.Streams > 1_000_000 || p.WindowSeconds < 0 || p.WindowSeconds > 86400 {
		return nil, errors.New("invalid key limit policy")
	}
	if p.WindowSeconds == 0 {
		p.WindowSeconds = 60
	}
	isStream := 0
	if stream {
		isStream = 1
	}
	n, err := acquireScript.Run(ctx, l.Client, l.keys(key, id), id, p.Requests, p.WindowSeconds, p.Tokens, p.Streams, tokens, isStream, l.ttl().Milliseconds()).Int()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrLimited
	}
	if n != 1 {
		return nil, errors.New("invalid limit state")
	}
	return &Lease{limiter: l, key: key, id: id}, nil
}
func (l *Lease) Finish(ctx context.Context, actual int, authoritative bool) error {
	if actual < 0 || actual > 1_000_000_000 {
		return errors.New("invalid usage")
	}
	auth := 0
	if authoritative {
		auth = 1
	}
	n, err := finishScript.Run(ctx, l.limiter.Client, l.limiter.keys(l.key, l.id), l.id, actual, auth).Int()
	if err == nil && n != 1 {
		return errors.New("limit receipt missing")
	}
	return err
}
func (l *Lease) Renew(ctx context.Context) error {
	n, err := renewScript.Run(ctx, l.limiter.Client, l.limiter.keys(l.key, l.id), l.id, l.limiter.ttl().Milliseconds()).Int()
	if err == nil && n != 1 {
		return errors.New("stream lease expired")
	}
	return err
}
