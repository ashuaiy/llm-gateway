package billing

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/warm3snow/llm-gateway/internal/models"
	"github.com/warm3snow/llm-gateway/internal/types"
	"gorm.io/gorm"
)

type Service struct {
	Store            *Store
	DB               *gorm.DB
	DefaultMaxTokens int
	MaxOutputTokens  int
}
type Session struct {
	ID          string
	reservation Reservation
	store       *Store
}

func (s *Service) Begin(ctx context.Context, key models.VirtualKey, provider string, req *types.ChatCompletionRequest) (*Session, error) {
	if provider != "openai" && provider != "deterministic" {
		return nil, errors.New("provider adapter has not been verified for authoritative usage")
	}
	if req.N < 0 || req.N > 1 || len(req.Tools) > 0 || req.ToolChoice != nil || len(req.ResponseFormat) > 0 || req.MaxTokens < 0 || len(req.Messages) == 0 {
		return nil, errors.New("reservations support single-choice text chat without tools")
	}
	for _, m := range req.Messages {
		if _, ok := m.Content.(string); !ok {
			return nil, errors.New("reservations require text-only messages")
		}
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = s.DefaultMaxTokens
	}
	if req.MaxTokens <= 0 || req.MaxTokens > s.MaxOutputTokens {
		return nil, errors.New("max_tokens exceeds reservation output limit")
	}
	if s.DB == nil {
		return nil, errors.New("billing database unavailable")
	}
	var price models.ModelPricing
	if err := s.DB.WithContext(ctx).Where("provider = ? AND model = ?", provider, req.Model).First(&price).Error; err != nil {
		return nil, errors.New("exact model pricing required for reservations")
	}
	if strings.ToUpper(price.Currency) != "USD" {
		return nil, errors.New("reservations require USD pricing")
	}
	inputRate, err := scaledUnits(price.InputPrice, NanoUSD/100, true)
	if err != nil {
		return nil, err
	}
	outputRate, err := scaledUnits(price.OutputPrice, NanoUSD/100, true)
	if err != nil {
		return nil, err
	}
	limit, err := scaledUnits(key.BudgetTotal, NanoUSD, false)
	if err != nil {
		return nil, err
	}
	spent, err := scaledUnits(key.BudgetUsed, NanoUSD, true)
	if err != nil {
		return nil, err
	}
	if key.BudgetTotal > 0 && limit == 0 {
		return nil, ErrInsufficient
	}
	data, err := json.Marshal(req.Messages)
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("reservation input exceeds 1 MiB")
	}
	// Conservative byte-based estimate for text chat, including message overhead.
	// It is not a provider-independent hard bound. Exceedances remain unresolved.
	inputBound := len(data) + 256*len(req.Messages) + 256
	amount, err := tokenCost(inputBound, req.MaxTokens, inputRate, outputRate)
	if err != nil {
		return nil, err
	}
	r := Reservation{ID: uuid.NewString(), KeyID: key.ID, TenantID: key.TenantID, Limit: limit, InitialSpent: spent, Amount: amount, Provider: provider, Model: req.Model, InputRate: inputRate, OutputRate: outputRate}
	if err = s.Store.Reserve(ctx, r); err != nil {
		return nil, err
	}
	return &Session{ID: r.ID, reservation: r, store: s.Store}, nil
}
func (s *Session) Complete(ctx context.Context, usage *types.TokenUsage) error {
	if usage == nil || usage.Source != "provider" {
		return s.store.Unknown(ctx, s.ID)
	}
	actual, err := tokenCost(usage.PromptTokens, usage.CompletionTokens, s.reservation.InputRate, s.reservation.OutputRate)
	if err == nil {
		err = s.store.Settle(ctx, s.ID, Settlement{Actual: actual, InputTokens: usage.PromptTokens, OutputTokens: usage.CompletionTokens, Source: "provider"})
	}
	if err != nil {
		_ = s.store.Unknown(ctx, s.ID)
	}
	return err
}
func (s *Session) Release(ctx context.Context) error { return s.store.Release(ctx, s.ID) }
func tokenCost(input, output int, inputRate, outputRate int64) (int64, error) {
	if input < 0 || output < 0 || inputRate < 0 || outputRate < 0 {
		return 0, errors.New("negative token cost")
	}
	if inputRate > 0 && int64(input) > MaxUnits/inputRate {
		return 0, errors.New("input cost out of range")
	}
	a := int64(input) * inputRate
	if outputRate > 0 && int64(output) > (MaxUnits-a)/outputRate {
		return 0, errors.New("output cost out of range")
	}
	return a + int64(output)*outputRate, nil
}
func scaledUnits(value float64, scale int64, ceil bool) (int64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, errors.New("invalid money amount")
	}
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(value, 'f', -1, 64))
	if !ok {
		return 0, errors.New("invalid money amount")
	}
	r.Mul(r, new(big.Rat).SetInt64(scale))
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(r.Num(), r.Denom(), rem)
	if ceil && rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() || q.Int64() > MaxUnits {
		return 0, errors.New("money amount out of range")
	}
	return q.Int64(), nil
}
