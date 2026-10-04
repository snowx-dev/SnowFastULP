package history

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/pathident"

	_ "modernc.org/sqlite"
)

const (
	applicationID = 1397114953
	schemaVersion = 1
)

const createSchemaV1 = `CREATE TABLE history_entries (
    digest        BLOB    NOT NULL CHECK(length(digest) = 8),
    size_bytes    INTEGER NOT NULL CHECK(size_bytes >= 0),
    completed_at  INTEGER NOT NULL,
    PRIMARY KEY (digest, size_bytes)
) WITHOUT ROWID;`

type sqliteStore struct {
	db *sql.DB
}

func Open(path string) (Store, error) {
	return openWithClaimHook(path, nil)
}

func openWithClaimHook(path string, afterInspection func() error) (Store, error) {
	resolved, err := resolveDatabasePath(path)
	if err != nil {
		return nil, err
	}
	path = resolved
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("history: create database directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		if closeErr := f.Close(); closeErr != nil {
			return nil, fmt.Errorf("history: create database: %w", closeErr)
		}
	} else if os.IsExist(err) {
		// The claim runs on the resolved FILE path, so two processes given
		// the same empty directory still serialize on O_EXCL. The recheck
		// below is advisory: a racing process could swap the file for a
		// directory between stat and openDatabase — in that case sqlite
		// still fails closed with its own open error.
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, fmt.Errorf("history: stat database: %w", statErr)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("history: database is a directory: %s", path)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("history: database path is not a regular file: %s", path)
		}
	} else {
		return nil, fmt.Errorf("history: create database: %w", err)
	}

	db, err := openDatabase(path, false)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (Store, error) {
		db.Close()
		return nil, err
	}
	if err := claimOrValidate(db, afterInspection); err != nil {
		return closeOnError(err)
	}
	if err := configureWriter(db); err != nil {
		return closeOnError(err)
	}
	if err := bootstrapWAL(db); err != nil {
		return closeOnError(err)
	}
	return &sqliteStore{db: db}, nil
}

// defaultDatabaseName is the database file adopted when -history-path names
// a directory. It is the same name internal/config's DefaultHistoryPath and
// README's default pick, so a directory that previously held the default
// database is found instead of shadowed.
const defaultDatabaseName = "history.sqlite3"

// EffectiveDatabasePaths resolves a user-supplied -history-path spelling into
// the database file exactly as Open will, plus the SQLite WAL/SHM sidecar
// paths that live next to it while the connection is open. Callers that must
// protect the database from being clobbered by other artifacts (JSON-stream
// targets, deletion protection) use this before the store is opened.
func EffectiveDatabasePaths(path string) ([]string, error) {
	resolved, err := resolveDatabasePath(path)
	if err != nil {
		return nil, err
	}
	return []string{resolved, resolved + "-wal", resolved + "-shm"}, nil
}

// JSONOutHistoryCollision reports whether a prospective -json stream
// target names the effective history database or one of its WAL/SHM sidecars,
// returning the colliding path ("" when clear). rawPath is the user-supplied
// -history-path spelling; "" (history off) is always clear. The stream
// truncates its target before the store opens, so this must resolve the
// database exactly as Open will and run before anything is opened; it
// performs no writes. A target or history path that cannot be resolved here
// is reported by the callers' own validation or by Open, not here.
func JSONOutHistoryCollision(target, rawPath string) string {
	if target == "" || target == "-" || strings.TrimSpace(rawPath) == "" {
		return ""
	}
	paths, err := EffectiveDatabasePaths(rawPath)
	if err != nil {
		return ""
	}
	tgt, err := pathident.CanonicalProspective(target)
	if err != nil {
		return ""
	}
	for _, p := range paths {
		if c, e := pathident.CanonicalProspective(p); e == nil && c == tgt {
			return p
		}
		if same, e := pathident.SameFile(tgt, p); e == nil && same {
			return p
		}
	}
	// The target's final component may itself be a symlink — possibly
	// dangling, since the database does not exist yet. Canonical and SameFile
	// compares both pass on a dangling link, and the stream's create follows
	// the link, materializing and truncating the database when the store
	// opens. Compare the referent chain against every protected endpoint too.
	for _, p := range paths {
		if pathident.LinkRefersTo(target, p) {
			return p
		}
	}
	return ""
}

// resolveDatabasePath maps a user-supplied history path onto the database
// file to open. A trailing separator is a directory hint (the same
// convention as -o / outdir.IsDirHint): the path is adopted as a directory
// even when it does not exist yet — the writer creates it (Open's
// MkdirAll), a read-only open finds no database instead of creating
// anything. An existing regular file is used as-is; with a trailing
// separator that is a conflict and an error, since the user asked for a
// directory. An existing directory adopts the default database name inside
// it, opening that file when it exists and creating it otherwise. The
// database name is fixed, so a directory's other contents never influence
// the decision beyond the presence or absence of the default database
// itself — unrelated files like logs or notes are simply left alone.
func resolveDatabasePath(path string) (string, error) {
	dirHint := strings.HasSuffix(path, "/") || strings.HasSuffix(path, string(os.PathSeparator))
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			if dirHint {
				return filepath.Join(path, defaultDatabaseName), nil
			}
			return path, nil
		}
		return "", fmt.Errorf("history: stat database: %w", err)
	}
	if info.Mode().IsRegular() {
		if dirHint {
			return "", fmt.Errorf("history: database path ends with a separator but names a regular file: %s", path)
		}
		return path, nil
	}
	if !info.IsDir() {
		return "", fmt.Errorf("history: database path is not a regular file: %s", path)
	}
	return filepath.Join(path, defaultDatabaseName), nil
}

func OpenReadOnly(path string) (store Store, exists bool, err error) {
	resolved, err := resolveDatabasePath(path)
	if err != nil {
		return nil, true, err
	}
	path = resolved
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("history: stat database: %w", err)
	}
	if info.IsDir() {
		return nil, true, fmt.Errorf("history: database is a directory: %s", path)
	}

	db, err := openDatabase(path, true)
	if err != nil {
		return nil, true, err
	}
	appID, version, err := readOwnership(db)
	if err != nil {
		db.Close()
		return nil, true, err
	}
	if appID != applicationID {
		db.Close()
		return nil, true, fmt.Errorf("history: database application ID is %d, want %d", appID, applicationID)
	}
	if version != schemaVersion {
		db.Close()
		return nil, true, fmt.Errorf("history: database schema version is %d, want %d", version, schemaVersion)
	}
	if err := validateSchema(db); err != nil {
		db.Close()
		return nil, true, err
	}
	return &sqliteStore{db: db}, true, nil
}

func openDatabase(path string, readOnly bool) (*sql.DB, error) {
	// Classification happens on the raw path: on Windows a UNC path is
	// already absolute, and Unix semantics resolve it themselves below.
	uri, err := sqliteFileURI(path)
	if err != nil {
		return nil, fmt.Errorf("history: build database URI: %w", err)
	}
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("history: build database URI: %w", err)
	}
	query := u.Query()
	// busy_timeout rides the DSN so every pooled connection waits out lock
	// contention (up to 5s) instead of surfacing SQLITE_BUSY: writers block on
	// a concurrent claim in BEGIN IMMEDIATE, and read-only connections —
	// which skip the retrySQLite belt below entirely — still contend with
	// writers that hold RESERVED while draining a write transaction.
	query.Add("_pragma", "busy_timeout(5000)")
	if readOnly {
		query.Set("mode", "ro")
		// H-11: mode=ro still needs writable WAL/SHM sidecars, so a cleanly
		// closed database on read-only media could not be consulted at all.
		// When neither sidecar exists no writer holds the database (a live
		// writer always has -wal/-shm on disk), so the open takes the
		// immutable path: SQLite skips locking and sidecar creation and reads
		// the main file as a snapshot. Snapshot assumption, documented: the
		// read reflects the last clean checkpoint; a crashed writer's
		// uncheckpointed WAL tail is invisible to immutable readers. If either
		// sidecar IS present, stay on plain mode=ro so the (existing, hence
		// writable-context) sidecars are honored normally.
		if _, err := os.Stat(path + "-wal"); os.IsNotExist(err) {
			if _, err := os.Stat(path + "-shm"); os.IsNotExist(err) {
				query.Set("immutable", "1")
			}
		}
	} else {
		query.Set("_txlock", "immediate")
	}
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("history: open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("history: open database: %w", err)
	}
	return db, nil
}

// sqliteFileURI converts a database path into a SQLite file URI. The path's
// own shape decides: a drive path keeps its drive in the path component
// (file:///C:/...), a UNC path becomes file:////server/share/dir/db.sqlite3 —
// an empty authority plus the //server/share/... path, the form SQLite's own
// docs give for network filenames. An authority-bearing file://server/... URI
// is NOT emitted: SQLite rejects any authority but empty/localhost with
// "invalid uri authority" unless built with SQLITE_ALLOW_URI_AUTHORITY, which
// the vendored modernc build is not. A malformed UNC — a server without a
// share, or neither — is an actionable error instead of an invalid URI. Every
// other path is filename data that is percent-escaped verbatim; notably a
// Unix backslash must survive as literal data, never be read as a separator.
func sqliteFileURI(path string) (string, error) {
	normalized, err := fileURIPath(path)
	if err != nil {
		return "", err
	}
	return (&url.URL{Scheme: "file", Path: normalized}).String(), nil
}

// fileURIPath classifies a database path into a URI path component. UNC
// classification must see the raw path: \\server\share is already absolute,
// and resolving it first would hide the shape behind the process working
// directory.
func fileURIPath(path string) (normalized string, err error) {
	if strings.HasPrefix(path, `\\`) {
		parts := strings.Split(strings.TrimPrefix(path, `\\`), `\`)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return "", fmt.Errorf("history: database path %s is a malformed UNC path: a UNC database needs \\server\\share\\dir\\history.sqlite3", path)
		}
		// file:////server/share/... — the authority stays empty (SQLite
		// errors on any other value) and the //server/share/... path is
		// handed to the VFS, which reads it as a UNC filename on Windows.
		return "//" + strings.Join(parts, "/"), nil
	}
	if len(path) >= 2 && path[1] == ':' {
		return driveURIPath(path), nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("history: resolve database path: %w", err)
	}
	// Abs on Windows resolves relative paths onto the current drive.
	if len(absolute) >= 2 && absolute[1] == ':' {
		return driveURIPath(absolute), nil
	}
	return absolute, nil
}

// driveURIPath keeps the drive in the path component. Without the leading
// slash net/url emits file://C:/..., which SQLite interprets as an invalid
// URI authority.
func driveURIPath(path string) string {
	normalized := strings.ReplaceAll(path, `\`, "/")
	if normalized[0] != '/' {
		normalized = "/" + normalized
	}
	return normalized
}

func configureWriter(db *sql.DB) error {
	// busy_timeout is deliberately absent here: openDatabase bakes it into
	// every connection's DSN, so a pool-level Exec could only ever restate it.
	for _, statement := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = FULL",
		"PRAGMA trusted_schema = OFF",
	} {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("history: configure database: %w", err)
		}
	}
	return nil
}

func readOwnership(db *sql.DB) (appID, version int, err error) {
	if err := db.QueryRow("PRAGMA application_id").Scan(&appID); err != nil {
		return 0, 0, fmt.Errorf("history: read application ID: %w", err)
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0, 0, fmt.Errorf("history: read schema version: %w", err)
	}
	return appID, version, nil
}

func verifyWritableOwnership(appID, version int) error {
	if appID != 0 && appID != applicationID {
		return fmt.Errorf("history: foreign database application ID %d", appID)
	}
	if version > schemaVersion {
		return fmt.Errorf("history: database schema version %d is newer than supported version %d", version, schemaVersion)
	}
	if version < 0 {
		return fmt.Errorf("history: invalid database schema version %d", version)
	}
	if version == schemaVersion && appID != applicationID {
		return fmt.Errorf("history: database schema version %d has no SnowFast application ID", version)
	}
	return nil
}

func bootstrapWAL(db *sql.DB) error {
	return retrySQLite(func() error {
		var mode string
		if err := db.QueryRow("PRAGMA journal_mode = WAL").Scan(&mode); err != nil {
			return fmt.Errorf("history: enable WAL: %w", err)
		}
		if !strings.EqualFold(mode, "wal") {
			return fmt.Errorf("history: enable WAL: journal mode is %q", mode)
		}
		return nil
	})
}

func claimOrValidate(db *sql.DB, afterInspection func() error) error {
	return retrySQLite(func() error {
		tx, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			return fmt.Errorf("history: begin ownership claim: %w", err)
		}
		defer tx.Rollback()

		var appID, version int
		if err := tx.QueryRow("PRAGMA application_id").Scan(&appID); err != nil {
			return fmt.Errorf("history: read application ID during claim: %w", err)
		}
		if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			return fmt.Errorf("history: read schema version during claim: %w", err)
		}
		if err := verifyWritableOwnership(appID, version); err != nil {
			return err
		}
		var hasUserSchema bool
		if err := tx.QueryRow(`SELECT EXISTS(
			SELECT 1 FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'
		)`).Scan(&hasUserSchema); err != nil {
			return fmt.Errorf("history: inspect database schema during claim: %w", err)
		}
		if afterInspection != nil {
			if err := afterInspection(); err != nil {
				return fmt.Errorf("history: ownership claim inspection hook: %w", err)
			}
		}

		switch {
		case appID == 0 && version == 0:
			if hasUserSchema {
				return errors.New("history: refusing to claim unowned database with an existing schema")
			}
			if _, err := tx.Exec(fmt.Sprintf("PRAGMA application_id = %d", applicationID)); err != nil {
				return fmt.Errorf("history: set application ID: %w", err)
			}
			if _, err := tx.Exec(createSchemaV1); err != nil {
				return fmt.Errorf("history: create schema: %w", err)
			}
			if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
				return fmt.Errorf("history: set schema version: %w", err)
			}
		case appID == applicationID && version == schemaVersion:
			if err := validateSchemaQuery(tx); err != nil {
				return err
			}
		default:
			return fmt.Errorf("history: SnowFast database schema version is %d, want %d", version, schemaVersion)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("history: commit ownership claim: %w", err)
		}
		return nil
	})
}

func validateSchema(db *sql.DB) error {
	return validateSchemaQuery(db)
}

type schemaQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func validateSchemaQuery(db schemaQueryer) error {
	var schema string
	if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'history_entries'").Scan(&schema); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("history: history_entries table is missing")
		}
		return fmt.Errorf("history: read schema: %w", err)
	}
	want := strings.TrimSuffix(strings.TrimSpace(createSchemaV1), ";")
	if normalizeSQL(schema) != normalizeSQL(want) {
		return errors.New("history: history_entries schema does not match version 1")
	}
	return nil
}

func normalizeSQL(statement string) string {
	return strings.ToLower(strings.Join(strings.Fields(statement), " "))
}

func retrySQLite(operation func() error) error {
	deadline := time.Now().Add(5 * time.Second)
	backoff := 10 * time.Millisecond
	for {
		err := operation()
		if err == nil || !isBusyOrLocked(err) || time.Now().After(deadline) {
			return err
		}
		jitter := time.Duration(rand.IntN(int(backoff/2) + 1))
		delay := backoff + jitter
		if remaining := time.Until(deadline); delay > remaining {
			delay = remaining
		}
		if delay <= 0 {
			return err
		}
		time.Sleep(delay)
		if backoff < 250*time.Millisecond {
			backoff *= 2
		}
	}
}

func isBusyOrLocked(err error) bool {
	var coded interface{ Code() int }
	if !errors.As(err, &coded) {
		return false
	}
	code := coded.Code() & 0xff
	return code == 5 || code == 6
}

func (s *sqliteStore) Lookup(ctx context.Context, identities []Identity) (map[Identity]struct{}, error) {
	unique := deduplicateIdentities(identities)
	hits := make(map[Identity]struct{})
	for start := 0; start < len(unique); start += 400 {
		end := min(start+400, len(unique))
		chunk := unique[start:end]
		predicates := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)*2)
		for i, identity := range chunk {
			predicates[i] = "(digest = ? AND size_bytes = ?)"
			args = append(args, encodeDigest(identity.Hash), identity.Size)
		}
		query := "SELECT digest, size_bytes FROM history_entries WHERE " + strings.Join(predicates, " OR ")
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("history: lookup: %w", err)
		}
		for rows.Next() {
			var digest []byte
			var size int64
			if err := rows.Scan(&digest, &size); err != nil {
				rows.Close()
				return nil, fmt.Errorf("history: scan lookup: %w", err)
			}
			if len(digest) != 8 {
				rows.Close()
				return nil, fmt.Errorf("history: invalid digest length %d in database", len(digest))
			}
			hits[Identity{Hash: binary.BigEndian.Uint64(digest), Size: size}] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("history: lookup rows: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("history: close lookup rows: %w", err)
		}
	}
	return hits, nil
}

func (s *sqliteStore) Record(ctx context.Context, identities []Identity) error {
	unique := deduplicateIdentities(identities)
	if len(unique) == 0 {
		return nil
	}
	return retrySQLite(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("history: begin record: %w", err)
		}
		defer tx.Rollback()
		stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO history_entries(digest,size_bytes,completed_at)
VALUES(?,?,?)`)
		if err != nil {
			return fmt.Errorf("history: prepare record: %w", err)
		}
		defer stmt.Close()
		completedAt := time.Now().Unix()
		for _, identity := range unique {
			if _, err := stmt.ExecContext(ctx, encodeDigest(identity.Hash), identity.Size, completedAt); err != nil {
				return fmt.Errorf("history: record identity: %w", err)
			}
		}
		if err := stmt.Close(); err != nil {
			return fmt.Errorf("history: close record statement: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("history: commit record: %w", err)
		}
		return nil
	})
}

func deduplicateIdentities(identities []Identity) []Identity {
	seen := make(map[Identity]struct{}, len(identities))
	unique := make([]Identity, 0, len(identities))
	for _, identity := range identities {
		if _, ok := seen[identity]; ok {
			continue
		}
		seen[identity] = struct{}{}
		unique = append(unique, identity)
	}
	return unique
}

func encodeDigest(hash uint64) []byte {
	digest := make([]byte, 8)
	binary.BigEndian.PutUint64(digest, hash)
	return digest
}

func (s *sqliteStore) Close() error {
	return s.db.Close()
}
