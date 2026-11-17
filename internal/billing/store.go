// Package billing implements opt-in Redis budget reservations and an outbox.
package billing

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrInsufficient = errors.New("insufficient available budget")
var ErrMissing = errors.New("missing reservation")

// NanoUSD is the ledger unit. Lua numbers represent integers exactly only up to
// 2^53-1; reject values outside this range before calling Redis.
const NanoUSD int64 = 1_000_000_000
const MaxUnits int64 = 1<<53 - 1

//go:embed reserve.lua
var reserveLua string

//go:embed finish.lua
var finishLua string
var reserveScript = redis.NewScript(reserveLua)
var finishScript = redis.NewScript(finishLua)

type Reservation struct {
	ID           string `json:"id"`
	KeyID        uint   `json:"key_id"`
	TenantID     uint   `json:"tenant_id"`
	Limit        int64  `json:"limit"`
	InitialSpent int64  `json:"initial_spent"`
	Amount       int64  `json:"amount"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	InputRate    int64  `json:"input_rate"`
	OutputRate   int64  `json:"output_rate"`
}
type Settlement struct {
	Actual       int64  `json:"actual"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	Source       string `json:"source"`
}
type Job struct {
	Reservation        Reservation `json:"reservation"`
	Settlement         Settlement  `json:"settlement"`
	State              string      `json:"state"`
	Resolution         *Resolution `json:"resolution,omitempty"`
	ReportedSettlement *Settlement `json:"reported_settlement,omitempty"`
	UnresolvedReason   string      `json:"unresolved_reason,omitempty"`
	CreatedMillis      int64       `json:"created_millis"`
}
type Store struct {
	client     *redis.Client
	prefix     string
	StaleAfter time.Duration
}

func NewStore(client *redis.Client, prefix string) *Store {
	return &Store{client: client, prefix: prefix}
}
func (s *Store) accountKey(keyID uint) string { return fmt.Sprintf("%saccount:%d", s.prefix, keyID) }
func (s *Store) Reserve(ctx context.Context, r Reservation) error {
	if r.ID == "" || len(r.ID) > 128 || r.KeyID == 0 {
		return errors.New("invalid reservation identity")
	}
	for _, n := range []int64{r.Amount, r.Limit, r.InitialSpent, r.InputRate, r.OutputRate} {
		if n < 0 || n > MaxUnits {
			return errors.New("budget value out of range")
		}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	code, err := reserveScript.Run(ctx, s.client, []string{s.accountKey(r.KeyID), s.prefix + "reservation:" + r.ID, s.prefix + "accounts", s.prefix + "active"}, r.KeyID, r.Limit, r.InitialSpent, r.Amount, string(b), time.Now().UnixMilli(), r.ID, MaxUnits).Int()
	if err != nil {
		return err
	}
	if code == 0 {
		return ErrInsufficient
	}
	if code != 1 {
		return fmt.Errorf("reservation rejected: state code %d", code)
	}
	return nil
}
func (s *Store) Settle(ctx context.Context, id string, v Settlement) error {
	if v.Actual < 0 || v.Actual > MaxUnits || v.InputTokens < 0 || v.OutputTokens < 0 || v.Source != "provider" {
		return errors.New("invalid authoritative settlement")
	}
	return s.finish(ctx, id, "settled", v)
}
func (s *Store) finish(ctx context.Context, id, state string, v Settlement) error {
	job, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	code, err := finishScript.Run(ctx, s.client, []string{s.accountKey(job.Reservation.KeyID), s.prefix + "reservation:" + id, s.prefix + "pending", s.prefix + "active"}, state, v.Actual, string(b), time.Now().UnixMilli(), id, MaxUnits).Int()
	if err != nil {
		return err
	}
	if code != 1 {
		return fmt.Errorf("settlement rejected: state code %d", code)
	}
	return nil
}
func (s *Store) Unknown(ctx context.Context, id string) error {
	_, err := s.client.Eval(ctx, `local state=redis.call('HGET',KEYS[1],'state'); if not state then return redis.error_reply('missing reservation') end; if state=='reserved' then redis.call('HSET',KEYS[1],'state','unknown') end; return 1`, []string{s.prefix + "reservation:" + id}).Result()
	return err
}
func (s *Store) Release(ctx context.Context, id string) error {
	return s.finish(ctx, id, "released", Settlement{Source: "not_dispatched"})
}
func (s *Store) Pending(ctx context.Context, n int64) ([]string, error) {
	return s.client.ZRange(ctx, s.prefix+"pending", 0, n-1).Result()
}
func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	fields, err := s.client.HGetAll(ctx, s.prefix+"reservation:"+id).Result()
	if err != nil {
		return Job{}, err
	}
	job := Job{State: fields["state"]}
	if job.State == "" {
		return job, ErrMissing
	}
	if err = json.Unmarshal([]byte(fields["reservation"]), &job.Reservation); err != nil {
		return job, err
	}
	if v := fields["settlement"]; v != "" {
		err = json.Unmarshal([]byte(v), &job.Settlement)
	}
	if err == nil && fields["resolution"] != "" {
		err = json.Unmarshal([]byte(fields["resolution"]), &job.Resolution)
	}
	if err == nil && fields["reported_settlement"] != "" {
		err = json.Unmarshal([]byte(fields["reported_settlement"]), &job.ReportedSettlement)
	}
	job.UnresolvedReason = fields["unresolved_reason"]
	_, _ = fmt.Sscan(fields["created_millis"], &job.CreatedMillis)
	return job, err
}
