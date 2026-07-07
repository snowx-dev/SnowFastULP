// Package ulpengine is the shared ULP dedup + library-merge engine used by the
// sfu CLI and (in-process) by sfl. It was extracted verbatim from cmd/sfu's
// package main; behavior must remain identical to the pre-extraction sfu. Only
// the minimum API surface (Config, Resolved, Resolve, Run, Metrics, ...) is
// exported for the two command binaries to drive it.
package ulpengine
