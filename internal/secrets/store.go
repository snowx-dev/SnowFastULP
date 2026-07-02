package secrets

import (
	"database/sql"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

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

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
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
	ins, err := s.db.Prepare(`INSERT OR IGNORE INTO secrets
		(rule_id, rule_name, secret, score, severity, validation, source_path, first_seen, last_seen, seen_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`)
	if err != nil {
		s.err = err
		for range s.ch { // drain so Add never blocks
		}
		return
	}
	defer ins.Close()
	upd, err := s.db.Prepare(`UPDATE secrets SET last_seen=?, seen_count=seen_count+1 WHERE rule_id=? AND secret=?`)
	if err != nil {
		s.err = err
		for range s.ch {
		}
		return
	}
	defer upd.Close()

	for f := range s.ch {
		k := f.key()
		if _, dup := s.seen[k]; dup {
			s.stat.DupInRun++
			continue
		}
		s.seen[k] = struct{}{}
		now := time.Now().Unix()
		score := sql.NullInt64{Int64: int64(f.Score), Valid: f.Score >= 0}
		// INSERT OR IGNORE hits the UNIQUE(rule_id, secret) constraint on repeat
		// secrets. RowsAffected==1 means a genuinely new row; ==0 means it already
		// existed (from a prior run), so bump last_seen/seen_count. source_path is
		// only ever written on the first insert, so the first location is retained.
		res, e := ins.Exec(f.RuleID, f.RuleName, f.Secret, score, nullStr(f.Severity), nullStr(f.Validation), f.SourcePath, now, now)
		if e != nil {
			if s.err == nil {
				s.err = e
			}
			continue
		}
		if n, _ := res.RowsAffected(); n == 1 {
			s.stat.New++
		} else {
			if _, e := upd.Exec(now, f.RuleID, f.Secret); e != nil && s.err == nil {
				s.err = e
			}
			s.stat.Existing++
		}
	}
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
