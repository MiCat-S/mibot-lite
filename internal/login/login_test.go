package login

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gotdsession "github.com/gotd/td/session"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/config"
	"github.com/MiCat-S/mibot-lite/internal/session"
)

func TestLoginRefusesLockedDeployment(t *testing.T) {
	root := t.TempDir()
	lock, err := app.LockRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	var output bytes.Buffer
	if err := Run(t.Context(), Options{Root: root, Force: true, In: lock, Out: &output}); err == nil || !strings.Contains(err.Error(), "instance is running") {
		t.Fatalf("forced login on locked deployment: %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("started prompting before acquiring lock: %s", output.String())
	}
}

func TestLoginRejectsInvalidProxyBeforePrompt(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, config.ConfigFile), []byte(`{"proxy":{"socksType":4}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(filepath.Join(root, config.ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	var output bytes.Buffer
	if err := Run(t.Context(), Options{Root: root, In: input, Out: &output}); err == nil || !strings.Contains(err.Error(), "socksType 5") {
		t.Fatalf("login ignored invalid proxy: %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("started prompting before checking proxy: %s", output.String())
	}
}

func TestWriteAccountReplacesSessionAndClearsAccountState(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := []string{config.StateFile, config.StateFile + ".cursor", config.RestartReceiptFile}
	for _, name := range append(stale, config.ConfigFile, config.SessionFile, "data/alias.json", config.EnvFile) {
		if err := os.WriteFile(filepath.Join(root, name), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	account := &session.StringSession{DC: 2, Address: "149.154.167.51", Port: 443, AuthKey: bytes.Repeat([]byte{8}, session.AuthKeyLength)}
	document := []byte(`{"session":"new"}`)
	if err := writeAccount(t.Context(), root, document, account.Data()); err != nil {
		t.Fatal(err)
	}
	for _, name := range stale {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("stale state %s survived: %v", name, err)
		}
	}
	for name, expected := range map[string]string{config.ConfigFile: string(document), "data/alias.json": "old", config.EnvFile: "old"} {
		if content, err := os.ReadFile(filepath.Join(root, name)); err != nil || string(content) != expected {
			t.Fatalf("%s = %q, %v", name, content, err)
		}
	}
	loader := gotdsession.Loader{Storage: &gotdsession.FileStorage{Path: filepath.Join(root, config.SessionFile)}}
	data, err := loader.Load(t.Context())
	if err != nil || !bytes.Equal(data.AuthKey, account.AuthKey) {
		t.Fatalf("new gotd session could not be loaded: %v", err)
	}
}

func TestWriteAccountPreservesConfigOnSessionRemovalFailure(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, config.ConfigFile)
	if err := os.WriteFile(cfg, []byte("old config"), 0o600); err != nil {
		t.Fatal(err)
	}
	badPath := filepath.Join(root, config.SessionFile)
	if err := os.Mkdir(badPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badPath, "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeAccount(t.Context(), root, []byte("new config"), &gotdsession.Data{}); err == nil {
		t.Fatal("unexpectedly replaced unusable session location")
	}
	if data, err := os.ReadFile(cfg); err != nil || string(data) != "old config" {
		t.Fatalf("old config lost: %q %v", data, err)
	}
}
