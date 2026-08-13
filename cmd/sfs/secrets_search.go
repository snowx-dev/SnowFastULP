package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/search"
	"github.com/snowx-dev/SnowFastULP/internal/secrets"
)

const secretsDBName = "sfl-secrets.sqlite"

// secretsSearchArgs carries the resolved CLI inputs for a `sfs -sec` run.
type secretsSearchArgs struct {
	root        string // positional DIR (or [sfs].dir); the DB default lives here
	pattern     string // type filter; "*" or "" => all rows
	secretsPath string // -sec-path override
	since       string // raw -since window ("" => no lower bound)
	limit       int    // -l cap (0 => unlimited)
	outFile     string // -o (explicit); "" when streaming/default
	clean       bool   // -clean
}

// secretsUnsupportedFlags lists archive-search options that do not apply to
// -sec. stats/txt are hard errors; the rest are stderr notes.
type secretsFlagCheck struct {
	Stats           bool
	Txt             bool
	WorkersSet      bool
	DecodeStepSet   bool
	MaxHitsChunkSet bool
}

// checkSecretsFlags rejects -stats/-txt with -sec and notes other unused flags.
func checkSecretsFlags(c secretsFlagCheck) (warns []string, err error) {
	if c.Stats {
		return nil, fmt.Errorf("-stats is not supported with -sec (use -o FILE for file-only output)")
	}
	if c.Txt {
		return nil, fmt.Errorf("-txt is not supported with -sec")
	}
	if c.WorkersSet {
		warns = append(warns, "note: -j/-workers is ignored with -sec")
	}
	if c.DecodeStepSet {
		warns = append(warns, "note: -decode-step is ignored with -sec")
	}
	if c.MaxHitsChunkSet {
		warns = append(warns, "note: -max-hits-per-chunk is ignored with -sec")
	}
	return warns, nil
}

// runSecretsSearch answers a `sfs -sec` query: it reads the secrets DB
// (read-only) and writes matching rows. When streaming to a real terminal
// (no -o, stdout a TTY) the rows render as a styled table; a pipe or explicit
// -o keeps the grep/cut-friendly TSV (-o is file-only, never tees).
func runSecretsSearch(a secretsSearchArgs) error {
	opts, err := buildSecretsQueryOpts(a)
	if err != nil {
		return err
	}
	dbPath := resolveSecretsDBPath(a.secretsPath, a.root)
	if a.outFile == "" && stdoutIsTTY() {
		return runSecretsTable(dbPath, opts)
	}
	return runSecretsTSV(a, dbPath, opts)
}

// runSecretsTSV is the plain output path: an explicit -o file, or streaming TSV
// to a piped stdout. Reuses the ULP search writer so -clean matches archive search.
func runSecretsTSV(a secretsSearchArgs, dbPath string, opts secrets.QueryOpts) error {
	out, w, err := openSecretsOutput(a.outFile)
	if err != nil {
		return err
	}
	sink := search.NewWriter(w, a.clean)
	n, qerr := secrets.QueryDB(dbPath, opts, func(m secrets.Match) error {
		return sink.WriteHit(search.Hit{Line: formatSecretLine(m)})
	})
	if ferr := sink.Flush(); ferr != nil && qerr == nil {
		qerr = ferr
	}
	if out != nil {
		if cerr := out.Close(); cerr != nil && qerr == nil {
			qerr = cerr
		}
	}
	if qerr != nil {
		return qerr
	}
	printSecretsSummary(n, a.outFile, a.outFile == "")
	return nil
}

func buildSecretsQueryOpts(a secretsSearchArgs) (secrets.QueryOpts, error) {
	opts := secrets.QueryOpts{Limit: a.limit}
	if a.pattern != "*" {
		opts.Type = a.pattern
	}
	if a.since != "" {
		dur, err := parseSince(a.since)
		if err != nil {
			return secrets.QueryOpts{}, err
		}
		opts.Since = time.Now().Add(-dur)
	}
	return opts, nil
}

// openSecretsOutput returns a file handle (if any) and the hit writer.
// Explicit -o is file-only (no stdout tee); empty path streams to stdout.
func openSecretsOutput(outFile string) (*os.File, io.Writer, error) {
	if outFile == "" {
		return nil, os.Stdout, nil
	}
	if dir := filepath.Dir(outFile); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("create output dir: %w", err)
		}
	}
	f, err := os.Create(outFile)
	if err != nil {
		return nil, nil, fmt.Errorf("open output: %w", err)
	}
	return f, f, nil
}

// resolveSecretsDBPath mirrors sfl's resolver: an explicit -sec-path wins,
// else the DB is expected at <root>/sfl-secrets.sqlite (root defaults to CWD).
func resolveSecretsDBPath(flag, root string) string {
	if flag != "" {
		return flag
	}
	if root == "" {
		root = "."
	}
	return filepath.Join(root, secretsDBName)
}

// formatSecretLine renders one row as a tab-separated line: type, secret, and
// where it was first seen. Tabs keep it grep/cut-friendly and match the
// plain-text nature of the ULP lines sfs already emits.
func formatSecretLine(m secrets.Match) string {
	return m.RuleName + "\t" + m.Secret + "\t" + m.SourcePath
}

func printSecretsSummary(n int, out string, stream bool) {
	noun := "secrets"
	if n == 1 {
		noun = "secret"
	}
	if stream || out == "" {
		fmt.Fprintf(os.Stderr, "%d %s\n", n, noun)
		return
	}
	fmt.Fprintf(os.Stderr, "%d %s → %s\n", n, noun, out)
}
