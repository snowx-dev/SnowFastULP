package search

import "bytes"

const (
	asciiSetSize      = 256
	suffixProbeBytes  = 16 // below this width compare the whole pattern; above, probe the suffix first
)

// patternMatcher implements Boyes-Moore-Horspool search, matching zindex.cpp.
type patternMatcher struct {
	pat []byte
	bad [asciiSetSize]int
}

func newPatternMatcher(pattern []byte) patternMatcher {
	m := patternMatcher{pat: append([]byte(nil), pattern...)}
	buildBadCharTable(m.pat, m.bad[:])
	return m
}

func buildBadCharTable(pattern []byte, bad []int) {
	patLen := len(pattern)
	if patLen == 0 {
		return
	}
	for i := range bad {
		bad[i] = patLen
	}
	for i := 0; i+1 < patLen; i++ {
		bad[pattern[i]] = patLen - 1 - i
	}
}

// find returns the index of the first match of m.pat in hay, or -1.
func (m *patternMatcher) find(hay []byte) int {
	patLen := len(m.pat)
	hayLen := len(hay)
	if patLen == 0 || hayLen < patLen {
		return -1
	}

	i := 0
	for i <= hayLen-patLen {
		match := false
		if patLen <= suffixProbeBytes {
			match = bytes.Equal(hay[i:i+patLen], m.pat)
		} else if bytes.Equal(hay[i+patLen-suffixProbeBytes:i+patLen], m.pat[patLen-suffixProbeBytes:]) {
			j := 0
			for j+suffixProbeBytes < patLen && hay[i+j] == m.pat[j] {
				j++
			}
			match = j+suffixProbeBytes >= patLen
		}
		if match {
			return i
		}
		last := hay[i+patLen-1]
		shift := m.bad[last]
		if shift <= 0 {
			shift = 1
		}
		i += shift
	}
	return -1
}
