package ulpengine

import (
	"bufio"
	"io"
)

const (
	maxParsedLineLen = 4096
	// CRLF/LF framing around max record. parse() does the post-trim check.
	maxInputLineBytes = maxParsedLineLen + 2
)

// one \n-delimited record, no unbounded mem on missing newlines.
// over maxLen, drain to delim and report tooLong
func readBoundedLine(br *bufio.Reader, maxLen int) (line string, consumed int64, tooLong bool, err error) {
	if maxLen < 0 {
		maxLen = 0
	}

	var out []byte
	for {
		frag, rerr := br.ReadSlice('\n')
		consumed += int64(len(frag))

		if !tooLong {
			if len(out)+len(frag) > maxLen {
				tooLong = true
				out = nil
			} else {
				out = append(out, frag...)
			}
		}

		switch rerr {
		case nil:
			if tooLong {
				return "", consumed, true, nil
			}
			return string(out), consumed, false, nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if consumed == 0 {
				return "", 0, false, io.EOF
			}
			if tooLong {
				return "", consumed, true, nil
			}
			return string(out), consumed, false, nil
		default:
			return "", consumed, tooLong, rerr
		}
	}
}
