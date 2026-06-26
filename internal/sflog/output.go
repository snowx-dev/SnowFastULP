package sflog

import (
	"bufio"
	"context"
	"io"
)

// WriteULPLines formats and deduplicates a slice of credentials in one shot.
// Retained for direct/test use; the streaming Engine handles the production
// path with bounded memory.
func WriteULPLines(w io.Writer, creds []Credential, noURI bool) (WriteStats, error) {
	bw := bufio.NewWriter(w)
	defer bw.Flush()

	seen := make(map[string]struct{}, len(creds))
	var stats WriteStats
	for _, c := range creds {
		stats.Seen++
		line := FormatULPLine(c, noURI)
		if _, ok := seen[line]; ok {
			stats.Duplicates++
			continue
		}
		seen[line] = struct{}{}
		if _, err := bw.WriteString(line + "\n"); err != nil {
			return stats, err
		}
		stats.Emitted++
	}
	return stats, nil
}

// ExtractPathToWriter is the no-password convenience wrapper.
func ExtractPathToWriter(input string, w io.Writer, noURI bool) (ExtractStats, error) {
	return ExtractPathToWriterWithPasswords(input, w, noURI, []string{""})
}

// ExtractPathToWriterWithPasswords runs a single-worker extraction with no live
// progress, preserving the original synchronous API on top of the Engine.
func ExtractPathToWriterWithPasswords(input string, w io.Writer, noURI bool, passwords []string) (ExtractStats, error) {
	if len(passwords) == 0 {
		passwords = []string{""}
	}
	eng := &Engine{Workers: 1, NoURI: noURI, Passwords: passwords}
	stats, _, err := eng.Run(context.Background(), input, w)
	return stats, err
}
