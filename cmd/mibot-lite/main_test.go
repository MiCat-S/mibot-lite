package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInvalidModeCombinationsAreRejectedBeforeBackup(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("MIBOT_PREFIX=.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "backup.tar.gz")
	if code := run(flags{root: root, check: true, backupTo: archive}); code != 2 {
		t.Fatalf("ambiguous command returned %d, want usage error", code)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatalf("ambiguous command wrote a backup: %v", err)
	}
}
