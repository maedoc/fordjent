package session

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type SessionRecord struct {
	SessionKey  string
	Repository  string
	IssueNumber int
	PRNumber    int
	WorkDir     string
	RepoDir     string
	CreatedAt   time.Time
	LastActive  time.Time
}

func NewStore(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	schema := `
CREATE TABLE IF NOT EXISTS sessions (
    session_key  TEXT PRIMARY KEY,
    repository   TEXT NOT NULL,
    issue_number INTEGER NOT NULL DEFAULT 0,
    pr_number    INTEGER NOT NULL DEFAULT 0,
    work_dir     TEXT NOT NULL,
    repo_dir     TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    last_active  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_last_active ON sessions(last_active);
`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Create(rec *SessionRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO sessions (session_key, repository, issue_number, pr_number, work_dir, repo_dir, created_at, last_active)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.SessionKey,
		rec.Repository,
		rec.IssueNumber,
		rec.PRNumber,
		rec.WorkDir,
		rec.RepoDir,
		rec.CreatedAt.UTC().Format(time.RFC3339),
		rec.LastActive.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

func (s *Store) Get(sessionKey string) (*SessionRecord, error) {
	row := s.db.QueryRow(
		`SELECT session_key, repository, issue_number, pr_number, work_dir, repo_dir, created_at, last_active
		 FROM sessions WHERE session_key = ?`,
		sessionKey,
	)

	var rec SessionRecord
	var createdAtStr, lastActiveStr string

	err := row.Scan(
		&rec.SessionKey,
		&rec.Repository,
		&rec.IssueNumber,
		&rec.PRNumber,
		&rec.WorkDir,
		&rec.RepoDir,
		&createdAtStr,
		&lastActiveStr,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("session not found: %w", err)
		}
		return nil, fmt.Errorf("scan session: %w", err)
	}

	createdAt, err := time.Parse(time.RFC3339, createdAtStr)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	rec.CreatedAt = createdAt

	lastActive, err := time.Parse(time.RFC3339, lastActiveStr)
	if err != nil {
		return nil, fmt.Errorf("parse last_active: %w", err)
	}
	rec.LastActive = lastActive

	return &rec, nil
}

func (s *Store) ListAll() ([]SessionRecord, error) {
	rows, err := s.db.Query(
		`SELECT session_key, repository, issue_number, pr_number, work_dir, repo_dir, created_at, last_active
		 FROM sessions ORDER BY last_active DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var records []SessionRecord
	for rows.Next() {
		var rec SessionRecord
		var createdAtStr, lastActiveStr string

		err := rows.Scan(
			&rec.SessionKey,
			&rec.Repository,
			&rec.IssueNumber,
			&rec.PRNumber,
			&rec.WorkDir,
			&rec.RepoDir,
			&createdAtStr,
			&lastActiveStr,
		)
		if err != nil {
			return nil, fmt.Errorf("scan session row: %w", err)
		}

		createdAt, err := time.Parse(time.RFC3339, createdAtStr)
		if err != nil {
			return nil, fmt.Errorf("parse created_at: %w", err)
		}
		rec.CreatedAt = createdAt

		lastActive, err := time.Parse(time.RFC3339, lastActiveStr)
		if err != nil {
			return nil, fmt.Errorf("parse last_active: %w", err)
		}
		rec.LastActive = lastActive

		records = append(records, rec)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}

	return records, nil
}

func (s *Store) UpdateLastActive(sessionKey string, t time.Time) error {
	res, err := s.db.Exec(
		`UPDATE sessions SET last_active = ? WHERE session_key = ?`,
		t.UTC().Format(time.RFC3339),
		sessionKey,
	)
	if err != nil {
		return fmt.Errorf("update last_active: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("check rows affected: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}

	return nil
}

func (s *Store) Delete(sessionKey string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE session_key = ?`, sessionKey)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (s *Store) DeleteAll() error {
	_, err := s.db.Exec(`DELETE FROM sessions`)
	if err != nil {
		return fmt.Errorf("delete all sessions: %w", err)
	}
	return nil
}

func (s *Store) DeleteOlderThan(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(
		`DELETE FROM sessions WHERE last_active < ?`,
		cutoff.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("delete older than: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("check rows affected: %w", err)
	}

	return n, nil
}

func (s *Store) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}
