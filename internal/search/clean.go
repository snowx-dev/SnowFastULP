package search

// schemes are the URL scheme prefixes cleanLine strips from a hit line for
// display. Inlined from the former internal/output package (search was its
// only caller).
var schemes = []string{
	"https://",
	"http://",
	"ftp://",
	"ftps://",
	"sftp://",
	"ws://",
	"wss://",
}

// cleanLine removes URL scheme prefixes from a line for display. Nested
// schemes (a scheme appearing after an earlier strip) are removed too.
//
// Matching folds ASCII case only and scans the ORIGINAL bytes: scheme
// literals are ASCII, and multi-byte UTF-8 (Kelvin sign, dotted I, sharp S,
// CJK, emoji) must never shift byte offsets the way a strings.ToLower index
// source would — ToLower can change byte lengths per rune, corrupting the
// slice positions computed from it.
func cleanLine(line string) string {
	out := line
	for {
		changed := false
		for _, scheme := range schemes {
			if idx := indexASCIIFold(out, scheme); idx >= 0 {
				out = out[:idx] + out[idx+len(scheme):]
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return out
}

// indexASCIIFold returns the byte offset of the first ASCII-case-insensitive
// occurrence of literal in s, or -1. Every position is checked against the
// raw bytes of s, so non-ASCII bytes are simply never equal to ASCII
// literals regardless of their lowercased forms.
func indexASCIIFold(s, literal string) int {
	n := len(literal)
	for i := 0; i+n <= len(s); i++ {
		if asciiEqualFold(s[i:i+n], literal) {
			return i
		}
	}
	return -1
}

func asciiEqualFold(a, b string) bool {
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
