package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type outputMode struct {
	OutFile   string
	Stream    bool
	Stats     bool
	Generated bool
}

// resolveOutputMode picks hit destination from -stats / -json / -o.
// Default (stats=false, jsonOut=false): stream to stdout; with -o, tee stdout+file.
// Stats: file-only (auto sfs_results_*.txt or explicit -o) for the live TUI path.
// jsonOut: stdout belongs to the NDJSON stream, so hits go file-only with
// the same shapes as -stats — but only to an EXPLICIT -o: main refuses
// json without -o before this runs, and no default sfs_results_*.txt is
// ever generated for the stream (the error return keeps the generation path
// unreachable even if a caller forgets). The caller passes false in -f -o
// DIR mode, where the per-term files already own the hits.
func resolveOutputMode(requestedOut string, stats, jsonOut bool, cwd string, started time.Time) (outputMode, error) {
	if jsonOut {
		if requestedOut == "" {
			return outputMode{}, errors.New("-json requires an explicit -o output")
		}
		return outputMode{OutFile: requestedOut, Stats: true}, nil
	}
	if stats {
		if requestedOut != "" {
			return outputMode{OutFile: requestedOut, Stats: true}, nil
		}
		outFile, err := defaultOutputPath(cwd, started)
		if err != nil {
			return outputMode{}, err
		}
		return outputMode{OutFile: outFile, Generated: true, Stats: true}, nil
	}
	if requestedOut != "" {
		return outputMode{OutFile: requestedOut, Stream: true}, nil
	}
	return outputMode{Stream: true}, nil
}

func defaultOutputPath(cwd string, started time.Time) (string, error) {
	if cwd == "" {
		return "", fmt.Errorf("resolve default output: empty cwd")
	}
	stamp := started.Format("20060102-1504")
	base := filepath.Join(cwd, "sfs_results_"+stamp+".txt")
	if available, err := pathAvailable(base); err != nil {
		return "", err
	} else if available {
		return base, nil
	}
	for i := 2; i < 1000; i++ {
		candidate := filepath.Join(cwd, fmt.Sprintf("sfs_results_%s_%d.txt", stamp, i))
		if available, err := pathAvailable(candidate); err != nil {
			return "", err
		} else if available {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not allocate unique default output path under %s", cwd)
}

func pathAvailable(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return false, nil
	}
	if os.IsNotExist(err) {
		return true, nil
	}
	return false, fmt.Errorf("check output path %q: %w", path, err)
}

// finalizeEmptyOutput discards a generated-default output file that received
// zero hits so a fruitless search never litters CWD with a 0-byte
// sfs_results_*.txt, and returns what to display as the summary's output:
// "(no matches)" when the empty file was removed, otherwise the path unchanged.
// An explicit -o is always preserved (generated=false). removed reports whether
// the file was unlinked, so the caller can log it.
func finalizeEmptyOutput(outFile string, generated bool, hits int64) (summaryOut string, removed bool) {
	if generated && hits == 0 {
		_ = os.Remove(outFile)
		return "(no matches)", true
	}
	return outFile, false
}
