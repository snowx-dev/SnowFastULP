package config

import "fmt"

// File is the decoded config.toml.
type File struct {
	path string

	SFU     SFUSection     `toml:"sfu"`
	SFS     SFSSection     `toml:"sfs"`
	SFL     SFLSection     `toml:"sfl"`
	History HistorySection `toml:"history"`
}

type HistorySection struct {
	Enabled *bool  `toml:"enabled"`
	Path    string `toml:"path"`
}

// SFUSection maps to sfu CLI flags. Input fills positional INPUT_PATH, CLI wins.
type SFUSection struct {
	Input           string `toml:"input"`
	O               string `toml:"o"`
	OD              string `toml:"od"`
	ODR             bool   `toml:"odr"`
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
	ParseDelims     string `toml:"parse_delims"`
	ParseRules      string `toml:"parse_rules"`
	NoEncodingSniff bool   `toml:"no_encoding_sniff"`
	Debug           bool   `toml:"debug"`
	DebugReject     bool   `toml:"debug_reject"`
	NoUpdateCheck   bool   `toml:"no_update_check"`
	Bell            bool   `toml:"bell"`
	NoFastPath      bool   `toml:"no_fast_path"`
	JSONOut         string `toml:"json_out"`
	JSONEvery       string `toml:"json_every"`
}

// SFSSection maps to sfs CLI flags and default search dir.
type SFSSection struct {
	Dir           string `toml:"dir"`
	Txt           bool   `toml:"txt"`
	O             string `toml:"o"`
	Stats         bool   `toml:"stats"`
	Clean         bool   `toml:"clean"`
	Combo         bool   `toml:"combo"`
	J             *int   `toml:"j"`
	Debug         bool   `toml:"debug"`
	NoUpdateCheck bool   `toml:"no_update_check"`
	Bell          bool   `toml:"bell"`
	DecodeStep    *int   `toml:"decode_step"`
	// Stream and Silent are accepted and ignored (parse-compat): stream is
	// the default output mode, mirroring the no-op -s/-silent CLI flags.
	Stream bool `toml:"stream"`
	Silent bool `toml:"silent"`

	MaxHitsPerChunk *int   `toml:"max_hits_per_chunk"`
	Limit           *int   `toml:"l"`
	Since           string `toml:"since"`

	JSONOut   string `toml:"json_out"`
	JSONEvery string `toml:"json_every"`
}

// SFLSection maps to sfl CLI flags. Input fills positional INPUT_PATH, CLI wins.
type SFLSection struct {
	Input         string `toml:"input"`
	O             string `toml:"o"`
	OD            string `toml:"od"`
	ODR           bool   `toml:"odr"`
	Password      string `toml:"p"`
	Workers       *int   `toml:"workers"`
	TempDir       string `toml:"temp_dir"`
	NoTUI         bool   `toml:"no_tui"`
	Zst           bool   `toml:"zst"`
	Del           bool   `toml:"del"`
	NoURI         bool   `toml:"no_uri"`
	Loose         bool   `toml:"loose"`
	Debug         bool   `toml:"debug"`
	DebugReject   bool   `toml:"debug_reject"`
	NoUpdateCheck bool   `toml:"no_update_check"`
	Bell          bool   `toml:"bell"`
	Env           bool   `toml:"env"`
	JSONOut       string `toml:"json_out"`
	JSONEvery     string `toml:"json_every"`
}

// Path returns the loaded config file path.
func (f File) Path() string { return f.path }

// ResolvedSFUDir returns [sfu].o, [sfu].od or [sfu].input resolved against CWD.
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
	return ResolvePath(raw)
}

// ResolvedSFSDir returns [sfs].dir resolved against CWD.
func (f File) ResolvedSFSDir() (string, error) {
	return ResolvePath(f.SFS.Dir)
}

// ResolvedSFLDir returns [sfl].o, [sfl].od or [sfl].input resolved against CWD.
func (f File) ResolvedSFLDir(key string) (string, error) {
	var raw string
	switch key {
	case "o":
		raw = f.SFL.O
	case "od":
		raw = f.SFL.OD
	case "input":
		raw = f.SFL.Input
	default:
		return "", fmt.Errorf("config: unknown sfl dir key %q", key)
	}
	return ResolvePath(raw)
}
