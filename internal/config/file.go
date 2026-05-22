package config

import "fmt"

// File is the decoded config.toml.
type File struct {
	path    string
	baseDir string

	SFU SFUSection `toml:"sfu"`
	SFS SFSSection `toml:"sfs"`
}

// SFUSection maps to sfu CLI flags. Input fills positional INPUT_PATH, CLI wins.
type SFUSection struct {
	Input           string `toml:"input"`
	O               string `toml:"o"`
	OD              string `toml:"od"`
	Workers         *int   `toml:"workers"`
	Dedup           *int   `toml:"dedup"`
	Buckets         *int   `toml:"buckets"`
	TempDir         string `toml:"temp_dir"`
	NoTUI           bool   `toml:"no_tui"`
	Zst             bool   `toml:"zst"`
	SplitZst        *int64 `toml:"split_zst"`
	Del             bool   `toml:"del"`
	NoURI           bool   `toml:"no_uri"`
	Loose           bool   `toml:"loose"`
	NoEncodingSniff bool   `toml:"no_encoding_sniff"`
	Debug           bool   `toml:"debug"`
	DebugReject     bool   `toml:"debug_reject"`
}

// SFSSection maps to sfs CLI flags and default search dir.
type SFSSection struct {
	Dir             string `toml:"dir"`
	Txt             bool   `toml:"txt"`
	O               string `toml:"o"`
	Silent          bool   `toml:"silent"`
	Clean           bool   `toml:"clean"`
	J               *int   `toml:"j"`
	Debug           bool   `toml:"debug"`
	DecodeStep      *int   `toml:"decode_step"`
	MaxHitsPerChunk *int   `toml:"max_hits_per_chunk"`
}

// Path returns the loaded config file path.
func (f File) Path() string { return f.path }

// BaseDir is the dir containing the config file.
func (f File) BaseDir() string { return f.baseDir }

// ResolvedSFUDir returns [sfu].o, [sfu].od or [sfu].input resolved against base dir.
func (f File) ResolvedSFUDir(key string) (string, error) {
	var raw string
	switch key {
	case "o":
		raw = f.SFU.O
	case "od":
		raw = f.SFU.OD
	case "input":
		raw = f.SFU.Input
	default:
		return "", fmt.Errorf("config: unknown sfu dir key %q", key)
	}
	return ResolvePath(f.baseDir, raw)
}

// ResolvedSFSDir returns [sfs].dir resolved against base dir.
func (f File) ResolvedSFSDir() (string, error) {
	return ResolvePath(f.baseDir, f.SFS.Dir)
}
