// Package pathident reports whether two paths refer to the same on-disk file.
// uses os.SameFile, catches `./x` vs `x`, case folding, hardlink/symlink aliases
package pathident

import "os"

// SameFile reports whether a and b point at the same file.
// returns (false, nil) if either doesnt exist, for preflight checks
func SameFile(a, b string) (bool, error) {
	infoA, errA := os.Stat(a)
	if errA != nil {
		if os.IsNotExist(errA) {
			return false, nil
		}
		return false, errA
	}
	infoB, errB := os.Stat(b)
	if errB != nil {
		if os.IsNotExist(errB) {
			return false, nil
		}
		return false, errB
	}
	return os.SameFile(infoA, infoB), nil
}
