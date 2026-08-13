//go:build !unix && !windows

package sflog

import "os"

func openReadNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
