// Package textenc decodes leak-log text that is frequently produced on Windows:
// UTF-16 (LE/BE, with a BOM) from stealer families like RedLine/Vidar, and
// UTF-8 with a BOM from Notepad-class tooling. It exposes a reader-based wrapper
// so any io.Reader (a zip member, a rar/7z stream, a loose file) can be parsed
// as UTF-8 regardless of its on-disk encoding.
package textenc

import (
	"bufio"
	"io"

	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// Encoding is the detected source encoding of a byte stream.
type Encoding int

const (
	UTF8 Encoding = iota
	UTF16LE
	UTF16BE
)

func (e Encoding) String() string {
	switch e {
	case UTF16LE:
		return "utf-16-le"
	case UTF16BE:
		return "utf-16-be"
	default:
		return "utf-8"
	}
}

// Sniff classifies a stream from its first few bytes and reports how many BOM
// bytes to skip. head may be shorter than 3 bytes (short files).
func Sniff(head []byte) (enc Encoding, bomLen int) {
	if len(head) >= 2 {
		if head[0] == 0xff && head[1] == 0xfe {
			return UTF16LE, 2
		}
		if head[0] == 0xfe && head[1] == 0xff {
			return UTF16BE, 2
		}
	}
	if len(head) >= 3 && head[0] == 0xef && head[1] == 0xbb && head[2] == 0xbf {
		return UTF8, 3
	}
	return UTF8, 0
}

// WrapReader returns an io.Reader that yields UTF-8 text from r regardless of
// r's encoding: it peeks the BOM (without consuming non-BOM bytes), drops it,
// and transcodes UTF-16LE/BE to UTF-8. A plain UTF-8 (or BOM-less) stream is
// returned essentially unchanged. The returned reader is single-use and not
// safe across goroutines; wrap a fresh reader per source.
func WrapReader(r io.Reader) io.Reader {
	// bufio.Peek lets us inspect the BOM and then either Discard it (BOM
	// present) or leave every byte in place (no BOM), so non-BOM content is
	// never lost.
	br := bufio.NewReader(r)
	head, _ := br.Peek(3) // short read is fine; Sniff tolerates < 3 bytes
	enc, bomLen := Sniff(head)
	if bomLen > 0 {
		_, _ = br.Discard(bomLen)
	}
	switch enc {
	case UTF16LE:
		return transform.NewReader(br, unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder())
	case UTF16BE:
		return transform.NewReader(br, unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM).NewDecoder())
	default:
		return br
	}
}
