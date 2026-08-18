// Package metrics owns the accounting database: its schema, its ingestion
// path, its rollups and its queries. Nothing else opens the database.
package metrics

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	_ "modernc.org/sqlite"
)

// Store is the accounting database.
type Store struct {
	db *sql.DB
}

// Open creates or opens the database at path, creating its directory, applying
// migrations, and enforcing permissions.
func Open(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create metrics dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("chmod metrics dir: %w", err)
	}

	// WAL keeps readers (queries, the UI) from blocking the writer;
	// synchronous=NORMAL is the right trade for accounting data, which is
	// valuable but not worth an fsync per transaction. busy_timeout stops a
	// concurrent reader from turning into an immediate SQLITE_BUSY error.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	s, err := open(dsn)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		s.Close()
		return nil, fmt.Errorf("chmod metrics db: %w", err)
	}
	// WAL mode's own -wal and -shm files hold the same usage history as the main
	// database file and are created during migrate() above, before this point —
	// so without this they are left at the process umask (typically 0644,
	// world-readable) even though the main file is locked down to 0600 right
	// above. Tolerate NotExist: a driver that has already checkpointed and
	// removed them leaves nothing to chmod.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Chmod(path+suffix, 0o600); err != nil && !os.IsNotExist(err) {
			s.Close()
			return nil, fmt.Errorf("chmod metrics %s: %w", suffix, err)
		}
	}
	return s, nil
}

// memCounter gives each OpenMemory call its own named in-memory database.
// "file::memory:?cache=shared" (without a name) aliases every in-process
// caller onto the SAME database, so two stores open at once — or two tests
// running in parallel — would silently share rows. A unique name per call
// keeps cache=shared's benefit (multiple connections to withstand
// db.SetMaxOpenConns(1) plus migration's own connection) without that
// cross-test contamination.
var memCounter atomic.Int64

// OpenMemory returns an in-memory store for tests, isolated from every other
// OpenMemory store in the process.
func OpenMemory() (*Store, error) {
	n := memCounter.Add(1)
	return open(fmt.Sprintf("file:memdb%d?mode=memory&cache=shared&_pragma=busy_timeout(5000)", n))
}

func open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open metrics db: %w", err)
	}
	// One writer. SQLite serializes writes anyway, and the ingester is
	// single-goroutine by design, so a larger pool only invites lock contention.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(
		`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}

	var current int
	err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&current)
	if err == sql.ErrNoRows {
		if _, err := s.db.Exec(`INSERT INTO schema_version(version) VALUES (0)`); err != nil {
			return fmt.Errorf("seed schema_version: %w", err)
		}
		current = 0
	} else if err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}

	for i := current; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`UPDATE schema_version SET version = ?`, i+1); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", i+1, err)
		}
	}
	return nil
}
