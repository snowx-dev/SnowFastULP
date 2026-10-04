package history_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/history"
)

// The table pins the SQLite URI contract. The path's own shape decides: a
// drive path keeps the drive in the path component, a UNC path becomes the
// docs' network form (empty authority, //server/share/... path), and every
// other byte — spaces, #, Unicode, and on Unix a backslash itself — is
// filename data that is percent-escaped verbatim.
func TestSQLiteFileURI(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{name: "drive path", path: `C:\dir\history.sqlite3`, want: "file:///C:/dir/history.sqlite3"},
		{name: "drive forward slashes", path: "C:/dir/history.sqlite3", want: "file:///C:/dir/history.sqlite3"},
		{name: "valid UNC", path: `\\server\share\dir\history.sqlite3`, want: "file:////server/share/dir/history.sqlite3"},
		{name: "UNC spaces", path: `\\server\my share\dir with space\db.sqlite3`, want: "file:////server/my%20share/dir%20with%20space/db.sqlite3"},
		{name: "UNC hash", path: `\\server\share\di#r\db.sqlite3`, want: "file:////server/share/di%23r/db.sqlite3"},
		{name: "UNC unicode", path: `\\server\share\дир\db.sqlite3`, want: "file:////server/share/%D0%B4%D0%B8%D1%80/db.sqlite3"},
		{name: "unix absolute", path: "/srv/data/history.sqlite3", want: "file:///srv/data/history.sqlite3"},
		{name: "unix space", path: "/srv/my data/history.sqlite3", want: "file:///srv/my%20data/history.sqlite3"},
		{name: "unix hash", path: "/srv/di#r/history.sqlite3", want: "file:///srv/di%23r/history.sqlite3"},
		{name: "unix unicode", path: "/srv/дир/history.sqlite3", want: "file:///srv/%D0%B4%D0%B8%D1%80/history.sqlite3"},
		{name: "unix backslash is filename data", path: `/tmp/back\slash/history.sqlite3`, want: "file:///tmp/back%5Cslash/history.sqlite3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := history.SQLiteFileURI(tc.path)
			if err != nil {
				t.Fatalf("SQLiteFileURI(%q) error = %v", tc.path, err)
			}
			if got != tc.want {
				t.Fatalf("SQLiteFileURI(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// A path that names a server but no share (or neither) is a malformed UNC:
// openDatabase must reject it with an error that names the problem and the
// path instead of feeding SQLite an invalid authority.
func TestOpenDatabaseRejectsMalformedUNC(t *testing.T) {
	for _, path := range []string{
		`\\`,
		`\\server`,
		`\\server\`,
		`\\\server\share\dir\history.sqlite3`,
	} {
		err := history.OpenDatabaseProbe(path, true)
		if err == nil {
			t.Fatalf("OpenDatabaseProbe(%q) succeeded; want malformed-UNC rejection", path)
		}
		if !strings.Contains(err.Error(), "UNC") || !strings.Contains(err.Error(), path) {
			t.Fatalf("OpenDatabaseProbe(%q) error = %v, want an actionable UNC error naming the path", path, err)
		}
	}
}

// Mid-path backslashes are ordinary filename bytes on Unix-flavored relative
// paths: the URI escapes them as literal data. (On a drive-shaped path the
// backslash is a separator and is normalized instead, per the table above.)
func TestSQLiteFileURIEscapesMidPathBackslashAsData(t *testing.T) {
	got, err := history.SQLiteFileURI(`we\ird/db.sqlite3`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `%5C`) {
		t.Fatalf("SQLiteFileURI = %q, want backslash escaped as %%5C", got)
	}
}

// The emitted UNC URI must round-trip through net/url the way openDatabase
// consumes it: empty authority, //server/share/... path, query params appended
// without disturbing the network path.
func TestSQLiteFileURIUNCRoundTripsThroughURIParse(t *testing.T) {
	uri, err := history.SQLiteFileURI(`\\server\my share\dir\history.sqlite3`)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", uri, err)
	}
	if parsed.Host != "" {
		t.Fatalf("authority = %q, want empty (SQLite rejects any other)", parsed.Host)
	}
	if parsed.Path != `//server/my share/dir/history.sqlite3` {
		t.Fatalf("path = %q, want the unescaped //server/share/... network path", parsed.Path)
	}
	query := parsed.Query()
	query.Set("mode", "ro")
	parsed.RawQuery = query.Encode()
	if got := parsed.String(); got != `file:////server/my%20share/dir/history.sqlite3?mode=ro` {
		t.Fatalf("round trip = %q", got)
	}
}

// The emitted UNC form must be one SQLite actually accepts: the failure has to
// come from the VFS not finding the share, never from URI parsing.
func TestOpenDatabaseUNCURIReachesVFS(t *testing.T) {
	err := history.OpenDatabaseProbe(`\\server\share\dir\history.sqlite3`, true)
	if err == nil {
		t.Fatal("probe unexpectedly opened a nonexistent share")
	}
	if strings.Contains(err.Error(), "invalid uri authority") {
		t.Fatalf("SQLite rejected the URI form; UNC databases could never open: %v", err)
	}
}
