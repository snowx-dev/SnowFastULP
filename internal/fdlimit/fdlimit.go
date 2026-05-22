// Package fdlimit reports the process-level fd cap (RLIMIT_NOFILE on Unix).
package fdlimit

// MaxOpenFiles returns the soft fd limit. ok=false on platforms w/o per-process fd accounting.
func MaxOpenFiles() (int, bool) { return platformMaxOpenFiles() }
