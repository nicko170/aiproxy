package metrics

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenCreatesSchemaAndSetsPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "metrics.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	for _, table := range []string{"requests", "quota_samples", "usage_buckets", "schema_version"} {
		var name string
		err := s.DB().QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing: %v", table, err)
		}
	}

	var mode string
	if err := s.DB().QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}

	var version int
	if err := s.DB().QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Errorf("schema version = %d, want %d", version, SchemaVersion)
	}
}

// The database holds a usage history; it must not be world-readable, and the
// directory must be created if absent.
func TestOpenEnforcesPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	path := filepath.Join(dir, "metrics.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("db perm = %o, want 600", perm)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 700", perm)
	}

	// WAL mode's own -wal and -shm files hold the same usage history and are
	// created during migrate(), before the main file's Chmod — so without their
	// own Chmod they are left world-readable at the process umask even though
	// the main file is locked down.
	for _, suffix := range []string{"-wal", "-shm"} {
		fi, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatalf("stat %s%s: %v", path, suffix, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s perm = %o, want 600", suffix, perm)
		}
	}
}

// Opening an existing database must be idempotent, not destructive.
func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.DB().Exec(
		`INSERT INTO requests(started_at, account_id, provider) VALUES (1, 'a', 'p')`); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	var n int
	if err := s2.DB().QueryRow(`SELECT count(*) FROM requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("row count = %d, want 1 — reopening must not recreate the schema", n)
	}
}

func TestOpenMemoryWorksForTests(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer s.Close()
	if _, err := s.DB().Exec(
		`INSERT INTO requests(started_at, account_id, provider) VALUES (1, 'a', 'p')`); err != nil {
		t.Errorf("insert into in-memory store: %v", err)
	}
}

// Two OpenMemory stores held open at once must not alias the same database.
// "file::memory:?cache=shared" without a unique name does exactly that, and it
// only went unnoticed because every prior test closed its store before the
// next one opened.
func TestOpenMemoryStoresAreIsolatedFromEachOther(t *testing.T) {
	s1, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory s1: %v", err)
	}
	defer s1.Close()
	s2, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory s2: %v", err)
	}
	defer s2.Close()

	if _, err := s1.DB().Exec(
		`INSERT INTO requests(started_at, account_id, provider) VALUES (1, 'a', 'p')`); err != nil {
		t.Fatalf("insert into s1: %v", err)
	}

	var n int
	if err := s2.DB().QueryRow(`SELECT count(*) FROM requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("s2 sees %d rows written to s1, want 0 — the two stores are aliasing the same database", n)
	}
}
