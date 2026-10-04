//go:build !unix

package fdlimit

func platformMaxOpenFiles() (int, bool) { return 0, false }
