package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
)

// Visited records which CLI flags the user set explicitly.
type Visited map[string]bool

// NewVisited builds a set from flag.Visit after flag.Parse.
func NewVisited() Visited {
	v := Visited{}
	flag.Visit(func(f *flag.Flag) {
		v[f.Name] = true
	})
	return v
}

func (v Visited) set(name string) bool { return v[name] }

// jsonOutControlTokens are the json_out spellings that OutTarget.Set
// interprets itself: stdout spellings, the on/off tokens, and the file:
// literal-path escape. Everything else is a real filesystem path and is
// resolved here so config paths behave like every other config path
// (~/ expands to home, relative values resolve against the process CWD).
// CLI flag semantics are unchanged: resolution happens only in this
// config-merge step.
func resolveJSONOutConfig(raw string) (string, error) {
	switch raw {
	case "", "-", "stdout", "true", "false":
		return raw, nil
	}
	if strings.HasPrefix(raw, "file:") {
		return raw, nil // literal-path escape, parsed by OutTarget.Set
	}
	resolved, err := ResolvePath(raw)
	if err != nil {
		return "", fmt.Errorf("config: json_out: %w", err)
	}
	return resolved, nil
}

// ResolveIntAlias collapses an int flag alias into its canonical flag (used for
// the worker-count pair: -workers on sfu/sfl, -j on sfs, each accepting the
// other). An explicitly set canonical always wins. When only the alias was set
// on the CLI, its value is copied into the canonical pointer and the canonical
// is marked visited, so the alias beats a config-file value exactly as the
// canonical flag would. Call after NewVisited, before the Apply* merge.
func (v Visited) ResolveIntAlias(canonical, alias *int, canonicalName, aliasName string) {
	if v.set(canonicalName) || !v.set(aliasName) {
		return
	}
	*canonical = *alias
	v[canonicalName] = true
}

// ResolveStringAlias is the string counterpart of ResolveIntAlias (e.g. sfl
// -workers / -j). Same precedence: an explicit canonical wins;
// when only the alias was set its value is copied into the canonical pointer
// and the canonical is marked visited so it beats a config-file value.
func (v Visited) ResolveStringAlias(canonical, alias *string, canonicalName, aliasName string) {
	if v.set(canonicalName) || !v.set(aliasName) {
		return
	}
	*canonical = *alias
	v[canonicalName] = true
}

type HistoryFlags struct {
	Enabled *bool
	Path    *string
}

// ApplyHistory applies shared history settings not explicitly set on the CLI.
func (f File) ApplyHistory(v Visited, fl HistoryFlags) error {
	if !v.set("history") && f.History.Enabled != nil && fl.Enabled != nil {
		*fl.Enabled = *f.History.Enabled
	}
	if !v.set("history-path") && f.History.Path != "" && fl.Path != nil {
		path, err := ResolvePath(f.History.Path)
		if err != nil {
			return err
		}
		// H-12: a trailing separator is the history store's directory hint
		// (adopt DIR/history.sqlite3, create the directory if missing), but
		// ResolvePath cleans it away. Re-attach the hint so a configured
		// "new-history/" keeps meaning "directory", matching CLI semantics.
		if strings.HasSuffix(f.History.Path, "/") || strings.HasSuffix(f.History.Path, `\`) {
			path += "/"
		}
		*fl.Path = path
	}
	return nil
}

// SFUFlags holds pointers to sfu flag variables for config merge.
type SFUFlags struct {
	O, OD, TempDir          *string
	ODR                     *bool
	Workers, Dedup, Buckets *int
	SplitZst                *int64
	NoTUI, Zst, Del, NoURI  *bool
	Loose, NoEncodingSniff  *bool
	NoFastPath              *bool
	Debug, DebugReject      *bool
	NoUpdateCheck           *bool
	Bell                    *bool
	ParseDelims             *string
	ParseRules              *string
	// JSONOut receives config json_out via OutTarget.Set (empty = stdout,
	// "-" = stdout, a path = file); JSONEvery parses a Go duration string.
	JSONOut   *cliargs.OutTarget
	JSONEvery *time.Duration
}

// ApplySFU applies unvisited config values to sfu flags.
func (f File) ApplySFU(v Visited, fl SFUFlags) error {
	// Any CLI output flag (-o/-od/-odr) suppresses the config o/od pull so
	// CLI wins. When none are on the CLI and the config sets both o and od,
	// -od takes priority (library mode) and o is ignored — a legacy -o in the
	// config won't silently override -od.
	odFromCfg := false
	if !v.set("o") && !v.set("od") && !v.set("odr") && f.SFU.OD != "" {
		p, err := f.ResolvedSFUDir("od")
		if err != nil {
			return err
		}
		*fl.OD = p
		odFromCfg = true
	}
	if !v.set("o") && !v.set("od") && !v.set("odr") && !odFromCfg && f.SFU.O != "" {
		p, err := f.ResolvedSFUDir("o")
		if err != nil {
			return err
		}
		*fl.O = p
	}
	// config odr=true flips dry-run on a -od run (reuses the od path). CLI
	// -odr sets dry-run directly in main. An explicit CLI -o, -od, or -odr
	// owns the complete output-mode tuple, so the configured odr applies
	// only when none of them was on the command line — otherwise an
	// explicit -o would abort with an odr error from configured odr=true.
	if !v.set("o") && !v.set("od") && !v.set("odr") && f.SFU.ODR {
		*fl.ODR = true
	}
	if !v.set("workers") && f.SFU.Workers != nil {
		*fl.Workers = *f.SFU.Workers
	}
	if !v.set("dedup") && f.SFU.Dedup != nil {
		*fl.Dedup = *f.SFU.Dedup
	}
	if !v.set("buckets") && f.SFU.Buckets != nil {
		*fl.Buckets = *f.SFU.Buckets
	}
	if !v.set("temp-dir") && f.SFU.TempDir != "" {
		p, err := ResolvePath(f.SFU.TempDir)
		if err != nil {
			return err
		}
		*fl.TempDir = p
	}
	if !v.set("no-tui") && f.SFU.NoTUI {
		*fl.NoTUI = true
	}
	if !v.set("zst") && f.SFU.Zst {
		*fl.Zst = true
	}
	if !v.set("split-zst") && f.SFU.SplitZst != nil {
		*fl.SplitZst = *f.SFU.SplitZst
	}
	if !v.set("del") && f.SFU.Del {
		*fl.Del = true
	}
	if !v.set("no-uri") && f.SFU.NoURI {
		*fl.NoURI = true
	}
	if !v.set("loose") && f.SFU.Loose {
		*fl.Loose = true
	}
	// Any CLI custom-parser flag (-parse-delims / -parse-rules) suppresses
	// both config pulls so CLI wins the XOR pair (mirrors -o/-od/-odr).
	if !v.set("parse-delims") && !v.set("parse-rules") {
		if f.SFU.ParseDelims != "" && fl.ParseDelims != nil {
			*fl.ParseDelims = f.SFU.ParseDelims
		}
		if f.SFU.ParseRules != "" && fl.ParseRules != nil {
			p, err := ResolvePath(f.SFU.ParseRules)
			if err != nil {
				return err
			}
			*fl.ParseRules = p
		}
	}
	if !v.set("no-encoding-sniff") && f.SFU.NoEncodingSniff {
		*fl.NoEncodingSniff = true
	}
	if !v.set("no-fast-path") && f.SFU.NoFastPath && fl.NoFastPath != nil {
		*fl.NoFastPath = true
	}
	if !v.set("debug") && f.SFU.Debug {
		*fl.Debug = true
	}
	if !v.set("debug-reject") && f.SFU.DebugReject {
		*fl.DebugReject = true
	}
	if !v.set("no-update-check") && f.SFU.NoUpdateCheck && fl.NoUpdateCheck != nil {
		*fl.NoUpdateCheck = true
	}
	if !v.set("bell") && f.SFU.Bell && fl.Bell != nil {
		*fl.Bell = true
	}
	if !v.set("json") && f.SFU.JSONOut != "" && fl.JSONOut != nil {
		out, err := resolveJSONOutConfig(f.SFU.JSONOut)
		if err != nil {
			return err
		}
		if err := fl.JSONOut.Set(out); err != nil {
			return err
		}
	}
	if !v.set("json-every") && f.SFU.JSONEvery != "" && fl.JSONEvery != nil {
		d, err := time.ParseDuration(f.SFU.JSONEvery)
		if err != nil {
			return fmt.Errorf("[sfu] json_every: %w", err)
		}
		*fl.JSONEvery = d
	}
	return nil
}

// SFSFlags holds pointers to sfs flag variables for config merge.
type SFSFlags struct {
	O               *string
	Txt             *bool
	Stats           *bool
	Clean           *bool
	Combo           *bool
	J               *int
	Debug           *bool
	NoUpdateCheck   *bool
	Bell            *bool
	DecodeStep      *int
	MaxHitsPerChunk *int
	Limit           *int
	Since           *string
	// JSONOut receives config json_out via OutTarget.Set (empty = stdout,
	// "-" = stdout, a path = file); JSONEvery parses a Go duration string.
	// Both mirror the sfu/sfl fields.
	JSONOut   *cliargs.OutTarget
	JSONEvery *time.Duration
}

// ApplySFS applies unvisited config values to sfs flags.
func (f File) ApplySFS(v Visited, fl SFSFlags) error {
	if !v.set("o") && f.SFS.O != "" {
		p, err := ResolvePath(f.SFS.O)
		if err != nil {
			return err
		}
		*fl.O = p
	}
	if !v.set("txt") && f.SFS.Txt {
		*fl.Txt = true
	}
	if !v.set("stats") && f.SFS.Stats && fl.Stats != nil {
		*fl.Stats = true
	}
	if !v.set("clean") && f.SFS.Clean {
		*fl.Clean = true
	}
	if !v.set("combo") && f.SFS.Combo && fl.Combo != nil {
		*fl.Combo = true
	}
	if !v.set("j") && f.SFS.J != nil {
		*fl.J = *f.SFS.J
	}
	if !v.set("debug") && f.SFS.Debug {
		*fl.Debug = true
	}
	if !v.set("no-update-check") && f.SFS.NoUpdateCheck && fl.NoUpdateCheck != nil {
		*fl.NoUpdateCheck = true
	}
	if !v.set("bell") && f.SFS.Bell && fl.Bell != nil {
		*fl.Bell = true
	}
	if !v.set("decode-step") && f.SFS.DecodeStep != nil {
		*fl.DecodeStep = *f.SFS.DecodeStep
	}
	if !v.set("max-hits-per-chunk") && f.SFS.MaxHitsPerChunk != nil {
		*fl.MaxHitsPerChunk = *f.SFS.MaxHitsPerChunk
	}
	if !v.set("l") && f.SFS.Limit != nil {
		*fl.Limit = *f.SFS.Limit
	}
	if !v.set("since") && f.SFS.Since != "" {
		*fl.Since = f.SFS.Since
	}
	if !v.set("json") && f.SFS.JSONOut != "" && fl.JSONOut != nil {
		out, err := resolveJSONOutConfig(f.SFS.JSONOut)
		if err != nil {
			return err
		}
		if err := fl.JSONOut.Set(out); err != nil {
			return err
		}
	}
	if !v.set("json-every") && f.SFS.JSONEvery != "" && fl.JSONEvery != nil {
		d, err := time.ParseDuration(f.SFS.JSONEvery)
		if err != nil {
			return fmt.Errorf("[sfs] json_every: %w", err)
		}
		*fl.JSONEvery = d
	}
	return nil
}

// SFLFlags holds pointers to sfl flag variables for config merge.
type SFLFlags struct {
	O, OD, TempDir, Password *string
	ODR                      *bool
	Workers                  *int
	NoTUI, Zst, Del, NoURI   *bool
	Loose                    *bool
	Debug, DebugReject       *bool
	NoUpdateCheck            *bool
	Bell                     *bool
	Env                      *bool
	// JSONOut receives config json_out via OutTarget.Set; JSONEvery parses a
	// Go duration string. Both mirror the sfu fields.
	JSONOut   *cliargs.OutTarget
	JSONEvery *time.Duration
}

// ApplySFL applies unvisited config values to sfl flags. Every flag pointer
// dereference is nil-guarded because sfl callers may pass a partially populated
// SFLFlags (only the flags relevant to the selected subcommand). ApplySFU
// guards optional custom-parser pointers; both real callers fully populate the
// remaining fields today. Visited itself is nil-safe: a nil map makes every
// v.set(...) return false, so a nil Visited means "nothing was set on the
// command line" and every config value applies.
func (f File) ApplySFL(v Visited, fl SFLFlags) error {
	// Any CLI output flag (-o/-od/-odr) suppresses the config o/od pull so
	// CLI wins. When none are on the CLI and the config sets both o and od,
	// -od takes priority (library mode) and o is ignored — mirrors ApplySFU.
	odFromCfg := false
	if !v.set("o") && !v.set("od") && !v.set("odr") && f.SFL.OD != "" && fl.OD != nil {
		p, err := f.ResolvedSFLDir("od")
		if err != nil {
			return err
		}
		*fl.OD = p
		odFromCfg = true
	}
	if !v.set("o") && !v.set("od") && !v.set("odr") && !odFromCfg && f.SFL.O != "" && fl.O != nil {
		p, err := f.ResolvedSFLDir("o")
		if err != nil {
			return err
		}
		*fl.O = p
	}
	// config odr=true flips dry-run on a -od run (reuses the od path). An
	// explicit CLI -o, -od, or -odr owns the complete output-mode tuple, so
	// the configured odr applies only when none of them was on the command
	// line (mirrors ApplySFU).
	if !v.set("o") && !v.set("od") && !v.set("odr") && f.SFL.ODR && fl.ODR != nil {
		*fl.ODR = true
	}
	if !v.set("workers") && f.SFL.Workers != nil && fl.Workers != nil {
		*fl.Workers = *f.SFL.Workers
	}
	if !v.set("temp-dir") && f.SFL.TempDir != "" && fl.TempDir != nil {
		p, err := ResolvePath(f.SFL.TempDir)
		if err != nil {
			return err
		}
		*fl.TempDir = p
	}
	if !v.set("p") && f.SFL.Password != "" && fl.Password != nil {
		p := f.SFL.Password
		if resolved, err := ResolvePath(p); err == nil {
			if _, statErr := os.Stat(resolved); statErr == nil {
				p = resolved
			}
		}
		*fl.Password = p
	}
	if !v.set("no-tui") && f.SFL.NoTUI && fl.NoTUI != nil {
		*fl.NoTUI = true
	}
	if !v.set("zst") && f.SFL.Zst && fl.Zst != nil {
		*fl.Zst = true
	}
	if !v.set("del") && f.SFL.Del && fl.Del != nil {
		*fl.Del = true
	}
	if !v.set("no-uri") && f.SFL.NoURI && fl.NoURI != nil {
		*fl.NoURI = true
	}
	if !v.set("loose") && f.SFL.Loose && fl.Loose != nil {
		*fl.Loose = true
	}
	if !v.set("debug") && f.SFL.Debug && fl.Debug != nil {
		*fl.Debug = true
	}
	if !v.set("debug-reject") && f.SFL.DebugReject && fl.DebugReject != nil {
		*fl.DebugReject = true
	}
	if !v.set("no-update-check") && f.SFL.NoUpdateCheck && fl.NoUpdateCheck != nil {
		*fl.NoUpdateCheck = true
	}
	if !v.set("bell") && f.SFL.Bell && fl.Bell != nil {
		*fl.Bell = true
	}
	if !v.set("env") && f.SFL.Env && fl.Env != nil {
		*fl.Env = true
	}
	if !v.set("json") && f.SFL.JSONOut != "" && fl.JSONOut != nil {
		out, err := resolveJSONOutConfig(f.SFL.JSONOut)
		if err != nil {
			return err
		}
		if err := fl.JSONOut.Set(out); err != nil {
			return err
		}
	}
	if !v.set("json-every") && f.SFL.JSONEvery != "" && fl.JSONEvery != nil {
		d, err := time.ParseDuration(f.SFL.JSONEvery)
		if err != nil {
			return fmt.Errorf("[sfl] json_every: %w", err)
		}
		*fl.JSONEvery = d
	}
	return nil
}
