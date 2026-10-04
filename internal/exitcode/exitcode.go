// Package exitcode defines the shared process exit-code policy for the
// SnowFastMerge CLIs (sfl, sfu, sfs). One coherent contract so scripts can
// branch on a single set of codes across tools:
//
//	0   clean run: every source succeeded or was skipped by history; results
//	    are complete (for sfs, the documented deliberate -l hit cap counts as
//	    complete).
//	1   hard runtime/environment error (unopenable input, I/O failure, ...).
//	2   usage error: bad flags, missing input path, invalid configuration.
//	3   partial failure: the run produced results but some sources failed, or
//	    results were truncated (-max-hits-per-chunk).
//	4   nothing usable: every discovered source failed (with nothing written),
//	    or nothing was discovered at all from the given input. sfl refines the
//	    all-failed arm: a run whose salvage still wrote usable output (a
//	    truncated multi-volume RAR whose parts decoded before the gap) is 3.
//	130 interrupted by SIGINT.
//	143 interrupted by SIGTERM (conventional 128+signal distinction).
//
// Tool-specific mappings are documented in each binary's -h output. sfu has no
// per-source failure model: line-level rejects are normal noise (exit 0 while
// some lines parsed); a run that parses zero lines out of the lines it read,
// or discovers no inputs at all (no .txt files under the input path, aligned
// with sfl), is a total failure (4).
package exitcode

const (
	// Clean is a fully successful run: exit 0.
	Clean = 0
	// Error is a hard runtime/environment error: exit 1.
	Error = 1
	// Usage is an invocation/flag error: exit 2.
	Usage = 2
	// Partial reports incomplete results: some sources failed or results
	// were truncated: exit 3.
	Partial = 3
	// NothingUsable reports that no usable result was produced: every source
	// failed or nothing was discovered: exit 4.
	NothingUsable = 4
	// Interrupted is a SIGINT shutdown: exit 130.
	Interrupted = 130
	// Terminated is a SIGTERM shutdown: exit 143 (128+15), the conventional
	// shell distinction from a Ctrl-C.
	Terminated = 143
)
