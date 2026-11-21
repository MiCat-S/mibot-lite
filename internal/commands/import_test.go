package commands

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/app"
)

func TestImportMiBoxRefusesLockedDeployment(t *testing.T) {
	root := t.TempDir()
	lock, err := app.LockRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := ImportMiBox(t.TempDir(), filepath.Join(root, "data"), io.Discard); !errors.Is(err, app.ErrRunning) {
		t.Fatalf("migrating into locked deployment: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "data")); !os.IsNotExist(err) {
		t.Fatalf("refused migration changed target: %v", err)
	}
}
