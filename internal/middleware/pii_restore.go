package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Opt-in restoration buffers at most 1 MiB, including SSE. Inner middleware
// observes only placeholders; the mapping never enters logs, caches or SQL.
type piiResponseWriter struct {
	gin.ResponseWriter
	body     bytes.Buffer
	status   int
	overflow bool
}

func (w *piiResponseWriter) WriteHeader(code int) {
	if w.body.Len() == 0 {
		w.status = code
	}
}
func (w *piiResponseWriter) WriteHeaderNow()                   {}
func (w *piiResponseWriter) Status() int                       { return w.status }
func (w *piiResponseWriter) Size() int                         { return w.body.Len() }
func (w *piiResponseWriter) Written() bool                     { return w.body.Len() > 0 }
func (w *piiResponseWriter) Flush()                            {}
func (w *piiResponseWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *piiResponseWriter) Write(p []byte) (int, error) {
	if w.overflow || w.body.Len()+len(p) > 1<<20 {
		w.overflow = true
		return 0, errors.New("PII restoration buffer exceeded")
	}
	return w.body.Write(p)
}
func writeRestoredResponse(c *gin.Context, replacements map[string]string) {
	original := c.Writer
	w := &piiResponseWriter{ResponseWriter: original, status: http.StatusOK}
	c.Writer = w
	c.Next()
	c.Writer = original
	original.Header().Del("Content-Length")
	var body []byte
	var err error
	if w.overflow {
		err = errors.New("buffer exceeded")
	} else if strings.Contains(original.Header().Get("Content-Type"), "text/event-stream") {
		body, err = restoreSSE(w.body.Bytes(), replacements)
	} else if w.body.Len() > 0 {
		var value any
		value, err = decodePIIJSON(w.body.Bytes())
		if err == nil {
			value, err = newPIIRestorer(replacements).value(value)
			if err == nil {
				body, err = json.Marshal(value)
			}
		}
	}
	if len(body) > 1<<20 {
		err = errors.New("restored body exceeds limit")
	}
	if err != nil {
		original.Header().Set("Content-Type", "application/json")
		original.WriteHeader(http.StatusBadGateway)
		_, _ = original.Write([]byte(`{"error":{"type":"pii_restore_error","message":"Response could not be safely restored"}}`))
		return
	}
	if manager := guardrailManagerFromContext(c); manager != nil && manager.ValidateLocalOutput(body, strings.Contains(original.Header().Get("Content-Type"), "text/event-stream")) != nil {
		slog.InfoContext(c.Request.Context(), "guardrail_output_blocked", "trace_id", c.GetString("trace_id"), "virtual_key_id", GetVirtualKeyID(c), "stage", "pii_restored")
		original.Header().Set("Content-Type", "application/json")
		abortWithGuardrailBlocked(c, "Restored response blocked by guardrail")
		return
	}
	original.WriteHeader(w.status)
	_, _ = original.Write(body)
}
func decodePIIJSON(b []byte) (any, error) {
	var value any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	err := d.Decode(&value)
	return value, err
}

type piiRestorer struct {
	remaining int
	replacer  *strings.Replacer
}

func newPIIRestorer(replacements map[string]string) *piiRestorer {
	pairs := make([]string, 0, len(replacements)*2)
	for token, original := range replacements {
		pairs = append(pairs, token, original)
	}
	return &piiRestorer{remaining: 1 << 20, replacer: strings.NewReplacer(pairs...)}
}

type boundedPIIString struct {
	strings.Builder
	remaining *int
}

func (b *boundedPIIString) WriteString(s string) (int, error) {
	if len(s) > *b.remaining {
		return 0, errors.New("restored text exceeds limit")
	}
	*b.remaining -= len(s)
	return b.Builder.WriteString(s)
}
func (b *boundedPIIString) Write(p []byte) (int, error) { return b.WriteString(string(p)) }
func (r *piiRestorer) value(v any) (any, error) {
	switch x := v.(type) {
	case string:
		// Write incrementally: a short token repeated many times must not
		// allocate an unbounded restored value before checking its size.
		b := &boundedPIIString{remaining: &r.remaining}
		if _, err := r.replacer.WriteString(b, x); err != nil {
			return nil, err
		}
		return b.String(), nil
	case map[string]any:
		for key, value := range x {
			updated, err := r.value(value)
			if err != nil {
				return nil, err
			}
			x[key] = updated
		}
	case []any:
		for i, value := range x {
			updated, err := r.value(value)
			if err != nil {
				return nil, err
			}
			x[i] = updated
		}
	}
	return v, nil
}
func restoreSSE(body []byte, replacements map[string]string) ([]byte, error) {
	// Restored SSE retains usage and DONE, but combines split text into the
	// first content event per choice. Buffering is explicit in tokenize mode.
	frames := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n\n")
	values := make(map[int]any)
	first := make(map[string]map[string]any)
	texts := make(map[string]string)
	for i, frame := range frames {
		var data []string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		payload := strings.Join(data, "\n")
		if payload == "" || payload == "[DONE]" {
			continue
		}
		value, err := decodePIIJSON([]byte(payload))
		if err != nil {
			return nil, err
		}
		values[i] = value
		obj, _ := value.(map[string]any)
		choices, _ := obj["choices"].([]any)
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			for key := range delta {
				if key != "content" && key != "role" {
					return nil, errors.New("restoration supports text deltas only")
				}
			}
			content, ok := delta["content"].(string)
			if !ok {
				continue
			}
			index, _ := choice["index"].(json.Number)
			key := string(index)
			if first[key] == nil {
				first[key] = delta
			}
			texts[key] += content
			delta["content"] = ""
		}
	}
	for key, delta := range first {
		delta["content"] = texts[key]
	}
	restorer := newPIIRestorer(replacements)
	total := len(body)
	for i, value := range values {
		value, err := restorer.value(value)
		if err != nil {
			return nil, err
		}
		b, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		var lines []string
		for _, line := range strings.Split(frames[i], "\n") {
			if !strings.HasPrefix(line, "data:") {
				lines = append(lines, line)
			}
		}
		lines = append(lines, "data: "+string(b))
		updated := strings.Join(lines, "\n")
		total += len(updated) - len(frames[i])
		if total > 1<<20 {
			return nil, errors.New("restored SSE exceeds limit")
		}
		frames[i] = updated
	}
	return []byte(strings.Join(frames, "\n\n")), nil
}
