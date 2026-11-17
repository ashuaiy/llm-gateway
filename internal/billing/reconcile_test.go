package billing

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReconcileUnknownWithEvidenceAndFrozenPrices(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	require.NoError(t, s.Reserve(ctx, Reservation{ID: "unknown", KeyID: 1, Limit: 100, Amount: 50, InputRate: 2, OutputRate: 5}))
	require.NoError(t, s.Unknown(ctx, "unknown"))
	r := Resolution{ID: "resolution-1", Actor: "admin:1", Reason: "provider invoice confirmed", Evidence: "invoice/ref-123", Action: "settle", InputTokens: 10, OutputTokens: 2}
	require.NoError(t, s.Reconcile(ctx, "unknown", r))
	require.NoError(t, s.Reconcile(ctx, "unknown", r))
	job, err := s.Get(ctx, "unknown")
	require.NoError(t, err)
	require.Equal(t, int64(30), job.Settlement.Actual)
	r.OutputTokens = 3
	require.Error(t, s.Reconcile(ctx, "unknown", r))
}
func TestReconcileRetainsEvidenceForOverageAndRejectsUnsafeRelease(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	require.NoError(t, s.Reserve(ctx, Reservation{ID: "over", KeyID: 1, Limit: 100, Amount: 50, OutputRate: 10}))
	require.NoError(t, s.Unknown(ctx, "over"))
	r := Resolution{ID: "r", Actor: "admin:1", Reason: "provider invoice confirmed", Evidence: "invoice/ref-456", Action: "settle", OutputTokens: 12}
	require.NoError(t, s.Reconcile(ctx, "over", r))
	require.ErrorIs(t, s.Reserve(ctx, Reservation{ID: "next", KeyID: 1, Limit: 100, Amount: 1}), ErrInsufficient)
	require.NoError(t, s.Reserve(ctx, Reservation{ID: "unbilled", KeyID: 2, Limit: 100, Amount: 50}))
	r.Action = "release"
	r.InputTokens = 0
	r.OutputTokens = 0
	require.Error(t, s.Reconcile(ctx, "unbilled", r), "in-flight reservations cannot be released")
	require.NoError(t, s.Unknown(ctx, "unbilled"))
	r.Reason = "confirmed_not_billed"
	r.Evidence = ""
	require.Error(t, s.Reconcile(ctx, "unbilled", r))
	r.Evidence = "provider cancellation receipt"
	require.NoError(t, s.Reconcile(ctx, "unbilled", r))
	job, err := s.Get(ctx, "unbilled")
	require.NoError(t, err)
	require.Equal(t, "released", job.State)
}
