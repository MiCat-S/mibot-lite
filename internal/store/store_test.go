package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type document struct {
	Count int            `json:"count"`
	Items map[string]int `json:"items"`
}

func newTestStore(t *testing.T) *Store[document] {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "doc.json"), func() document { return document{Items: map[string]int{}} })
}

func TestReadMissingFileReturnsDefaults(t *testing.T) {
	store := newTestStore(t)
	value, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if value.Count != 0 || value.Items == nil {
		t.Fatalf("unexpected defaults: %+v", value)
	}
}

func TestUpdatePersists(t *testing.T) {
	store := newTestStore(t)
	if err := store.Update(func(value *document) error {
		value.Count = 3
		value.Items["a"] = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reopened := New(store.Path(), func() document { return document{Items: map[string]int{}} })
	value, err := reopened.Read()
	if err != nil {
		t.Fatal(err)
	}
	if value.Count != 3 || value.Items["a"] != 1 {
		t.Fatalf("value did not survive: %+v", value)
	}
	if info, err := os.Stat(store.Path()); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("stored document must be owner-only, got %v %v", info.Mode(), err)
	}
}

// A failed mutation must leave the file exactly as it was: these documents
// hold API keys and task state, and a half-applied change is worse than a
// rejected one.
func TestFailedUpdateLeavesFileAlone(t *testing.T) {
	store := newTestStore(t)
	_ = store.Update(func(value *document) error { value.Count = 1; return nil })
	failure := errors.New("no")
	if err := store.Update(func(value *document) error { value.Count = 99; return failure }); !errors.Is(err, failure) {
		t.Fatalf("expected the mutation error, got %v", err)
	}
	value, _ := store.Read()
	if value.Count != 1 {
		t.Fatalf("count is %d, the failed update leaked", value.Count)
	}
}

// Read hands out a copy: a caller that mutates what it got must not change
// what the next reader sees.
func TestReadReturnsCopy(t *testing.T) {
	store := newTestStore(t)
	_ = store.Update(func(value *document) error { value.Items["a"] = 1; return nil })
	first, _ := store.Read()
	first.Items["a"] = 42
	second, _ := store.Read()
	if second.Items["a"] != 1 {
		t.Fatalf("mutating a read value changed the store: %d", second.Items["a"])
	}
}
