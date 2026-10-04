//go:build !linux

package search_test

func procArchiveFDCount(dir string) int { return 0 }
