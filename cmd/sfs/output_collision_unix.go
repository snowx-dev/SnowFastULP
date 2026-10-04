//go:build !windows

package main

func pathsLookEqual(a, b string) bool { return a == b }
