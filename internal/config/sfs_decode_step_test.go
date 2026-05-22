package config_test

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/config"
)

// merge wires [sfs].decode_step into flag when CLI didnt set it
func TestApplySFSDecodeStepFromTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
[sfs]
decode_step = 524288
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}

	fs := flag.NewFlagSet("sfs", flag.ContinueOnError)
	o := fs.String("o", "", "")
	txt := fs.Bool("txt", false, "")
	silent := fs.Bool("silent", false, "")
	clean := fs.Bool("clean", false, "")
	j := fs.Int("j", 0, "")
	debug := fs.Bool("debug", false, "")
	decodeStep := fs.Int("decode-step", 0, "")
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}

	visited := config.Visited{}
	fs.Visit(func(fl *flag.Flag) { visited[fl.Name] = true })

	if err := f.ApplySFS(visited, config.SFSFlags{
		O: o, Txt: txt, Silent: silent, Clean: clean, J: j, Debug: debug,
		DecodeStep: decodeStep,
	}); err != nil {
		t.Fatal(err)
	}
	if *decodeStep != 524288 {
		t.Fatalf("decode-step = %d, want 524288 (from TOML)", *decodeStep)
	}
}

// explicit CLI flag overrides [sfs].decode_step from config
func TestApplySFSDecodeStepCLIWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
[sfs]
decode_step = 524288
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}

	fs := flag.NewFlagSet("sfs", flag.ContinueOnError)
	o := fs.String("o", "", "")
	txt := fs.Bool("txt", false, "")
	silent := fs.Bool("silent", false, "")
	clean := fs.Bool("clean", false, "")
	j := fs.Int("j", 0, "")
	debug := fs.Bool("debug", false, "")
	decodeStep := fs.Int("decode-step", 0, "")
	if err := fs.Parse([]string{"-decode-step", "262144"}); err != nil {
		t.Fatal(err)
	}

	visited := config.Visited{}
	fs.Visit(func(fl *flag.Flag) { visited[fl.Name] = true })

	if err := f.ApplySFS(visited, config.SFSFlags{
		O: o, Txt: txt, Silent: silent, Clean: clean, J: j, Debug: debug,
		DecodeStep: decodeStep,
	}); err != nil {
		t.Fatal(err)
	}
	if *decodeStep != 262144 {
		t.Fatalf("decode-step = %d, want 262144 (CLI overrides TOML)", *decodeStep)
	}
}
