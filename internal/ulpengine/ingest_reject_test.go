package ulpengine

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ingestRejectFixture writes a three-line ULP: two valid lines and one whose
// password exceeds the 64-char cap (the dominant real-world reject class).
func ingestRejectFixture(t *testing.T, dir string) string {
	t.Helper()
	in := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(in, []byte(
		"https://a.example.com/p1:user1:pass1\n"+
			"https://b.example.com/p2:user2:pass2\n"+
			"https://c.example.com/p3:user3:"+strings.Repeat("p", 65)+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	return in
}

func TestIngestRejectRecorderCapturesPasswordTooLong(t *testing.T) {
	d := t.TempDir()
	in := ingestRejectFixture(t, d)
	lib := filepath.Join(d, "lib")

	f, rejectPath, err := CreateArtifactFile(d, "ingest-rejected", ".txt", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	rr := NewRejectRecorderFile(f)

	m := &Metrics{}
	if _, err := Ingest(context.Background(), IngestOptions{
		ULPPath:    in,
		LibraryDir: lib,
		Workers:    2,
		Reject:     rr,
	}, m); err != nil {
		t.Fatal(err)
	}
	if err := rr.Close(); err != nil {
		t.Fatal(err)
	}

	if got := m.LinesAccepted.Load(); got != 2 {
		t.Fatalf("linesAccepted = %d, want 2", got)
	}
	if got := m.LinesRejected.Load(); got != 1 {
		t.Fatalf("linesRejected = %d, want 1", got)
	}
	if got := m.LinesPasswordTooLong.Load(); got != 1 {
		t.Fatalf("linesPasswordTooLong = %d, want 1", got)
	}
	if got := m.LinesTooLong.Load() + m.LinesMalformed.Load() + m.LinesUnrepresentable.Load(); got != 0 {
		t.Fatalf("unexpected other reject tallies: tooLong=%d malformed=%d unrepresentable=%d",
			m.LinesTooLong.Load(), m.LinesMalformed.Load(), m.LinesUnrepresentable.Load())
	}

	raw, err := os.ReadFile(rejectPath)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	if !strings.Contains(out, "[password>64]") {
		t.Fatalf("reject artifact missing [password>64] row:\n%s", out)
	}
	if !strings.Contains(out, "in.txt") {
		t.Fatalf("reject artifact missing source path column:\n%s", out)
	}
}

func TestIngestNilRejectStaysSilent(t *testing.T) {
	d := t.TempDir()
	in := ingestRejectFixture(t, d)
	lib := filepath.Join(d, "lib")

	m := &Metrics{}
	if _, err := Ingest(context.Background(), IngestOptions{
		ULPPath:    in,
		LibraryDir: lib,
		Workers:    2,
		// Reject deliberately nil: the engine must neither error nor create
		// any reject artifact of its own.
	}, m); err != nil {
		t.Fatal(err)
	}
	if got := m.LinesRejected.Load(); got != 1 {
		t.Fatalf("linesRejected = %d, want 1 (counters tick regardless of recorder)", got)
	}
	if got := m.LinesPasswordTooLong.Load(); got != 1 {
		t.Fatalf("linesPasswordTooLong = %d, want 1", got)
	}
	var rejects []string
	err := filepath.WalkDir(lib, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(de.Name(), "reject") {
			rejects = append(rejects, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rejects) != 0 {
		t.Fatalf("nil Reject must create no artifact, found: %v", rejects)
	}
}

func TestLogCompletionRejectReasonsLine(t *testing.T) {
	dbgPath := filepath.Join(t.TempDir(), "dbg.log")
	d, err := NewDebugLog(dbgPath)
	if err != nil {
		t.Fatal(err)
	}

	// Zero tallies: no rejectReasons line at all.
	d.LogCompletion(&Metrics{}, time.Second, nil)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dbgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "rejectReasons") {
		t.Fatalf("zero tallies must not emit a rejectReasons line:\n%s", raw)
	}

	// Nonzero tallies: full four-reason breakdown on one line.
	d, err = NewDebugLog(dbgPath)
	if err != nil {
		t.Fatal(err)
	}
	m := &Metrics{}
	m.LinesTooLong.Store(2)
	m.LinesPasswordTooLong.Store(112000)
	m.LinesMalformed.Store(1293)
	m.LinesUnrepresentable.Store(4)
	d.LogCompletion(m, time.Second, nil)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(dbgPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "rejectReasons tooLong=2 passwordTooLong=112000 malformed=1293 unrepresentable=4"
	if !strings.Contains(string(raw), want) {
		t.Fatalf("completion line missing %q:\n%s", want, raw)
	}
}
