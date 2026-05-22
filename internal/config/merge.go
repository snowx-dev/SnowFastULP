package config

import (
	"flag"
	"fmt"
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
	return nil
}
