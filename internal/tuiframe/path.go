package tuiframe

// TruncatePath keeps the end of a filesystem path when fitting a fixed column
// budget. Head-trim would hide the basename; summary frames should prefer
// outside-box footers instead of calling this.
//
// Every positive budget is honored (no artificial minimum-width floor that
// would overflow tight columns); max <= 0 yields "".
func TruncatePath(p string, max int) string {
	return TruncateLeft(p, max)
}
