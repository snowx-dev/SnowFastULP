package search

import "strings"

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
func cleanLine(line string) string {
	out := line
	for {
		changed := false
		for _, scheme := range schemes {
			if idx := strings.Index(strings.ToLower(out), scheme); idx >= 0 {
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
