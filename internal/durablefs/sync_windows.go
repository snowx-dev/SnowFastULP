//go:build windows

package durablefs

// syncDirectory is a no-op on Windows. Directory-entry durability of synced
// history outputs is not guaranteed on FAT/exFAT; NTFS metadata journaling
// makes the no-op acceptable. Portable directory FlushFileBuffers is not
// reliable across those file systems, while regular output files are still
// synced for real by File.Sync.
func syncDirectory(string) error { return nil }
