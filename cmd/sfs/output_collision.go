package main

import (
	"fmt"
	"path/filepath"

	"github.com/snowx-dev/SnowFastULP/internal/pathident"
)

// rejects -o clobbering a search target. output opens w/ O_TRUNC pre-scan
// so collisions must be caught early.
// identity via pathident.SameFile (dev/inode unix, handle info windows),
// nonexistent out falls back to clean+abs compare (case-fold on windows)
func ensureNoOutputCollision(outFile string, archives []string) error {
	// No TrimSpace: paths are byte-preserved (P5-W8); only "" is the empty
	// flag, whitespace is a literal file name.
	if outFile == "" {
		return nil
	}
	absOut, err := filepath.Abs(outFile)
	if err != nil {
		return fmt.Errorf("resolve -o: %w", err)
	}
	absOut = filepath.Clean(absOut)
	for _, arch := range archives {
		absArch, err := filepath.Abs(arch)
		if err != nil {
			continue
		}
		absArch = filepath.Clean(absArch)
		if same, err := pathident.SameFile(absOut, absArch); err == nil && same {
			return fmt.Errorf("-o would clobber a search target: %s", absArch)
		}
		if pathsLookEqual(absOut, absArch) {
			return fmt.Errorf("-o would clobber a search target: %s", absArch)
		}
	}
	return nil
}

// rejectJSONOutPreflightCollisions refuses a -json file target that names
// an input the stream would destroy BEFORE the general -o/root checks can
// run: the -f patterns file (read once, destroyed by the stream), the config
// file, the per-pattern outputs -f mode is about to create (allocation
// truncates them before the general checks), and the debug log the run will
// create in CWD. No filesystem writes; runs before ensureFileOutputDir and
// allocatePatternFiles so a rejected invocation leaves nothing behind.
func rejectJSONOutPreflightCollisions(target string, protected ...string) error {
	if target == "" || target == "-" {
		return nil
	}
	tgt, err := pathident.CanonicalProspective(target)
	if err != nil {
		return fmt.Errorf("invalid -json target %q: %w", target, err)
	}
	for _, p := range protected {
		if p == "" {
			continue
		}
		// Candidates that do not exist yet (per-pattern outputs) compare by
		// canonical spelling; existing files (patterns/config) also compare
		// by identity, so link aliases cannot slip through.
		if c, cerr := pathident.CanonicalProspective(p); cerr == nil && c == tgt {
			return fmt.Errorf("invalid -json target %q: overlaps %q", target, p)
		}
		if same, serr := pathident.SameFile(tgt, p); serr == nil && same {
			return fmt.Errorf("invalid -json target %q: overlaps %q", target, p)
		}
		// The target's final component may itself be a symlink — possibly
		// dangling, since a prospective per-pattern output does not exist
		// yet. Both compares above pass then (the canonical spelling of the
		// target is the link path itself), and the stream's create follows
		// the link onto the referent. Compare the referent chain against
		// every protected path too (same treatment as
		// history.JSONOutHistoryCollision).
		if pathident.LinkRefersTo(target, p) {
			return fmt.Errorf("invalid -json target %q: overlaps %q", target, p)
		}
	}
	return nil
}

// rejectJSONOutTargetCollisions refuses a -json file target that equals
// the hit output file or is (or lives under) the search root: the stream
// truncates its target before the scan starts, so either overlap would
// destroy data. No filesystem writes; runs before the stream file and every
// output file is opened, so a rejected invocation leaves nothing behind.
func rejectJSONOutTargetCollisions(target, outFile, root string) error {
	if target == "" || target == "-" {
		return nil
	}
	tgt, err := pathident.CanonicalProspective(target)
	if err != nil {
		return fmt.Errorf("invalid -json target %q: %w", target, err)
	}
	if outFile != "" {
		// CanonicalProspective on both sides: the -o file may not exist yet
		// (a generated default or a not-yet-created -o), so identity falls
		// back to the canonical spellings.
		oc, oerr := pathident.CanonicalProspective(outFile)
		same, serr := pathident.SameFile(tgt, outFile)
		if (oerr == nil && oc == tgt) || (serr == nil && same) {
			return fmt.Errorf("invalid -json target %q: overlaps -o output %q", target, outFile)
		}
		// Same dangling-link escape as the preflight: a link spelling the
		// not-yet-created -o file passes the compares above, but the
		// stream's create follows the chain onto the referent.
		if pathident.LinkRefersTo(target, outFile) {
			return fmt.Errorf("invalid -json target %q: overlaps -o output %q", target, outFile)
		}
	}
	if root != "" {
		rc, rerr := pathident.CanonicalProspective(root)
		if rerr == nil && (rc == tgt || pathident.WithinDir(tgt, rc)) {
			return fmt.Errorf("invalid -json target %q: overlaps search root %q", target, root)
		}
		// A dangling link from outside the root whose referent lands inside
		// it would have the stream create a file the scan then sees
		// mid-write; the plain WithinDir compare cannot see that landing.
		if pathident.LinkRefersWithinDir(target, root) {
			return fmt.Errorf("invalid -json target %q: overlaps search root %q", target, root)
		}
	}
	return nil
}
