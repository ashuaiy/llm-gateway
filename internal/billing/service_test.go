package billing

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/warm3snow/llm-gateway/internal/models"
	"github.com/warm3snow/llm-gateway/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestSessionUsesFrozenPricesAndHoldsUnknownUsage(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "session.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.ModelPricing{}))
	require.NoError(t, db.Create(&models.ModelPricing{Provider: "openai", Model: "test", InputPrice: 0.001, OutputPrice: 0.002, Currency: "USD"}).Error)
	svc := &Service{Store: s, DB: db, DefaultMaxTokens: 32, MaxOutputTokens: 128}
	req := &types.ChatCompletionRequest{Model: "test", Messages: []types.Message{{Role: "user", Content: "hello"}}}
	session, err := svc.Begin(ctx, models.VirtualKey{ID: 1, BudgetTotal: 1}, "openai", req)
	require.NoError(t, err)
	require.Equal(t, 32, req.MaxTokens)
	require.NoError(t, db.Model(&models.ModelPricing{}).Where("model = ?", "test").Update("output_price", 100).Error)
	require.NoError(t, session.Complete(ctx, &types.TokenUsage{PromptTokens: 5, CompletionTokens: 3, Source: "provider"}))
	job, err := s.Get(ctx, session.ID)
	require.NoError(t, err)
	require.Equal(t, int64(110000), job.Settlement.Actual)
	other, err := svc.Begin(ctx, models.VirtualKey{ID: 2, BudgetTotal: 1000}, "openai", req)
	require.NoError(t, err)
	require.NoError(t, other.Complete(ctx, &types.TokenUsage{PromptTokens: 5, CompletionTokens: 3, Source: "estimated"}))
	job, err = s.Get(ctx, other.ID)
	require.NoError(t, err)
	require.Equal(t, "unknown", job.State)
}

func TestSessionRejectsUnsupportedInputsAndMissingPrices(t *testing.T) {
	s := testStore(t)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "invalid.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.ModelPricing{}))
	svc := &Service{Store: s, DB: db, DefaultMaxTokens: 32, MaxOutputTokens: 128}
	for _, req := range []*types.ChatCompletionRequest{
		{Model: "missing", Messages: []types.Message{{Role: "user", Content: "hello"}}},
		{Model: "test", N: 2, Messages: []types.Message{{Role: "user", Content: "hello"}}},
		{Model: "test", MaxTokens: 129, Messages: []types.Message{{Role: "user", Content: "hello"}}},
		{Model: "test", Messages: []types.Message{{Role: "user", Content: []any{map[string]any{"type": "image_url"}}}}},
	} {
		_, err := svc.Begin(context.Background(), models.VirtualKey{ID: 1, BudgetTotal: 1}, "openai", req)
		require.Error(t, err)
	}
}

func TestReservationRejectsUnverifiedAdaptersAndResponseSchema(t *testing.T) {
	s := testStore(t)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "unsupported.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.ModelPricing{}))
	for _, provider := range []string{"openai", "gemini", "cohere"} {
		require.NoError(t, db.Create(&models.ModelPricing{Provider: provider, Model: "test", InputPrice: 0.001, OutputPrice: 0.002, Currency: "USD"}).Error)
	}
	svc := &Service{Store: s, DB: db, DefaultMaxTokens: 32, MaxOutputTokens: 128}
	for _, provider := range []string{"gemini", "cohere"} {
		_, err := svc.Begin(context.Background(), models.VirtualKey{ID: 1, BudgetTotal: 1}, provider, &types.ChatCompletionRequest{Model: "test", Messages: []types.Message{{Role: "user", Content: "hello"}}})
		require.Error(t, err)
	}
	_, err = svc.Begin(context.Background(), models.VirtualKey{ID: 1, BudgetTotal: 1}, "openai", &types.ChatCompletionRequest{Model: "test", Messages: []types.Message{{Role: "user", Content: "hello"}}, ResponseFormat: map[string]any{"type": "json_schema"}})
	require.Error(t, err)
}
