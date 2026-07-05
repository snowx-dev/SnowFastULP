// Package version exposes the build identifier embedded in each binary.
// release builds override via `-ldflags "-X .../version.String=<v>"`.
// MUST stay a var (not const) for -ldflags -X to work.
package version

// String is the version banner. edit only on a real source bump.
var String = "0.2-dev"
