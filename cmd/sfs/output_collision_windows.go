//go:build windows

package main

import "strings"

// NTFS is case-insensitive, mirror that for the nonexistent-out branch
func pathsLookEqual(a, b string) bool { return strings.EqualFold(a, b) }
