//go:build !windows

package selfupdate

// canonicalJournalPath is the non-Windows stub: paths have no 8.3 short-name
// spellings and the comparison stays case-sensitive (see sameJournalDir).
func canonicalJournalPath(path string) string { return path }

// spellingEqual is the non-Windows stub: file systems are case-sensitive.
func spellingEqual(a, b string) bool { return a == b }
