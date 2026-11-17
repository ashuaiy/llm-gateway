package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/warm3snow/llm-gateway/internal/types"
	"github.com/warm3snow/llm-gateway/pkg/guardrail"
)

func TestResponseInspectionBoundsApplyOnlyToTextEndpoints(t *testing.T) {
	manager, err := guardrail.NewManagerFromConfig(true, []types.GuardrailConfig{{Type: "response_policy", Parameters: map[string]interface{}{"blockPII": true}}})
	require.NoError(t, err)
	for path, status := range map[string]int{"/v1/chat/completions": 502, "/v1/audio/speech": 200} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", path, nil)
		h := NewProxyHandler(nil, nil)
		h.SetGuardrailManager(manager)
		h.handleResponse(c, &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 2<<20)))})
		require.Equal(t, status, w.Code, path)
	}
}
