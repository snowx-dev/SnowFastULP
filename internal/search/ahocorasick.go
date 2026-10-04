package search

import "sort"

// MultiMatcher is an Aho-Corasick automaton over a fixed set of byte patterns.
// It finds all occurrences of every pattern in a haystack in a single pass,
// O(n + matches) over the input — the right tool when many patterns must be
// searched together over large archives, where looping one BMH matcher per
// pattern would repeat I/O. Empty patterns are dropped; identical patterns
// collapse to a single trie node carrying every matching index.
//
// Transitions are stored sparsely: each node keeps only its real outgoing
// edges, finalized into a compact CSR table (edgeOff/edgeData below). Memory
// stays proportional to the total pattern bytes instead of allocating a
// 256-slot table per node — 100k disjoint 25-byte patterns build in ~50 MiB
// rather than ~65 GiB of dense tables.
//
// PatternIdx returned from EachMatch is the index into the patterns slice
// passed to NewMultiMatcher (after empty-pattern skipping), so callers can
// route hits back to the originating term (e.g. a per-pattern output file).
type MultiMatcher struct {
	// Finalized CSR transition table. edgeData[edgeOff[node]:edgeOff[node+1]]
	// holds node's outgoing edges sorted by byte value; a node with k children
	// stores k entries. Edge lookups binary-search this sorted range.
	edgeOff  []int32 // len = nodeCount+1; node 0 is the root
	edgeData []edge

	fail    []int32 // failure link (longest proper suffix that is a trie prefix)
	termOff []int32 // len = nodeCount+1, mirroring edgeOff
	// termData[termOff[node]:termOff[node+1]] lists the pattern indices
	// ending at this node (including via the suffix closure) in ascending
	// order; a node can be terminal for several identical-length patterns,
	// and any prefix of a longer pattern that is itself a pattern is
	// reported via the suffix chain.
	termData []int

	patternLen []int // length of each original pattern, indexed by original idx

	numPatterns int // count of non-empty input patterns (distinct from termData entries)
}

// edge is one trie transition from a parent to a child on byte b.
type edge struct {
	b    byte
	node int32
}

// nodeNil is the absent-child sentinel. Node ids are 1..N (0 is root), so 0
// doubles as "no child" without a separate bitmap.
const nodeNil = 0

// acBuilder is the mutable construction state; it is finalized into the
// compact MultiMatcher representation and released before returning.
type acBuilder struct {
	// edges[node] holds node's outgoing edges, kept sorted by byte so
	// construction lookups binary-search instead of scanning (or pre-
	// allocating 256 slots per pattern byte). Slices are backed by edge
	// arena blocks: mostly non-sharing tries give nearly every node exactly
	// one edge, and per-node 1-element allocations dominate the build cost
	// otherwise.
	edges    [][]edge
	terminal [][]int // pattern indices ending exactly at each node (pre-closure)
	fail     []int32
	blocks   [][]edge
	blockUse int // next free slot in blocks[len(blocks)-1]
}

const edgeBlockSize = 512

// allocEdges returns an empty edge slice with capacity for n, carved from an
// arena block. The slice's cap is clipped to n, so appends stay inside the
// reserved range and never collide with a later reservation.
func (b *acBuilder) allocEdges(n int) []edge {
	if n > edgeBlockSize {
		b.blocks = append(b.blocks, make([]edge, n))
		return b.blocks[len(b.blocks)-1][:0:n]
	}
	if len(b.blocks) == 0 || edgeBlockSize-b.blockUse < n {
		b.blocks = append(b.blocks, make([]edge, edgeBlockSize))
		b.blockUse = 0
	}
	blk := b.blocks[len(b.blocks)-1]
	start := b.blockUse
	b.blockUse += n
	return blk[start : start : start+n]
}

// childOf finds node's outgoing edge on byte b via binary search.
func (b *acBuilder) childOf(node int32, by byte) (int32, bool) {
	return findEdge(b.edges[node], by)
}

// findEdge binary-searches a sorted edge slice for byte b.
func findEdge(edges []edge, b byte) (int32, bool) {
	lo, hi := 0, len(edges)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if edges[mid].b < b {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(edges) && edges[lo].b == b {
		return edges[lo].node, true
	}
	return nodeNil, false
}

// addEdge records node's transition on byte b to child, growing node's edge
// slice in sorted position (arena-backed; moving to a fresh arena only when
// the current one is exhausted).
func (b *acBuilder) addEdge(node int32, by byte, child int32) {
	es := b.edges[node]
	if len(es) == cap(es) {
		grown := b.allocEdges(2*len(es) + 1)
		// allocEdges returns a zero-length slice; reslice to the old length
		// BEFORE copying, or copy moves zero elements and the node loses
		// every edge it had (a zero edge then corrupts fail-chain walks).
		grown = grown[:len(es)]
		copy(grown, es)
		es = grown
	}
	es = es[:len(es)+1]
	lo := len(es) - 1
	for lo > 0 && es[lo-1].b > by {
		es[lo] = es[lo-1]
		lo--
	}
	es[lo] = edge{b: by, node: child}
	b.edges[node] = es
}

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
	totalBytes := 0
	for i, p := range patterns {
		if len(p) == 0 {
			continue
		}
		ks = append(ks, kept{idx: i, pat: append([]byte(nil), p...)})
		totalBytes += len(p)
	}

	// Trie node count is bounded by 1 + total non-empty pattern bytes
	// (deduped prefixes only shrink it). Pre-sizing the outer slices to that
	// upper bound avoids repeated growth copies on large pattern sets.
	maxNodes := 1 + totalBytes
	b := &acBuilder{
		edges:    make([][]edge, 1, maxNodes), // node 0 = root
		terminal: make([][]int, 1, maxNodes),
	}
	m := &MultiMatcher{
		patternLen:  make([]int, len(patterns)),
		numPatterns: len(ks),
	}
	for _, k := range ks {
		m.patternLen[k.idx] = len(k.pat)
	}

	// Insert every pattern, growing the trie. Each edge slice stays sorted;
	// a miss creates the child node lazily.
	for _, k := range ks {
		node := int32(0)
		for _, by := range k.pat {
			child, ok := b.childOf(node, by)
			if !ok {
				child = int32(len(b.edges))
				b.edges = append(b.edges, nil)
				b.terminal = append(b.terminal, nil)
				b.addEdge(node, by, child)
			}
			node = child
		}
		b.terminal[node] = append(b.terminal[node], k.idx)
	}
	if len(b.edges) == 1 {
		// Only the root exists: keep the finalized empty representation.
		m.edgeOff = []int32{0, 0}
		m.fail = []int32{0}
		m.termOff = []int32{0, 0}
		return m
	}

	// Failure links via BFS. The root's direct children fail to root; deeper
	// nodes fail along the longest proper suffix that is a trie prefix.
	// bfsOrder records every non-root node in discovery order (depth-ordered)
	// for the suffix-closure folding below; the queue itself is drained in
	// place by the BFS loop, so we keep a separate slice.
	b.fail = make([]int32, len(b.edges))
	queue := make([]int32, 0, len(b.edges))
	bfsOrder := make([]int32, 0, len(b.edges))
	for _, e := range b.edges[0] {
		b.fail[e.node] = 0
		queue = append(queue, e.node)
		bfsOrder = append(bfsOrder, e.node)
	}
	for qi := 0; qi < len(queue); qi++ {
		v := queue[qi]
		for _, e := range b.edges[v] {
			child := e.node
			bfsOrder = append(bfsOrder, child)
			// Walk failure chain of v (ending at root) to find the longest
			// suffix that has a child on byte e.b; that child is child's
			// fail link. The root itself must be checked too: fail(sh) is
			// root's 'h' child, not root.
			f := b.fail[v]
			found := int32(0)
			for {
				if fc, ok := b.childOf(f, e.b); ok {
					if fc != child {
						found = fc
					}
					break
				}
				if f == 0 {
					break
				}
				f = b.fail[f]
			}
			b.fail[child] = found
			queue = append(queue, child)
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
		f := b.fail[node]
		if f != 0 && len(b.terminal[f]) > 0 {
			extra := b.terminal[f]
			// Avoid double-counting if already present (dedup safety).
			// Terminal lists are tiny (pattern multiplicity), so a linear
			// scan beats allocating a set per node.
			for _, x := range extra {
				dup := false
				for _, y := range b.terminal[node] {
					if y == x {
						dup = true
						break
					}
				}
				if !dup {
					b.terminal[node] = append(b.terminal[node], x)
				}
			}
		}
	}
	// Keep terminal indices stable per node (EachMatch order is
	// deterministic across runs).
	for i := range b.terminal {
		sort.Ints(b.terminal[i])
	}

	// Finalize into the compact CSR representation and release the per-node
	// construction slices.
	m.fail = b.fail
	m.edgeOff = make([]int32, len(b.edges)+1)
	total := 0
	for i, es := range b.edges {
		m.edgeOff[i] = int32(total)
		total += len(es)
	}
	m.edgeOff[len(b.edges)] = int32(total)
	m.edgeData = make([]edge, 0, total)
	for _, es := range b.edges {
		m.edgeData = append(m.edgeData, es...)
	}
	m.termOff = make([]int32, len(b.terminal)+1)
	total = 0
	for i, ts := range b.terminal {
		m.termOff[i] = int32(total)
		total += len(ts)
	}
	m.termOff[len(b.terminal)] = int32(total)
	m.termData = make([]int, 0, total)
	for _, ts := range b.terminal {
		m.termData = append(m.termData, ts...)
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

// child finds node's outgoing edge on byte b via binary search over the
// finalized CSR table.
func (m *MultiMatcher) child(node int32, b byte) (int32, bool) {
	return findEdge(m.edgeData[m.edgeOff[node]:m.edgeOff[node+1]], b)
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
	m.EachMatchUntil(hay, func(patternIdx, pos int) bool {
		fn(patternIdx, pos)
		return true
	})
}

// EachMatchUntil is the breakable form of EachMatch: fn runs for every
// pattern occurrence until it returns false, which stops the traversal
// immediately (no further occurrences are reported). Use it when the caller
// only needs the first N occurrences — dense one-byte patterns on long lines
// would otherwise drive the walk (and per-occurrence callbacks) through the
// whole haystack.
func (m *MultiMatcher) EachMatchUntil(hay []byte, fn func(patternIdx, pos int) bool) {
	if m.numPatterns == 0 || len(hay) == 0 {
		return
	}
	node := int32(0)
	for i := range len(hay) {
		b := hay[i]
		// Follow children, walking the failure chain until a node with a
		// child on b is found (or root). This is the standard AC follow step.
		next, ok := m.child(node, b)
		for !ok && node != 0 {
			node = m.fail[node]
			next, ok = m.child(node, b)
		}
		if ok {
			node = next
		} else {
			node = 0
		}
		if node == 0 {
			continue
		}
		// Report every pattern ending at this node (incl. suffix-closure
		// patterns, already folded into the terminal table). The match
		// position is the start of the pattern, so subtract that pattern's
		// length from the current end position i; patternLen was stored once
		// at build time for that.
		terms := m.termData[m.termOff[node]:m.termOff[node+1]]
		for _, pIdx := range terms {
			start := i - m.patternLen[pIdx] + 1
			if start < 0 {
				continue
			}
			if !fn(pIdx, start) {
				return
			}
		}
	}
}
