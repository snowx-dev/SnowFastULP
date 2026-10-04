package search

import "bytes"

// maxLineBytes caps a single assembled line. A line longer than this with no
// newline (pathological or binary input) is matched on its first maxLineBytes
// and then truncated, the remainder skipped to the next newline. This bounds the
// carry so streaming search stays low-RAM even on adversarial input.
//
// It must be comfortably larger than the read step (outWin) so a normal long
// line that merely spans a couple of reads is assembled whole before any
// truncation — only genuinely pathological newline-free runs hit the cap.
const maxLineBytes = 4 << 20

// lineAsmState is the portable snapshot of a lineAssembler's matching state
// (H-10): the trailing partial line (carry), its absolute offset, and the
// over-long-line skip flag. Chunks of one archive may be scanned by different
// workers in any order, so chunk k+1's assembler is seeded with chunk k's
// final state — lines that a concatenated zstd frame split mid-line survive
// the chunk boundary. The reusable `work` scratch is intentionally not part
// of the state.
type lineAsmState struct {
	carry []byte
	off   int64
	skip  bool
}

// state copies the assembler's portable state out. The carry is duplicated:
// the assembler keeps reusing (and clearing) its backing array.
func (a *lineAssembler) state() lineAsmState {
	s := lineAsmState{off: a.off, skip: a.skip}
	if len(a.carry) > 0 {
		s.carry = append([]byte(nil), a.carry...)
	}
	return s
}

// restore seeds the assembler from a previous chunk's final state.
func (a *lineAssembler) restore(s lineAsmState) {
	a.carry = append(a.carry[:0], s.carry...)
	a.off = s.off
	a.skip = s.skip
}

// processFn appends to dst the hits found in region — a slice of one or more
// complete '\n'-terminated lines whose first byte is at file offset regionOff.
//
// budget bounds how many NEW hits this call may append: <0 = unlimited
// (MaxHitsPerChunk 0), 0 = none, n>0 = at most n. The second return reports a
// REAL truncation: the budget ran out while at least one more match existed in
// the region. Callers fold it into the chunk's capped flag so OnChunkCapped
// fires exactly when a hit would have been discarded, and never at the exact
// cap boundary. Enforcement happens while matching (the scan stops at the
// cap), so a dense pattern on a huge line never materializes per-occurrence
// line strings beyond the cap.
type processFn func(dst []localHit, region []byte, regionOff int64, budget int) (hits []localHit, truncated bool)

// lineAssembler stitches complete lines across decode-step / read seams without
// seeking, so a matched line is never truncated at a buffer boundary. It is the
// single line-assembly mechanism shared by the pattern and match-all paths, in
// both the compressed (searchChunk) and plain-text (searchTxtFile) searchers.
//
// feed() hands `process` only COMPLETE, newline-terminated lines; the trailing
// partial line is carried to the next feed(). flush() finalizes the last partial
// line at EOF. Because matching happens on whole lines, a pattern straddling a
// seam needs no special overlap handling.
type lineAssembler struct {
	carry []byte // bytes after the last '\n' seen, awaiting their terminator
	off   int64  // absolute file offset of carry[0]
	skip  bool   // carry overflowed maxLineBytes; dropping until the next '\n'
	work  []byte // reusable carry+text scratch
}

// feed runs process over the complete lines in text, retaining the trailing
// partial line for the next call. The bulk of text is scanned in place (passed
// straight to process); only the small join that completes a carried partial
// line is copied, so feed does not duplicate the read buffer per step.
//
// budget is the remaining per-chunk hit allowance for this text (same
// semantics as processFn). Consumption within one feed is tracked as dst
// growth, and each process call receives the still-unspent remainder — once
// the cap is spent, later calls run in report-only mode (no appends, no
// per-occurrence work, bounded lookahead) purely to keep the truncation flag
// exact. The returned bool aggregates process truncation across all calls.
func (a *lineAssembler) feed(dst []localHit, text []byte, baseOff int64, process processFn, budget int) ([]localHit, bool) {
	truncated := false
	len0 := len(dst)

	// Finish dropping an over-long line begun in an earlier feed.
	if a.skip {
		i := bytes.IndexByte(text, '\n')
		if i < 0 {
			return dst, false // still inside the over-long line
		}
		a.skip = false
		text = text[i+1:]
		baseOff += int64(i + 1)
	}

	// Complete a carried partial line with this buffer's head (up to and
	// including its first newline). Only this small join is copied.
	if len(a.carry) > 0 {
		i := bytes.IndexByte(text, '\n')
		if i < 0 {
			dst, tr := a.extendCarry(dst, text, baseOff, process, budget)
			return dst, truncated || tr // line still open
		}
		a.work = append(a.work[:0], a.carry...)
		a.work = append(a.work, text[:i+1]...)
		var tr bool
		dst, tr = process(dst, a.work, a.off, budget)
		truncated = truncated || tr
		a.carry = a.carry[:0]
		text = text[i+1:]
		baseOff += int64(i + 1)
	}

	// Scan the complete-lines region of text in place; carry the trailing tail.
	lastNL := bytes.LastIndexByte(text, '\n')
	if lastNL < 0 {
		dst, tr := a.extendCarry(dst, text, baseOff, process, budget-len(dst)+len0)
		return dst, truncated || tr
	}
	dst, tr := process(dst, text[:lastNL+1], baseOff, budget-len(dst)+len0)
	truncated = truncated || tr
	dst, tr = a.extendCarry(dst, text[lastNL+1:], baseOff+int64(lastNL+1), process, budget-len(dst)+len0)
	return dst, truncated || tr
}

// extendCarry appends more (a run containing no newline) to the pending partial
// line. If the line would exceed maxLineBytes it force-emits the first
// maxLineBytes as a truncated line and arms skip mode to drop the rest until the
// next newline — bounding carry memory on pathological newline-free input.
// budget flows into the force-emit process call (see feed).
func (a *lineAssembler) extendCarry(dst []localHit, more []byte, moreOff int64, process processFn, budget int) ([]localHit, bool) {
	if len(a.carry) == 0 {
		a.off = moreOff
	}
	if len(a.carry)+len(more) >= maxLineBytes {
		head := make([]byte, 0, maxLineBytes+1)
		head = append(head, a.carry...)
		if take := maxLineBytes - len(head); take > 0 {
			head = append(head, more[:take]...)
		}
		head = append(head, '\n')
		// H-07: the capped head is emitted exactly ONCE. Clear the carry and
		// arm skip mode so the rest of the over-long line is dropped until the
		// next newline; keeping the stale carry here made every following
		// read (and EOF) re-process and re-emit the same prefix.
		a.skip = true
		a.carry = a.carry[:0]
		return process(dst, head, a.off, budget)
	}
	a.carry = append(a.carry, more...)
	return dst, false
}

// flush finalizes the trailing partial line (which has no terminating newline) at
// chunk/file EOF, synthesizing a terminator so process can treat it as a line.
func (a *lineAssembler) flush(dst []localHit, process processFn, budget int) ([]localHit, bool) {
	defer func() {
		a.carry = a.carry[:0]
		a.skip = false
	}()
	if a.skip || len(a.carry) == 0 {
		return dst, false
	}
	a.work = append(a.work[:0], a.carry...)
	a.work = append(a.work, '\n')
	return process(dst, a.work, a.off, budget)
}

// lineFromRange returns data[start:end] with a trailing '\r' stripped, or "" if
// the range is empty.
func lineFromRange(data []byte, start, end int) string {
	if end <= start {
		return ""
	}
	for end > start && data[end-1] == '\r' {
		end--
	}
	if end <= start {
		return ""
	}
	return string(data[start:end])
}

// matchAllRegion emits every non-empty line in region (the "*" pattern).
func matchAllRegion(dst []localHit, region []byte, regionOff int64, budget int) ([]localHit, bool) {
	start := 0
	appended := 0
	for i := range len(region) {
		if region[i] != '\n' {
			continue
		}
		if line := lineFromRange(region, start, i); line != "" {
			if budget >= 0 && appended >= budget {
				// this non-empty line would have been a hit, so the cap
				// discards it: a real truncation. stop scanning.
				return dst, true
			}
			dst = append(dst, localHit{offset: regionOff + int64(start), line: line})
			appended++
		}
		start = i + 1
	}
	return dst, false
}

// patternRegion returns a processFn that emits one hit per MATCHING LINE in
// region (grep semantics), each carrying the complete line containing the
// match. The first hit of a match span carries the FIRST occurrence's offset
// (regionOff + match index); a span covering several lines (a pattern with a
// raw '\n' via raw argv) emits one hit per covered line, tail hits carrying
// the position where the span enters that line. The scan then resumes after
// the LAST line of the span, so single-line matches never revisit their line
// and tail lines are never skipped.
//
// Once the budget is spent the scan stops appending; one bounded lookahead
// find decides whether a further match exists (real truncation) or not. A
// same-line repeat beyond the budget is structurally unreachable: the
// advance already skipped past every line the emitted spans covered.
func patternRegion(matcher *patternMatcher) processFn {
	patLen := len(matcher.pat)
	return func(dst []localHit, region []byte, regionOff int64, budget int) ([]localHit, bool) {
		rlen := len(region)
		if patLen == 0 || rlen < patLen {
			return dst, false
		}
		appended := 0
		offset := 0
		for offset+patLen <= rlen {
			if budget >= 0 && appended >= budget {
				// Budget spent: report-only mode. The scan always
				// advances past the last line of each emitted span, so
				// any further match lies on a line that would have
				// emitted — a real truncation.
				return dst, matcher.find(region[offset:rlen]) >= 0
			}
			rel := matcher.find(region[offset:rlen])
			if rel < 0 {
				break
			}
			pos := offset + rel
			spanEnd := pos + patLen
			// Emit one hit per line the span covers. The first line's hit
			// carries pos (first occurrence); a tail line of a multi-line
			// span carries its line start, where the span enters it.
			lineStart := pos
			if i := bytes.LastIndexByte(region[:pos], '\n'); i >= 0 {
				lineStart = i + 1
			}
			for lineStart < spanEnd {
				if budget >= 0 && appended >= budget {
					// The span's remaining lines are distinct lines
					// that would have emitted: real truncation.
					return dst, true
				}
				nl := bytes.IndexByte(region[lineStart:], '\n')
				lineEnd := rlen
				if nl >= 0 {
					lineEnd = lineStart + nl + 1
				}
				hitPos := pos
				if lineStart > pos {
					hitPos = lineStart
				}
				// extractLine may return "" (e.g. a bare-"\r" line):
				// the hit still emits and the loop still advances past
				// the line, so nothing stalls or skips.
				dst = append(dst, localHit{offset: regionOff + int64(hitPos), line: extractLine(region, lineEnd, hitPos)})
				appended++
				if nl < 0 {
					lineStart = rlen
				} else {
					lineStart += nl + 1
				}
			}
			offset = lineStart
		}
		return dst, false
	}
}

// multiPatternRegion returns a processFn that runs a MultiMatcher over the
// region and emits one localHit per (pattern, matching LINE), tagged with
// patternIdx so the drain loop can route hits to per-pattern output files —
// grep semantics, mirroring patternRegion: a line matching one pattern twice
// emits ONE hit (at the FIRST occurrence's offset), while a line matching two
// different patterns emits two hits. Dedupe is keyed by the LINE'S BYTE START
// OFFSET (not its text): two identical line texts in one region are distinct
// lines and both emit. extractLine runs once per emitted hit.
//
// Once the budget is spent the traversal continues in report-only mode (no
// appends, cheap offset bookkeeping per occurrence) so the truncation flag
// stays exact: it fires only when a further DISTINCT (pattern, line) pair
// exists — a same-line repeat of an already-hit pattern is not a truncation.
// The budget check before the matcher walk also preserves matchAllRegion's
// budget-0 shape: with no budget and any match present, truncated fires.
func multiPatternRegion(matcher *MultiMatcher) processFn {
	return func(dst []localHit, region []byte, regionOff int64, budget int) ([]localHit, bool) {
		rlen := len(region)
		if rlen == 0 {
			return dst, false
		}
		if budget == 0 {
			// No allowance: any match present is a real truncation.
			truncated := false
			matcher.EachMatchUntil(region, func(int, int) bool {
				truncated = true
				return false
			})
			return dst, truncated
		}
		appended := 0
		truncated := false
		// last[pIdx] = line-start offset of the line pIdx last emitted on
		// (-1 = none). An Aho-Corasick walk reports matches left to right by
		// end position, so within one line every pattern's occurrences arrive
		// before the walk reaches the next line: a one-slot-per-pattern
		// memory is exact, and occurrence work between emitted lines costs
		// one int comparison.
		last := make([]int64, matcher.numPatterns)
		for i := range last {
			last[i] = -1
		}
		matcher.EachMatchUntil(region, func(pIdx, pos int) bool {
			lineStart := int64(bytes.LastIndexByte(region[:pos], '\n') + 1)
			if lineStart == last[pIdx] {
				return true // same pattern, same line: already emitted
			}
			last[pIdx] = lineStart
			if budget > 0 && appended >= budget {
				truncated = true
				return false
			}
			if line := extractLine(region, rlen, pos); line != "" {
				dst = append(dst, localHit{
					// first occurrence's true byte position on this line
					offset:     regionOff + int64(pos),
					line:       line,
					patternIdx: pIdx,
				})
				appended++
			}
			return true
		})
		return dst, truncated
	}
}
