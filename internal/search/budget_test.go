package search

import (
	"bytes"
	"runtime"
	"testing"
)

// allocBytes runs fn and returns the total bytes allocated during it
// (cumulative TotalAlloc delta — GC-free, unlike HeapAlloc deltas).
func allocBytes(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestMultiMatcherEachMatchUntilStopsEarly pins the breakable traversal: the
// callback stops the walk, no further occurrences are reported, and the
// callback count stays bounded regardless of how many occurrences remain.
func TestMultiMatcherEachMatchUntilStopsEarly(t *testing.T) {
	m := NewMultiMatcher([][]byte{[]byte("x")})
	hay := []byte("xxxxx\n") // 5 occurrences
	calls := 0
	m.EachMatchUntil(hay, func(idx, pos int) bool {
		calls++
		return calls < 3 // stop after the 3rd
	})
	if calls != 3 {
		t.Fatalf("callback count = %d, want 3 (traversal stopped at cap)", calls)
	}

	// full traversal reports every occurrence, same as EachMatch
	calls = 0
	m.EachMatchUntil(hay, func(idx, pos int) bool {
		calls++
		return true
	})
	if calls != 5 {
		t.Fatalf("full traversal callbacks = %d, want 5", calls)
	}

	// empty haystack / empty matcher never invoke the callback
	empty := NewMultiMatcher(nil)
	n := 0
	empty.EachMatchUntil([]byte("xxxxx"), func(int, int) bool { n++; return true })
	if n != 0 {
		t.Fatalf("empty matcher invoked callback %d times", n)
	}
}

// TestPatternRegionBudgetStopsScanning pins "enforce the cap while matching"
// under per-matching-line semantics: a budgeted scan on a fixed-size dense
// region allocates per-cap-hit bytes, never per-line bytes, and appends exactly
// budget hits. Uncapped, hits count LINES (not occurrences): a single 8 KiB
// line of 'x' — 8192 overlapping one-byte matches — emits exactly 1.
func TestPatternRegionBudgetStopsScanning(t *testing.T) {
	const lineLen = 8 << 10 // 8 KiB of 'x' on ONE line
	m := newPatternMatcher([]byte("x"))
	proc := patternRegion(&m)

	// one dense line = one hit, regardless of how many occurrences it holds
	dst, truncated := proc(nil, append(bytes.Repeat([]byte("x"), lineLen), '\n'), 0, -1)
	if len(dst) != 1 || truncated {
		t.Fatalf("dense single line: hits = %d truncated = %v, want 1/false", len(dst), truncated)
	}

	// 1024 dense 1-KiB lines: budget 10 stops at 10 hits with a real
	// truncation (further DISTINCT lines remain), allocating O(cap x line)
	// — not O(lines x line).
	const lines = 1024
	const shortLen = 1 << 10
	denseLine := append(bytes.Repeat([]byte("x"), shortLen), '\n')
	region := bytes.Repeat(denseLine, lines)

	var cappedHits, uncappedHits int
	capped := allocBytes(func() {
		dst, truncated := proc(nil, region, 0, 10)
		cappedHits = len(dst)
		if !truncated {
			t.Error("budget 10 with further dense lines remaining: truncated = false, want true")
		}
	})
	uncapped := allocBytes(func() {
		dst, truncated := proc(nil, region, 0, -1)
		uncappedHits = len(dst)
		if truncated {
			t.Error("unlimited scan reported truncation")
		}
	})

	if cappedHits != 10 {
		t.Fatalf("budgeted hits = %d, want 10", cappedHits)
	}
	if uncappedHits != lines {
		t.Fatalf("uncapped hits = %d, want %d (one per line)", uncappedHits, lines)
	}
	if capped > 1<<20 {
		t.Fatalf("budgeted allocations = %d bytes, want O(cap x line), not O(lines x line)", capped)
	}
	if uncapped < 64*capped {
		t.Fatalf("uncapped allocations %d bytes not proportional to line count vs capped %d", uncapped, capped)
	}
}

// TestPatternRegionBudgetTruncationMeansDistinctLine pins the truncation flag's
// meaning under line semantics: same-line repeats beyond the budget are NOT a
// truncation; a further DISTINCT matching line is.
func TestPatternRegionBudgetTruncationMeansDistinctLine(t *testing.T) {
	m := newPatternMatcher([]byte("k"))
	proc := patternRegion(&m)

	// budget 1, both occurrences on ONE line: the cap fills exactly, no
	// truncation.
	dst, truncated := proc(nil, []byte("kxk\n"), 0, 1)
	if len(dst) != 1 || truncated {
		t.Fatalf("same-line repeat: hits = %d truncated = %v, want 1/false", len(dst), truncated)
	}

	// budget 1 with a second matching LINE: real truncation.
	dst, truncated = proc(nil, []byte("kx\nkx\n"), 0, 1)
	if len(dst) != 1 {
		t.Fatalf("hits = %d, want 1 (budget)", len(dst))
	}
	if !truncated {
		t.Fatal("truncated = false with a further distinct line beyond budget, want true")
	}

	// exact boundary: distinct matching lines == budget is NOT a truncation.
	dst, truncated = proc(nil, []byte("kx\nkx\n"), 0, 2)
	if len(dst) != 2 || truncated {
		t.Fatalf("exact boundary: hits = %d truncated = %v, want 2/false", len(dst), truncated)
	}
}

// TestMultiPatternRegionBudgetStopsTraversal pins the cap semantics for the
// multi matcher under line semantics: one hit per (pattern, line); same-line
// repeats never truncate; a further distinct (pattern, line) beyond the budget
// does.
func TestMultiPatternRegionBudgetStopsTraversal(t *testing.T) {
	m := NewMultiMatcher([][]byte{[]byte("ab")})
	proc := multiPatternRegion(m)

	// 4 occurrences on ONE line: per-line dedupe emits 1; the repeats beyond
	// the budget are the same line — not a truncation.
	dst, truncated := proc(nil, []byte("ab ab ab ab\n"), 0, 1)
	if len(dst) != 1 || truncated {
		t.Fatalf("one line: hits = %d truncated = %v, want 1/false", len(dst), truncated)
	}

	// three matching lines, budget 2: 2 hits and a REAL truncation (a further
	// distinct line would have matched).
	dst, truncated = proc(nil, []byte("ab\nab x\nab y\n"), 0, 2)
	if len(dst) != 2 {
		t.Fatalf("hits = %d, want 2 (budget)", len(dst))
	}
	if !truncated {
		t.Fatal("truncated = false with a further distinct line beyond budget, want true")
	}

	// exact boundary: distinct matching lines == budget is NOT a truncation
	dst, truncated = proc(nil, []byte("ab\nab x\nab y\n"), 0, 3)
	if len(dst) != 3 || truncated {
		t.Fatalf("exact boundary: hits = %d truncated = %v, want 3/false", len(dst), truncated)
	}

	// budget 0 with a matching line present: nothing may be appended and the
	// first candidate is a real truncation (mirrors matchAllRegion).
	dst, truncated = proc(nil, []byte("ab ab\n"), 0, 0)
	if len(dst) != 0 || !truncated {
		t.Fatalf("budget 0: hits = %d truncated = %v, want 0/true", len(dst), truncated)
	}

	// budget 0 with NO matching line: not a truncation.
	dst, truncated = proc(nil, []byte("nothing here\n"), 0, 0)
	if len(dst) != 0 || truncated {
		t.Fatalf("no matches: hits = %d truncated = %v, want 0/false", len(dst), truncated)
	}
}

// TestMatchAllRegionBudgetStops pins the cap for the "*" path: a non-empty
// line beyond the budget is a real truncation; blank lines are not.
func TestMatchAllRegionBudgetStops(t *testing.T) {
	region := []byte("one\n\nthree\nfour\n")
	dst, truncated := matchAllRegion(nil, region, 0, 2)
	if len(dst) != 2 {
		t.Fatalf("hits = %d, want 2", len(dst))
	}
	if !truncated {
		t.Fatal("truncated = false with non-empty lines beyond budget, want true")
	}

	// budget fills exactly: no truncation
	dst, truncated = matchAllRegion(nil, []byte("one\ntwo\n"), 0, 2)
	if len(dst) != 2 || truncated {
		t.Fatalf("exact boundary: hits = %d truncated = %v, want 2/false", len(dst), truncated)
	}
}

// TestLineAssemblerBudgetCarriesAcrossCalls pins that the per-chunk budget
// spans every process call inside one feed (carry join + bulk region), not
// per region.
func TestLineAssemblerBudgetCarriesAcrossCalls(t *testing.T) {
	m := newPatternMatcher([]byte("k"))
	proc := patternRegion(&m)
	var a lineAssembler
	// carry join completes "xk" and the bulk line "kz\n" holds another match:
	// budget 1 must be shared across both calls.
	hits, truncated := a.feed(nil, []byte("xkz"), 0, proc, 0)
	_ = hits
	// no newline yet: nothing processed
	if truncated {
		t.Fatal("unterminated carry reported truncation")
	}

	var b lineAssembler
	hits, truncated = b.feed(nil, []byte("xk\nkz\n"), 0, proc, 1)
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1 (budget spans both regions)", len(hits))
	}
	if !truncated {
		t.Fatal("second match beyond budget not reported as truncation")
	}
}
