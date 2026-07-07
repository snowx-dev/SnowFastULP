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

// processFn appends to dst the hits found in region — a slice of one or more
// complete '\n'-terminated lines whose first byte is at file offset regionOff.
type processFn func(dst []localHit, region []byte, regionOff int64) []localHit

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
func (a *lineAssembler) feed(dst []localHit, text []byte, baseOff int64, process processFn) []localHit {
	// Finish dropping an over-long line begun in an earlier feed.
	if a.skip {
		i := bytes.IndexByte(text, '\n')
		if i < 0 {
			return dst // still inside the over-long line
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
			return a.extendCarry(dst, text, baseOff, process) // line still open
		}
		a.work = append(a.work[:0], a.carry...)
		a.work = append(a.work, text[:i+1]...)
		dst = process(dst, a.work, a.off)
		a.carry = a.carry[:0]
		text = text[i+1:]
		baseOff += int64(i + 1)
	}

	// Scan the complete-lines region of text in place; carry the trailing tail.
	lastNL := bytes.LastIndexByte(text, '\n')
	if lastNL < 0 {
		return a.extendCarry(dst, text, baseOff, process)
	}
	dst = process(dst, text[:lastNL+1], baseOff)
	return a.extendCarry(dst, text[lastNL+1:], baseOff+int64(lastNL+1), process)
}

// extendCarry appends more (a run containing no newline) to the pending partial
// line. If the line would exceed maxLineBytes it force-emits the first
// maxLineBytes as a truncated line and arms skip mode to drop the rest until the
// next newline — bounding carry memory on pathological newline-free input.
func (a *lineAssembler) extendCarry(dst []localHit, more []byte, moreOff int64, process processFn) []localHit {
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
		dst = process(dst, head, a.off)
		a.carry = a.carry[:0]
		a.skip = true
		return dst
	}
	a.carry = append(a.carry, more...)
	return dst
}

// flush finalizes the trailing partial line (which has no terminating newline) at
// chunk/file EOF, synthesizing a terminator so process can treat it as a line.
func (a *lineAssembler) flush(dst []localHit, process processFn) []localHit {
	defer func() {
		a.carry = a.carry[:0]
		a.skip = false
	}()
	if a.skip || len(a.carry) == 0 {
		return dst
	}
	a.work = append(a.work[:0], a.carry...)
	a.work = append(a.work, '\n')
	return process(dst, a.work, a.off)
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
func matchAllRegion(dst []localHit, region []byte, regionOff int64) []localHit {
	start := 0
	for i := 0; i < len(region); i++ {
		if region[i] == '\n' {
			if line := lineFromRange(region, start, i); line != "" {
				dst = append(dst, localHit{offset: regionOff + int64(start), line: line})
			}
			start = i + 1
		}
	}
	return dst
}

// patternRegion returns a processFn that emits one hit per pattern occurrence in
// region, each carrying the complete line containing the match. Offsets are the
// true byte position of the match (regionOff + match index).
func patternRegion(matcher *patternMatcher) processFn {
	patLen := len(matcher.pat)
	return func(dst []localHit, region []byte, regionOff int64) []localHit {
		rlen := len(region)
		if patLen == 0 || rlen < patLen {
			return dst
		}
		offset := 0
		for offset+patLen <= rlen {
			rel := matcher.find(region[offset:rlen])
			if rel < 0 {
				break
			}
			pos := offset + rel
			if line := extractLine(region, rlen, pos); line != "" {
				dst = append(dst, localHit{offset: regionOff + int64(pos), line: line})
			}
			offset = pos + 1
		}
		return dst
	}
}
