// Package store is the persistence layer: one JSON file per command family,
// written atomically, guarded by a mutex. No database.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store keeps one JSON document of type T on disk.
type Store[T any] struct {
	path     string
	defaults func() T
	mu       sync.Mutex
	cached   *T
}

// New builds a store. defaults produces the document a missing or empty file
// stands for; it must return a fresh value each call.
func New[T any](path string, defaults func() T) *Store[T] {
	return &Store[T]{path: path, defaults: defaults}
}

// Path is where the document lives.
func (s *Store[T]) Path() string { return s.path }

func (s *Store[T]) load() (*T, error) {
	if s.cached != nil {
		return s.cached, nil
	}
	value := s.defaults()
	raw, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	case len(raw) > 0:
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("%s: %w", s.path, err)
		}
	}
	s.cached = &value
	return s.cached, nil
}

// Read returns a copy of the document.
func (s *Store[T]) Read() (T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, err := s.load()
	if err != nil {
		var zero T
		return zero, err
	}
	return clone(*value)
}

// Update applies mutate to the document and writes it back. An error from
// mutate leaves the file untouched.
func (s *Store[T]) Update(mutate func(value *T) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.load()
	if err != nil {
		return err
	}
	working, err := clone(*current)
	if err != nil {
		return err
	}
	if err := mutate(&working); err != nil {
		return err
	}
	if err := writeAtomic(s.path, working); err != nil {
		return err
	}
	s.cached = &working
	return nil
}

func clone[T any](value T) (T, error) {
	var out T
	raw, err := json.Marshal(value)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	return out, nil
}

func writeAtomic(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".")
	if err != nil {
		return err
	}
	name := temporary.Name()
	cleanup := func() { temporary.Close(); os.Remove(name) }
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
