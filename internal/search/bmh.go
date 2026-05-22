package search

import "bytes"

const asciiSetSize = 256

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

func bytesEqual(a, b []byte) bool {
	return bytes.Equal(a, b)
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
		if patLen <= 16 {
			match = bytesEqual(hay[i:i+patLen], m.pat)
		} else if bytesEqual(hay[i+patLen-16:i+patLen], m.pat[patLen-16:]) {
			j := 0
			for j+16 < patLen && hay[i+j] == m.pat[j] {
				j++
			}
			match = j+16 >= patLen
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
