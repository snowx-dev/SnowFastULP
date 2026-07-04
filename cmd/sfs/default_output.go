package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type outputMode struct {
	OutFile   string
	Stream    bool
	Generated bool
}

func streamRequested(streamFlag, silentAlias bool) bool {
	return streamFlag || silentAlias
}

func resolveOutputMode(requestedOut string, stream bool, cwd string, started time.Time) (outputMode, error) {
	if requestedOut != "" {
		return outputMode{OutFile: requestedOut}, nil
	}
	if stream {
		return outputMode{Stream: true}, nil
	}
	outFile, err := defaultOutputPath(cwd, started)
	if err != nil {
		return outputMode{}, err
	}
	return outputMode{OutFile: outFile, Generated: true}, nil
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

// defaultSecretsExportPath allocates a non-colliding sfs_secrets_<stamp>.txt in
// cwd for the post-search "export secrets only" offer, mirroring
// defaultOutputPath's collision-avoidance so repeated runs never overwrite.
func defaultSecretsExportPath(cwd string, now time.Time) (string, error) {
	if cwd == "" {
		return "", fmt.Errorf("resolve secrets export: empty cwd")
	}
	stamp := now.Format("20060102-1504")
	base := filepath.Join(cwd, "sfs_secrets_"+stamp+".txt")
	if available, err := pathAvailable(base); err != nil {
		return "", err
	} else if available {
		return base, nil
	}
	for i := 2; ; i++ {
		candidate := filepath.Join(cwd, fmt.Sprintf("sfs_secrets_%s_%d.txt", stamp, i))
		if available, err := pathAvailable(candidate); err != nil {
			return "", err
		} else if available {
			return candidate, nil
		}
	}
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
