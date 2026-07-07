package search

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestBMHFindShortPattern(t *testing.T) {
	m := newPatternMatcher([]byte("needle"))
	hay := []byte("alpha needle beta")
	if got := m.find(hay); got != 6 {
		t.Fatalf("find = %d, want 6", got)
	}
}

func TestBMHFindLongPatternSuffixFirst(t *testing.T) {
	pat := bytes.Repeat([]byte("x"), 20)
	pat[19] = 'Z'
	m := newPatternMatcher(pat)
	hay := append(bytes.Repeat([]byte("a"), 30), pat...)
	hay = append(hay, bytes.Repeat([]byte("b"), 10)...)
	if got := m.find(hay); got != 30 {
		t.Fatalf("find = %d, want 30", got)
	}
}

func TestBMHMatchesBytesIndex(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 200; trial++ {
		patLen := 1 + rng.Intn(64)
		hayLen := patLen + rng.Intn(512)
		pat := make([]byte, patLen)
		hay := make([]byte, hayLen)
		for i := range pat {
			pat[i] = byte('a' + rng.Intn(26))
		}
		for i := range hay {
			hay[i] = byte('a' + rng.Intn(26))
		}
		if rng.Intn(4) == 0 {
			start := rng.Intn(hayLen - patLen + 1)
			copy(hay[start:], pat)
		}

		m := newPatternMatcher(pat)
		want := bytes.Index(hay, pat)
		got := m.find(hay)
		if got != want {
			t.Fatalf("trial %d: BMH=%d bytes.Index=%d pat=%q", trial, got, want, pat)
		}
	}
}

func TestPatternRegionMultipleMatches(t *testing.T) {
	m := newPatternMatcher([]byte("ab"))
	region := []byte("xxab yyab zz\n")
	hits := patternRegion(&m)(nil, region, 0)
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	if hits[0].offset != 2 || hits[1].offset != 7 {
		t.Fatalf("offsets = %d,%d want 2,7", hits[0].offset, hits[1].offset)
	}
}

func TestPatternRegionReusesBackingArray(t *testing.T) {
	m := newPatternMatcher([]byte("ab"))
	region := []byte("ab cd ab\n")
	dst := make([]localHit, 0, 8)
	headAddr := &dst[:cap(dst)][0]
	dst = patternRegion(&m)(dst, region, 0)
	if len(dst) != 2 {
		t.Fatalf("len = %d, want 2", len(dst))
	}
	if &dst[:cap(dst)][0] != headAddr {
		t.Fatal("patternRegion reallocated within capacity; should reuse caller slice")
	}
}
