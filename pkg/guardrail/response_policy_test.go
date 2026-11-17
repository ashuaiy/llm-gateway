package guardrail

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func streamEvent(text string) []byte {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}}}})
	return append(append([]byte("data: "), b...), []byte("\n\n")...)
}
func TestStreamFilterBlocksSplitKeywordBeforeSendingIt(t *testing.T) {
	p, err := newResponsePolicy(map[string]interface{}{"keywords": []string{"内部密码"}})
	require.NoError(t, err)
	f := p.NewStreamFilter()
	out, err := f.Feed(streamEvent("安全内容。内部"))
	require.NoError(t, err)
	require.Empty(t, out)
	out, err = f.Feed(streamEvent("密码是秘密"))
	require.Error(t, err)
	require.Empty(t, out)
}
func TestStreamFilterPreservesSafeProtocolAndBuffersRegex(t *testing.T) {
	p, err := newResponsePolicy(map[string]interface{}{"keywords": []string{"禁止"}})
	require.NoError(t, err)
	f := p.NewStreamFilter()
	first := streamEvent("允许的内容")
	last := []byte("data: [DONE]\n\n")
	out, err := f.Feed(first)
	require.NoError(t, err)
	tail, err := f.Feed(last)
	require.NoError(t, err)
	out = append(out, tail...)
	require.Equal(t, string(first)+string(last), string(joinFrames(out)))
	p, err = newResponsePolicy(map[string]interface{}{"patterns": []string{`a.{300}z`}})
	require.NoError(t, err)
	f = p.NewStreamFilter()
	out, err = f.Feed(streamEvent("a" + strings.Repeat("x", 300)))
	require.NoError(t, err)
	require.Empty(t, out)
	out, err = f.Feed(streamEvent("z"))
	require.NoError(t, err)
	require.Empty(t, out)
	out, err = f.Finish()
	require.Error(t, err)
	require.Empty(t, out)
}
func joinFrames(frames [][]byte) []byte {
	var out []byte
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

func TestStreamFilterRejectsUninspectedDeltaFields(t *testing.T) {
	p, err := newResponsePolicy(map[string]interface{}{"keywords": []string{"secret"}})
	require.NoError(t, err)
	for _, field := range []string{"reasoning", "refusal", "audio", "tool_calls"} {
		f := p.NewStreamFilter()
		out, err := f.Feed([]byte(`data: {"choices":[{"index":0,"delta":{"` + field + `":"secret"}}]}` + "\n\n"))
		require.Error(t, err, field)
		require.Empty(t, out)
	}
}

func TestResponseRegexAnchorsMatchVisibleText(t *testing.T) {
	for _, pattern := range []string{`^secret$`, `secret$`} {
		p, err := newResponsePolicy(map[string]interface{}{"patterns": []string{pattern}})
		require.NoError(t, err)
		result, err := p.ValidateResponse(`{"id":"metadata","choices":[{"message":{"role":"assistant","content":"secret"}}]}`)
		require.NoError(t, err)
		require.False(t, result.Passed)
		f := p.NewStreamFilter()
		_, err = f.Feed(streamEvent("secret"))
		require.NoError(t, err)
		_, err = f.Finish()
		require.Error(t, err)
	}
}

func TestResponseRegexWaitsForCompleteStream(t *testing.T) {
	p, err := newResponsePolicy(map[string]interface{}{"patterns": []string{`secret$`}})
	require.NoError(t, err)
	f := p.NewStreamFilter()
	out, err := f.Feed(streamEvent("secret"))
	require.NoError(t, err)
	require.Empty(t, out)
	out, err = f.Feed(streamEvent("ary"))
	require.NoError(t, err)
	require.Empty(t, out)
	out, err = f.Finish()
	require.NoError(t, err)
	require.NotEmpty(t, out)
}
