package ulpengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAmbiguityCountsAcrossFastAndBucketedWorkerModes(t *testing.T) {
	const recordCount = 24
	line := "https://example.com/login:alice:pw:tail\n"
	for _, tc := range []struct {
		name        string
		workers     int
		fastPathOff bool
		wantFast    bool
	}{
		{name: "fast_workers_1", workers: 1, wantFast: true},
		{name: "fast_workers_4", workers: 4, wantFast: true},
		{name: "bucketed_workers_1", workers: 1, fastPathOff: true},
		{name: "bucketed_workers_4", workers: 4, fastPathOff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "input.txt")
			if err := os.WriteFile(input, []byte(strings.Repeat(line, recordCount)), 0o600); err != nil {
				t.Fatal(err)
			}
			debug, err := NewDebugLog(filepath.Join(dir, "debug.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer debug.Close()
			cfg, err := Resolve(Config{
				Inputs:       []string{input},
				Output:       filepath.Join(dir, "out.txt"),
				TempDir:      filepath.Join(dir, "tmp"),
				Workers:      tc.workers,
				DedupWorkers: tc.workers,
				Buckets:      4,
				FastPathOff:  tc.fastPathOff,
				Debug:        debug,
			})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.UseFastPath != tc.wantFast {
				t.Fatalf("resolved fast path = %v, want %v", cfg.UseFastPath, tc.wantFast)
			}
			metrics := &Metrics{}
			if err := Run(context.Background(), cfg, metrics); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := metrics.AmbiguityTotal.Load(); got != recordCount {
				t.Fatalf("pre-dedup ambiguity total = %d, want %d", got, recordCount)
			}
			if got := metrics.AmbiguityPathPassword.Load(); got != recordCount {
				t.Fatalf("path/password ambiguity total = %d, want %d", got, recordCount)
			}
			if got := metrics.LinesUnique.Load(); got != 1 {
				t.Fatalf("deduplicated output count = %d, want 1", got)
			}
			if output, err := os.ReadFile(cfg.Cfg.Output); err != nil || strings.Count(string(output), "\n") != 1 {
				t.Fatalf("output line count = %d, read error %v", strings.Count(string(output), "\n"), err)
			}
		})
	}
}

// TestRepairWhitespaceVariantsCountThroughFastAndBucketedIngest extends the
// ambiguity pipeline contract to the review finding: the three accepted
// whitespace variants of the ambiguous line must each be witnessed exactly
// once (total=3 / path_or_password=3, pre-dedup) and dedup to one record.
func TestRepairWhitespaceVariantsCountThroughFastAndBucketedIngest(t *testing.T) {
	variants := []string{
		"https://example.com/login:alice:pw:tail\n",
		"https://example.com/login:alice :pw:tail\n",
		"https://example.com/login:alice\t:pw:tail\n",
	}
	for _, tc := range []struct {
		name        string
		workers     int
		fastPathOff bool
		wantFast    bool
	}{
		{name: "fast_workers_1", workers: 1, wantFast: true},
		{name: "fast_workers_4", workers: 4, wantFast: true},
		{name: "bucketed_workers_1", workers: 1, fastPathOff: true},
		{name: "bucketed_workers_4", workers: 4, fastPathOff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(lines []string, out string) *Metrics {
				t.Helper()
				dir := t.TempDir()
				input := filepath.Join(dir, "input.txt")
				if err := os.WriteFile(input, []byte(strings.Join(lines, "")), 0o600); err != nil {
					t.Fatal(err)
				}
				debug, err := NewDebugLog(filepath.Join(dir, "debug.log"))
				if err != nil {
					t.Fatal(err)
				}
				defer debug.Close()
				cfg, err := Resolve(Config{
					Inputs:       []string{input},
					Output:       out,
					TempDir:      filepath.Join(dir, "tmp"),
					Workers:      tc.workers,
					DedupWorkers: tc.workers,
					Buckets:      4,
					FastPathOff:  tc.fastPathOff,
					Debug:        debug,
				})
				if err != nil {
					t.Fatal(err)
				}
				if cfg.UseFastPath != tc.wantFast {
					t.Fatalf("resolved fast path = %v, want %v", cfg.UseFastPath, tc.wantFast)
				}
				metrics := &Metrics{}
				if err := Run(context.Background(), cfg, metrics); err != nil {
					t.Fatalf("Run: %v", err)
				}
				if got := metrics.AmbiguityTotal.Load(); got != int64(len(variants)) {
					t.Fatalf("pre-dedup ambiguity total = %d, want %d", got, len(variants))
				}
				if got := metrics.AmbiguityPathPassword.Load(); got != int64(len(variants)) {
					t.Fatalf("path/password ambiguity total = %d, want %d", got, len(variants))
				}
				return metrics
			}
			dir := t.TempDir()
			variantOut := filepath.Join(dir, "variants.txt")
			metrics := run(variants, variantOut)
			if got := metrics.LinesUnique.Load(); got != 1 {
				t.Fatalf("deduplicated output count = %d, want 1", got)
			}
			variantOutput, err := os.ReadFile(variantOut)
			if err != nil {
				t.Fatal(err)
			}
			baseOut := filepath.Join(dir, "base.txt")
			run([]string{variants[0], variants[0], variants[0]}, baseOut)
			baseOutput, err := os.ReadFile(baseOut)
			if err != nil {
				t.Fatal(err)
			}
			if string(variantOutput) != string(baseOutput) {
				t.Fatalf("whitespace variants output %q, want clean-line output %q", variantOutput, baseOutput)
			}
			if lines := strings.Count(string(variantOutput), "\n"); lines != 1 {
				t.Fatalf("variant output line count = %d, want 1", lines)
			}
		})
	}
}

// repairCustomParser delegates to the builtin selected parse while bypassing
// the diagnostic witness machinery entirely, mirroring Config.Parser contract.
type repairCustomParser struct{}

func (repairCustomParser) Parse(line string) (host, url, login, password string, ok bool) {
	return ParseLine(line, false)
}

// TestRepairCustomParserBypassesAmbiguityWitnesses pins the custom-parser
// bypass: with Config.Parser set, diagnostics stay silent (counts zero) even
// for ambiguous lines and an active debug log, in both ingest modes.
func TestRepairCustomParserBypassesAmbiguityWitnesses(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fastPathOff bool
	}{
		{name: "fastpath"},
		{name: "bucketed", fastPathOff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "input.txt")
			body := strings.Repeat("https://example.com/login:alice:pw:tail\n", 3)
			if err := os.WriteFile(input, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			debug, err := NewDebugLog(filepath.Join(dir, "debug.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer debug.Close()
			cfg, err := Resolve(Config{
				Inputs:       []string{input},
				Output:       filepath.Join(dir, "out.txt"),
				TempDir:      filepath.Join(dir, "tmp"),
				Workers:      2,
				DedupWorkers: 2,
				Buckets:      4,
				FastPathOff:  tc.fastPathOff,
				Parser:       repairCustomParser{},
				Debug:        debug,
			})
			if err != nil {
				t.Fatal(err)
			}
			metrics := &Metrics{}
			if err := Run(context.Background(), cfg, metrics); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := metrics.AmbiguityTotal.Load(); got != 0 {
				t.Fatalf("custom parser ambiguity total = %d, want 0", got)
			}
			if got := metrics.AmbiguityPathPassword.Load(); got != 0 {
				t.Fatalf("custom parser path/password count = %d, want 0", got)
			}
			if got := metrics.LinesUnique.Load(); got != 1 {
				t.Fatalf("deduplicated output count = %d, want 1", got)
			}
			output, err := os.ReadFile(cfg.Cfg.Output)
			if err != nil || strings.Count(string(output), "\n") != 1 {
				t.Fatalf("output line count = %d, read error %v", strings.Count(string(output), "\n"), err)
			}
		})
	}
}
