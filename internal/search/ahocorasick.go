package search

import (
	"sort"
)

// MultiMatcher is an Aho-Corasick automaton over a fixed set of byte patterns.
// It finds all occurrences of every pattern in a haystack in a single pass,
// O(n + matches) over the input — the right tool when many patterns must be
// searched together over large archives, where looping one BMH matcher per
// pattern would repeat I/O. Empty patterns are dropped; identical patterns
// collapse to a single trie node carrying every matching index.
//
// PatternIdx returned from EachMatch is the index into the patterns slice
// passed to NewMultiMatcher (after empty-pattern skipping), so callers can
// route hits back to the originating term (e.g. a per-pattern output file).
type MultiMatcher struct {
	// Compact trie: node 0 is the root. children are keyed by byte value;
	// fail/suffix links are precomputed by build(). terminal lists the
	// pattern indices that end at each node (a node can be terminal for
	// several identical-length patterns, and any prefix of a longer pattern
	// that is itself a pattern is reported via the suffix chain).
	children    [][]int32 // [node][byte] -> child node id, or 0 = none
	fail        []int32   // failure link (longest proper suffix that is a trie prefix)
	terminal    [][]int   // pattern indices ending at this node (incl. via suffix)
	patternLen  []int     // length of each original pattern, indexed by original idx
	numPatterns int       // count of non-empty input patterns (distinct from terminal entries)
}

// nodeNil is the absent-child sentinel. Node ids are 1..N (0 is root), so 0
// doubles as "no child" without a separate bitmap.
const nodeNil = 0

// NewMultiMatcher builds an Aho-Corasick automaton from patterns. Empty
// patterns are skipped (a zero-length pattern would match everywhere and is
// never useful). Duplicate patterns are deduped on the trie; EachMatch
// reports each surviving pattern's index once per occurrence.
func NewMultiMatcher(patterns [][]byte) *MultiMatcher {
	// Collect non-empty patterns, remembering the original index each
	// survivor maps to so EachMatch callers can route hits back to the
	// user-facing term.
	type kept struct {
		idx int
		pat []byte
	}
	var ks []kept
	for i, p := range patterns {
		if len(p) == 0 {
			continue
		}
		ks = append(ks, kept{idx: i, pat: append([]byte(nil), p...)})
	}
	if len(ks) == 0 {
		return &MultiMatcher{
			children:    [][]int32{{}},
			fail:        []int32{0},
			terminal:    [][]int{nil},
			patternLen:  make([]int, len(patterns)),
			numPatterns: 0,
		}
	}

	m := &MultiMatcher{
		children:    [][]int32{{}}, // root
		terminal:    [][]int{nil},
		patternLen:  make([]int, len(patterns)),
		numPatterns: len(ks),
	}
	for _, k := range ks {
		m.patternLen[k.idx] = len(k.pat)
	}

	// Insert every pattern, growing the trie. children[node] is a 256-slot
	// table allocated lazily so a sparse trie stays small.
	insert := func(pat []byte, patIdx int) {
		node := int32(0)
		for _, b := range pat {
			if int(node) >= len(m.children) {
				// Should not happen: node ids are assigned sequentially.
				panic("ahocorasick: node out of range")
			}
			if len(m.children[node]) == 0 {
				m.children[node] = make([]int32, 256)
				for i := range m.children[node] {
					m.children[node][i] = nodeNil
				}
			}
			child := m.children[node][b]
			if child == nodeNil {
				child = int32(len(m.children))
				m.children = append(m.children, make([]int32, 256))
				for i := range m.children[child] {
					m.children[child][i] = nodeNil
				}
				m.terminal = append(m.terminal, nil)
				m.children[node][b] = child
			}
			node = child
		}
		m.terminal[node] = append(m.terminal[node], patIdx)
	}
	for _, k := range ks {
		insert(k.pat, k.idx)
	}

	// Failure links via BFS. The root's direct children fail to root; deeper
	// nodes fail along the longest proper suffix that is a trie prefix.
	// bfsOrder records every non-root node in discovery order (depth-ordered)
	// for the suffix-closure folding below; the queue itself is drained by the
	// BFS loop, so we keep a separate slice.
	m.fail = make([]int32, len(m.children))
	type qe struct{ node int32 }
	queue := make([]qe, 0, len(m.children))
	bfsOrder := make([]int32, 0, len(m.children))
	for b := 0; b < 256; b++ {
		child := m.children[0][b]
		if child != nodeNil {
			m.fail[child] = 0
			queue = append(queue, qe{child})
			bfsOrder = append(bfsOrder, child)
		}
	}
	for len(queue) > 0 {
		v := queue[0].node
		queue = queue[1:]
		for b := 0; b < 256; b++ {
			child := m.children[v][b]
			if child == nodeNil {
				continue
			}
			bfsOrder = append(bfsOrder, child)
			// Walk failure chain of v to find the longest suffix that has a
			// child on byte b; that child is child's fail link.
			f := m.fail[v]
			for f != 0 && m.children[f][b] == nodeNil {
				f = m.fail[f]
			}
			if m.children[f][b] != nodeNil && m.children[f][b] != child {
				m.fail[child] = m.children[f][b]
			} else {
				m.fail[child] = 0
			}
			queue = append(queue, qe{child})
		}
	}

	// Precompute the suffix-terminal closure: terminal[node] should report
	// not only patterns ending at node, but also patterns ending at any node
	// reachable through its failure chain. We fold those in once, walking
	// nodes in BFS discovery order (bfsOrder is depth-ordered, so a node's
	// fail target — always shallower — has its closure complete by the time
	// we process it). This lets EachMatch do a single slice read per node,
	// no chain walking on the hot path.
	for _, node := range bfsOrder {
		f := m.fail[node]
		if f != 0 && len(m.terminal[f]) > 0 {
			extra := m.terminal[f]
			// Avoid double-counting if already present (dedup safety).
			seen := make(map[int]bool, len(m.terminal[node]))
			for _, x := range m.terminal[node] {
				seen[x] = true
			}
			for _, x := range extra {
				if !seen[x] {
					m.terminal[node] = append(m.terminal[node], x)
				}
			}
		}
	}
	// Keep terminal indices stable per node (EachMatch order is
	// deterministic across runs).
	for i := range m.terminal {
		sort.Ints(m.terminal[i])
	}

	return m
}

// NumPatterns reports how many non-empty patterns were registered. A matcher
// built from all-empty input matches nothing. This is the count of distinct
// input patterns, NOT the sum of terminal entries (a substring pattern
// appears in its own node and in deeper nodes' suffix closures, so summing
// terminal lengths would overcount).
func (m *MultiMatcher) NumPatterns() int {
	return m.numPatterns
}

// EachMatch walks hay once, invoking fn for every pattern occurrence. fn
// receives the pattern's original index (per NewMultiMatcher) and the byte
// offset of the start of the match. Matches are reported as their ending
// position advances through hay (left to right); at a single ending
// position, multiple patterns are reported in ascending patternIdx order.
// Start positions across matches of different lengths are therefore not
// strictly monotonic — callers that need start-order sorting must sort the
// results themselves. fn may be invoked many times; it must not retain hay.
func (m *MultiMatcher) EachMatch(hay []byte, fn func(patternIdx, pos int)) {
	if len(m.children) <= 1 || len(hay) == 0 {
		return
	}
	node := int32(0)
	for i := 0; i < len(hay); i++ {
		b := hay[i]
		// Follow children, walking the failure chain until a node with a
		// child on b is found (or root). This is the standard AC follow step.
		for node != 0 && m.children[node][b] == nodeNil {
			node = m.fail[node]
		}
		next := m.children[node][b]
		if next != nodeNil {
			node = next
		} else {
			node = 0
		}
		if node == 0 {
			continue
		}
		// Report every pattern ending at this node (incl. suffix-closure
		// patterns, already folded into terminal[node]). The match position
		// is the start of the pattern, so subtract that pattern's length from
		// the current end position i. We need the per-pattern length to do
		// that; store it once at build time.
		terms := m.terminal[node]
		if len(terms) == 0 {
			continue
		}
		for _, pIdx := range terms {
			plen := m.patternLen[pIdx]
			start := i - plen + 1
			if start < 0 {
				continue
			}
			fn(pIdx, start)
		}
	}
}

// patternLen stores the byte length of each original pattern index so
// EachMatch can recover the match start position. Indexed by the original
// pattern index (the same index reported to fn).
