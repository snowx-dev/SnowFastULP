package config

import (
	"flag"
	"fmt"
	"os"
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

// SFUFlags holds pointers to sfu flag variables for config merge.
type SFUFlags struct {
	O, OD, TempDir          *string
	Workers, Dedup, Buckets *int
	SplitZst                *int64
	NoTUI, Zst, Del, NoURI  *bool
	Loose, NoEncodingSniff  *bool
	Debug, DebugReject      *bool
}

// ApplySFU applies unvisited config values to sfu flags.
// rejects CLI -o vs cfg od (and vice versa) w/ clear msg
func (f File) ApplySFU(v Visited, fl SFUFlags) error {
	cfgPath := f.path
	if cfgPath == "" {
		cfgPath = "<config>"
	}
	if v.set("o") && f.SFU.OD != "" {
		return fmt.Errorf("config: -o on CLI conflicts with [sfu].od in %s", cfgPath)
	}
	if v.set("od") && f.SFU.O != "" {
		return fmt.Errorf("config: -od on CLI conflicts with [sfu].o in %s", cfgPath)
	}
	if !v.set("o") && f.SFU.O != "" {
		p, err := f.ResolvedSFUDir("o")
		if err != nil {
			return err
		}
		*fl.O = p
	}
	if !v.set("od") && f.SFU.OD != "" {
		p, err := f.ResolvedSFUDir("od")
		if err != nil {
			return err
		}
		*fl.OD = p
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
		p, err := ResolvePath(f.baseDir, f.SFU.TempDir)
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
	if !v.set("no-encoding-sniff") && f.SFU.NoEncodingSniff {
		*fl.NoEncodingSniff = true
	}
	if !v.set("debug") && f.SFU.Debug {
		*fl.Debug = true
	}
	if !v.set("debug-reject") && f.SFU.DebugReject {
		*fl.DebugReject = true
	}
	return nil
}

// SFSFlags holds pointers to sfs flag variables for config merge.
type SFSFlags struct {
	O               *string
	Txt             *bool
	Silent          *bool
	Clean           *bool
	J               *int
	Debug           *bool
	DecodeStep      *int
	MaxHitsPerChunk *int
	Limit           *int
	Since           *string
}

// ApplySFS applies unvisited config values to sfs flags.
func (f File) ApplySFS(v Visited, fl SFSFlags) error {
	if !v.set("o") && f.SFS.O != "" {
		p, err := ResolvePath(f.baseDir, f.SFS.O)
		if err != nil {
			return err
		}
		*fl.O = p
	}
	if !v.set("txt") && f.SFS.Txt {
		*fl.Txt = true
	}
	if !v.set("silent") && f.SFS.Silent {
		*fl.Silent = true
	}
	if !v.set("clean") && f.SFS.Clean {
		*fl.Clean = true
	}
	if !v.set("j") && f.SFS.J != nil {
		*fl.J = *f.SFS.J
	}
	if !v.set("debug") && f.SFS.Debug {
		*fl.Debug = true
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
	return nil
}

// SFLFlags holds pointers to sfl flag variables for config merge.
type SFLFlags struct {
	O, OD, TempDir, Password *string
	Workers                  *int
	NoTUI, Zst, Del, NoURI   *bool
	Debug, NoUpdateCheck     *bool
}

// ApplySFL applies unvisited config values to sfl flags.
// rejects CLI -o vs cfg od (and vice versa) w/ clear msg.
func (f File) ApplySFL(v Visited, fl SFLFlags) error {
	cfgPath := f.path
	if cfgPath == "" {
		cfgPath = "<config>"
	}
	if v.set("o") && f.SFL.OD != "" {
		return fmt.Errorf("config: -o on CLI conflicts with [sfl].od in %s", cfgPath)
	}
	if v.set("od") && f.SFL.O != "" {
		return fmt.Errorf("config: -od on CLI conflicts with [sfl].o in %s", cfgPath)
	}
	if !v.set("o") && f.SFL.O != "" && fl.O != nil {
		p, err := f.ResolvedSFLDir("o")
		if err != nil {
			return err
		}
		*fl.O = p
	}
	if !v.set("od") && f.SFL.OD != "" && fl.OD != nil {
		p, err := f.ResolvedSFLDir("od")
		if err != nil {
			return err
		}
		*fl.OD = p
	}
	if !v.set("workers") && f.SFL.Workers != nil && fl.Workers != nil {
		*fl.Workers = *f.SFL.Workers
	}
	if !v.set("temp-dir") && f.SFL.TempDir != "" && fl.TempDir != nil {
		p, err := ResolvePath(f.baseDir, f.SFL.TempDir)
		if err != nil {
			return err
		}
		*fl.TempDir = p
	}
	if !v.set("p") && f.SFL.Password != "" && fl.Password != nil {
		p := f.SFL.Password
		if resolved, err := ResolvePath(f.baseDir, p); err == nil {
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
	if !v.set("debug") && f.SFL.Debug && fl.Debug != nil {
		*fl.Debug = true
	}
	if !v.set("no-update-check") && f.SFL.NoUpdateCheck && fl.NoUpdateCheck != nil {
		*fl.NoUpdateCheck = true
	}
	return nil
}
