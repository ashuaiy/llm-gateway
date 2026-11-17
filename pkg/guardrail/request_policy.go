package guardrail

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/warm3snow/llm-gateway/internal/types"
	"golang.org/x/text/unicode/norm"
)

// RequestPolicy implements local, deterministic screening. It is not a semantic
// prompt-injection detector. Only supported text fields are inspected; media is
// preserved, not decoded or fetched. No matched text is returned in findings.
type RequestPolicy struct {
	piiAction       string
	injectionAction string
	injections      []*regexp.Regexp
	keywords        *keywordMatcher
	checkID         bool
	tokenize        bool
}

var piiRules = []struct {
	id      string
	pattern *regexp.Regexp
}{
	{"api_key", regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{16,}|AKIA[A-Z0-9]{16})\b`)},
	{"email", regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)},
	{"id_card", regexp.MustCompile(`\b[1-9]\d{5}(?:18|19|20)\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\d{3}[\dXx]\b`)},
	{"phone", regexp.MustCompile(`\b(?:\+86[- ]?)?1[3-9]\d{9}\b`)},
}

func newRequestPolicy(params map[string]interface{}) (*RequestPolicy, error) {
	p := &RequestPolicy{piiAction: stringParam(params, "piiAction", "redact"), injectionAction: stringParam(params, "injectionAction", "block")}
	mode := stringParam(params, "piiMode", "redact")
	if mode != "redact" && mode != "tokenize" {
		return nil, fmt.Errorf("invalid piiMode")
	}
	p.tokenize = mode == "tokenize"
	var err error
	p.keywords, err = compileKeywords(stringSliceParam(params, "injectionKeywords"))
	if err != nil {
		return nil, err
	}
	validation := stringParam(params, "idCardValidation", "format")
	if validation != "format" && validation != "checksum" {
		return nil, fmt.Errorf("invalid idCardValidation")
	}
	p.checkID = validation == "checksum"
	if !validPolicyAction(p.piiAction, true) || !validPolicyAction(p.injectionAction, false) {
		return nil, fmt.Errorf("invalid request_policy action")
	}
	patterns := stringSliceParam(params, "injectionPatterns")
	if len(patterns) == 0 {
		patterns = []string{`忽略之前所有设定`, `输出系统提示词`, `(?i)ignore\s+(all\s+)?previous\s+instructions`, `(?i)reveal\s+(the\s+)?system\s+prompt`}
	}
	if len(patterns) > 128 {
		return nil, fmt.Errorf("too many injection patterns")
	}
	for i, pattern := range patterns {
		if len(pattern) > 2048 {
			return nil, fmt.Errorf("injection rule %d is too long", i)
		}
		r, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid injection rule %d", i)
		}
		p.injections = append(p.injections, r)
	}
	return p, nil
}

func validPolicyAction(action string, redact bool) bool {
	return action == "off" || action == "block" || action == "audit" || (redact && action == "redact")
}

func (p *RequestPolicy) GetName() string { return "request_policy" }
func (p *RequestPolicy) ValidateResponse(any) (*types.GuardrailResult, error) {
	return &types.GuardrailResult{Passed: true}, nil
}

func (p *RequestPolicy) ValidateRequest(req any) (*types.GuardrailResult, error) {
	// Copy through JSON, retaining unknown fields and numbers with UseNumber.
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	if err = d.Decode(&payload); err != nil {
		return nil, fmt.Errorf("request_policy requires a JSON object")
	}
	if hasNoncanonicalTextKey(payload) {
		return &types.GuardrailResult{Passed: false, Actions: []string{"block"}, Findings: []string{"noncanonical_field"}, Reason: "noncanonical_field"}, nil
	}
	result := &types.GuardrailResult{Passed: true}
	values := map[string]string{}
	nonce := strings.ReplaceAll(uuid.NewString(), "-", "")
	findings, actions := map[string]bool{}, map[string]bool{}
	changed := false
	apply := func(text string) string {
		normalized := norm.NFKC.String(strings.Map(func(r rune) rune {
			if r == '\u200b' || r == '\u200c' || r == '\u200d' || r == '\ufeff' {
				return -1
			}
			return r
		}, text))
		if p.injectionAction != "off" {
			if p.keywords.matches(normalized) {
				findings["injection_keyword"] = true
				actions[p.injectionAction] = true
				if p.injectionAction == "block" {
					result.Passed = false
				}
			}
			for i, pattern := range p.injections {
				if pattern.MatchString(normalized) {
					findings[fmt.Sprintf("injection_%d", i+1)] = true
					actions[p.injectionAction] = true
					if p.injectionAction == "block" {
						result.Passed = false
					}
				}
			}
		}
		if p.piiAction != "off" {
			for _, rule := range piiRules {
				validMatch := func(v string) bool { return rule.id != "id_card" || !p.checkID || validIDChecksum(v) }
				found := false
				for _, v := range rule.pattern.FindAllString(text, -1) {
					if validMatch(v) {
						found = true
						break
					}
				}
				if found {
					findings[rule.id] = true
					actions[p.piiAction] = true
					if p.piiAction == "block" {
						result.Passed = false
					}
					if p.piiAction == "redact" {
						text = rule.pattern.ReplaceAllStringFunc(text, func(v string) string {
							if !validMatch(v) {
								return v
							}
							if p.tokenize && rule.id != "api_key" {
								if token, ok := values[v]; ok {
									return token
								}
								token := fmt.Sprintf("[PII_%s_%d]", nonce, len(values)+1)
								values[v] = token
								if result.Replacements == nil {
									result.Replacements = map[string]string{}
								}
								result.Replacements[token] = v
								return token
							}
							return "[REDACTED_" + strings.ToUpper(rule.id) + "]"
						})
						changed = true
					}
				}
			}
		}
		return text
	}
	for _, key := range []string{"prompt", "input", "user"} {
		if v, ok := payload[key]; ok {
			payload[key] = transformTextContent(v, apply)
		}
	}
	if messages, ok := payload["messages"].([]any); ok {
		for _, v := range messages {
			if m, ok := v.(map[string]any); ok {
				if name, ok := m["name"].(string); ok {
					m["name"] = apply(name)
				}
				if content, exists := m["content"]; exists {
					m["content"] = transformTextContent(content, apply)
				}
				// Tool arguments are strings; replacing matches preserves JSON quoting.
				if calls, ok := m["tool_calls"].([]any); ok {
					for _, call := range calls {
						if c, ok := call.(map[string]any); ok {
							if f, ok := c["function"].(map[string]any); ok {
								if a, ok := f["arguments"].(string); ok {
									f["arguments"] = apply(a)
								}
							}
						}
					}
				}
			}
		}
	}
	for id := range findings {
		result.Findings = append(result.Findings, id)
	}
	for action := range actions {
		result.Actions = append(result.Actions, action)
	}
	sort.Strings(result.Findings)
	sort.Strings(result.Actions)
	result.Reason = strings.Join(result.Findings, ",")
	if changed && result.Passed {
		b, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		result.MaskedContent = string(b)
	}
	return result, nil
}

func hasNoncanonicalTextKey(value any) bool {
	// Go's struct decoder accepts case-insensitive tag aliases. Reject those
	// aliases so screening and forwarding cannot interpret different messages.
	known := []string{"messages", "role", "content", "prompt", "input", "text", "type", "name", "user", "tools", "tool_calls", "function", "arguments", "model", "response_format", "stream", "max_tokens"}
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			for _, canonical := range known {
				if key != canonical && strings.EqualFold(key, canonical) {
					return true
				}
			}
			if hasNoncanonicalTextKey(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if hasNoncanonicalTextKey(child) {
				return true
			}
		}
	}
	return false
}

func transformTextContent(value any, apply func(string) string) any {
	switch v := value.(type) {
	case string:
		return apply(v)
	case []any:
		for i, part := range v {
			switch p := part.(type) {
			case string:
				v[i] = apply(p)
			case map[string]any:
				if p["type"] == "text" || p["type"] == "input_text" || p["type"] == "output_text" {
					if text, ok := p["text"].(string); ok {
						p["text"] = apply(text)
					}
				}
			}
		}
	}
	return value
}
