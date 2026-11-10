package gateway

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct {
	db  *sql.DB
	dir string
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "happy.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS state (id INTEGER PRIMARY KEY CHECK(id=1), body BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS admin (id INTEGER PRIMARY KEY CHECK(id=1), hash BLOB NOT NULL);`); err != nil {
		db.Close()
		return nil, err
	}
	if err = os.Chmod(filepath.Join(dir, "happy.db"), 0600); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, dir: dir}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) load() (State, error) {
	var b []byte
	err := s.db.QueryRow("SELECT body FROM state WHERE id=1").Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		v, e := initialState()
		if e != nil {
			return v, e
		}
		return v, s.save(v)
	}
	if err != nil {
		return State{}, err
	}
	var v State
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	err = decoder.Decode(&v)
	return v, err
}
func (s *Store) save(v State) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO state(id,body) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body", b)
	return err
}
func (s *Store) SetPassword(hash []byte) error {
	_, err := s.db.Exec("INSERT INTO admin(id,hash) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET hash=excluded.hash", hash)
	return err
}

// InitializePassword never overwrites a password changed through the UI.
func (s *Store) InitializePassword(hash []byte) error {
	_, err := s.db.Exec("INSERT INTO admin(id,hash) VALUES(1,?) ON CONFLICT(id) DO NOTHING", hash)
	return err
}
func (s *Store) password() ([]byte, error) {
	var b []byte
	err := s.db.QueryRow("SELECT hash FROM admin WHERE id=1").Scan(&b)
	return b, err
}

// A durable revocation latch prevents a failed database write from restoring an
// old exit or peer after a restart. It is cleared only after the new state commits.
func (s *Store) latchClose() error {
	f, err := os.OpenFile(filepath.Join(s.dir, "closed"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err = f.WriteString("closed\n"); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return s.syncDir()
}
func (s *Store) syncDir() error {
	f, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (s *Store) clearLatch() error {
	err := os.Remove(filepath.Join(s.dir, "closed"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.syncDir()
}
func (s *Store) isLatched() (bool, error) {
	_, err := os.Stat(filepath.Join(s.dir, "closed"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
