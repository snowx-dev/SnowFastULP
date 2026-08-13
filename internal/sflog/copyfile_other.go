//go:build !unix

package sflog

import "os"

func openReadNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
