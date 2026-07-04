//go:build !vectorscan

package secrets

// MatcherBackend names the regexp engine titus linked at build time. Without
// the vectorscan tag titus falls back to its pure-Go regex matcher — portable
// (no C lib) but much slower on large dumps. sfl's TUI uses this to surface a
// tasteful "build with -tags vectorscan + libhs for speed" warning in the live
// header so users don't unknowingly run the slow engine.
const MatcherBackend = "go-regex"
