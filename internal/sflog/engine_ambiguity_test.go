package sflog

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEngineDebugAmbiguityCountsAcceptedSourceRecords(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "Passwords.txt")
	const line = "https://example.com/login:alice:pw:tail\n"
	if err := os.WriteFile(input, []byte(line+line), 0o600); err != nil {
		t.Fatal(err)
	}

	engine := &Engine{Workers: 2, Debug: func(string, ...any) {}}
	var output bytes.Buffer
	stats, _, err := engine.Run(context.Background(), input, &output)
	if err != nil {
		t.Fatal(err)
	}
	if stats.AmbiguityTotal != 2 || stats.AmbiguityPathOrPassword != 2 {
		t.Fatalf("debug ambiguity counts = total %d path %d, want 2 each", stats.AmbiguityTotal, stats.AmbiguityPathOrPassword)
	}

	quiet := &Engine{Workers: 2}
	var quietOutput bytes.Buffer
	quietStats, _, err := quiet.Run(context.Background(), input, &quietOutput)
	if err != nil {
		t.Fatal(err)
	}
	if quietStats.AmbiguityTotal != 0 || quietOutput.String() != output.String() {
		t.Fatalf("quiet result ambiguity=%d output parity=%v", quietStats.AmbiguityTotal, quietOutput.String() == output.String())
	}
}

func TestEngineDebugAmbiguityCountsArchiveRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	member, err := zw.Create("victim/Passwords.txt")
	if err != nil {
		t.Fatal(err)
	}
	const line = "https://example.com/login:alice:pw:tail\n"
	if _, err := member.Write([]byte(line + line)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	engine := &Engine{Workers: 2, Debug: func(string, ...any) {}}
	var output bytes.Buffer
	stats, _, err := engine.Run(context.Background(), path, &output)
	if err != nil {
		t.Fatal(err)
	}
	if stats.AmbiguityTotal != 2 || stats.AmbiguityPathOrPassword != 2 {
		t.Fatalf("archive ambiguity counts = total %d path %d, want 2 each", stats.AmbiguityTotal, stats.AmbiguityPathOrPassword)
	}
}

// TestRepairWhitespaceVariantsCountInEngineWorkers extends the sfl ambiguity
// counts to the review finding: three accepted whitespace variants yield
// exactly total=3 / path_or_password=3 (pre-dedup, not zero, not
// double-counted) across worker counts, and dedup to the clean line's output.
func TestRepairWhitespaceVariantsCountInEngineWorkers(t *testing.T) {
	variants := []string{
		"https://example.com/login:alice:pw:tail\n",
		"https://example.com/login:alice :pw:tail\n",
		"https://example.com/login:alice\t:pw:tail\n",
	}
	run := func(t *testing.T, body, debugTag string, workers int, withDebug bool) (int, int, string) {
		t.Helper()
		input := filepath.Join(t.TempDir(), "Passwords.txt")
		if err := os.WriteFile(input, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		engine := &Engine{Workers: workers}
		if withDebug {
			engine.Debug = func(string, ...any) {}
		}
		var output bytes.Buffer
		stats, _, err := engine.Run(context.Background(), input, &output)
		if err != nil {
			t.Fatalf("%s: %v", debugTag, err)
		}
		return stats.AmbiguityTotal, stats.AmbiguityPathOrPassword, output.String()
	}
	baseBody := strings.Repeat(variants[0], 3)
	variantBody := strings.Join(variants, "")
	for _, workers := range []int{1, 4} {
		variantTotal, variantPath, variantOutput := run(t, variantBody, fmt.Sprintf("workers=%d", workers), workers, true)
		if variantTotal != 3 || variantPath != 3 {
			t.Fatalf("workers=%d whitespace variant counts = total %d path %d, want 3 each", workers, variantTotal, variantPath)
		}
		_, _, baseOutput := run(t, baseBody, fmt.Sprintf("base workers=%d", workers), workers, true)
		if variantOutput != baseOutput {
			t.Fatalf("workers=%d variant output %q, want clean-line output %q", workers, variantOutput, baseOutput)
		}
		quietTotal, _, quietOutput := run(t, variantBody, fmt.Sprintf("quiet workers=%d", workers), workers, false)
		if quietTotal != 0 || quietOutput != variantOutput {
			t.Fatalf("workers=%d quiet engine counts total %d output parity %v", workers, quietTotal, quietOutput == variantOutput)
		}
	}
}

// TestRepairEngineWitnessPinnedAsLabeledFields pins the distinct witness
// identity through the labeled SFL layout: fed as literal labeled fields the
// record is accepted (mixed layout wins) and contributes no ambiguity count,
// because labeled fields carry no colon-splitting choice to witness.
func TestRepairEngineWitnessPinnedAsLabeledFields(t *testing.T) {
	body := "URL: https://example.com/login\nUsername: alice\nPassword: pw:tail\n"
	var counts AmbiguityCounts
	var credential Credential
	emitted := 0
	_, err := ParseCredentialsStreamWithDiagnostics(strings.NewReader(body), "source", t.TempDir(), false, true, func(c AmbiguityCounts) {
		counts = c
	}, func(c Credential) error {
		emitted++
		credential = c
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if emitted != 1 {
		t.Fatalf("emitted = %d, want 1 labeled witness record", emitted)
	}
	if counts.Total != 0 || counts.PathOrPassword != 0 || counts.PortOrLogin != 0 {
		t.Fatalf("labeled witness contributed ambiguity counts: %+v", counts)
	}
	if credential.Username != "alice" || credential.Password != "pw:tail" || credential.URL != "https://example.com/login" {
		t.Fatalf("labeled witness record = %+v, want (https://example.com/login, alice, pw:tail)", credential)
	}
}

// TestRepairEngineConcurrentIndependentRuns pins that independent concurrent
// engine runs each count their own witnesses: no cross-run drift or loss.
func TestRepairEngineConcurrentIndependentRuns(t *testing.T) {
	body := strings.Join([]string{
		"https://example.com/login:alice:pw:tail\n",
		"https://example.com/login:alice :pw:tail\n",
		"https://example.com/login:alice\t:pw:tail\n",
	}, "")
	inputs := make([]string, 4)
	tempDirs := make([]string, 4)
	for i := range 4 {
		inputs[i] = filepath.Join(t.TempDir(), "Passwords.txt")
		if err := os.WriteFile(inputs[i], []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		tempDirs[i] = t.TempDir()
	}
	type result struct {
		total int
		path  int
		out   string
		err   error
	}
	results := make(chan result, 4)
	for i := range 4 {
		input, tempDir := inputs[i], tempDirs[i]
		go func() {
			engine := &Engine{Workers: 4, TempDir: tempDir, Debug: func(string, ...any) {}}
			var output bytes.Buffer
			stats, _, runErr := engine.Run(context.Background(), input, &output)
			res := result{out: output.String(), err: runErr}
			if runErr == nil {
				res.total, res.path = stats.AmbiguityTotal, stats.AmbiguityPathOrPassword
			}
			results <- res
		}()
	}
	var reference string
	for range 4 {
		res := <-results
		if res.err != nil {
			t.Fatalf("concurrent run: %v", res.err)
		}
		if res.total != 3 || res.path != 3 {
			t.Fatalf("concurrent run counts = total %d path %d, want 3 each", res.total, res.path)
		}
		if reference == "" {
			reference = res.out
		} else if res.out != reference {
			t.Fatalf("concurrent run output %q, want stable %q", res.out, reference)
		}
	}
}
