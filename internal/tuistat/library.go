package tuistat

// Shared live-TUI copy for library work. sfu and sfl hit the same engine
// phases; these strings are the only user-facing names for those phases.
const (
	LibraryScanning  = "scanning library…"
	LibraryPreparing = "preparing library…"
	// sfu-only: names the one-time per-part re-index explicitly. sfl's TUI
	// and sfl's JSON status keep LibraryPreparing for the same phase.
	LibraryUpdating  = "updating library — one-time re-index"
	LibraryUpgrading = "upgrading library — do not interrupt"
	LibraryMerging   = "merging…"
)
