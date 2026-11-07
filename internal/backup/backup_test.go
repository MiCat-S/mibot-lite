package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/config"
)

const account = `{"api_id": 1234567, "api_hash": "abc", "session": "1xyz"}`

// deployment 按运行中部署的样子布置一个目录，连缓存都有，
// 这样测试能看出哪些东西没有被带走。
func deployment(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"config.json":              account,
		"gotd-session.json":        `{"dc":2}`,
		".env":                     "MIBOT_PREFIX=.\n",
		"updates.json":             `{"pts":1}`,
		"updates.json.cursor":      `{"pts":2}`,
		"restart-receipt.json":     `{"pending":{"chatId":"999","messageId":1}}`,
		"mibot-lite":               "binary",
		"mibot-lite.lock":          "",
		"data/ai.json":             `{"key":"sk-secret"}`,
		"data/alias.json":          `{"aliases":{"测速":"speedtest"}}`,
		"data/eatgif/abc.png":      "cache",
		"data/eatgif/abc.json":     `{"cache":true}`,
		"data/speedtest/speedtest": "cli",
		"data/notes.txt":           "not json",
	}
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestRoundTripKeepsConfigurationAndDropsTheRest(t *testing.T) {
	source := deployment(t)
	archive, names, err := Create(source, "0.1.12", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".env", "config.json", "data/ai.json", "data/alias.json", "gotd-session.json"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("packed %v, want %v", names, want)
	}

	target := t.TempDir()
	restored, err := Restore(bytes.NewReader(archive), target, false)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(restored)
	if strings.Join(restored, ",") != strings.Join(want, ",") {
		t.Fatalf("restored %v, want %v", restored, want)
	}
	for _, name := range want {
		got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		original, _ := os.ReadFile(filepath.Join(source, filepath.FromSlash(name)))
		if !bytes.Equal(got, original) {
			t.Errorf("%s changed in the round trip", name)
		}
		info, _ := os.Stat(filepath.Join(target, filepath.FromSlash(name)))
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s restored as %v; it holds keys and must be 0600", name, info.Mode().Perm())
		}
	}
	for _, left := range []string{"updates.json", "mibot-lite", "data/eatgif", "data/speedtest", "data/notes.txt"} {
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(left))); err == nil {
			t.Errorf("%s was carried over; it is a cache or machine state, not configuration", left)
		}
	}
}

// 这些文件名在别处各有一份定义；任何一边改了，备份就会悄悄地
// 不再带上会话。
func TestSessionFileNameMatchesTheApp(t *testing.T) {
	found := false
	for _, name := range rootFiles {
		found = found || name == config.SessionFile
	}
	if !found {
		t.Fatalf("backup does not carry %s", config.SessionFile)
	}
}

func TestRefusesToReplaceAnAccountUnlessAsked(t *testing.T) {
	archive, _, err := Create(deployment(t), "0.1.12", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	existing := `{"api_id": 7, "api_hash": "other", "session": "1other"}`
	os.WriteFile(filepath.Join(target, "config.json"), []byte(existing), 0o600)
	if _, err := Restore(bytes.NewReader(archive), target, false); !errors.Is(err, ErrExists) {
		t.Fatalf("restoring over an account without overwrite: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "config.json")); string(got) != existing {
		t.Fatal("the refused restore still touched config.json")
	}
	if _, err := Restore(bytes.NewReader(archive), target, true); err != nil {
		t.Fatalf("overwrite was asked for: %v", err)
	}
}

// Restore 要避开的正是这个坑：启动时旧的 gotd-session.json 优先于
// config.json，留下一个就会继续用旧账号。
func TestOverwriteRemovesASessionTheBackupDoesNotReplace(t *testing.T) {
	source := deployment(t)
	os.Remove(filepath.Join(source, "gotd-session.json"))
	archive, _, err := Create(source, "0.1.12", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	os.WriteFile(filepath.Join(target, "config.json"), []byte(account), 0o600)
	os.WriteFile(filepath.Join(target, "gotd-session.json"), []byte(`{"other":"account"}`), 0o600)
	os.WriteFile(filepath.Join(target, "updates.json"), []byte(`{"pts":99}`), 0o600)
	if _, err := Restore(bytes.NewReader(archive), target, true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gotd-session.json", "updates.json"} {
		if _, err := os.Stat(filepath.Join(target, name)); err == nil {
			t.Errorf("%s from the previous account survived the restore", name)
		}
	}
}

// forge 手工构造一个归档，用来写出 Create 永远不会写的内容。
func forge(t *testing.T, entries map[string]string, withManifest bool, kind byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zipped := gzip.NewWriter(&out)
	packed := tar.NewWriter(zipped)
	if withManifest {
		names := make([]string, 0, len(entries))
		for name := range entries {
			names = append(names, name)
		}
		raw, _ := json.Marshal(manifest{Format: format, Version: "x", Files: names})
		body := string(raw)
		packed.WriteHeader(&tar.Header{Name: Manifest, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg})
		packed.Write([]byte(body))
	}
	for name, body := range entries {
		header := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: kind}
		if kind == tar.TypeSymlink {
			header.Size, header.Linkname = 0, "/etc/passwd"
		}
		packed.WriteHeader(header)
		if kind == tar.TypeReg {
			packed.Write([]byte(body))
		}
	}
	packed.Close()
	zipped.Close()
	return out.Bytes()
}

// 下面每一个都必须被拒绝，而且要在写入任何东西之前拒绝。
func TestRejectsWhatABackupNeverHolds(t *testing.T) {
	cases := map[string][]byte{
		"climbs out":          forge(t, map[string]string{"../escape.json": "{}"}, true, tar.TypeReg),
		"absolute path":       forge(t, map[string]string{"/etc/cron.d/x": "{}"}, true, tar.TypeReg),
		"replaces the binary": forge(t, map[string]string{"mibot-lite": "evil"}, true, tar.TypeReg),
		"nested under data":   forge(t, map[string]string{"data/eatgif/x.json": "{}"}, true, tar.TypeReg),
		"hidden in data":      forge(t, map[string]string{"data/.x.json": "{}"}, true, tar.TypeReg),
		"a symlink":           forge(t, map[string]string{"config.json": ""}, true, tar.TypeSymlink),
		"no manifest":         forge(t, map[string]string{"data/ai.json": "{}"}, false, tar.TypeReg),
		"broken config":       forge(t, map[string]string{"config.json": `{"api_id":0}`}, true, tar.TypeReg),
		"not gzip":            []byte("PK\x03\x04 this is a zip"),
	}
	for label, archive := range cases {
		target := t.TempDir()
		if _, err := Restore(bytes.NewReader(archive), target, true); err == nil {
			t.Errorf("%s: accepted", label)
		}
		if entries, _ := os.ReadDir(target); len(entries) != 0 {
			t.Errorf("%s: wrote %d entries before refusing", label, len(entries))
		}
	}
}

// 下载到一半断掉时，目录必须保持原样。
func TestTruncatedArchiveWritesNothing(t *testing.T) {
	archive, _, err := Create(deployment(t), "0.1.12", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if _, err := Restore(bytes.NewReader(archive[:len(archive)/2]), target, true); err == nil {
		t.Fatal("half an archive was accepted")
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatalf("a truncated archive wrote %d entries", len(entries))
	}
}

func TestOversizedArchiveIsRefused(t *testing.T) {
	huge := strings.Repeat("0", maxTotal+10)
	archive := forge(t, map[string]string{"data/big.json": huge}, true, tar.TypeReg)
	if _, err := Restore(bytes.NewReader(archive), t.TempDir(), true); err == nil {
		t.Fatal("an archive past the size bound was accepted")
	}
}

func TestEmptyDeploymentHasNothingToBackUp(t *testing.T) {
	if _, _, err := Create(t.TempDir(), "0.1.12", time.Now()); err == nil {
		t.Fatal("an empty directory produced a backup")
	}
}

// 备份中缺少的设置表示默认值，不能继承目标旧账号的授权或定时动作。
func TestAccountRestoreRemovesTargetOnlySettings(t *testing.T) {
	source := t.TempDir()
	os.WriteFile(filepath.Join(source, "config.json"), []byte(account), 0o600)
	archive, _, err := Create(source, "v1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	target := deployment(t)
	for _, name := range []string{"sudo.json", "sure.json", "acron.json", "keyword.json"} {
		os.WriteFile(filepath.Join(target, "data", name), []byte(`{"users":[{"id":"999"}]}`), 0o600)
	}
	if _, err := Restore(bytes.NewReader(archive), target, true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".env", "gotd-session.json", "updates.json", "updates.json.cursor", "restart-receipt.json", "data/ai.json", "data/alias.json", "data/sudo.json", "data/sure.json", "data/acron.json", "data/keyword.json"} {
		if _, err := os.Stat(filepath.Join(target, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("old account setting survived: %s (%v)", name, err)
		}
	}
	for _, name := range []string{"config.json", "mibot-lite", "data/eatgif/abc.png", "data/eatgif/abc.json", "data/notes.txt"} {
		if _, err := os.Stat(filepath.Join(target, name)); err != nil {
			t.Errorf("out-of-scope file lost: %s (%v)", name, err)
		}
	}
}

// 不含账号的设置备份只做合并，不清除原会话和连接状态。
func TestSettingsOnlyRestoreKeepsAccountAndUnrelatedSettings(t *testing.T) {
	source := t.TempDir()
	os.Mkdir(filepath.Join(source, "data"), 0o700)
	os.WriteFile(filepath.Join(source, "data/ai.json"), []byte(`{"key":"new"}`), 0o600)
	archive, _, err := Create(source, "v1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	target := deployment(t)
	if _, err := Restore(bytes.NewReader(archive), target, true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.json", "gotd-session.json", "updates.json", "updates.json.cursor", "restart-receipt.json", ".env", "data/alias.json"} {
		if _, err := os.Stat(filepath.Join(target, name)); err != nil {
			t.Errorf("settings import removed %s: %v", name, err)
		}
	}
	got, _ := os.ReadFile(filepath.Join(target, "data/ai.json"))
	if string(got) != `{"key":"new"}` {
		t.Fatalf("setting not merged: %s", got)
	}
}

func TestSessionOnlyRestoreIsRejected(t *testing.T) {
	archive := forge(t, map[string]string{"gotd-session.json": `{"dc":2}`}, true, tar.TypeReg)
	target := deployment(t)
	if _, err := Restore(bytes.NewReader(archive), target, true); err == nil {
		t.Fatal("session without account accepted")
	}
	got, _ := os.ReadFile(filepath.Join(target, "config.json"))
	if string(got) != account {
		t.Fatal("rejected restore changed account")
	}
}

// 已暂存的新设置无法放入时，不能留下一个可启动、却带着混合授权的账号。
func TestFailedAccountRestoreWithholdsAccount(t *testing.T) {
	source := deployment(t)
	target := deployment(t)
	os.Remove(filepath.Join(target, "data/ai.json"))
	os.Mkdir(filepath.Join(target, "data/ai.json"), 0o700)
	archive, _, err := Create(source, "v1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(bytes.NewReader(archive), target, true); err == nil || !strings.Contains(err.Error(), "config.json is withheld") {
		t.Fatalf("restore failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed restore left account enabled: %v", err)
	}
}

// 有清单但文件条目缺失的归档不算部分导入，避免恢复时误删或继承设置。
func TestIncompleteManifestWritesNothing(t *testing.T) {
	var out bytes.Buffer
	zipped := gzip.NewWriter(&out)
	packed := tar.NewWriter(zipped)
	put(packed, Manifest, []byte(`{"format":1,"files":["config.json","data/sudo.json"]}`), time.Now())
	put(packed, "config.json", []byte(account), time.Now())
	packed.Close()
	zipped.Close()
	target := deployment(t)
	if _, err := Restore(bytes.NewReader(out.Bytes()), target, true); err == nil {
		t.Fatal("incomplete snapshot accepted")
	}
	got, _ := os.ReadFile(filepath.Join(target, "gotd-session.json"))
	if string(got) != `{"dc":2}` {
		t.Fatal("incomplete snapshot touched deployment")
	}
}
