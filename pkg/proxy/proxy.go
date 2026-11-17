package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	pathpkg "path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/warm3snow/llm-gateway/internal/billing"
	"github.com/warm3snow/llm-gateway/internal/config"
	"github.com/warm3snow/llm-gateway/internal/limits"
	"github.com/warm3snow/llm-gateway/internal/metrics"
	"github.com/warm3snow/llm-gateway/internal/provider"
	"github.com/warm3snow/llm-gateway/internal/service"
	"github.com/warm3snow/llm-gateway/internal/types"
	"github.com/warm3snow/llm-gateway/pkg/cache"
	"github.com/warm3snow/llm-gateway/pkg/guardrail"
	"github.com/warm3snow/llm-gateway/pkg/retry"
)

// ProxyHandler 代理处理器
type ProxyHandler struct {
	Billing          *billing.Service
	Limiter          *limits.Limiter
	Config           *config.Config
	ProviderFactory  *provider.ProviderFactory
	Retryer          *retry.Retryer
	Cache            cache.Cache
	ModelSelector    *service.ModelSelector
	ModelTracker     *service.ModelConcurrencyTracker
	GuardrailManager *guardrail.GuardrailManager
}

// NewProxyHandler 创建代理处理器
func NewProxyHandler(cfg *config.Config, c cache.Cache) *ProxyHandler {
	tracker := service.NewModelConcurrencyTracker()
	return &ProxyHandler{
		Config:          cfg,
		ProviderFactory: provider.GetGlobalFactory(),
		Retryer:         retry.NewRetryer(retry.DefaultRetryConfig()),
		Cache:           c,
		ModelSelector:   service.NewModelSelector(cfg, tracker),
		ModelTracker:    tracker,
	}
}

func (h *ProxyHandler) SetGuardrailManager(manager *guardrail.GuardrailManager) {
	h.GuardrailManager = manager
}

// HandleChatCompletion 处理聊天补全请求
func (h *ProxyHandler) HandleChatCompletion(c *gin.Context) {
	start := time.Now()
	maxDuration := 120 * time.Second
	if h.Config.Gateway.MaxRequestTimeout > 0 {
		maxDuration = time.Duration(h.Config.Gateway.MaxRequestTimeout) * time.Millisecond
	}
	requestCtx, cancelRequest := context.WithTimeout(c.Request.Context(), maxDuration)
	defer cancelRequest()
	c.Request = c.Request.WithContext(requestCtx)
	var req types.ChatCompletionRequest

	// 解析请求
	if err := c.ShouldBindJSON(&req); err != nil {
		h.abortWithError(c, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Invalid request: %v", err))
		return
	}

	if c.GetBool("force_stream") {
		req.Stream = true
	}
	if req.Stream {
		if req.StreamOptions == nil {
			req.StreamOptions = &types.StreamOptions{}
		}
		req.StreamOptions.IncludeUsage = true
	}
	// 获取配置
	opts := h.getOptionsFromContext(c)
	finishSelection := func() {}
	if isAutoModel(req.Model) {
		if !h.autoModeEnabled() {
			h.abortWithError(c, http.StatusBadRequest, "invalid_request_error", "auto mode is disabled")
			return
		}
		selection, done, err := h.selectAutoModel(c, &req)
		if err != nil {
			h.abortWithError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		opts = &selection.Options
		req.Model = selection.Model
		finishSelection = done
	}
	defer finishSelection()

	// 创建 provider
	prov, err := h.ProviderFactory.Create(opts.Provider, opts)
	if err != nil {
		h.abortWithError(c, http.StatusInternalServerError, "provider_error", fmt.Sprintf("Failed to create provider: %v", err))
		return
	}

	// 发送请求
	finishLimits, allowed := h.beginLimits(c, &req)
	if !allowed {
		return
	}
	defer finishLimits()
	session, ok := h.beginBudget(c, &req, opts)
	if !ok {
		return
	}
	defer h.finishBudget(c, session)
	ctx := c.Request.Context()
	var resp *http.Response
	var respErr error

	c.Set("budget_upstream_attempted", true)
	if opts.Retry != nil {
		// 使用重试
		resp, respErr = h.Retryer.DoWithProvider(ctx, opts.Provider, func() (*http.Response, error) {
			return prov.ChatCompletion(ctx, &req, opts)
		})
	} else {
		resp, respErr = prov.ChatCompletion(ctx, &req, opts)
	}

	if respErr != nil {
		h.abortWithError(c, http.StatusInternalServerError, "request_error", fmt.Sprintf("Request failed: %v", respErr))
		return
	}
	defer resp.Body.Close()

	// 处理响应
	if req.Stream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		h.streamResponse(c, resp, &req, opts.Provider, start)
		return
	}
	h.handleResponse(c, resp)
}

// HandleCompletion 处理文本补全请求
func (h *ProxyHandler) HandleCompletion(c *gin.Context) {
	var req types.CompletionRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		h.abortWithError(c, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Invalid request: %v", err))
		return
	}

	opts := h.getOptionsFromContext(c)
	prov, err := h.ProviderFactory.Create(opts.Provider, opts)
	if err != nil {
		h.abortWithError(c, http.StatusInternalServerError, "provider_error", fmt.Sprintf("Failed to create provider: %v", err))
		return
	}

	ctx := c.Request.Context()
	resp, err := prov.Completion(ctx, &req, opts)
	if err != nil {
		h.abortWithError(c, http.StatusInternalServerError, "request_error", fmt.Sprintf("Request failed: %v", err))
		return
	}
	defer resp.Body.Close()

	h.handleResponse(c, resp)
}

// HandleEmbedding 处理嵌入请求
func (h *ProxyHandler) HandleEmbedding(c *gin.Context) {
	var req types.EmbeddingRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		h.abortWithError(c, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Invalid request: %v", err))
		return
	}

	opts := h.getOptionsFromContext(c)
	prov, err := h.ProviderFactory.Create(opts.Provider, opts)
	if err != nil {
		h.abortWithError(c, http.StatusInternalServerError, "provider_error", fmt.Sprintf("Failed to create provider: %v", err))
		return
	}

	ctx := c.Request.Context()
	resp, err := prov.Embedding(ctx, &req, opts)
	if err != nil {
		h.abortWithError(c, http.StatusInternalServerError, "request_error", fmt.Sprintf("Request failed: %v", err))
		return
	}
	defer resp.Body.Close()

	h.handleResponse(c, resp)
}

func (h *ProxyHandler) abortWithError(c *gin.Context, status int, errType, msg string) {
	c.AbortWithStatusJSON(status, types.ErrorResponse{
		Message: msg,
		Type:    errType,
	})
}

func isAutoModel(model string) bool {
	model = strings.TrimSpace(strings.ToLower(model))
	return model == "" || model == "auto"
}

func (h *ProxyHandler) autoModeEnabled() bool {
	if h == nil || h.Config == nil {
		return true
	}
	autoMode := h.Config.Gateway.AutoMode
	if autoMode.Enabled {
		return true
	}
	return autoMode.CostWeight == 0 &&
		autoMode.ConcurrencyWeight == 0 &&
		autoMode.RecentUsageWeight == 0 &&
		autoMode.ErrorWeight == 0 &&
		autoMode.ProviderWeightPenaltyWeight == 0 &&
		autoMode.RecentWindowSeconds == 0 &&
		autoMode.DefaultMaxConcurrency == 0 &&
		autoMode.DefaultOutputTokens == 0
}

func (h *ProxyHandler) selectAutoModel(c *gin.Context, req *types.ChatCompletionRequest) (*service.Selection, func(), error) {
	if h.ModelSelector == nil {
		return nil, nil, fmt.Errorf("auto-mode selector is not configured")
	}
	hint := service.SelectionHint{ProviderName: c.GetHeader("x-llm-provider")}
	if allowed, ok := c.Get("virtual_key_allowed_providers"); ok {
		if values, ok := allowed.([]string); ok {
			hint.AllowedProviders = values
		}
	}
	selection, done, err := h.ModelSelector.SelectAndReserve(c.Request.Context(), req, hint)
	if err != nil {
		return nil, nil, err
	}
	c.Set("selected_provider", selection.ProviderType)
	c.Set("selected_provider_name", selection.ProviderName)
	c.Set("selected_provider_type", selection.ProviderType)
	c.Set("selected_model", selection.Model)
	c.Header("x-llm-auto-mode", "true")
	c.Header("x-llm-selected-provider", selection.ProviderName)
	c.Header("x-llm-selected-provider-type", selection.ProviderType)
	c.Header("x-llm-selected-model", selection.Model)
	return selection, done, nil
}

// HandleModels 处理模型列表请求
func (h *ProxyHandler) HandleModels(c *gin.Context) {
	opts := h.getOptionsFromContext(c)
	prov, err := h.ProviderFactory.Create(opts.Provider, opts)
	if err != nil {
		h.abortWithError(c, http.StatusInternalServerError, "provider_error", fmt.Sprintf("Failed to create provider: %v", err))
		return
	}

	ctx := c.Request.Context()
	resp, err := prov.Models(ctx, opts)
	if err != nil {
		h.abortWithError(c, http.StatusInternalServerError, "request_error", fmt.Sprintf("Request failed: %v", err))
		return
	}
	defer resp.Body.Close()

	h.handleResponse(c, resp)
}

// handleResponse 处理响应
func (h *ProxyHandler) handleResponse(c *gin.Context, resp *http.Response) {
	// 读取响应体
	var reader io.Reader = resp.Body
	bounded := c.GetBool("pii_restore") || (h.shouldApplyResponseGuardrail(c, resp.StatusCode) && h.GuardrailManager.NewStreamFilter() != nil)
	if bounded {
		reader = io.LimitReader(reader, (1<<20)+1)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		h.abortWithError(c, http.StatusInternalServerError, "response_error", fmt.Sprintf("Failed to read response: %v", err))
		return
	}
	if bounded && len(body) > 1<<20 {
		h.abortWithError(c, http.StatusBadGateway, "response_too_large", "Response inspection exceeds 1 MiB")
		return
	}

	// Capture authoritative usage even if a response guardrail rejects the body.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if u := extractUsageFromChunk(body); u != nil {
			u.Source = "provider"
			c.Set("stream_usage", *u)
		}
	}

	if h.shouldApplyResponseGuardrail(c, resp.StatusCode) {
		result, err := h.GuardrailManager.ValidateResponse(string(body))
		if err != nil {
			h.abortWithError(c, http.StatusInternalServerError, "guardrail_error", "Guardrail validation failed")
			return
		}
		if result != nil && !result.Passed {
			c.AbortWithStatusJSON(http.StatusForbidden, types.ErrorResponse{
				Message: "Response blocked by guardrail",
				Type:    "guardrail_error",
				Code:    "guardrail_blocked",
			})
			return
		}
	}

	// 设置响应头
	for key, values := range resp.Header {
		for _, value := range values {
			c.Header(key, value)
		}
	}

	// 设置状态码
	c.Status(resp.StatusCode)

	// 写入响应体
	c.Writer.Write(body)
}

func (h *ProxyHandler) shouldApplyResponseGuardrail(c *gin.Context, status int) bool {
	if h.GuardrailManager == nil || !h.GuardrailManager.Enabled() || h.GuardrailManager.Empty() {
		return false
	}
	if c.Request.Method != http.MethodPost || status < 200 || status >= 300 {
		return false
	}
	switch guardrailEndpointPath(c.Request.URL.Path) {
	case "/chat/completions", "/completions":
		return true
	default:
		return false
	}
}

func guardrailEndpointPath(path string) string {
	path = strings.TrimPrefix(path, "/v1")
	path = strings.TrimPrefix(path, "/proxy")
	path = "/" + strings.TrimLeft(path, "/")
	return pathpkg.Clean(path)
}

// getOptionsFromContext 从请求上下文获取选项
func (h *ProxyHandler) getOptionsFromContext(c *gin.Context) *types.Options {
	providerName := h.Config.Gateway.DefaultProvider
	if provider := c.GetHeader("x-llm-provider"); provider != "" {
		providerName = provider
	}

	opts := &types.Options{
		Provider:       providerName,
		RequestTimeout: h.Config.Gateway.MaxRequestTimeout,
	}

	// providerName is the configured provider entry name. The stored Options.Provider
	// is the concrete provider type/factory name (e.g. openai). Use the stored
	// options as the base so custom hosts, timeouts, Azure/AWS fields, etc. are honored.
	h.Config.Gateway.ProvidersMu.RLock()
	providerConfig, ok := h.Config.Gateway.Providers[providerName]
	h.Config.Gateway.ProvidersMu.RUnlock()
	if ok {
		copied := providerConfig
		opts = &copied
	}

	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = h.Config.Gateway.MaxRequestTimeout
	}

	return opts
}

// ProxyRequest 代理请求
func (h *ProxyHandler) ProxyRequest(c *gin.Context) {
	// 获取目标 URL
	targetURL := h.getTargetURL(c)
	if targetURL == "" {
		h.abortWithError(c, http.StatusBadRequest, "invalid_request_error", "Target URL not found")
		return
	}

	// 读取请求体
	body, err := c.GetRawData()
	if err != nil {
		h.abortWithError(c, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Failed to read request body: %v", err))
		return
	}

	// 创建新的请求
	ctx := c.Request.Context()
	req, err := http.NewRequestWithContext(ctx, c.Request.Method, targetURL, bytes.NewBuffer(body))
	if err != nil {
		h.abortWithError(c, http.StatusInternalServerError, "request_error", fmt.Sprintf("Failed to create request: %v", err))
		return
	}

	// 复制请求头
	for key, values := range c.Request.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// 发送请求
	client := &http.Client{
		Timeout: time.Duration(h.Config.Gateway.MaxRequestTimeout) * time.Millisecond,
	}

	resp, err := client.Do(req)
	if err != nil {
		h.abortWithError(c, http.StatusInternalServerError, "request_error", fmt.Sprintf("Request failed: %v", err))
		return
	}
	defer resp.Body.Close()

	// 处理响应
	h.handleResponse(c, resp)
}

// getTargetURL 获取目标 URL
func (h *ProxyHandler) getTargetURL(c *gin.Context) string {
	opts := h.getOptionsFromContext(c)

	// 如果有自定义 URL，使用它
	if opts.URLToFetch != "" {
		return opts.URLToFetch
	}

	// 根据 provider 构建 URL
	prov, err := h.ProviderFactory.Create(opts.Provider, opts)
	if err != nil {
		return ""
	}

	baseURL := strings.TrimRight(prov.GetBaseURL(), "/")
	path := strings.TrimLeft(c.Param("path"), "/")

	return baseURL + "/" + path
}

// HandleStreamRequest 处理流式请求
func (h *ProxyHandler) HandleStreamRequest(c *gin.Context) {
	c.Set("force_stream", true)
	h.HandleChatCompletion(c)
}

// streamResponse 流式传输响应，同时解析 SSE 事件中的 usage 字段。
// 当上游在流结束时仍未发送 usage（如 Ollama 的 OpenAI 兼容端点会忽略
// stream_options.include_usage），则基于累积的输出内容做字符近似估算。
func (h *ProxyHandler) streamResponse(c *gin.Context, resp *http.Response, req *types.ChatCompletionRequest, providerName string, start time.Time) {
	// 创建 flusher
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		h.abortWithError(c, http.StatusInternalServerError, "stream_error", "Streaming not supported")
		return
	}

	// StreamUsageKey is the gin context key under which the final usage is stored.
	// UsageRecordMiddleware reads this after c.Next() returns.
	const StreamUsageKey = "stream_usage"

	// Use bufio.Scanner with a custom split on SSE event boundaries so that
	// multi-line events and large data chunks aren't split mid-frame.
	scanner := bufio.NewScanner(resp.Body)
	// Allow large SSE events (default Scanner buffer is 64KB; raise to 1MB).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	scanner.Split(scanSSEEvents)

	// 累积输出内容，用于在上游不返回 usage 时做估算。
	var contentBuf bytes.Buffer
	var sawUsage bool
	// ttftRecorded ensures TTFT is observed exactly once, on the first chunk
	// that carries actual content.
	var ttftRecorded bool
	model := ""
	if req != nil {
		model = req.Model
	}

	defer func() {
		// 上游未发送 usage 时，做字符近似估算。
		if !sawUsage {
			promptTokens := estimateTokens(promptTextFromRequest(req))
			completionTokens := estimateTokens(contentBuf.String())
			if promptTokens > 0 || completionTokens > 0 {
				c.Set(StreamUsageKey, streamUsage{
					Source:           "estimated",
					PromptTokens:     promptTokens,
					CompletionTokens: completionTokens,
					TotalTokens:      promptTokens + completionTokens,
				})
			}
		}
	}()
	outputFilter := h.GuardrailManager.NewStreamFilter()
	writeFrames := func(frames [][]byte) bool {
		for _, frame := range frames {
			if _, err := c.Writer.Write(frame); err != nil {
				return false
			}
			if !bytes.HasSuffix(frame, []byte("\n\n")) {
				if _, err := c.Writer.Write([]byte("\n\n")); err != nil {
					return false
				}
			}
			flusher.Flush()
		}
		return true
	}
	blockOutput := func() {
		c.Set("output_blocked", true)
		slog.Warn("guardrail_output_blocked", "virtual_key_id", c.GetUint("virtual_key_id"))
		_, _ = c.Writer.Write([]byte("event: error\ndata: {\"error\":{\"type\":\"output_blocked\",\"message\":\"Response stopped by output policy\"}}\n\n"))
		flusher.Flush()
	}

	for scanner.Scan() {
		event := scanner.Bytes()
		if len(event) == 0 {
			continue
		}
		// Inspect data: lines for a usage payload.
		// SSE event format: optional "event:" / "id:" / "retry:" lines plus
		// one or more "data:" lines, terminated by a blank line.
		for _, line := range bytes.Split(event, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}
			// Try to extract a top-level "usage" object from the chunk.
			// OpenAI sends usage only on the final chunk when
			// stream_options.include_usage=true.
			if u := extractFinalStreamUsage(payload); u != nil {
				u.Source = "provider"
				c.Set(StreamUsageKey, *u)
				sawUsage = true
			}
			// 累积 delta.content，用于估算。
			if content := extractContentDelta(payload); len(content) > 0 {
				if !ttftRecorded {
					metrics.TimeToFirstToken.WithLabelValues(
						metrics.LabelOrUnknown(providerName),
						metrics.LabelOrUnknown(model),
					).Observe(time.Since(start).Seconds())
					ttftRecorded = true
				}
				contentBuf.Write(content)
			}
		}
		frames := [][]byte{event}
		if outputFilter != nil {
			var err error
			frames, err = outputFilter.Feed(event)
			if err != nil {
				blockOutput()
				return
			}
		}
		if !writeFrames(frames) {
			return
		}

		if c.Request.Context().Err() != nil {
			return
		}
	}
	if outputFilter != nil && scanner.Err() == nil && c.Request.Context().Err() == nil {
		frames, err := outputFilter.Finish()
		if err != nil {
			blockOutput()
			return
		}
		writeFrames(frames)
	}

}

// streamUsage carries the token counts extracted from a streaming chunk.
type streamUsage = types.TokenUsage

// extractUsageFromChunk parses a single SSE data payload (JSON) and returns
// the usage block if present.
func extractUsageFromChunk(payload []byte) *streamUsage {
	var chunk struct {
		Usage *struct {
			Prompt     *int `json:"prompt_tokens"`
			Completion *int `json:"completion_tokens"`
			Total      *int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil
	}
	u := chunk.Usage
	if u == nil || u.Prompt == nil || u.Completion == nil || u.Total == nil || *u.Prompt < 0 || *u.Completion < 0 || *u.Total < 0 || *u.Prompt > *u.Total || *u.Completion != *u.Total-*u.Prompt {
		return nil
	}
	return &streamUsage{PromptTokens: *u.Prompt, CompletionTokens: *u.Completion, TotalTokens: *u.Total}
}

func extractFinalStreamUsage(payload []byte) *streamUsage {
	var event struct {
		Choices *[]json.RawMessage `json:"choices"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return nil
	}
	// With include_usage, the OpenAI-compatible final usage event has no choices.
	if event.Choices == nil || len(*event.Choices) != 0 {
		return nil
	}
	return extractUsageFromChunk(payload)
}

// extractContentDelta extracts the `.delta.content` string from a streaming
// chat completion chunk payload. Returns nil for non-content chunks.
func extractContentDelta(payload []byte) []byte {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil
	}
	for _, ch := range chunk.Choices {
		if ch.Delta.Content != "" {
			return []byte(ch.Delta.Content)
		}
	}
	return nil
}

// promptTextFromRequest flattens the request's messages into a single string
// for token estimation purposes.
func promptTextFromRequest(req *types.ChatCompletionRequest) string {
	if req == nil {
		return ""
	}
	var sb strings.Builder
	for _, m := range req.Messages {
		switch v := m.Content.(type) {
		case string:
			sb.WriteString(v)
		default:
			b, _ := json.Marshal(v)
			sb.Write(b)
		}
		sb.WriteByte(' ')
	}
	return sb.String()
}

// estimateTokens returns a rough token estimate using character heuristics:
//   - ASCII / Latin: ~4 chars per token
//   - CJK / other multibyte: ~1.5 chars per token
//
// This is intentionally cheap — it's only used when the upstream provider
// doesn't return a real usage block (e.g. Ollama streaming).
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	var cjk, ascii int
	for _, r := range s {
		if r < 0x80 {
			ascii++
		} else {
			cjk++
		}
	}
	return ascii/4 + cjk*2/3
}

// scanSSEEvents is a bufio.SplitFunc that splits on SSE event boundaries
// (a blank line — i.e. "\n\n"). The returned token includes the trailing
// "\n\n" so the forwarded bytes remain valid SSE.
func scanSSEEvents(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	// A blank line ends the event. Accept both LF and CRLF without merging
	// adjacent JSON payloads (which would lose authoritative final usage).
	lineStart := 0
	for i, b := range data {
		if b != '\n' {
			continue
		}
		if i == lineStart || (i == lineStart+1 && data[lineStart] == '\r') {
			return i + 1, data[:i+1], nil
		}
		lineStart = i + 1
	}
	// If at EOF, return the remaining data as a final token.
	if atEOF {
		return len(data), data, nil
	}
	// Request more data.
	return 0, nil, nil
}

// RegisterRoutes 注册路由
func (h *ProxyHandler) RegisterRoutes(router *gin.Engine) {
	// 聊天补全
	router.POST("/v1/chat/completions", h.HandleChatCompletion)

	// 文本补全
	router.POST("/v1/completions", h.HandleCompletion)

	// 嵌入
	router.POST("/v1/embeddings", h.HandleEmbedding)

	// 模型列表
	router.GET("/v1/models", h.HandleModels)

	// 图像生成
	router.POST("/v1/images/generations", h.HandleImageGeneration)

	// 音频
	router.POST("/v1/audio/speech", h.HandleAudioSpeech)
	router.POST("/v1/audio/transcriptions", h.HandleAudioTranscription)
	router.POST("/v1/audio/translations", h.HandleAudioTranslation)

	// 代理
	router.Any("/v1/*path", h.ProxyRequest)

	// 流式请求
	router.POST("/v1/chat/completions/stream", h.HandleStreamRequest)
}

// HandleImageGeneration 处理图像生成请求
func (h *ProxyHandler) HandleImageGeneration(c *gin.Context) {
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		h.abortWithError(c, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Invalid request: %v", err))
		return
	}

	opts := h.getOptionsFromContext(c)
	prov, err := h.ProviderFactory.Create(opts.Provider, opts)
	if err != nil {
		h.abortWithError(c, http.StatusInternalServerError, "provider_error", fmt.Sprintf("Failed to create provider: %v", err))
		return
	}

	resp, err := prov.ImageGeneration(c.Request.Context(), req, opts)
	if err != nil {
		h.abortWithError(c, http.StatusInternalServerError, "request_error", fmt.Sprintf("Request failed: %v", err))
		return
	}
	defer resp.Body.Close()

	h.handleResponse(c, resp)
}

// HandleAudioSpeech 处理文本转语音请求
func (h *ProxyHandler) HandleAudioSpeech(c *gin.Context) {
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		h.abortWithError(c, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Invalid request: %v", err))
		return
	}

	opts := h.getOptionsFromContext(c)
	prov, err := h.ProviderFactory.Create(opts.Provider, opts)
	if err != nil {
		h.abortWithError(c, http.StatusInternalServerError, "provider_error", fmt.Sprintf("Failed to create provider: %v", err))
		return
	}

	// The upstream returns binary audio; handleResponse forwards it verbatim.
	resp, err := prov.AudioSpeech(c.Request.Context(), req, opts)
	if err != nil {
		h.abortWithError(c, http.StatusInternalServerError, "request_error", fmt.Sprintf("Request failed: %v", err))
		return
	}
	defer resp.Body.Close()

	h.handleResponse(c, resp)
}

// HandleAudioTranscription 处理语音转文本请求
func (h *ProxyHandler) HandleAudioTranscription(c *gin.Context) {
	h.handleAudioUpload(c, func(prov provider.Provider, req *types.AudioRequest, opts *types.Options) (*http.Response, error) {
		return prov.AudioTranscription(c.Request.Context(), req, opts)
	})
}

// HandleAudioTranslation 处理语音翻译请求
func (h *ProxyHandler) HandleAudioTranslation(c *gin.Context) {
	h.handleAudioUpload(c, func(prov provider.Provider, req *types.AudioRequest, opts *types.Options) (*http.Response, error) {
		return prov.AudioTranslation(c.Request.Context(), req, opts)
	})
}

func (h *ProxyHandler) handleAudioUpload(c *gin.Context, call func(provider.Provider, *types.AudioRequest, *types.Options) (*http.Response, error)) {
	req, err := parseAudioRequest(c)
	if err != nil {
		h.abortWithError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	opts := h.getOptionsFromContext(c)
	prov, err := h.ProviderFactory.Create(opts.Provider, opts)
	if err != nil {
		h.abortWithError(c, http.StatusInternalServerError, "provider_error", fmt.Sprintf("Failed to create provider: %v", err))
		return
	}

	resp, err := call(prov, req, opts)
	if err != nil {
		h.abortWithError(c, http.StatusInternalServerError, "request_error", fmt.Sprintf("Request failed: %v", err))
		return
	}
	defer resp.Body.Close()

	h.handleResponse(c, resp)
}

func parseAudioRequest(c *gin.Context) (*types.AudioRequest, error) {
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		return nil, fmt.Errorf("audio request must be multipart/form-data: %w", err)
	}
	form := c.Request.MultipartForm
	if form == nil || len(form.File["file"]) == 0 {
		return nil, fmt.Errorf("audio file is required")
	}
	return &types.AudioRequest{FileHeader: form.File["file"][0], Fields: form.Value}, nil
}
