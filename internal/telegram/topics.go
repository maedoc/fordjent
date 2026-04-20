package telegram

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// TopicMapping maps a Telegram forum topic to a Fordjent session key.
type TopicMapping struct {
	ChatID      int64
	ThreadID   int
	Repository string
	SessionKey string
	IssueNumber int
	PRNumber    int
}

// MappingStore persists topic mappings in SQLite.
type MappingStore struct {
	db *sql.DB
}

// NewMappingStore opens/creates the SQLite database at the given path.
func NewMappingStore(dbPath string) (*MappingStore, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &MappingStore{db: db}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS topic_mappings (
			chat_id       INTEGER NOT NULL,
			thread_id     INTEGER NOT NULL,
			repository    TEXT    NOT NULL,
			session_key   TEXT    NOT NULL UNIQUE,
			issue_number  INTEGER NOT NULL DEFAULT 0,
			pr_number     INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (chat_id, thread_id)
		);
		CREATE INDEX IF NOT EXISTS idx_session_key ON topic_mappings(session_key);
	`)
	return err
}

// Close closes the database.
func (s *MappingStore) Close() error {
	return s.db.Close()
}

// CreateMapping inserts a new topic mapping.
func (s *MappingStore) CreateMapping(m *TopicMapping) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO topic_mappings (chat_id, thread_id, repository, session_key, issue_number, pr_number)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		m.ChatID, m.ThreadID, m.Repository, m.SessionKey, m.IssueNumber, m.PRNumber,
	)
	return err
}

// GetByThread returns the mapping for a given chat+thread, or nil if not found.
func (s *MappingStore) GetByThread(chatID int64, threadID int) (*TopicMapping, error) {
	row := s.db.QueryRow(
		`SELECT chat_id, thread_id, repository, session_key, issue_number, pr_number
		 FROM topic_mappings WHERE chat_id = ? AND thread_id = ?`,
		chatID, threadID,
	)
	var m TopicMapping
	err := row.Scan(&m.ChatID, &m.ThreadID, &m.Repository, &m.SessionKey, &m.IssueNumber, &m.PRNumber)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// GetBySessionKey returns the mapping for a session key, or nil if not found.
func (s *MappingStore) GetBySessionKey(sessionKey string) (*TopicMapping, error) {
	row := s.db.QueryRow(
		`SELECT chat_id, thread_id, repository, session_key, issue_number, pr_number
		 FROM topic_mappings WHERE session_key = ?`,
		sessionKey,
	)
	var m TopicMapping
	err := row.Scan(&m.ChatID, &m.ThreadID, &m.Repository, &m.SessionKey, &m.IssueNumber, &m.PRNumber)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// DeleteBySessionKey removes a mapping.
func (s *MappingStore) DeleteBySessionKey(sessionKey string) error {
	_, err := s.db.Exec(`DELETE FROM topic_mappings WHERE session_key = ?`, sessionKey)
	return err
}
