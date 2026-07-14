// Package store is a file-backed registry of sessions and namespaces. It holds
// no open handles or locks, so any number of processes may read and write it
// concurrently; writes are made atomic via temp-file-then-rename.
package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ysmood/tm/pkg/config"
)

// ErrNotFound is returned when a session id has no metadata file.
var ErrNotFound = errors.New("session not found")

// Store reads and writes session/namespace state under a Paths root.
type Store struct {
	paths config.Paths
}

// New builds a Store over the given paths.
func New(p config.Paths) *Store { return &Store{paths: p} }

// Open resolves paths from the environment and ensures the directories exist.
func Open() (*Store, error) {
	p, err := config.New()
	if err != nil {
		return nil, err
	}

	if err := p.EnsureDirs(); err != nil {
		return nil, err
	}

	return &Store{paths: p}, nil
}

// Paths exposes the resolved storage locations.
func (s *Store) Paths() config.Paths { return s.paths }

// SaveSession atomically writes a session's metadata file.
func (s *Store) SaveSession(sess Session) error {
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}

	return atomicWrite(s.paths.SessionFile(sess.ID), data, 0o600)
}

// GetSession reads one session by id.
func (s *Store) GetSession(id string) (Session, error) {
	return readSession(s.paths.SessionFile(id))
}

// RenameSession changes a session's display name in place, leaving its id — and
// so its socket, log and every other derived path — untouched, which is why a
// rename is safe while the session is running. Names must be unique within a
// namespace (the generated defaults are, see naming.Unique), so a name another
// session in the same namespace already holds is rejected.
func (s *Store) RenameSession(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name cannot be empty")
	}

	sess, err := s.GetSession(id)
	if err != nil {
		return err
	}

	if sess.Name == name {
		return nil // renaming to the same name: nothing to write
	}

	siblings, err := s.ListByNamespace(sess.Namespace)
	if err != nil {
		return err
	}

	for _, sib := range siblings {
		if sib.ID != id && sib.Name == name {
			return errors.New("name already in use: " + name)
		}
	}

	sess.Name = name

	return s.SaveSession(sess)
}

// DeleteSession removes a session's own directory — its metadata, log and daemon
// log go with it — plus the transient files it owns outside that directory (its
// socket and ready marker, which live under Sock). Missing files are ignored.
func (s *Store) DeleteSession(id string) error {
	_ = os.Remove(s.paths.SockFile(id))
	_ = os.Remove(s.paths.ReadyFile(id))

	return os.RemoveAll(s.paths.SessionDir(id))
}

// ListSessions returns all sessions sorted by creation time then name. A session
// is a directory holding a metadata file; anything else under Sessions (a stray
// file, a directory with no readable metadata — say, one half-written by a
// concurrent create) is skipped rather than failing the listing.
func (s *Store) ListSessions() ([]Session, error) {
	entries, err := os.ReadDir(s.paths.Sessions())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, err
	}

	var out []Session

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		sess, err := readSession(s.paths.SessionFile(e.Name()))
		if err != nil {
			continue
		}

		out = append(out, sess)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].Name < out[j].Name
		}

		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})

	return out, nil
}

// ListByNamespace returns sessions in ns; ns == AllNamespaces returns everything.
func (s *Store) ListByNamespace(ns string) ([]Session, error) {
	all, err := s.ListSessions()
	if err != nil {
		return nil, err
	}

	if ns == AllNamespaces {
		return all, nil
	}

	var out []Session

	for _, sess := range all {
		if sess.Namespace == ns {
			out = append(out, sess)
		}
	}

	return out, nil
}

// CreateNamespace creates a marker so an empty namespace persists in listings.
func (s *Store) CreateNamespace(name string) error {
	if name == "" || name == AllNamespaces {
		return errors.New("invalid namespace name")
	}

	return atomicWrite(s.paths.NamespaceFile(name), nil, 0o600)
}

// DeleteNamespace reassigns the namespace's sessions to DefaultNamespace, then
// removes the marker. The default and "*" namespaces cannot be dropped.
func (s *Store) DeleteNamespace(name string) error {
	if name == DefaultNamespace || name == AllNamespaces || name == "" {
		return errors.New("cannot drop this namespace")
	}

	sessions, err := s.ListByNamespace(name)
	if err != nil {
		return err
	}

	for _, sess := range sessions {
		sess.Namespace = DefaultNamespace

		err := s.SaveSession(sess)
		if err != nil {
			return err
		}
	}

	if err := os.Remove(s.paths.NamespaceFile(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

// ListNamespaces returns the union of DefaultNamespace, marker files, and
// namespaces referenced by existing sessions, sorted.
func (s *Store) ListNamespaces() ([]string, error) {
	set := map[string]bool{DefaultNamespace: true}

	entries, err := os.ReadDir(s.paths.Namespaces())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	for _, e := range entries {
		if !e.IsDir() {
			set[e.Name()] = true
		}
	}

	sessions, err := s.ListSessions()
	if err != nil {
		return nil, err
	}

	for _, sess := range sessions {
		if sess.Namespace != "" {
			set[sess.Namespace] = true
		}
	}

	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}

	sort.Strings(out)

	return out, nil
}

// Prune removes every session for which alive returns false.
func (s *Store) Prune(alive func(Session) bool) error {
	sessions, err := s.ListSessions()
	if err != nil {
		return err
	}

	for _, sess := range sessions {
		if !alive(sess) {
			err := s.DeleteSession(sess.ID)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func readSession(path string) (Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Session{}, ErrNotFound
		}

		return Session{}, err
	}

	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return Session{}, err
	}

	return sess, nil
}

// atomicWrite writes data to path via a temp file in the same directory followed
// by an atomic rename, so readers never observe a partial file.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}

	tmp := f.Name()

	defer func() { _ = os.Remove(tmp) }()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()

		return err
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()

		return err
	}

	if err := f.Close(); err != nil {
		return err
	}

	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}

	return os.Rename(tmp, path)
}
