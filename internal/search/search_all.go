package search

// appendAllLines extracts newline-terminated lines from text, appending to dst.
// A partial line tail is stored in carry for the next call; baseOff is the file
// offset of text[0].
func appendAllLines(dst []localHit, carry *[]byte, carryOff *int64, text []byte, baseOff int64) []localHit {
	data := text
	off := baseOff
	if len(*carry) > 0 {
		merged := make([]byte, 0, len(*carry)+len(text))
		merged = append(merged, (*carry)...)
		merged = append(merged, text...)
		data = merged
		off = *carryOff
		*carry = (*carry)[:0]
	}
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			if line := lineFromRange(data, start, i); line != "" {
				dst = append(dst, localHit{offset: off + int64(start), line: line})
			}
			start = i + 1
		}
	}
	if start < len(data) {
		*carry = append((*carry)[:0], data[start:]...)
		*carryOff = off + int64(start)
	}
	return dst
}

// flushLineCarry emits a trailing partial line at chunk/file EOF.
func flushLineCarry(dst []localHit, carry []byte, carryOff int64) []localHit {
	if len(carry) == 0 {
		return dst
	}
	if line := lineFromRange(carry, 0, len(carry)); line != "" {
		dst = append(dst, localHit{offset: carryOff, line: line})
	}
	return dst
}

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
