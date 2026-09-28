package fsutil

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// 同时写同一个文件：最后留下的一定是某一次完整的内容，目录里不留临时文件。
func TestWriteFileAtomicConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "asset.png")
	contents := [][]byte{bytes.Repeat([]byte("a"), 1<<20), bytes.Repeat([]byte("b"), 1<<20)}
	var wg sync.WaitGroup
	for round := 0; round < 8; round++ {
		for _, content := range contents {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := WriteFileAtomic(path, content, 0o600); err != nil {
					t.Error(err)
				}
			}()
		}
	}
	wg.Wait()
	got, err := os.ReadFile(path)
	if err != nil || (!bytes.Equal(got, contents[0]) && !bytes.Equal(got, contents[1])) {
		t.Fatalf("the file is a mix of two writes (%d bytes, err %v)", len(got), err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temporary files were left behind: %d entries", len(entries))
	}
}
