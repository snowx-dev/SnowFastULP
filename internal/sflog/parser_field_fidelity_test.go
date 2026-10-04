package sflog

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

func TestT02F01F02F03FormatCredentialPreservesSuppliedFields(t *testing.T) {
	cases := []struct {
		name, url, login, password, line      string
		host, expectedLogin, expectedPassword string
	}{
		{
			name: "colon path",
			url:  "https://example.com/oauth:callback", login: "alice", password: "pw",
			line: "example.com:alice:pw", host: "example.com", expectedLogin: "alice", expectedPassword: "pw",
		},
		{
			name: "numeric login and colon password",
			url:  "example.com", login: "12345", password: "a:b:c",
			line: "example.com/:12345:a:b:c", host: "example.com", expectedLogin: "12345", expectedPassword: "a:b:c",
		},
	}
	for _, tc := range cases {
		for _, loose := range []bool{false, true} {
			for _, noURI := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/loose=%t/no-uri=%t", tc.name, loose, noURI), func(t *testing.T) {
					eng := &Engine{Loose: loose, NoURI: noURI}
					got, err := eng.formatCredentialWith(ulpengine.NewStableFormatter(), Credential{URL: tc.url, Username: tc.login, Password: tc.password})
					if err != nil {
						t.Fatal(err)
					}
					if got != tc.line {
						t.Fatalf("formatted line = %q, want %q", got, tc.line)
					}
					host, _, login, password, ok := ulpengine.ParseLine(got, loose)
					if !ok || host != tc.host || login != tc.expectedLogin || password != tc.expectedPassword {
						t.Fatalf("parsed tuple = (%q, %q, %q), ok=%t; want (%q, %q, %q)", host, login, password, ok, tc.host, tc.expectedLogin, tc.expectedPassword)
					}
				})
			}
		}
	}
}

func TestT02F07F08EngineLabeledFieldFidelityMemorySpillAndZip(t *testing.T) {
	t.Run("memory sink", func(t *testing.T) {
		body := "URL: https://example.com/oauth:callback\nUSER: alice\nPASS: pw\n"
		lines := runLabeledEngine(t, body, false)
		if len(lines) != 1 || lines[0] != "example.com:alice:pw" {
			t.Fatalf("output = %q, want [example.com:alice:pw]", lines)
		}
	})

	t.Run("forced spill", func(t *testing.T) {
		var body strings.Builder
		for i := range 100 {
			fmt.Fprintf(&body, "URL: https://example-%03d.com/%s\nUSER: 12345\nPASS: a:b:c\n\n", i, strings.Repeat("x", 120))
		}
		lines := runLabeledEngine(t, body.String(), false)
		if len(lines) != 100 {
			t.Fatalf("emitted %d records, want 100", len(lines))
		}
		want := "example-041.com/" + strings.Repeat("x", 120) + ":12345:a:b:c"
		if lines[41] != want {
			t.Fatalf("spilled record = %q, want %q", lines[41], want)
		}
		for _, line := range lines {
			host, _, login, password, ok := ulpengine.ParseLine(line, false)
			if !ok || login != "12345" || password != "a:b:c" || !strings.HasPrefix(host, "example-") {
				t.Fatalf("spilled line lost its tuple: %q -> (%q, %q, %q), ok=%t", line, host, login, password, ok)
			}
		}
	})

	t.Run("ZIP member", func(t *testing.T) {
		body := "URL: https://example.com/oauth:callback\nUSER: alice\nPASS: pw\n"
		root := t.TempDir()
		archive := filepath.Join(root, "victim.zip")
		if err := os.WriteFile(archive, zipBytes(t, map[string][]byte{"victim/Passwords.txt": []byte(body)}), 0o644); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		eng := &Engine{Workers: 1, Passwords: []string{""}}
		stats, _, err := eng.Run(context.Background(), archive, &out)
		if err != nil {
			t.Fatal(err)
		}
		if stats.Emitted != 1 || out.String() != "example.com:alice:pw\n" {
			t.Fatalf("emitted=%d output=%q, want one faithful credential", stats.Emitted, out.String())
		}
	})
}

func TestT02F09F12F15SflOutputSurvivesSfuPathsAndODRegeneration(t *testing.T) {
	root, err := os.MkdirTemp(".", ".t02-field-fidelity-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	input := filepath.Join(root, "victim", "Passwords.txt")
	if err := os.MkdirAll(filepath.Dir(input), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "URL: https://example.com/oauth:callback\nUSER: alice\nPASS: pw\n\n" +
		"URL: https://example.com/alternate\nUSER: alice\nPASS: pw\n\n" +
		"URL: example.net\nUSER: 12345\nPASS: a:b:c\n"
	if err := os.WriteFile(input, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sflOutput := filepath.Join(root, "sfl-output.txt")
	outFile, err := os.Create(sflOutput)
	if err != nil {
		t.Fatal(err)
	}
	sfl := &Engine{Workers: 1, Passwords: []string{""}, DedupKey: func(line string) (uint64, bool) {
		return ulpengine.DedupKeyForLine(line, false)
	}}
	sflStats, _, err := sfl.Run(context.Background(), input, outFile)
	if closeErr := outFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if sflStats.Credentials != 3 || sflStats.Emitted != 2 || sflStats.Duplicates != 1 {
		t.Fatalf("sfl stats = %+v, want 3 accepted fields, 2 emitted identities, 1 duplicate URL", sflStats)
	}
	wantULP := "example.com:alice:pw\nexample.net/:12345:a:b:c\n"
	if got, err := os.ReadFile(sflOutput); err != nil || string(got) != wantULP {
		t.Fatalf("sfl ULP output = %q, err=%v; want %q", got, err, wantULP)
	}

	for _, tc := range []struct {
		name        string
		fastPathOff bool
	}{
		{name: "fast", fastPathOff: false},
		{name: "bucketed", fastPathOff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(root, "sfu-"+tc.name+".txt")
			resolved, err := ulpengine.Resolve(ulpengine.Config{
				Inputs: []string{sflOutput}, Output: out, Workers: 1, DedupWorkers: 1,
				FastPathOff: tc.fastPathOff, RunStamp: "20261003_t02_" + tc.name,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !tc.fastPathOff {
				resolved.UseFastPath = true
			} else if resolved.UseFastPath {
				t.Fatal("FastPathOff selected the fast path")
			}
			metrics := &ulpengine.Metrics{}
			if err := ulpengine.Run(context.Background(), resolved, metrics); err != nil {
				t.Fatal(err)
			}
			if got := metrics.LinesUnique.Load(); got != 2 {
				t.Fatalf("sfu %s unique lines = %d, want 2", tc.name, got)
			}
			if got, err := os.ReadFile(out); err != nil || string(got) != wantULP {
				t.Fatalf("sfu %s output = %q, err=%v; want %q", tc.name, got, err, wantULP)
			}
		})
	}

	libDir := filepath.Join(root, "synthetic-library")
	ingest := func(stamp string) *ulpengine.Metrics {
		t.Helper()
		metrics := &ulpengine.Metrics{}
		_, err := ulpengine.Ingest(context.Background(), ulpengine.IngestOptions{
			ULPPath: sflOutput, LibraryDir: libDir, Workers: 1, DedupWorkers: 1,
			FastPathOff: true, RunStamp: stamp,
		}, metrics)
		if err != nil {
			t.Fatal(err)
		}
		return metrics
	}
	first := ingest("20261003_t02_od1")
	if got := first.LinesUnique.Load(); got != 2 {
		t.Fatalf("initial -od unique lines = %d, want 2", got)
	}
	second := ingest("20261003_t02_od2")
	if got := second.LinesUnique.Load(); got != 0 || second.LinesSkippedByDest.Load() != 2 {
		t.Fatalf("duplicate -od run unique=%d skipped-by-dest=%d, want 0/2", got, second.LinesSkippedByDest.Load())
	}

	removed := 0
	if err := filepath.WalkDir(libDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".idx") {
			if err := os.Remove(path); err != nil {
				return err
			}
			removed++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if removed == 0 {
		t.Fatal("-od run did not create a sidecar to exercise regeneration")
	}
	regen := ingest("20261003_t02_od3")
	if got := regen.LinesUnique.Load(); got != 0 || regen.LinesSkippedByDest.Load() != 2 {
		t.Fatalf("post-regeneration -od unique=%d skipped-by-dest=%d, want 0/2", got, regen.LinesSkippedByDest.Load())
	}
}

func runLabeledEngine(t *testing.T, body string, loose bool) []string {
	t.Helper()
	root := t.TempDir()
	input := filepath.Join(root, "victim", "Passwords.txt")
	if err := os.MkdirAll(filepath.Dir(input), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	eng := &Engine{Workers: 1, Loose: loose, Passwords: []string{""}}
	if _, _, err := eng.Run(context.Background(), input, &out); err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
}

// Repairs review finding 2: labeled records whose raw-decoded fields match the
// strict P3 colon fallback (bare fallback host + strict-valid login +
// colon-bearing password) must survive validation with the exact supplied
// fields, as the baseline stores them.
func TestRepairLabeledBareIPColonPasswordFieldsSurvive(t *testing.T) {
	body := "URL: 103.181.181.122\nUSER: alice\nPASS: a:b\n\n" +
		"URL: 192.0.2.1\nUSER: alice\nPASS: a:b\n\n" +
		"URL: 103.1.2.3\nUSER: alice\nPASS: pw:extra\n"
	lines := runLabeledEngine(t, body, false)
	if len(lines) != 3 {
		t.Fatalf("emitted %d records, want 3: %q", len(lines), lines)
	}
	for i, tc := range []struct{ line, host, login, password string }{
		{"103.181.181.122:alice:a:b", "103.181.181.122", "alice", "a:b"},
		{"192.0.2.1:alice:a:b", "192.0.2.1", "alice", "a:b"},
		{"103.1.2.3:alice:pw:extra", "103.1.2.3", "alice", "pw:extra"},
	} {
		if lines[i] != tc.line {
			t.Fatalf("line %d = %q, want %q", i, lines[i], tc.line)
		}
		host, _, login, password, ok := ulpengine.ParseLine(lines[i], false)
		if !ok || host != tc.host || login != tc.login || password != tc.password {
			t.Fatalf("parsed tuple = (%q, %q, %q, %v), want (%q, %q, %q, true)", host, login, password, ok, tc.host, tc.login, tc.password)
		}
	}
}

// A bare-IP port/query fragment stays rejected in strict URI-preserving mode,
// while the NoURI host-only projection (which trims the ?x at projection time,
// like the strict ULP branch trimming before finishParse) keeps admitting the
// host:port record.
func TestRepairLabeledPortQueryFragmentURIVsNoURI(t *testing.T) {
	uri := &Engine{}
	if _, err := uri.formatCredentialWith(ulpengine.NewStableFormatter(), Credential{URL: "103.1.2.3:8080?x", Username: "alice", Password: "pw"}); err == nil {
		t.Fatal("strict URI-preserving mode admitted the bare-IP port/query fragment")
	}
	noURI := &Engine{NoURI: true}
	line, err := noURI.formatCredentialWith(ulpengine.NewStableFormatter(), Credential{URL: "103.1.2.3:8080?x", Username: "alice", Password: "pw"})
	if err != nil {
		t.Fatalf("NoURI host-only projection rejected the record: %v", err)
	}
	if line != "103.1.2.3:8080:alice:pw" {
		t.Fatalf("NoURI line = %q, want %q", line, "103.1.2.3:8080:alice:pw")
	}
}

// Repair, review finding 3: a loose labeled record whose serialized form
// contains a junk pattern spanning the inserted delimiters (password
// "target=x" makes the line contain ":target=") must be rejected at
// validation so nothing reaches the output sink.
func TestRepairLooseLabeledTargetPasswordBoundaryDropped(t *testing.T) {
	body := "URL: example.com\nUSER: $alice\nPASS: target=x\n\n"
	lines := runLabeledEngine(t, body, true)
	if len(lines) != 1 || lines[0] != "" {
		t.Fatalf("loose labeled engine emitted %d record(s) for target=x password: %q; want none", len(lines), lines)
	}
}

// Repair, lane gate: strict-lane admissions (records ValidateFields admits via
// its strict component predicate) skip the raw-consumer ParseLine confirmation
// in formatCredentialWith — the stored round-trip remains the only post-check —
// while loose-lane defect tuples keep the full dual-fidelity check and stay
// rejected at emission.
func TestRepairStrictLaneSkipsRawRecheck(t *testing.T) {
	// Strict-lane records must still emit exactly as before, in both strict
	// and loose engine modes, with the exact supplied fields.
	for _, loose := range []bool{false, true} {
		lines := runLabeledEngine(t, "URL: example.com\nUSER: alice\nPASS: pw\n\n"+
			"URL: https://example.com/oauth:callback\nUSER: bob\nPASS: s3cret\n", loose)
		want := []string{"example.com:alice:pw", "example.com:bob:s3cret"}
		if len(lines) != len(want) {
			t.Fatalf("loose=%t: emitted %d line(s): %q; want %q", loose, len(lines), lines, want)
		}
		for i, w := range want {
			if lines[i] != w {
				t.Fatalf("loose=%t: line %d = %q, want %q", loose, i, lines[i], w)
			}
			host, _, login, password, ok := ulpengine.ParseLine(lines[i], loose)
			if !ok || host != "example.com" || login != strings.SplitN(w, ":", 3)[1] || password != strings.SplitN(w, ":", 3)[2] {
				t.Fatalf("loose=%t: parsed %q = (%q, %q, %q, %v)", loose, lines[i], host, login, password, ok)
			}
		}
	}

	// Loose-lane defect tuples remain rejected at emission: the raw-consumer
	// confirmation is unchanged for records admitted via the loose-only branch.
	for _, tc := range []struct{ url, user, pass string }{
		{"example.com", "$alice", "a:b"},
		{"example.com", "Zoë", "a:b"},
		{"example.com", "$alice", "target=x"},
	} {
		body := "URL: " + tc.url + "\nUSER: " + tc.user + "\nPASS: " + tc.pass + "\n"
		lines := runLabeledEngine(t, body, true)
		if len(lines) != 1 || lines[0] != "" {
			t.Fatalf("loose labeled engine emitted %d record(s) for %q/%q/%q: %q; want none",
				len(lines), tc.url, tc.user, tc.pass, lines)
		}
	}

	// Android-admitted records bypass the strict component check inside
	// ValidateFields, so they keep the full dual-fidelity check (safe default)
	// and must still emit with exact fields.
	eng := &Engine{}
	line, err := eng.formatCredentialWith(ulpengine.NewStableFormatter(), Credential{
		URL:      "android://com.example.app",
		Username: "alice",
		Password: "pw",
	})
	if err != nil {
		t.Fatalf("android-admitted record rejected: %v", err)
	}
	if line == "" {
		t.Fatal("android-admitted record emitted an empty line")
	}
}

// Repair, review finding 1 (HIGH): dual-fidelity emission. The raw consumer
// used by DedupKeyForLine and sfl's downstream ingest (ParseLine) rejects the
// identities that the stored decoder alone used to accept, so those explicit
// controls must stay rejected — never "some decoder accepted the output".
func TestRepairRawConsumerControlsRejectDefectEmissions(t *testing.T) {
	for _, line := range []string{
		"example.com:$alice:a:b",
		"example.com:Zoë:a:b",
		"103.1.2.3:8080?x:alice:pw",
	} {
		if _, _, _, _, ok := ulpengine.ParseLine(line, true); ok {
			t.Fatalf("raw loose consumer accepted defective emission %q", line)
		}
		if _, _, _, _, ok := ulpengine.ParseLine(line, false); ok {
			t.Fatalf("raw strict consumer accepted defective emission %q", line)
		}
	}
}

// Repair, review finding 1 (HIGH): baseline truth — the defect tuples that
// stored decoding alone used to emit (loose $alice/Zoë with colon-bearing
// password "a:b") must be rejected at emission through the malformed-quality
// path, because the raw consumer rejects them too.
func TestRepairLooseLabeledDefectTuplesRejectedAtEmission(t *testing.T) {
	for _, tc := range []struct{ url, user, pass string }{
		{"example.com", "$alice", "a:b"},
		{"example.com", "Zoë", "a:b"},
	} {
		body := "URL: " + tc.url + "\nUSER: " + tc.user + "\nPASS: " + tc.pass + "\n"
		lines := runLabeledEngine(t, body, true)
		if len(lines) != 1 || lines[0] != "" {
			t.Fatalf("loose labeled engine emitted %d record(s) for %q/%q: %q; want none",
				len(lines), tc.user, tc.pass, lines)
		}
	}
}

// formatCredentialWith error-reason and fallback paths: the password>64
// reason is preserved through the field validator, and a strict host:port/path
// record whose URI-preserving stable form fails raw fidelity emits exactly
// once via the permitted host-only fallback (same supplied tuple).
func TestRepairFormatCredentialWithCapReasonAndHostOnlyFallback(t *testing.T) {
	eng := &Engine{}
	_, err := eng.formatCredentialWith(ulpengine.NewStableFormatter(), Credential{
		URL: "example.com", Username: "alice", Password: strings.Repeat("a", 65),
	})
	if err == nil || !strings.Contains(err.Error(), "password>64") {
		t.Fatalf("over-cap password: want password>64 reason, got %v", err)
	}

	strict := &Engine{}
	line, err := strict.formatCredentialWith(ulpengine.NewStableFormatter(), Credential{
		URL: "103.1.2.3:8080/pa:th", Username: "alice", Password: "pw",
	})
	if err != nil {
		t.Fatalf("host:port/path record rejected: %v", err)
	}
	if want := "103.1.2.3:8080:alice:pw"; line != want {
		t.Fatalf("host-only fallback line=%q, want %q", line, want)
	}

	// NoURI mode must not attempt the host-only fallback (the caller already
	// projected the URL): the projected host-only form is admitted directly
	// with the exact supplied tuple (preserved baseline behavior).
	noURI := &Engine{NoURI: true}
	line, err = noURI.formatCredentialWith(ulpengine.NewStableFormatter(), Credential{
		URL: "103.1.2.3:8080?x", Username: "alice", Password: "pw",
	})
	if err != nil {
		t.Fatalf("NoURI-projected host-only form rejected: %v", err)
	}
	if want := "103.1.2.3:8080:alice:pw"; line != want {
		t.Fatalf("NoURI projected line=%q, want %q", line, want)
	}
}
