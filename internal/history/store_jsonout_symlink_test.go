package history_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/history"
)

// The dangling-symlink escape: a -json target that is a symlink to the
// not-yet-created history database dangles at validation time, so canonical
// and SameFile compares both pass — and the stream's create then follows the
// link, materializing and truncating the database. The collision helper must
// resolve the target's symlink chain against the effective endpoints too.
func TestJSONOutHistoryCollisionSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	histDir := filepath.Join(dir, "hist")
	if err := os.MkdirAll(histDir, 0o700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(histDir, "history.sqlite3")
	link := filepath.Join(histDir, "link.jsonl")
	if err := os.Symlink(db, link); err != nil {
		t.Fatal(err)
	}
	// The link truncates the database file itself: it collides with the
	// spellings whose store would open that file, not with a -history-path
	// that names a WAL sidecar as its database.
	for _, raw := range []string{histDir, histDir + string(filepath.Separator), db} {
		if hit := history.JSONOutHistoryCollision(link, raw); hit == "" {
			t.Fatalf("symlink target must collide with %q history endpoint", raw)
		}
	}
	// An unrelated symlink stays clear.
	other := filepath.Join(dir, "other.jsonl")
	if err := os.Symlink(filepath.Join(dir, "nowhere.ndjson"), other); err != nil {
		t.Fatal(err)
	}
	if hit := history.JSONOutHistoryCollision(other, histDir); hit != "" {
		t.Fatalf("unrelated symlink reported as collision: %q", hit)
	}
	// A plain (non-symlink) foreign target stays clear.
	if hit := history.JSONOutHistoryCollision(filepath.Join(dir, "stats.jsonl"), histDir); hit != "" {
		t.Fatalf("plain foreign target reported as collision: %q", hit)
	}
}
