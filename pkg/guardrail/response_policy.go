package guardrail

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"

	"github.com/warm3snow/llm-gateway/internal/types"
)

type ResponsePolicy struct {
	keywords *keywordMatcher
	patterns []*regexp.Regexp
}

func newResponsePolicy(params map[string]interface{}) (*ResponsePolicy, error) {
	m, err := compileKeywords(stringSliceParam(params, "keywords"))
	if err != nil {
		return nil, err
	}
	p := &ResponsePolicy{keywords: m}
	patterns := stringSliceParam(params, "patterns")
	if len(patterns) > 128 {
		return nil, fmt.Errorf("too many response patterns")
	}
	for _, pattern := range patterns {
		if len(pattern) > 2048 {
			return nil, fmt.Errorf("response pattern too long")
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid response pattern")
		}
		p.patterns = append(p.patterns, re)
	}
	if boolParam(params, "blockPII", false) {
		for _, rule := range piiRules {
			p.patterns = append(p.patterns, rule.pattern)
		}
	}
	return p, nil
}
func (p *ResponsePolicy) GetName() string { return "response_policy" }
func (p *ResponsePolicy) ValidateRequest(any) (*types.GuardrailResult, error) {
	return &types.GuardrailResult{Passed: true}, nil
}
func (p *ResponsePolicy) blocked(text string) bool {
	if p.keywords.matches(text) {
		return true
	}
	for _, re := range p.patterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}
func (p *ResponsePolicy) ValidateResponse(v any) (*types.GuardrailResult, error) {
	text, ok := v.(string)
	if !ok {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		text = string(b)
	}
	var value any
	texts := []string{text}
	if json.Unmarshal([]byte(text), &value) == nil {
		texts = allText(value)
	}
	blocked := false
	for _, field := range texts {
		if p.blocked(field) {
			blocked = true
			break
		}
	}
	return &types.GuardrailResult{Passed: !blocked, Reason: func() string {
		if blocked {
			return "output_rule"
		}
		return ""
	}()}, nil
}
func allText(value any) []string {
	var fields []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			fields = append(fields, x)
		case []any:
			for _, child := range x {
				walk(child)
			}
		case map[string]any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(value)
	return fields
}

type heldFrame struct {
	event []byte
	text  string
	runes int
}
type StreamFilter struct {
	policies []*ResponsePolicy
	frames   []heldFrame
	text     string
	bytes    int
	hold     int
	whole    bool
	closed   bool
}

func (p *ResponsePolicy) NewStreamFilter() *StreamFilter { f := &StreamFilter{}; f.add(p); return f }
func (f *StreamFilter) add(p *ResponsePolicy) {
	f.policies = append(f.policies, p)
	if p.keywords.longest-1 > f.hold {
		f.hold = p.keywords.longest - 1
	}
	f.whole = f.whole || len(p.patterns) > 0
}
func (m *GuardrailManager) NewStreamFilter() *StreamFilter {
	if !m.Enabled() {
		return nil
	}
	f := &StreamFilter{}
	for _, g := range m.guardrails {
		if p, ok := g.(*ResponsePolicy); ok {
			f.add(p)
		}
	}
	if len(f.policies) == 0 {
		return nil
	}
	return f
}

// ValidateLocalOutput rechecks restored PII locally without sending originals
// to third-party guardrail services. Only response_policy participates.
func (m *GuardrailManager) ValidateLocalOutput(body []byte, stream bool) error {
	if m == nil || !m.Enabled() {
		return nil
	}
	if stream {
		f := m.NewStreamFilter()
		if f == nil {
			return nil
		}
		for _, frame := range bytes.Split(bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n")), []byte("\n\n")) {
			if len(bytes.TrimSpace(frame)) == 0 {
				continue
			}
			if _, err := f.Feed(append(bytes.Clone(frame), '\n', '\n')); err != nil {
				return err
			}
		}
		_, err := f.Finish()
		return err
	}
	for _, g := range m.guardrails {
		if p, ok := g.(*ResponsePolicy); ok {
			result, err := p.ValidateResponse(string(body))
			if err != nil {
				return err
			}
			if !result.Passed {
				return errors.New("output rule matched")
			}
		}
	}
	return nil
}
func (f *StreamFilter) Feed(event []byte) ([][]byte, error) {
	if f.closed {
		return nil, errors.New("stream filter already ended")
	}
	var lines [][]byte
	for _, line := range bytes.Split(event, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			lines = append(lines, bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
		}
	}
	data := bytes.Join(lines, []byte("\n"))
	final := bytes.Equal(data, []byte("[DONE]"))
	text := ""
	if len(data) > 0 && !final {
		var value struct {
			Choices []struct {
				Index int                        `json:"index"`
				Delta map[string]json.RawMessage `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, errors.New("unsupported output event")
		}
		if len(value.Choices) > 1 {
			return nil, errors.New("output policy requires single-choice text")
		}
		for _, choice := range value.Choices {
			if choice.Index != 0 {
				return nil, errors.New("unsupported output content")
			}
			for field, raw := range choice.Delta {
				if field != "content" && field != "role" {
					return nil, errors.New("unsupported output content")
				}
				var content string
				if err := json.Unmarshal(raw, &content); err != nil {
					return nil, errors.New("unsupported output content")
				}
				if field == "content" {
					text += content
				}
			}
		}
	}
	f.bytes += len(event)
	if f.bytes > 1<<20 {
		return nil, errors.New("output inspection buffer exceeded")
	}
	f.frames = append(f.frames, heldFrame{event: bytes.Clone(event), text: text, runes: utf8.RuneCountInString(text)})
	f.text += text
	for _, p := range f.policies {
		if p.keywords.matches(f.text) {
			f.closed = true
			return nil, errors.New("output rule matched")
		}
	}
	if final {
		return f.Finish()
	}
	if f.whole {
		return nil, nil
	}
	remaining := utf8.RuneCountInString(f.text)
	var out [][]byte
	consumed := 0
	for _, frame := range f.frames {
		if frame.runes > 0 && remaining-frame.runes < f.hold {
			break
		}
		out = append(out, frame.event)
		remaining -= frame.runes
		f.text = f.text[len(frame.text):]
		f.bytes -= len(frame.event)
		consumed++
	}
	f.frames = f.frames[consumed:]
	return out, nil
}
func (f *StreamFilter) Finish() ([][]byte, error) {
	if f.closed {
		return nil, nil
	}
	f.closed = true
	// Regex anchors describe a complete output, not an intermediate prefix.
	// Whole-response policies retain all text until DONE or clean EOF.
	for _, p := range f.policies {
		if p.blocked(f.text) {
			return nil, errors.New("output rule matched")
		}
	}
	out := make([][]byte, 0, len(f.frames))
	for _, frame := range f.frames {
		out = append(out, frame.event)
	}
	f.frames = nil
	f.text = ""
	return out, nil
}
