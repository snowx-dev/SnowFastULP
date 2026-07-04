//go:build vectorscan

package secrets

// MatcherBackend names the regexp engine titus linked at build time. The
// vectorscan tag selects titus's libhs (Hyperscan/Vectorscan) matcher —
// hardware-speed, CGO. sfl's TUI uses this to stay quiet: no warning when the
// fast engine is in use.
const MatcherBackend = "libhs"
