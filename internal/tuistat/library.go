package tuistat

// Shared live-TUI copy for library work. sfu and sfl hit the same engine
// phases; these strings are the only user-facing names for those phases.
const (
	LibraryScanning  = "scanning library…"
	LibraryPreparing = "preparing library…"
	LibraryUpgrading = "upgrading library — do not interrupt"
	LibraryMerging   = "merging…"
)
