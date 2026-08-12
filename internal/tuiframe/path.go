package tuiframe

// TruncatePath keeps the end of a filesystem path when fitting a fixed column
// budget. Head-trim would hide the basename; summary frames should prefer
// outside-box footers instead of calling this.
func TruncatePath(p string, max int) string {
	if max < 8 {
		max = 8
	}
	// Count and slice on rune boundaries so a UTF-8 path (or the "▸" nested
	// separator) is never cut mid-rune into mojibake.
	r := []rune(p)
	if len(r) <= max {
		return p
	}
	return "…" + string(r[len(r)-(max-1):])
}
