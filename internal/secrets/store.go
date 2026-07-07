package secrets

import (
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// maxSecretLen bounds a stored secret. Titus's matched span can be large or
// multi-line (an AWS hit spans two lines), so cap it to keep the DB and the
// dedup key bounded while retaining enough to identify and triage the secret.
const maxSecretLen = 4 << 10 // 4 KiB

// capSecret truncates s to maxSecretLen on a rune boundary so the stored value
// stays valid UTF-8 for a SQLite TEXT column.
func capSecret(s string) string {
	if len(s) <= maxSecretLen {
		return s
	}
	cut := maxSecretLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// sanitizeSecret makes a matched span safe to store and to emit as one record:
// a Titus match can span lines (an AWS hit is two) or contain tabs, which would
// break both sfs's line- and tab-delimited output and the DB's one-secret-per-
// row model. CR/LF/TAB collapse to spaces (applied before the dedup key is
// computed, so dedup stays deterministic), then the value is length-capped.
func sanitizeSecret(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		return r
	}, s)
	return capSecret(s)
}

// fileURI builds a SQLite URI filename carrying query. Only %, ? and # are
// special in a URI path (SQLite percent-decodes the rest), so escaping just
// those preserves arbitrary paths (spaces, unicode); ToSlash keeps Windows
// backslash paths valid as URIs.
func fileURI(path, query string) string {
	p := filepath.ToSlash(path)
	p = strings.ReplaceAll(p, "%", "%25")
	p = strings.ReplaceAll(p, "?", "%3f")
	p = strings.ReplaceAll(p, "#", "%23")
	return "file:" + p + "?" + query
}

// Finding is one detected secret, ready to persist.
type Finding struct {
	RuleID     string
	RuleName   string
	Secret     string
	Score      int    // 0-100 from rule metadata; -1 if unknown
	Severity   string // info..critical; "" if unknown
	Validation string // confirmed|denied|unknown|"" (validation off in v1)
	SourcePath string // archive!member provenance (first location only)
}

// key is the within-run dedup identity, matching the DB's UNIQUE(rule_id,secret).
// A string key is collision-free (unlike a 64-bit hash) and cheap at secret volume.
func (f Finding) key() string { return f.RuleID + "\x00" + f.Secret }

// Stats reports the outcome of a run, mirroring the library's added/already/dup.
type Stats struct {
	New      int64 // first time this secret entered the store
	Existing int64 // already in the store from a prior run (last_seen bumped)
	DupInRun int64 // exact repeat seen earlier in this same run
	Deduped  int64 // member scans skipped because identical content was already scanned
}

const schema = `
CREATE TABLE IF NOT EXISTS secrets (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  rule_id     TEXT NOT NULL,
  rule_name   TEXT NOT NULL,
  secret      TEXT NOT NULL,
  score       INTEGER,
  severity    TEXT,
  validation  TEXT,
  source_path TEXT,            -- first location only
  first_seen  INTEGER NOT NULL,
  last_seen   INTEGER NOT NULL,
  seen_count  INTEGER NOT NULL DEFAULT 1,
  UNIQUE(rule_id, secret)
);`

// Store accumulates Findings into a SQLite DB. A single writer goroutine owns
// the DB handle (SQLite is single-writer); callers Add concurrently. An in-mem
// key set collapses within-run repeats before they reach the DB, mirroring the
// engine's runWriter hash set.
type Store struct {
	db   *sql.DB
	ch   chan Finding
	wg   sync.WaitGroup
	seen map[string]struct{}
	stat Stats
	err  error
}

// secretsBatchSize caps findings per transaction. Per-row auto-commit means one
// WAL fsync per finding, which on a big run (thousands of scanned members) lets
// a backlog build faster than the single writer can drain it — the flush then
// stalls for minutes at 100%. One commit per batch keeps the writer well ahead.
const secretsBatchSize = 1000

// secretsFlushInterval bounds how long a partially-filled batch waits before it
// commits, so a slow trickle over a long extraction still lands steadily (WAL
// stays bounded and a concurrent sfs -sec sees fresh rows).
const secretsFlushInterval = 2 * time.Second

func Open(path string) (*Store, error) {
	// WAL lets a concurrent sfs -sec read the store while this run writes, and
	// busy_timeout waits out a transient lock instead of failing the write.
	db, err := sql.Open("sqlite", fileURI(path, "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"))
	if err != nil {
		return nil, err
	}
	// One dedicated writer connection: the store has a single writer goroutine,
	// so a pool would only add connections that contend for the write lock (now
	// that busy_timeout is set, such a conflict blocks up to 5s instead of
	// erroring). Pinning to one conn keeps all writes on the same handle.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, ch: make(chan Finding, 1024), seen: make(map[string]struct{}, 1024)}
	s.wg.Add(1)
	go s.run()
	return s, nil
}

// Add queues a finding. Safe for concurrent callers.
func (s *Store) Add(f Finding) { s.ch <- f }

func (s *Store) run() {
	defer s.wg.Done()

	batch := make([]Finding, 0, secretsBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.writeBatch(batch); err != nil && s.err == nil {
			s.err = err
		}
		batch = batch[:0]
	}

	ticker := time.NewTicker(secretsFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case f, ok := <-s.ch:
			if !ok {
				flush() // channel closed: commit the tail and finish
				return
			}
			k := f.key()
			if _, dup := s.seen[k]; dup {
				s.stat.DupInRun++
				continue
			}
			s.seen[k] = struct{}{}
			batch = append(batch, f)
			if len(batch) >= secretsBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// writeBatch persists a group of unique findings in one transaction. Stats are
// accumulated locally and only applied on a successful commit, so a rolled-back
// batch never over-counts New/Existing.
func (s *Store) writeBatch(batch []Finding) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	ins, err := tx.Prepare(`INSERT OR IGNORE INTO secrets
		(rule_id, rule_name, secret, score, severity, validation, source_path, first_seen, last_seen, seen_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer ins.Close()
	upd, err := tx.Prepare(`UPDATE secrets SET last_seen=?, seen_count=seen_count+1 WHERE rule_id=? AND secret=?`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer upd.Close()

	now := time.Now().Unix()
	var newN, existN int64
	for _, f := range batch {
		score := sql.NullInt64{Int64: int64(f.Score), Valid: f.Score >= 0}
		// INSERT OR IGNORE hits the UNIQUE(rule_id, secret) constraint on repeat
		// secrets. RowsAffected==1 means a genuinely new row; ==0 means it already
		// existed (from a prior run), so bump last_seen/seen_count. source_path is
		// only ever written on the first insert, so the first location is retained.
		res, err := ins.Exec(f.RuleID, f.RuleName, f.Secret, score, nullStr(f.Severity), nullStr(f.Validation), f.SourcePath, now, now)
		if err != nil {
			tx.Rollback()
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			newN++
		} else {
			if _, err := upd.Exec(now, f.RuleID, f.Secret); err != nil {
				tx.Rollback()
				return err
			}
			existN++
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.stat.New += newN
	s.stat.Existing += existN
	return nil
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// Close stops the writer, flushes, and returns run stats and the first error.
func (s *Store) Close() (Stats, error) {
	close(s.ch)
	s.wg.Wait()
	cerr := s.db.Close()
	if s.err != nil {
		return s.stat, s.err
	}
	return s.stat, cerr
}
