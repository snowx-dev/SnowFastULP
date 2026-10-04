package history_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/history"
	_ "modernc.org/sqlite"
)

const historyApplicationID = 1397114953

func openRawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		t.Fatal(err)
	}
	return db
}

func pragmaInt(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var got int
	if err := db.QueryRow("PRAGMA " + name).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestOpenCreatesParentAndDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "history.sqlite3")
	store, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("database mode = %v, want regular file", info.Mode())
	}
}

func TestSchemaPragmasAndConstraints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite3")
	store, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	db := openRawDB(t, path)
	defer db.Close()
	if got := pragmaInt(t, db, "application_id"); got != historyApplicationID {
		t.Fatalf("application_id = %d, want %d", got, historyApplicationID)
	}
	if got := pragmaInt(t, db, "user_version"); got != 1 {
		t.Fatalf("user_version = %d, want 1", got)
	}
	var journal string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if strings.ToLower(journal) != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journal)
	}
	if _, err := db.Exec("INSERT INTO history_entries(digest,size_bytes,completed_at) VALUES(?,?,?)", []byte{1}, 1, 1); err == nil {
		t.Fatal("schema accepted digest with invalid length")
	}
	if _, err := db.Exec("INSERT INTO history_entries(digest,size_bytes,completed_at) VALUES(?,?,?)", make([]byte, 8), -1, 1); err == nil {
		t.Fatal("schema accepted negative size")
	}
	var schema string
	if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='history_entries'").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToUpper(schema), "WITHOUT ROWID") {
		t.Fatalf("schema = %q, want WITHOUT ROWID", schema)
	}
}

func TestOpenDatabasePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "history.sqlite3")
	store, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm() & 0o077; got != 0 {
		t.Fatalf("database group/other permissions = %#o, want 0", got)
	}
}

func TestOpenRejectsForeignApplicationID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign.sqlite3")
	db := openRawDB(t, path)
	if _, err := db.Exec("PRAGMA application_id = 1234"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if store, err := history.Open(path); err == nil {
		store.Close()
		t.Fatal("Open accepted foreign application ID")
	}
	db = openRawDB(t, path)
	defer db.Close()
	if got := pragmaInt(t, db, "application_id"); got != 1234 {
		t.Fatalf("application_id modified to %d", got)
	}
}

func TestOpenRejectsUnownedDatabaseWithoutModification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unowned.sqlite3")
	db := openRawDB(t, path)
	if _, err := db.Exec("CREATE TABLE unrelated(value TEXT); INSERT INTO unrelated(value) VALUES ('keep')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if store, err := history.Open(path); err == nil {
		store.Close()
		t.Fatal("Open accepted an unowned database with a user schema")
	}

	db = openRawDB(t, path)
	defer db.Close()
	if got := pragmaInt(t, db, "application_id"); got != 0 {
		t.Fatalf("application_id modified to %d", got)
	}
	if got := pragmaInt(t, db, "user_version"); got != 0 {
		t.Fatalf("user_version modified to %d", got)
	}
	var journal string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if strings.ToLower(journal) != "delete" {
		t.Fatalf("journal_mode modified to %q", journal)
	}
	var schema string
	if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='unrelated'").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if schema != "CREATE TABLE unrelated(value TEXT)" {
		t.Fatalf("unrelated schema modified to %q", schema)
	}
	var value string
	if err := db.QueryRow("SELECT value FROM unrelated").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "keep" {
		t.Fatalf("unrelated data modified to %q", value)
	}
}

func TestOpenRejectsFutureSchemaWithoutModification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.sqlite3")
	db := openRawDB(t, path)
	if _, err := db.Exec("PRAGMA application_id = 1397114953; PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if store, err := history.Open(path); err == nil {
		store.Close()
		t.Fatal("Open accepted future schema")
	}
	db = openRawDB(t, path)
	defer db.Close()
	if got := pragmaInt(t, db, "user_version"); got != 2 {
		t.Fatalf("user_version modified to %d", got)
	}
}

func TestOpenReadOnlyMissingDoesNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "history.sqlite3")
	store, exists, err := history.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if store != nil || exists {
		t.Fatalf("OpenReadOnly() = (%v, %v), want (nil, false)", store, exists)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("missing database was created or unexpected stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("missing parent was created or unexpected stat error: %v", err)
	}
}

func TestOpenCreatesDatabaseInEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	store, err := history.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("database mode = %v, want regular file", info.Mode())
	}
	// A second open of the same directory reuses the existing database and
	// sees what the first one wrote.
	store, err = history.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity := history.Identity{Hash: 1, Size: 2}
	if err := store.Record(context.Background(), []history.Identity{identity}); err != nil {
		t.Fatal(err)
	}
	store2, err := history.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	hits, err := store2.Lookup(context.Background(), []history.Identity{identity})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hits[identity]; !ok {
		t.Fatal("identity recorded by the first open was not found by the second")
	}
}

func TestOpenReusesDirectoryContainingDefaultDatabase(t *testing.T) {
	dir := t.TempDir()
	identity := history.Identity{Hash: 1, Size: 2}
	store, err := history.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), []history.Identity{identity}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// A trailing separator must still resolve to the existing database.
	path := dir + string(filepath.Separator)
	store, err = history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hits, err := store.Lookup(context.Background(), []history.Identity{identity})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hits[identity]; !ok {
		t.Fatal("identity recorded before reopening the directory was not found")
	}
}

func TestOpenCreatesMissingTrailingSeparatorDirectory(t *testing.T) {
	base := filepath.Join(t.TempDir(), "hist")
	store, err := history.Open(base + string(filepath.Separator))
	if err != nil {
		t.Fatal(err)
	}
	identity := history.Identity{Hash: 1, Size: 2}
	if err := store.Record(context.Background(), []history.Identity{identity}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// The hinted directory itself now exists and holds the default database.
	info, err := os.Stat(filepath.Join(base, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("database mode = %v, want regular file", info.Mode())
	}
	// The plain directory path (no separator) resolves to the same database.
	store, err = history.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hits, err := store.Lookup(context.Background(), []history.Identity{identity})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hits[identity]; !ok {
		t.Fatal("identity recorded via the trailing-separator path was not found via the directory path")
	}
}

func TestOpenRejectsTrailingSeparatorOnRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.sqlite3")
	if err := os.WriteFile(path, []byte("pretend"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := history.Open(path + string(filepath.Separator))
	if err == nil || !strings.Contains(err.Error(), "names a regular file") {
		t.Fatalf("Open() error = %v, want it to contain %q", err, "names a regular file")
	}
}

func TestOpenReadOnlyMissingTrailingSeparatorDoesNotCreate(t *testing.T) {
	base := filepath.Join(t.TempDir(), "missing")
	store, exists, err := history.OpenReadOnly(base + string(filepath.Separator))
	if err != nil {
		t.Fatal(err)
	}
	if store != nil || exists {
		t.Fatalf("OpenReadOnly() = (%v, %v), want (nil, false)", store, exists)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatalf("hinted directory was created by a read-only open: %v", err)
	}
}

func TestOpenRejectsDirectoryNamedDefaultDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.sqlite3")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := history.Open(dir)
	if err == nil || !strings.Contains(err.Error(), "database is a directory") {
		t.Fatalf("Open() error = %v, want it to contain %q", err, "database is a directory")
	}
}

func TestOpenUsesDirectoryWithOtherFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"one.txt", "two.txt", "three.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := history.Open(dir)
	if err != nil {
		t.Fatalf("Open() against a directory with unrelated files: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one.txt", "two.txt", "three.txt", "history.sqlite3"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("directory entry %s missing after Open(): %v", name, err)
		}
	}
}

func TestOpenReadOnlyDirectoryResolvesDefaultDatabase(t *testing.T) {
	dir := t.TempDir()
	store, exists, err := history.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if store != nil || exists {
		t.Fatalf("OpenReadOnly() on empty directory = (%v, %v), want (nil, false)", store, exists)
	}
	if _, err := history.Open(filepath.Join(dir, "history.sqlite3")); err != nil {
		t.Fatal(err)
	}
	store, exists, err = history.OpenReadOnly(dir)
	if err != nil || !exists {
		t.Fatalf("OpenReadOnly() on directory holding the database = (%v, %v, %v), want (store, true, nil)", store, exists, err)
	}
	if store == nil {
		t.Fatal("OpenReadOnly() returned nil store with exists=true")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenReadOnlyUsesDirectoryWithOtherFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Without the default database inside, a read-only open sees nothing.
	store, exists, err := history.OpenReadOnly(dir)
	if store != nil || exists || err != nil {
		t.Fatalf("OpenReadOnly() = (%v, %v, %v), want (nil, false, nil)", store, exists, err)
	}
	// Once the default database exists alongside the unrelated file, it is opened.
	if _, err := history.Open(filepath.Join(dir, "history.sqlite3")); err != nil {
		t.Fatal(err)
	}
	store, exists, err = history.OpenReadOnly(dir)
	if err != nil || !exists {
		t.Fatalf("OpenReadOnly() = (%v, %v, %v), want (store, true, nil)", store, exists, err)
	}
	if store == nil {
		t.Fatal("OpenReadOnly() returned nil store with exists=true")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLookupUnknownIdentityMisses(t *testing.T) {
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hits, err := store.Lookup(context.Background(), []history.Identity{{Hash: 1, Size: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits = %v, want empty", hits)
	}
}

func TestRecordHitsAfterReopenAndSizeIsPartOfIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite3")
	id := history.Identity{Hash: 0x0102030405060708, Size: 42}
	store, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), []history.Identity{id}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	otherSize := history.Identity{Hash: id.Hash, Size: id.Size + 1}
	hits, err := store.Lookup(context.Background(), []history.Identity{id, otherSize})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hits[id]; !ok {
		t.Fatalf("recorded identity missing from %v", hits)
	}
	if _, ok := hits[otherSize]; ok {
		t.Fatalf("same hash with different size unexpectedly hit: %v", hits)
	}
}

func TestRecordDuplicateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite3")
	id := history.Identity{Hash: 9, Size: 10}
	store, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), []history.Identity{id, id}); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), []history.Identity{id}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db := openRawDB(t, path)
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM history_entries").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want 1", count)
	}
}

func TestRecordBatchIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite3")
	store, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db := openRawDB(t, path)
	if _, err := db.Exec(`CREATE TRIGGER fail_record BEFORE INSERT ON history_entries
WHEN NEW.size_bytes = 2 BEGIN SELECT RAISE(ABORT, 'forced record failure'); END`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	first := history.Identity{Hash: 1, Size: 1}
	forcedFailure := history.Identity{Hash: 2, Size: 2}
	if err := store.Record(context.Background(), []history.Identity{first, forcedFailure}); err == nil {
		t.Fatal("Record unexpectedly committed a trigger-aborted batch")
	}
	hits, err := store.Lookup(context.Background(), []history.Identity{first})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("partial batch was committed: %v", hits)
	}
}

func TestLookupReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite3")
	id := history.Identity{Hash: 100, Size: 200}
	store, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), []history.Identity{id}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, exists, err := history.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("OpenReadOnly reported existing database missing")
	}
	defer store.Close()
	hits, err := store.Lookup(context.Background(), []history.Identity{id})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hits[id]; !ok {
		t.Fatalf("read-only lookup missed %v", id)
	}
}

func TestConcurrentFirstOpenBootstrapsOneEmptyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite3")
	const stores = 8
	start := make(chan struct{})
	errs := make(chan error, stores)
	var wg sync.WaitGroup
	for range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			store, err := history.Open(path)
			if err == nil {
				err = store.Close()
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	store, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
}

func TestConcurrentStoresInsertDisjointBatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite3")
	initial, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}

	const stores = 4
	const batchSize = 25
	all := make([]history.Identity, 0, stores*batchSize)
	batches := make([][]history.Identity, stores)
	for i := range stores {
		for j := range batchSize {
			id := history.Identity{Hash: uint64(i*batchSize + j + 1), Size: int64(i + j + 1)}
			batches[i] = append(batches[i], id)
			all = append(all, id)
		}
	}

	start := make(chan struct{})
	errs := make(chan error, stores)
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func(batch []history.Identity) {
			defer wg.Done()
			<-start
			store, err := history.Open(path)
			if err == nil {
				err = store.Record(context.Background(), batch)
				if closeErr := store.Close(); err == nil {
					err = closeErr
				}
			}
			errs <- err
		}(batches[i])
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	store, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hits, err := store.Lookup(context.Background(), all)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != stores*batchSize {
		t.Fatalf("final hit count = %d, want %d", len(hits), stores*batchSize)
	}
}

func TestCorruptDatabaseIsNotRecreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.sqlite3")
	want := []byte("not a sqlite database")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if store, err := history.Open(path); err == nil {
		store.Close()
		t.Fatal("Open accepted corrupt database")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("corrupt database was modified: %q", got)
	}
}

func TestOpenRejectsMalformedSchemaWithoutRecreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "malformed.sqlite3")
	db := openRawDB(t, path)
	if _, err := db.Exec(fmt.Sprintf("PRAGMA application_id = %d; PRAGMA user_version = 1; CREATE TABLE history_entries(digest BLOB)", historyApplicationID)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if store, err := history.Open(path); err == nil {
		store.Close()
		t.Fatal("Open accepted malformed history schema")
	}
	db = openRawDB(t, path)
	defer db.Close()
	var schema string
	if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE name='history_entries'").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(schema, "digest BLOB") || strings.Contains(strings.ToUpper(schema), "WITHOUT ROWID") {
		t.Fatalf("malformed schema was replaced: %q", schema)
	}
}
