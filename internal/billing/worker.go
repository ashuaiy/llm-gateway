package billing

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"github.com/warm3snow/llm-gateway/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Ledger is the durable, exact ledger. The legacy budget_used field is a view.
type Ledger struct {
	ReservationID          string    `gorm:"primaryKey;size:128" json:"reservation_id"`
	VirtualKeyID           uint      `gorm:"index" json:"virtual_key_id"`
	TenantID               uint      `gorm:"index" json:"tenant_id"`
	Provider               string    `json:"provider"`
	Model                  string    `json:"model"`
	ReservedUnits          int64     `json:"reserved_units"`
	ActualUnits            int64     `json:"actual_units"`
	InputRate              int64     `json:"input_rate"`
	OutputRate             int64     `json:"output_rate"`
	InputTokens            int       `json:"input_tokens"`
	OutputTokens           int       `json:"output_tokens"`
	State                  string    `json:"state"`
	Source                 string    `json:"source"`
	CreatedAt              time.Time `json:"created_at"`
	ResolutionJSON         string    `gorm:"type:text" json:"resolution_json,omitempty"`
	ReportedSettlementJSON string    `gorm:"type:text" json:"reported_settlement_json,omitempty"`
	UnresolvedReason       string    `json:"unresolved_reason,omitempty"`
	RequestCreatedMillis   int64     `json:"request_created_millis"`
}

func (Ledger) TableName() string { return "budget_ledgers" }

type Worker struct {
	Store       *Store
	DB          *gorm.DB
	Retention   time.Duration
	Lease       time.Duration
	BatchSize   int
	Concurrency int
}

func redisZ(id string) redis.Z { return redis.Z{Score: float64(time.Now().UnixMilli()), Member: id} }
func (w *Worker) Flush(ctx context.Context) error {
	_, err := w.flush(ctx)
	return err
}
func (w *Worker) flush(ctx context.Context) (int, error) {
	if w.DB == nil {
		return 0, errors.New("billing database unavailable")
	}
	batch, lease, retention := w.options()
	ids, token, err := w.Store.claim(ctx, batch, lease)
	if err != nil {
		return 0, err
	}
	var failures []error
	for _, id := range ids {
		job, err := w.Store.Get(ctx, id)
		if err == nil {
			err = w.persist(ctx, job)
		}
		if err == nil {
			err = w.Store.acknowledge(ctx, id, token, retention)
		}
		if err != nil {
			failures = append(failures, err)
			// Back off poison tasks. If ctx expired, the lease itself makes them
			// eligible again. Never pop a task before its SQL transaction commits.
			_ = w.Store.retry(ctx, id, token, time.Second)
		}
	}
	return len(ids), errors.Join(failures...)
}
func (w *Worker) options() (int, time.Duration, time.Duration) {
	batch := w.BatchSize
	if batch <= 0 {
		batch = 100
	}
	if batch > 1000 {
		batch = 1000
	}
	lease := w.Lease
	if lease <= 0 {
		lease = 30 * time.Second
	}
	retention := w.Retention
	if retention <= 0 {
		retention = 24 * time.Hour
	}
	return batch, lease, retention
}
func (w *Worker) persist(ctx context.Context, job Job) error {
	if job.State != "settled" && job.State != "released" {
		return errors.New("outbox contains unfinished reservation")
	}
	r, v := job.Reservation, job.Settlement
	row := Ledger{ReservationID: r.ID, VirtualKeyID: r.KeyID, TenantID: r.TenantID, Provider: r.Provider, Model: r.Model, ReservedUnits: r.Amount, ActualUnits: v.Actual, InputRate: r.InputRate, OutputRate: r.OutputRate, InputTokens: v.InputTokens, OutputTokens: v.OutputTokens, State: job.State, Source: v.Source}
	if job.Resolution != nil {
		b, err := json.Marshal(job.Resolution)
		if err != nil {
			return err
		}
		row.ResolutionJSON = string(b)
	}
	row.RequestCreatedMillis = job.CreatedMillis
	row.UnresolvedReason = job.UnresolvedReason
	if job.ReportedSettlement != nil {
		b, err := json.Marshal(job.ReportedSettlement)
		if err != nil {
			return err
		}
		row.ReportedSettlementJSON = string(b)
	}
	return w.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var result *gorm.DB
		if tx.Dialector.Name() == "mysql" {
			// MySQL ON DUPLICATE KEY UPDATE can report RowsAffected=1 for a
			// no-op with clientFoundRows. A plain INSERT has an unambiguous gate.
			result = tx.Create(&row)
			var duplicate *mysqlDriver.MySQLError
			if errors.As(result.Error, &duplicate) && duplicate.Number == 1062 {
				return nil
			}
		} else {
			result = tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		}
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		result = tx.Model(&models.VirtualKey{}).Where("id = ?", r.KeyID).UpdateColumn("budget_used", gorm.Expr("budget_used + ?", float64(v.Actual)/float64(NanoUSD)))
		if result.Error != nil {
			return result.Error
		}
		// A deleted virtual key has no projection to update. Its historical bill
		// must still be durable, otherwise the outbox can never be acknowledged.
		return nil
	})
}

// Run owns no request context. Cancelling an HTTP request never cancels billing.
func (w *Worker) Run(ctx context.Context) {
	n := w.Concurrency
	if n <= 0 {
		n = 4
	}
	if n > 32 {
		n = 32
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); w.maintain(ctx) }()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); w.run(ctx) }()
	}
	wg.Wait()
}
func (w *Worker) run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		n, err := w.flush(attempt)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Error("budget_outbox_retry", "error", err)
		}
		if err == nil && n > 0 {
			continue
		}
		delay := 100 * time.Millisecond
		if err != nil {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
