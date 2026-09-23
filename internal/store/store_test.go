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

// 修改失败时文件必须原封不动：这些文档里存着 API key 和任务状态，
// 改了一半比整个被拒绝更糟。
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

// Read 交出去的是副本：调用方改动自己拿到的那份，
// 不能影响下一个读取者看到的内容。
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
