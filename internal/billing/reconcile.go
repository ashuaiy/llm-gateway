package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Resolution records the administrator's evidence, never a prompt or API key.
type Resolution struct {
	ID           string `json:"resolution_id"`
	Actor        string `json:"actor"`
	Reason       string `json:"reason"`
	Evidence     string `json:"evidence"`
	Action       string `json:"action"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

var reconcileScript = redis.NewScript(`
local expected={'hash','hash','zset','zset'}
for i=1,4 do local t=redis.call('TYPE',KEYS[i]).ok; if t~='none' and t~=expected[i] then return -3 end end
local state=redis.call('HGET',KEYS[2],'state')
if state=='settled' or state=='released' then
 if redis.call('HGET',KEYS[2],'resolution')==ARGV[3] then return 1 end
 return -1
end
if state~='unknown' and state~='reserved' then return -2 end
local now=redis.call('TIME'); now=now[1]*1000+math.floor(now[2]/1000)
if state=='reserved' then
 local created=tonumber(redis.call('ZSCORE',KEYS[4],ARGV[1]))
 if not created or now-created<tonumber(ARGV[6]) then return -4 end
end
local amount=tonumber(redis.call('HGET',KEYS[2],'amount'))
local held=tonumber(redis.call('HGET',KEYS[1],'held'))
local spent=tonumber(redis.call('HGET',KEYS[1],'spent'))
local actual=tonumber(ARGV[4]);local max=tonumber(ARGV[7])
if not amount or not held or not spent or held<amount or spent<0 or actual>max-spent then return -3 end
redis.call('HSET',KEYS[1],'held',string.format('%.0f',held-amount),'spent',string.format('%.0f',spent+actual))
redis.call('HSET',KEYS[2],'state',ARGV[2],'settlement',ARGV[5],'resolution',ARGV[3])
redis.call('ZREM',KEYS[4],ARGV[1]);redis.call('ZADD',KEYS[3],now,ARGV[1])
return 1`)

func (s *Store) Reconcile(ctx context.Context, id string, r Resolution) error {
	if strings.TrimSpace(r.ID) == "" || len(r.ID) > 128 || strings.TrimSpace(r.Actor) == "" || len(r.Actor) > 128 || strings.TrimSpace(r.Reason) == "" || len(r.Reason) > 1000 || strings.TrimSpace(r.Evidence) == "" || len(r.Evidence) > 2000 {
		return errors.New("resolution identity, actor, reason and evidence required")
	}
	if r.InputTokens < 0 || r.OutputTokens < 0 {
		return errors.New("negative usage")
	}
	job, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	actual, err := tokenCost(r.InputTokens, r.OutputTokens, job.Reservation.InputRate, job.Reservation.OutputRate)
	if err != nil {
		return err
	}
	state := "settled"
	switch r.Action {
	case "settle":
	case "release":
		if r.Reason != "confirmed_not_billed" || r.InputTokens != 0 || r.OutputTokens != 0 {
			return errors.New("release requires confirmed_not_billed evidence and zero usage")
		}
		state = "released"
	default:
		return errors.New("invalid resolution action")
	}
	audit, _ := json.Marshal(r)
	settlement, _ := json.Marshal(Settlement{Actual: actual, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, Source: "reconciled"})
	stale := s.StaleAfter
	if stale <= 0 {
		stale = 10 * time.Minute
	}
	n, err := reconcileScript.Run(ctx, s.client, []string{s.accountKey(job.Reservation.KeyID), s.prefix + "reservation:" + id, s.prefix + "pending", s.prefix + "active"}, id, state, string(audit), actual, string(settlement), stale.Milliseconds(), MaxUnits).Int()
	if err == nil && n != 1 {
		return errors.New("resolution conflicts with reservation state")
	}
	return err
}

func (s *Store) Unresolved(ctx context.Context, offset, count int64) ([]Job, error) {
	if count < 1 || count > 100 || offset < 0 {
		return nil, errors.New("invalid pagination")
	}
	ids, err := s.client.ZRange(ctx, s.prefix+"active", offset, offset+count-1).Result()
	if err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(ids))
	for _, id := range ids {
		job, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if job.State == "reserved" || job.State == "unknown" {
			jobs = append(jobs, job)
		}
	}
	return jobs, nil
}
