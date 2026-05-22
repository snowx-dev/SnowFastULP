package output

import (
	"strings"
)

var schemes = []string{
	"https://",
	"http://",
	"ftp://",
	"ftps://",
	"sftp://",
	"ws://",
	"wss://",
}

// CleanLine removes URL scheme prefixes from a line for display.
func CleanLine(line string) string {
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
