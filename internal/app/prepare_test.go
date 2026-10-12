package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/config"
	"github.com/MiCat-S/mibot-lite/internal/session"
)

func deployment(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	text, err := (&session.StringSession{DC: 2, Address: "149.154.167.51", Port: 443, AuthKey: make([]byte, session.AuthKeyLength)}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"api_id": 123, "api_hash": "test", "session": text})
	if err := os.WriteFile(filepath.Join(root, config.ConfigFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestReadOnlyPrepareCreatesNoFiles(t *testing.T) {
	root := deployment(t)
	a, err := Prepare(context.Background(), Options{Root: root, ReadOnly: true, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != config.ConfigFile {
		t.Fatalf("read-only check wrote files: %v", entries)
	}
}

func TestPrepareLocksBeforeSessionImport(t *testing.T) {
	root := deployment(t)
	lock, err := LockRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := Prepare(context.Background(), Options{Root: root}); !errors.Is(err, ErrRunning) {
		t.Fatalf("did not enforce lock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, config.SessionFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("session imported before acquiring lock")
	}
}

func TestFailedPrepareReleasesLock(t *testing.T) {
	root := t.TempDir()
	raw, _ := json.Marshal(map[string]any{"api_id": 123, "api_hash": "test", "session": "invalid"})
	_ = os.WriteFile(filepath.Join(root, config.ConfigFile), raw, 0600)
	if _, err := Prepare(context.Background(), Options{Root: root}); err == nil {
		t.Fatal("invalid session accepted")
	}
	lock, err := LockRoot(root)
	if err != nil {
		t.Fatalf("failed preparation retained lock: %v", err)
	}
	lock.Close()
}
