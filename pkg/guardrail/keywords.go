package guardrail

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

type acNode struct {
	next  map[rune]int
	fail  int
	match bool
}
type keywordMatcher struct {
	nodes   []acNode
	longest int
}

// Immutable after construction, so the same automaton is safe across requests.
func compileKeywords(words []string) (*keywordMatcher, error) {
	if len(words) > 10000 {
		return nil, fmt.Errorf("keyword dictionary exceeds 10000 entries")
	}
	m := &keywordMatcher{nodes: []acNode{{next: map[rune]int{}}}}
	total := 0
	for _, word := range words {
		word = strings.ToLower(word)
		length := utf8.RuneCountInString(word)
		total += len(word)
		if length == 0 || length > 2048 || total > 1<<20 {
			return nil, fmt.Errorf("keyword dictionary exceeds size limits")
		}
		if length > m.longest {
			m.longest = length
		}
		state := 0
		for _, r := range word {
			next, ok := m.nodes[state].next[r]
			if !ok {
				next = len(m.nodes)
				m.nodes = append(m.nodes, acNode{next: map[rune]int{}})
				m.nodes[state].next[r] = next
			}
			state = next
		}
		m.nodes[state].match = true
	}
	queue := make([]int, 0, len(m.nodes))
	for _, n := range m.nodes[0].next {
		queue = append(queue, n)
	}
	for i := 0; i < len(queue); i++ {
		state := queue[i]
		for r, next := range m.nodes[state].next {
			fail := m.nodes[state].fail
			for fail != 0 {
				if _, ok := m.nodes[fail].next[r]; ok {
					break
				}
				fail = m.nodes[fail].fail
			}
			if target, ok := m.nodes[fail].next[r]; ok {
				m.nodes[next].fail = target
			}
			m.nodes[next].match = m.nodes[next].match || m.nodes[m.nodes[next].fail].match
			queue = append(queue, next)
		}
	}
	return m, nil
}
func (m *keywordMatcher) matches(text string) bool {
	if m == nil {
		return false
	}
	state := 0
	for _, r := range strings.ToLower(text) {
		for state != 0 {
			if _, ok := m.nodes[state].next[r]; ok {
				break
			}
			state = m.nodes[state].fail
		}
		if next, ok := m.nodes[state].next[r]; ok {
			state = next
		}
		if m.nodes[state].match {
			return true
		}
	}
	return false
}

func validIDChecksum(id string) bool {
	if len(id) != 18 {
		return false
	}
	weights := []int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	sum := 0
	for i := 0; i < 17; i++ {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
		sum += int(id[i]-'0') * weights[i]
	}
	return strings.ToUpper(id[17:]) == string("10X98765432"[sum%11])
}
