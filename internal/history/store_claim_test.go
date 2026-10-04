package history

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestSQLiteFileURIUsesPathNotAuthorityForWindowsDrive(t *testing.T) {
	got, err := sqliteFileURI(`C:\Users\analyst\history.sqlite3`)
	if err != nil {
		t.Fatal(err)
	}
	want := "file:///C:/Users/analyst/history.sqlite3"
	if got != want {
		t.Fatalf("sqliteFileURI() = %q, want %q", got, want)
	}
}

func TestOpenPreservesLiteralBackslashInUnixPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("backslash is a path separator on Windows")
	}
	path := filepath.Join(t.TempDir(), `history\literal.sqlite3`)
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database was not created at the literal-backslash path: %v", err)
	}
}

func TestOpenClaimExcludesForeignSchemaBetweenInspectionAndClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite3")
	foreign, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	foreign.SetMaxOpenConns(1)
	conn, err := foreign.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "PRAGMA busy_timeout = 0"); err != nil {
		t.Fatal(err)
	}

	foreignClaimed := false
	store, err := openWithClaimHook(path, func() error {
		if _, beginErr := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); beginErr != nil {
			if isBusyOrLocked(beginErr) {
				return nil
			}
			return fmt.Errorf("foreign begin: %w", beginErr)
		}
		foreignClaimed = true
		if _, createErr := conn.ExecContext(context.Background(), "CREATE TABLE foreign_data(value TEXT)"); createErr != nil {
			return createErr
		}
		_, commitErr := conn.ExecContext(context.Background(), "COMMIT")
		return commitErr
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if foreignClaimed {
		t.Fatal("foreign schema transaction acquired the database between ownership inspection and claim")
	}

	var appID, foreignObjects int
	if err := conn.QueryRowContext(context.Background(), "PRAGMA application_id").Scan(&appID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(context.Background(), "SELECT count(*) FROM sqlite_master WHERE name = 'foreign_data'").Scan(&foreignObjects); err != nil {
		t.Fatal(err)
	}
	if appID != applicationID || foreignObjects != 0 {
		t.Fatalf("database state = application_id %d, foreign objects %d; want SnowFast-owned without foreign schema", appID, foreignObjects)
	}
}

func TestOpenRejectsForeignSchemaThatWinsClaimRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite3")
	foreign, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	foreign.SetMaxOpenConns(1)
	conn, err := foreign.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The Open goroutine's claim transaction is retrying BEGIN IMMEDIATE while
	// this conn holds RESERVED; a rollback-journal COMMIT below needs
	// EXCLUSIVE, and under CI load a concurrent lock attempt can transiently
	// hold SHARED, surfacing SQLITE_BUSY on this previously bare (no
	// busy_timeout) connection — seen in parallel runs. busy_timeout converts
	// that transient into a bounded wait instead of a test failure.
	if _, err := conn.ExecContext(context.Background(), "PRAGMA busy_timeout = 5000"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "CREATE TABLE foreign_data(value TEXT)"); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	openErr := make(chan error, 1)
	go func() {
		close(started)
		store, err := Open(path)
		if err == nil {
			store.Close()
		}
		openErr <- err
	}()
	<-started
	if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err := <-openErr; err == nil {
		t.Fatal("Open accepted a foreign schema that won the ownership claim")
	} else if !strings.Contains(err.Error(), "refusing to claim unowned database") {
		// Only the schema-rejection path proves the claim raced correctly: a
		// SQLITE_BUSY timeout would also satisfy err != nil while testing
		// nothing about the foreign-schema handling.
		t.Fatalf("Open failed for the wrong reason: %v", err)
	}

	var appID, version, foreignObjects int
	if err := conn.QueryRowContext(context.Background(), "PRAGMA application_id").Scan(&appID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(context.Background(), "SELECT count(*) FROM sqlite_master WHERE name = 'foreign_data'").Scan(&foreignObjects); err != nil {
		t.Fatal(err)
	}
	if appID != 0 || version != 0 || foreignObjects != 1 {
		t.Fatalf("database state = application_id %d, version %d, foreign objects %d; want foreign and unmodified", appID, version, foreignObjects)
	}
}

// TestOpenDatabaseBusyTimeoutOnEveryConnection pins the SQLITE_BUSY fix that
// parallel CI load kept surfacing in this package: busy_timeout must ride the
// DSN so every pooled connection — writer and read-only alike — waits out
// lock contention instead of erroring. A connection-level PRAGMA only reaches
// one pooled handle, and the read-only path installs no per-connection belt
// at all, so the DSN is the only point that covers all opens.
func TestOpenDatabaseBusyTimeoutOnEveryConnection(t *testing.T) {
	const want = 5000
	probe := func(t *testing.T, readOnly bool) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "history.sqlite3")
		if readOnly {
			// A read-only open of a missing file fails before the pragma
			// check; an existing empty file opens fine (OpenReadOnly then
			// rejects the foreign ownership itself, so open raw here).
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			f.Close()
		}
		db, err := openDatabase(path, readOnly)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var got int
		// MaxIdleConns(0) forces a fresh driver connection per query: without
		// it the pool hands back the same handle the Ping opened, and the
		// "every connection" coverage the DSN pragma promises would go
		// unexercised.
		db.SetMaxIdleConns(0)
		for range 3 {
			if err := db.QueryRow("PRAGMA busy_timeout").Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("PRAGMA busy_timeout (readOnly=%v) = %d, want %d", readOnly, got, want)
			}
		}
	}
	probe(t, false)
	probe(t, true)
}
