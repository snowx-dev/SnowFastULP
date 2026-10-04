package search

import (
	"bytes"
	"math/rand/v2"
	"runtime"
	"testing"
)

// buildACPatterns returns n mostly non-sharing 25-byte patterns: an 8-byte
// unique big-endian counter prefix makes the tries near-disjoint (only the
// root is shared), and 17 trailing bytes from a 4-letter alphabet add the
// small amount of realistic suffix sharing.
func buildACPatterns(n int) [][]byte {
	pats := make([][]byte, n)
	for i := range pats {
		p := make([]byte, 25)
		p[0] = byte(i >> 24)
		p[1] = byte(i >> 16)
		p[2] = byte(i >> 8)
		p[3] = byte(i)
		for j := 8; j < 25; j++ {
			p[j] = byte('a' + rand.IntN(4))
		}
		pats[i] = p
	}
	return pats
}

// acLiveBytes measures the live heap delta across a build (both points
// GC'd), approximating the matcher's steady-state footprint.
func acLiveBytes(patterns [][]byte) (int64, *MultiMatcher) {
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	m := NewMultiMatcher(patterns)
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	return int64(after.HeapAlloc) - int64(before.HeapAlloc), m
}

// TestMultiMatcher100kPatternsMemory: 100,000 mostly non-sharing 25-byte
// patterns (2.5 MB of input, ~2.4M trie nodes/edges) must build into memory
// proportional to the input plus node/edge count — below 256 MiB. The dense
// [][]int32 children representation this pins out would need ~65 GiB of
// 256-slot tables; even a sparse-but-wasteful representation with per-node
// slice overheads lands near the ceiling. No timing assertion.
func TestMultiMatcher100kPatternsMemory(t *testing.T) {
	const n = 100000
	patterns := buildACPatterns(n)

	// Plant three identifiable probes for a correctness check at scale.
	probe1 := []byte("needleprobe-000000000000000") // 25 bytes
	probe2 := []byte("needleprobe-111111111111111")
	probe3 := []byte("needleprobe-222222222222222")
	patterns[3333] = probe1
	patterns[6666] = probe2
	patterns[9999] = probe3

	live, m := acLiveBytes(patterns)
	if m.NumPatterns() != n {
		t.Fatalf("NumPatterns = %d, want %d", m.NumPatterns(), n)
	}
	const limit = 256 << 20
	if live > limit {
		t.Fatalf("matcher live heap = %d bytes (%.1f MiB), want < %d (256 MiB)",
			live, float64(live)/(1<<20), limit)
	}
	t.Logf("100k×25B patterns: live heap after build+GC = %.1f MiB (limit %d MiB)",
		float64(live)/(1<<20), limit>>20)

	// Probes must still match at their exact offsets after the sparse
	// rewrite (hay: 10 x-separators between probes).
	hay := bytes.Repeat([]byte("x"), 10)
	hay = append(hay, probe1...)
	hay = append(hay, 'y')
	off2 := len(hay)
	hay = append(hay, probe2...)
	hay = append(hay, 'y')
	off3 := len(hay)
	hay = append(hay, probe3...)

	type hit struct{ idx, pos int }
	var got []hit
	m.EachMatch(hay, func(idx, pos int) { got = append(got, hit{idx, pos}) })
	want := []hit{{3333, 10}, {6666, off2}, {9999, off3}}
	if len(got) != len(want) {
		t.Fatalf("got %d matches %v, want %v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("match %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestMultiMatcherMemoryScalesLinearly: quadrupling the pattern set should
// roughly quadruple the footprint (generous 12x slack for allocator/GC
// noise), i.e. memory stays proportional to input + node/edge count.
func TestMultiMatcherMemoryScalesLinearly(t *testing.T) {
	small := buildACPatterns(25000)
	large := buildACPatterns(100000)
	l1, _ := acLiveBytes(small)
	l2, _ := acLiveBytes(large)
	if l1 <= 0 || l2 <= l1 {
		t.Fatalf("non-monotonic footprint: 25k=%d 100k=%d", l1, l2)
	}
	if ratio := float64(l2) / float64(l1); ratio > 12 {
		t.Fatalf("footprint not proportional: 25k=%d 100k=%d ratio=%.1f (want <= 12)",
			l1, l2, ratio)
	}
}

func BenchmarkMultiMatcher100kPatterns(b *testing.B) {
	patterns := buildACPatterns(100000)
	hay := bytes.Repeat([]byte("x"), 1<<20)
	copy(hay[1<<19:], patterns[50000])
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := NewMultiMatcher(patterns)
		count := 0
		m.EachMatch(hay, func(_, _ int) { count++ })
		if count != 1 {
			b.Fatalf("count = %d, want 1", count)
		}
	}
}
