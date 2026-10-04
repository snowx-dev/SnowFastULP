package ulpengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end: a custom parser replaces built-in input interpretation, but the
// stored output stays canonical colon ULP so libraries/sfs keep working.
func TestRunCustomDelimParserEmitsCanonicalULP(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fastPath bool
	}{
		{"bucketed", false},
		{"fastpath", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := t.TempDir()
			in := filepath.Join(d, "in.txt")
			// pipe-delimited input; the builtin strict parser rejects every line.
			writeFile(t, in, "https://example.com/login|user@example.com|s3cret\n"+
				"https://other.org/x|bob|hunter2\n"+
				"two|fields\n")
			out := filepath.Join(d, "out.txt")
			p, err := NewDelimParser("|")
			if err != nil {
				t.Fatal(err)
			}
			cfg := Config{
				Inputs:       []string{in},
				Output:       out,
				TempDir:      filepath.Join(d, "shards"),
				Workers:      2,
				DedupWorkers: 2,
				Buckets:      8,
				ChunkBytes:   1 << 20,
				FastPathOff:  !tc.fastPath,
				Parser:       p,
			}
			r, err := Resolve(cfg)
			if err != nil {
				t.Fatal(err)
			}
			r.UseFastPath = tc.fastPath
			m := &Metrics{TotalInputBytes: r.TotalInputs}
			if err := Run(context.Background(), r, m); err != nil {
				t.Fatal(err)
			}
			if got := m.LinesUnique.Load(); got != 2 {
				t.Fatalf("LinesUnique = %d, want 2", got)
			}
			if got := m.LinesRejected.Load(); got != 1 {
				t.Fatalf("LinesRejected = %d, want 1 (2-field line)", got)
			}
			raw, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
			if len(lines) != 2 {
				t.Fatalf("stored %d lines: %q", len(lines), lines)
			}
			for _, ln := range lines {
				// stored lines must be plain colon ULP: no input delimiter left,
				// and the archive reader (parseStored / regen) accepts them.
				if strings.Contains(ln, "|") {
					t.Fatalf("stored line still carries custom delimiter: %q", ln)
				}
				if _, _, _, _, ok := parseStored(ln); !ok {
					t.Fatalf("stored line not readable by parseStored: %q", ln)
				}
			}
		})
	}
}

// End-to-end with -od (DestDedup): custom-delimited input lands in the library
// as canonical ULP inside the zst shard, re-parseable by the builtin parser.
func TestRunCustomRulesIntoLibrary(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	writeFile(t, in, "CSV:example.com,bob,hunter2\nCSV:other.org,alice,s3cret\nignored line\n")
	rules := filepath.Join(d, "rules.txt")
	writeFile(t, rules, `^CSV:(?P<host>[^,]+),(?P<login>[^,]+),(?P<password>.+)$`+"\n")
	lib := filepath.Join(d, "library")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	p, _, err := NewRegexRulesParser(rules)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Inputs:       []string{in},
		Output:       filepath.Join(lib, "sfu_test.txt.zst"),
		TempDir:      filepath.Join(d, "shards"),
		Workers:      2,
		DedupWorkers: 2,
		Buckets:      8,
		ChunkBytes:   1 << 20,
		Compress:     true,
		Parser:       p,
		DestDedup:    true,
		DestDedupDir: lib,
	}
	r, err := Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.UseFastPath = false
	EnsureDestDedupMetrics(r)
	m := &Metrics{TotalInputBytes: r.TotalInputs}
	if err := Run(context.Background(), r, m); err != nil {
		t.Fatal(err)
	}
	if got := m.LinesUnique.Load(); got != 2 {
		t.Fatalf("LinesUnique = %d, want 2", got)
	}
	if len(r.OutputPaths) == 0 {
		t.Fatal("no library shard written")
	}
	var lines []string
	if err := streamArchiveLines(context.Background(), r.OutputPaths[0], 2, nil, func(line string) error {
		lines = append(lines, line)
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("library shard holds %d lines: %q", len(lines), lines)
	}
	for _, ln := range lines {
		if strings.Contains(ln, "CSV:") {
			t.Fatalf("stored line not rewritten to ULP: %q", ln)
		}
		if _, _, _, _, ok := parseStored(ln); !ok {
			t.Fatalf("stored line not readable by parseStored: %q", ln)
		}
	}
}

// Custom parser + Loose=true: the custom parser wins entirely (loose ignored).
func TestRunCustomParserIgnoresLoose(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	// loose would accept "203.0.113.5:8080:admin:p"; the delim parser must not.
	writeFile(t, in, "203.0.113.5:8080:admin:p\nexample.com|user|pw\n")
	p, _ := NewDelimParser("|")
	cfg := Config{
		Inputs:       []string{in},
		Output:       filepath.Join(d, "out.txt"),
		TempDir:      filepath.Join(d, "shards"),
		Workers:      2,
		DedupWorkers: 2,
		Buckets:      8,
		ChunkBytes:   1 << 20,
		FastPathOff:  true,
		Loose:        true, // must be ignored while Parser is set
		Parser:       p,
	}
	r, err := Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := &Metrics{TotalInputBytes: r.TotalInputs}
	if err := Run(context.Background(), r, m); err != nil {
		t.Fatal(err)
	}
	if got := m.LinesUnique.Load(); got != 1 {
		t.Fatalf("LinesUnique = %d, want 1 (loose-only line must be rejected)", got)
	}
	if got := m.LinesRejected.Load(); got != 1 {
		t.Fatalf("LinesRejected = %d, want 1", got)
	}
}

// Custom delim with a spaced login must survive FormatRecordStable and land
// in the output as colon ULP readable by parseStored.
func TestRunCustomDelimSpacedLoginStored(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	writeFile(t, in, "example.com|user with space|pw\nexample.com|clean|pw\n")
	out := filepath.Join(d, "out.txt")
	p, _ := NewDelimParser("|")
	cfg := Config{
		Inputs:       []string{in},
		Output:       out,
		TempDir:      filepath.Join(d, "shards"),
		Workers:      2,
		DedupWorkers: 2,
		Buckets:      8,
		ChunkBytes:   1 << 20,
		FastPathOff:  true,
		Parser:       p,
	}
	r, err := Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.UseFastPath = false
	m := &Metrics{TotalInputBytes: r.TotalInputs}
	if err := Run(context.Background(), r, m); err != nil {
		t.Fatal(err)
	}
	if got := m.LinesUnique.Load(); got != 2 {
		t.Fatalf("LinesUnique = %d, want 2", got)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	joined := string(raw)
	if !strings.Contains(joined, "example.com:user with space:pw") {
		t.Fatalf("spaced login missing from output: %q", joined)
	}
	for _, ln := range strings.Split(strings.TrimRight(joined, "\n"), "\n") {
		if _, _, _, _, ok := parseStored(ln); !ok {
			t.Fatalf("parseStored rejected %q", ln)
		}
	}
}
