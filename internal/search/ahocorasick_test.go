package search

import (
	"bytes"
	"math/rand"
	"sort"
	"testing"
)

func TestMultiMatcherEmptyPatterns(t *testing.T) {
	m := NewMultiMatcher(nil)
	if m.NumPatterns() != 0 {
		t.Fatalf("NumPatterns = %d, want 0", m.NumPatterns())
	}
	var n int
	m.EachMatch([]byte("anything"), func(idx, pos int) { n++ })
	if n != 0 {
		t.Fatalf("empty matcher reported %d matches", n)
	}
}

func TestMultiMatcherSkipsEmptyPattern(t *testing.T) {
	m := NewMultiMatcher([][]byte{{}, []byte("foo"), {}})
	if m.NumPatterns() != 1 {
		t.Fatalf("NumPatterns = %d, want 1", m.NumPatterns())
	}
	var hits []int
	m.EachMatch([]byte("xfooy"), func(idx, pos int) { hits = append(hits, pos) })
	if len(hits) != 1 || hits[0] != 1 {
		t.Fatalf("hits = %v, want [1]", hits)
	}
}

func TestMultiMatcherSinglePatternParityWithBMH(t *testing.T) {
	pat := []byte("needle")
	mm := NewMultiMatcher([][]byte{pat})
	bm := newPatternMatcher(pat)
	hay := []byte("alpha needle beta needle gamma")
	var acPos []int
	mm.EachMatch(hay, func(idx, pos int) { acPos = append(acPos, pos) })
	// BMH finds occurrences by advancing pos+1 after each match.
	var bmPos []int
	off := 0
	for off+len(pat) <= len(hay) {
		rel := bm.find(hay[off:])
		if rel < 0 {
			break
		}
		bmPos = append(bmPos, off+rel)
		off = off + rel + 1
	}
	if len(acPos) != len(bmPos) {
		t.Fatalf("count mismatch: AC=%v BMH=%v", acPos, bmPos)
	}
	for i := range acPos {
		if acPos[i] != bmPos[i] {
			t.Fatalf("pos %d: AC=%d BMH=%d", i, acPos[i], bmPos[i])
		}
	}
}

func TestMultiMatcherMultiplePatterns(t *testing.T) {
	m := NewMultiMatcher([][]byte{[]byte("he"), []byte("she"), []byte("his"), []byte("hers")})
	hay := []byte("ushers") // u(0) s(1) h(2) e(3) r(4) s(5)
	type match struct{ idx, pos int }
	var got []match
	m.EachMatch(hay, func(idx, pos int) { got = append(got, match{idx, pos}) })
	// "she" at 1 (s=1,h=2,e=3) -> idx1 pos1
	// "he"  at 2 (h=2,e=3)      -> idx0 pos2
	// "hers" at 2 (h=2,e=3,r=4,s=5) -> idx3 pos2
	// "his" none
	want := []match{{1, 1}, {0, 2}, {3, 2}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	sort.Slice(got, func(i, j int) bool {
		if got[i].pos != got[j].pos {
			return got[i].pos < got[j].pos
		}
		return got[i].idx < got[j].idx
	})
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestMultiMatcherSubstringPatterns(t *testing.T) {
	// "ab" is a prefix-suffix of "abc"; both should fire on "abc".
	m := NewMultiMatcher([][]byte{[]byte("ab"), []byte("abc")})
	hay := []byte("xabc")
	var got [][2]int
	m.EachMatch(hay, func(idx, pos int) { got = append(got, [2]int{idx, pos}) })
	// "abc" at 1 (idx1), "ab" at 1 (idx0)
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 matches", got)
	}
	seenIdx0, seenIdx1 := false, false
	for _, g := range got {
		if g[1] != 1 {
			t.Fatalf("pos %d, want 1", g[1])
		}
		if g[0] == 0 {
			seenIdx0 = true
		}
		if g[0] == 1 {
			seenIdx1 = true
		}
	}
	if !seenIdx0 || !seenIdx1 {
		t.Fatalf("missing pattern indices: %v", got)
	}
}

func TestMultiMatcherDuplicatePatterns(t *testing.T) {
	m := NewMultiMatcher([][]byte{[]byte("foo"), []byte("foo"), []byte("foo")})
	if m.NumPatterns() != 3 {
		t.Fatalf("NumPatterns = %d, want 3 (duplicates kept as separate indices)", m.NumPatterns())
	}
	var count int
	m.EachMatch([]byte("foo"), func(idx, pos int) { count++ })
	if count != 3 {
		t.Fatalf("got %d matches for 3 identical patterns, want 3", count)
	}
}

func TestNumPatternsDoesNotOvercountSubstrings(t *testing.T) {
	// "he" is a substring of "she"; suffix-closure folds "he" into the
	// "she" node's terminal list. NumPatterns must still report 3 (the
	// input count), not 4 (which a naive sum of terminal lengths would).
	m := NewMultiMatcher([][]byte{[]byte("he"), []byte("she"), []byte("hers")})
	if m.NumPatterns() != 3 {
		t.Fatalf("NumPatterns = %d, want 3", m.NumPatterns())
	}
}

func TestMultiMatcherNoMatches(t *testing.T) {
	m := NewMultiMatcher([][]byte{[]byte("xyz"), []byte("qqq")})
	var n int
	m.EachMatch([]byte("abcdefghijklmnop"), func(idx, pos int) { n++ })
	if n != 0 {
		t.Fatalf("expected 0 matches, got %d", n)
	}
}

func TestMultiMatcherRandomizedCrossCheck(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 300; trial++ {
		npatterns := 1 + rng.Intn(6)
		patterns := make([][]byte, npatterns)
		for i := range patterns {
			plen := 1 + rng.Intn(5)
			p := make([]byte, plen)
			for j := range p {
				p[j] = byte('a' + rng.Intn(3)) // small alphabet → frequent overlaps
			}
			patterns[i] = p
		}
		hayLen := rng.Intn(60)
		hay := make([]byte, hayLen)
		for i := range hay {
			hay[i] = byte('a' + rng.Intn(3))
		}

		m := NewMultiMatcher(patterns)
		type ref struct{ idx, pos int }
		var want []ref
		for i, p := range patterns {
			if len(p) == 0 {
				continue
			}
			off := 0
			for off+len(p) <= len(hay) {
				rel := bytes.Index(hay[off:], p)
				if rel < 0 {
					break
				}
				want = append(want, ref{i, off + rel})
				off = off + rel + 1
			}
		}
		// Sort reference by position then idx for stable compare.
		sort.Slice(want, func(i, j int) bool {
			if want[i].pos != want[j].pos {
				return want[i].pos < want[j].pos
			}
			return want[i].idx < want[j].idx
		})

		var got []ref
		m.EachMatch(hay, func(idx, pos int) { got = append(got, ref{idx, pos}) })
		sort.Slice(got, func(i, j int) bool {
			if got[i].pos != got[j].pos {
				return got[i].pos < got[j].pos
			}
			return got[i].idx < got[j].idx
		})

		if len(got) != len(want) {
			t.Fatalf("trial %d: patterns=%v hay=%q\ngot  %v\nwant %v", trial, patterns, hay, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("trial %d: patterns=%v hay=%q\nmismatch at %d: got %v want %v", trial, patterns, hay, i, got[i], want[i])
			}
		}
	}
}
