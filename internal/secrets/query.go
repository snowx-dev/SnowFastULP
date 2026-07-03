package secrets

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Match is one secret row returned by QueryDB (the read side of the store). It
// mirrors the schema in store.go, with NULL columns decoded to zero values
// (Score -1, empty strings) so callers never juggle sql.Null* types.
type Match struct {
	RuleID     string
	RuleName   string
	Secret     string
	SourcePath string
	Score      int    // -1 when the row stored NULL
	Severity   string // "" when NULL
	Validation string // "" when NULL
	FirstSeen  time.Time
	LastSeen   time.Time
	SeenCount  int64
}

// QueryOpts filters a QueryDB scan. The zero value returns every row.
type QueryOpts struct {
	// Type filters by secret type: rows whose rule_id OR rule_name contains
	// Type (case-insensitive substring). "" or "*" matches all rows.
	Type string
	// Since, when non-zero, keeps only rows whose last_seen is at or after it.
	Since time.Time
	// Limit, when > 0, caps the rows returned (newest last_seen first).
	Limit int
}

// QueryDB opens the secrets DB read-only and streams matching rows to emit in
// last_seen-descending order, returning the count emitted. It never creates or
// migrates the DB: a missing file yields a clear error, and read-only mode
// guarantees a search can't mutate a store a concurrent sfl run may be writing.
// If emit returns an error, iteration stops and that error is returned.
func QueryDB(path string, o QueryOpts, emit func(Match) error) (int, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("no secrets database at %s (run `sfl -secrets` with a `-tags secrets` build first)", path)
		}
		return 0, err
	}
	db, err := sql.Open("sqlite", readonlyURI(path))
	if err != nil {
		return 0, err
	}
	defer db.Close()

	query, args := buildQuery(o)
	rows, err := db.Query(query, args...)
	if err != nil {
		return 0, fmt.Errorf("query secrets: %w", err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		m, err := scanMatch(rows)
		if err != nil {
			return n, err
		}
		if err := emit(m); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

// readonlyURI opens the DB read-only (mode=ro): a search never creates, migrates
// or otherwise mutates a store a concurrent sfl run may own. busy_timeout lets
// the read wait out a writer's lock instead of failing immediately.
func readonlyURI(path string) string {
	return fileURI(path, "mode=ro&_pragma=busy_timeout(5000)")
}

// buildQuery assembles the parametrized SELECT + args for o. All user input is
// bound, never interpolated.
func buildQuery(o QueryOpts) (string, []any) {
	var (
		where []string
		args  []any
	)
	if t := strings.TrimSpace(o.Type); t != "" && t != "*" {
		// LIKE is case-insensitive for ASCII in SQLite by default; rule ids and
		// names are ASCII, so "aws" matches "AWS" with no COLLATE needed.
		like := "%" + t + "%"
		where = append(where, "(rule_id LIKE ? OR rule_name LIKE ?)")
		args = append(args, like, like)
	}
	if !o.Since.IsZero() {
		where = append(where, "last_seen >= ?")
		args = append(args, o.Since.Unix())
	}
	q := "SELECT rule_id, rule_name, secret, source_path, score, severity, validation, first_seen, last_seen, seen_count FROM secrets"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	// id is the tiebreak so equal last_seen rows come out in a stable order.
	q += " ORDER BY last_seen DESC, id DESC"
	if o.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, o.Limit)
	}
	return q, args
}

func scanMatch(rows *sql.Rows) (Match, error) {
	var (
		m           Match
		src         sql.NullString
		score       sql.NullInt64
		severity    sql.NullString
		validation  sql.NullString
		first, last int64
	)
	if err := rows.Scan(&m.RuleID, &m.RuleName, &m.Secret, &src, &score,
		&severity, &validation, &first, &last, &m.SeenCount); err != nil {
		return Match{}, err
	}
	m.SourcePath = src.String
	if score.Valid {
		m.Score = int(score.Int64)
	} else {
		m.Score = -1
	}
	m.Severity = severity.String
	m.Validation = validation.String
	m.FirstSeen = time.Unix(first, 0)
	m.LastSeen = time.Unix(last, 0)
	return m, nil
}
