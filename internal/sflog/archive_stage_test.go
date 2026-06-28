package sflog

import (
	"context"
	"path/filepath"
	"testing"
)

// stageRecorder captures the ordered stage transitions an archive reader
// publishes, so tests can assert the worker panel would show the right labels.
type stageRecorder struct {
	stages []WorkerStage
}

func (r *stageRecorder) sink() func(WorkerStage) {
	return func(s WorkerStage) { r.stages = append(r.stages, s) }
}

func (r *stageRecorder) first(s WorkerStage) int {
	for i, got := range r.stages {
		if got == s {
			return i
		}
	}
	return -1
}

func (r *stageRecorder) has(s WorkerStage) bool { return r.first(s) >= 0 }

func TestReadZipCredentialsPublishesTestingThenExtracting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.zip")
	writeEncryptedTestZip(t, path, "ice", "victim/Passwords.txt",
		"URL: https://stage.example.com/login\nUSER: a\nPASS: p\n")

	rec := &stageRecorder{}
	var emitted int
	ec := extractCtx{
		passwords: []string{"", "wrong", "ice"},
		display:   path,
		emit:      func(Credential) { emitted++ },
		onIssue:   func(string, IssueKind, error) {},
		setStage:  rec.sink(),
	}
	if _, err := readZipCredentials(context.Background(), path, ec, 100); err != nil {
		t.Fatalf("readZipCredentials: %v", err)
	}
	if emitted != 1 {
		t.Fatalf("emitted = %d, want 1", emitted)
	}
	tp, ex := rec.first(StageTestingPassword), rec.first(StageExtracting)
	if tp < 0 {
		t.Fatalf("encrypted zip never published testing-password: %v", rec.stages)
	}
	if ex < 0 {
		t.Fatalf("zip never published extracting: %v", rec.stages)
	}
	if tp > ex {
		t.Fatalf("testing-password (%d) must precede extracting (%d): %v", tp, ex, rec.stages)
	}
}

func TestReadZipCredentialsPlainSkipsTestingPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.zip")
	writeTestZip(t, path, map[string]string{
		"victim/Passwords.txt": "URL: https://plain.example.com/login\nUSER: a\nPASS: p\n",
	})

	rec := &stageRecorder{}
	ec := extractCtx{
		passwords: []string{""},
		display:   path,
		emit:      func(Credential) {},
		onIssue:   func(string, IssueKind, error) {},
		setStage:  rec.sink(),
	}
	if _, err := readZipCredentials(context.Background(), path, ec, 100); err != nil {
		t.Fatalf("readZipCredentials: %v", err)
	}
	if rec.has(StageTestingPassword) {
		t.Fatalf("plain zip should not test passwords: %v", rec.stages)
	}
	if !rec.has(StageExtracting) {
		t.Fatalf("plain zip never published extracting: %v", rec.stages)
	}
}
