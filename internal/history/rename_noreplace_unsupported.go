//go:build !linux && !darwin && !windows

package history

import "errors"

func renameNoReplace(string, string) error {
	return errors.New("history: atomic no-replace rename is unavailable on this platform")
}
