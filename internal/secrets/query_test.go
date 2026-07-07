package secrets

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// seedDB writes findings through the real Store (schema + accumulation) and
// returns the DB path, so QueryDB is exercised against production-shaped data.
func seedDB(t *testing.T, findings ...Finding) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.sqlite")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, f := range findings {
		st.Add(f)
	}
	if _, err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

func collect(t *testing.T, path string, o QueryOpts) []Match {
	t.Helper()
	var got []Match
	n, err := QueryDB(path, o, func(m Match) error {
		got = append(got, m)
		return nil
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != len(got) {
		t.Fatalf("returned count %d != emitted %d", n, len(got))
	}
	return got
}

var (
	awsFinding = Finding{RuleID: "aws-access-key", RuleName: "AWS Access Key",
		Secret: "AKIAIOSFODNN7EXAMPLE", Score: 80, Severity: "high", SourcePath: "log.zip!config.env"}
	ghFinding = Finding{RuleID: "github-pat", RuleName: "GitHub PAT",
		Secret: "ghp_1234567890abcdefghijklmnopqrstuvwx12", Score: -1, SourcePath: ""}
)

func TestQueryDBReturnsAll(t *testing.T) {
	path := seedDB(t, awsFinding, ghFinding)
	for _, typ := range []string{"", "*", "  "} {
		if got := collect(t, path, QueryOpts{Type: typ}); len(got) != 2 {
			t.Fatalf("Type=%q: got %d rows, want 2", typ, len(got))
		}
	}
}

func TestQueryDBFiltersByType(t *testing.T) {
	path := seedDB(t, awsFinding, ghFinding)
	cases := map[string]int{
		"aws":     1, // matches rule_id aws-access-key
		"AWS":     1, // case-insensitive
		"key":     1, // matches rule_name "AWS Access Key"
		"github":  1, // matches rule_id
		"pat":     1, // matches rule_name "GitHub PAT"
		"nomatch": 0,
	}
	for pattern, want := range cases {
		if got := collect(t, path, QueryOpts{Type: pattern}); len(got) != want {
			t.Fatalf("Type=%q: got %d rows, want %d", pattern, len(got), want)
		}
	}
}

func TestQueryDBSinceFilters(t *testing.T) {
	path := seedDB(t, awsFinding)
	if got := collect(t, path, QueryOpts{Since: time.Now().Add(-time.Hour)}); len(got) != 1 {
		t.Fatalf("since 1h ago: got %d, want 1", len(got))
	}
	if got := collect(t, path, QueryOpts{Since: time.Now().Add(time.Hour)}); len(got) != 0 {
		t.Fatalf("since 1h ahead: got %d, want 0", len(got))
	}
}

func TestQueryDBLimit(t *testing.T) {
	path := seedDB(t, awsFinding, ghFinding)
	if got := collect(t, path, QueryOpts{Limit: 1}); len(got) != 1 {
		t.Fatalf("limit 1: got %d, want 1", len(got))
	}
}

func TestQueryDBDecodesNullColumns(t *testing.T) {
	// ghFinding stores NULL score/severity/validation and NULL source_path.
	path := seedDB(t, ghFinding)
	got := collect(t, path, QueryOpts{})
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	m := got[0]
	if m.Score != -1 {
		t.Fatalf("Score = %d, want -1 for NULL", m.Score)
	}
	if m.Severity != "" || m.Validation != "" || m.SourcePath != "" {
		t.Fatalf("NULL text columns not decoded to empty: %+v", m)
	}
	if m.SeenCount != 1 {
		t.Fatalf("SeenCount = %d, want 1", m.SeenCount)
	}
	if m.LastSeen.IsZero() {
		t.Fatalf("LastSeen not decoded")
	}
}

func TestQueryDBNonNullColumns(t *testing.T) {
	path := seedDB(t, awsFinding)
	m := collect(t, path, QueryOpts{})[0]
	if m.Score != 80 || m.Severity != "high" || m.SourcePath != "log.zip!config.env" {
		t.Fatalf("non-null columns wrong: %+v", m)
	}
}

func TestQueryDBMissingDB(t *testing.T) {
	_, err := QueryDB(filepath.Join(t.TempDir(), "absent.sqlite"), QueryOpts{}, func(Match) error { return nil })
	if err == nil {
		t.Fatal("expected an error for a missing DB")
	}
}

func TestQueryDBReadOnlyDoesNotCreate(t *testing.T) {
	// A read against a missing path must not leave a DB behind (read-only,
	// existence-checked): sfs -sec should never fabricate an empty store.
	path := filepath.Join(t.TempDir(), "should-not-exist.sqlite")
	_, _ = QueryDB(path, QueryOpts{}, func(Match) error { return nil })
	if _, err := os.Stat(path); err == nil {
		t.Fatal("QueryDB created a DB file for a missing path")
	}
}

// TestQueryDBOrdersByLastSeenDesc locks in the most-recent-first ordering that
// `sfs -sec` relies on. The Store stamps every row with write-time `now`, so we
// reopen the DB and pin distinct last_seen values to make the order
// deterministic and independent of insert timing.
func TestQueryDBOrdersByLastSeenDesc(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.sqlite")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	slackFinding := Finding{RuleID: "slack-token", RuleName: "Slack Token",
		Secret: "SLACK_TOKEN_FIXTURE", Score: -1, SourcePath: ""}
	for _, f := range []Finding{awsFinding, ghFinding, slackFinding} {
		st.Add(f)
	}
	if _, err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	stamps := map[string]int64{
		"AWS Access Key": 1_000,
		"GitHub PAT":     3_000,
		"Slack Token":    2_000,
	}
	for name, ts := range stamps {
		if _, err := db.Exec("UPDATE secrets SET last_seen=? WHERE rule_name=?", ts, name); err != nil {
			t.Fatalf("set last_seen for %s: %v", name, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	got := collect(t, path, QueryOpts{})
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	wantOrder := []string{"GitHub PAT", "Slack Token", "AWS Access Key"} // 3000, 2000, 1000
	for i, want := range wantOrder {
		if got[i].RuleName != want {
			t.Fatalf("row %d = %q, want %q (desc by last_seen); got order: %s",
				i, got[i].RuleName, want, ruleNames(got))
		}
	}
}

func ruleNames(ms []Match) string {
	var b strings.Builder
	for i, m := range ms {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(m.RuleName)
	}
	return b.String()
}

// TestQueryDBLimitReturnsMostRecent verifies -l caps the result to the N newest
// rows (LIMIT applies after the ORDER BY), not an arbitrary N.
func TestQueryDBLimitReturnsMostRecent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.sqlite")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	slackFinding := Finding{RuleID: "slack-token", RuleName: "Slack Token",
		Secret: "SLACK_TOKEN_FIXTURE", Score: -1, SourcePath: ""}
	for _, f := range []Finding{awsFinding, ghFinding, slackFinding} {
		st.Add(f)
	}
	if _, err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	stamps := map[string]int64{
		"AWS Access Key": 1_000,
		"GitHub PAT":     3_000,
		"Slack Token":    2_000,
	}
	for name, ts := range stamps {
		if _, err := db.Exec("UPDATE secrets SET last_seen=? WHERE rule_name=?", ts, name); err != nil {
			t.Fatalf("set last_seen for %s: %v", name, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	got := collect(t, path, QueryOpts{Limit: 2})
	if len(got) != 2 {
		t.Fatalf("limit 2: got %d rows, want 2", len(got))
	}
	wantOrder := []string{"GitHub PAT", "Slack Token"} // two newest
	for i, want := range wantOrder {
		if got[i].RuleName != want {
			t.Fatalf("row %d = %q, want %q (two most recent); got order: %s",
				i, got[i].RuleName, want, ruleNames(got))
		}
	}
}
