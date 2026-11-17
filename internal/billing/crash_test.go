package billing

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/warm3snow/llm-gateway/internal/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// Executed in a real child process. Parent waits for a precise crash boundary,
// then sends SIGKILL via Process.Kill; no deferred cleanup can run in the child.
func TestBillingCrashHelper(t *testing.T) {
	stage := os.Getenv("LLMGW_CRASH_STAGE")
	if stage == "" {
		t.Skip("subprocess helper")
	}
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: os.Getenv("LLM_GATEWAY_TEST_REDIS")})
	s := NewStore(rdb, os.Getenv("LLMGW_CRASH_PREFIX"))
	var r Reservation
	require.NoError(t, json.Unmarshal([]byte(os.Getenv("LLMGW_CRASH_RESERVATION")), &r))
	require.NoError(t, s.Reserve(ctx, r))
	if stage != "reserved" {
		require.NoError(t, s.Settle(ctx, r.ID, Settlement{Actual: 100, Source: "provider"}))
	}
	if stage == "claimed" || stage == "committed" {
		_, _, err := s.claim(ctx, 1, 150*time.Millisecond)
		require.NoError(t, err)
	}
	if stage == "committed" {
		db, err := gorm.Open(mysql.Open(os.Getenv("LLM_GATEWAY_TEST_MYSQL")), &gorm.Config{})
		require.NoError(t, err)
		job, err := s.Get(ctx, r.ID)
		require.NoError(t, err)
		require.NoError(t, (&Worker{Store: s, DB: db}).persist(ctx, job))
	}
	fmt.Println("CRASH_BOUNDARY_READY")
	<-time.After(time.Hour)
}

func TestKilledWorkerRecoversReservationAndCommittedLedger(t *testing.T) {
	db := mysqlTestDB(t)
	for _, stage := range []string{"reserved", "settled", "claimed", "committed"} {
		t.Run(stage, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			key := models.VirtualKey{Name: "crash-" + stage, KeyHash: uuid.NewString(), BudgetTotal: 1}
			require.NoError(t, db.Create(&key).Error)
			id := uuid.NewString()
			t.Cleanup(func() { db.Delete(&Ledger{}, "reservation_id = ?", id); db.Delete(&models.VirtualKey{}, key.ID) })
			r := Reservation{ID: id, KeyID: key.ID, Amount: 1000, Limit: 1000}
			encoded, _ := json.Marshal(r)
			executable, err := os.Executable()
			require.NoError(t, err)
			cmd := exec.Command(executable, "-test.run=^TestBillingCrashHelper$", "-test.v")
			cmd.Env = append(os.Environ(), "LLMGW_CRASH_STAGE="+stage, "LLMGW_CRASH_PREFIX="+s.prefix, "LLMGW_CRASH_RESERVATION="+string(encoded))
			pipe, err := cmd.StdoutPipe()
			require.NoError(t, err)
			cmd.Stderr = cmd.Stdout
			require.NoError(t, cmd.Start())
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			})
			ready := make(chan bool, 1)
			go func() {
				scanner := bufio.NewScanner(pipe)
				for scanner.Scan() {
					if strings.Contains(scanner.Text(), "CRASH_BOUNDARY_READY") {
						ready <- true
						return
					}
				}
				ready <- false
			}()
			select {
			case ok := <-ready:
				require.True(t, ok, "child failed before crash boundary")
			case <-time.After(15 * time.Second):
				t.Fatal("child startup timed out")
			}
			require.NoError(t, cmd.Process.Kill())
			require.Error(t, cmd.Wait())
			if stage == "reserved" {
				require.ErrorIs(t, s.Reserve(ctx, Reservation{ID: "blocked", KeyID: key.ID, Amount: 1, Limit: 1000}), ErrInsufficient)
				s.StaleAfter = time.Millisecond
				require.NoError(t, s.Reconcile(ctx, id, Resolution{ID: "recovery", Actor: "test-admin", Action: "release", Reason: "confirmed_not_billed", Evidence: "test provider verified request was never dispatched"}))
			}
			w := &Worker{Store: NewStore(s.client, s.prefix), DB: db}
			require.Eventually(t, func() bool {
				if w.Flush(ctx) != nil {
					return false
				}
				ids, err := s.Pending(ctx, 1)
				return err == nil && len(ids) == 0
			}, 5*time.Second, 30*time.Millisecond)
			var count int64
			require.NoError(t, db.Model(&Ledger{}).Where("reservation_id = ?", id).Count(&count).Error)
			require.Equal(t, int64(1), count)
			var row models.VirtualKey
			require.NoError(t, db.First(&row, key.ID).Error)
			expected := float64(100) / float64(NanoUSD)
			if stage == "reserved" {
				expected = 0
			}
			require.InDelta(t, expected, row.BudgetUsed, 1e-10)
		})
	}
}

func TestClosedSQLConnectionRetainsDeliveryAndRecovers(t *testing.T) {
	db := mysqlTestDB(t)
	s := testStore(t)
	ctx := context.Background()
	id := uuid.NewString()
	key := models.VirtualKey{Name: "connection-fault", KeyHash: uuid.NewString()}
	require.NoError(t, db.Create(&key).Error)
	require.NoError(t, s.Reserve(ctx, Reservation{ID: id, KeyID: key.ID, Amount: 100, Limit: 100}))
	require.NoError(t, s.Settle(ctx, id, Settlement{Actual: 50, Source: "provider"}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	w := &Worker{Store: s, DB: db}
	require.Error(t, w.Flush(ctx))
	ids, err := s.Pending(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, []string{id}, ids)
	recovered := mysqlTestDB(t)
	t.Cleanup(func() {
		recovered.Delete(&Ledger{}, "reservation_id = ?", id)
		recovered.Delete(&models.VirtualKey{}, key.ID)
	})
	w.DB = recovered
	require.Eventually(t, func() bool { _ = w.Flush(ctx); ids, _ := s.Pending(ctx, 1); return len(ids) == 0 }, 5*time.Second, 50*time.Millisecond)
}
