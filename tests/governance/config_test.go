package governance

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/warm3snow/llm-gateway/internal/config"
	"github.com/warm3snow/llm-gateway/internal/handler"
	"github.com/warm3snow/llm-gateway/pkg/guardrail"
)

func TestPolicyActionsSurviveYAMLLoading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("gateway:\n  guardrailsEnabled: true\n  defaultConfig:\n    guardrails:\n      - type: request_policy\n        parameters:\n          piiAction: block\n          injectionAction: audit\n          injectionPatterns: ['custom-test-pattern']\n"), 0600))
	cfg, err := config.LoadConfig(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Gateway.DefaultConfig)
	m, err := guardrail.NewManagerFromConfig(cfg.Gateway.GuardrailsEnabled, cfg.Gateway.DefaultConfig.Guardrails)
	require.NoError(t, err)
	r, err := m.ValidateRequest(map[string]any{"prompt": "a@example.com"})
	require.NoError(t, err)
	require.False(t, r.Passed)
	r, err = m.ValidateRequest(map[string]any{"prompt": "custom-test-pattern"})
	require.NoError(t, err)
	require.True(t, r.Passed)
	require.Contains(t, r.Actions, "audit")
}

func TestReservationModeRejectsLegacyReset(t *testing.T) {
	h := &handler.VirtualKeyHandler{ReservationEnabled: true}
	r := gin.New()
	r.Use(gin.Recovery())
	r.POST("/keys/:id/reset", h.Reset)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/keys/1/reset", nil))
	require.Equal(t, 409, w.Code)
}

func TestMalformedConfigDoesNotDisableGovernance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("budget: [\n"), 0600))
	_, err := config.LoadConfig(path)
	require.Error(t, err)
}
