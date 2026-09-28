package commands

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/sudo"
)

// 表里写的命令都得真的存在；拼错了就等于悄悄少借出去一个。
func TestDelegableNamesExist(t *testing.T) {
	a := &app.App{Root: t.TempDir(), Registry: command.New([]string{"."}, slog.New(slog.NewTextHandler(io.Discard, nil)))}
	RegisterAll(a)
	for _, name := range sudo.Delegable() {
		if _, ok := a.Registry.Lookup(name); !ok {
			t.Errorf("lendable 里的 %q 不是已注册的命令", name)
		}
	}
	for _, ownerOnly := range []string{"sudo", "sure", "dme", "da", "acn", "autochangename", "prefix", "alias", "bf", "log", "save", "restart", "update", "sb", "unsb", "sysinfo", "refresh", "aban"} {
		if slices.Contains(sudo.Delegable(), ownerOnly) {
			t.Errorf("%q 不该能借出去", ownerOnly)
		}
	}
}

// 从一个模拟的 MiBox 目录迁移：原样复制的、要转换的（SQLite 别名）、TB_PREFIX 都要搬过来，
// 已有的不覆盖。
func TestImportMiBox(t *testing.T) {
	mibox, root := t.TempDir(), t.TempDir()
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	aliasDB, err := os.ReadFile("../mibox/testdata/alias.db")
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(mibox, "assets/alias/alias.db"), aliasDB)
	write(filepath.Join(mibox, "assets/dme/config.json"), []byte(`{"x":1}`))
	write(filepath.Join(mibox, ".env"), []byte("TB_PREFIX=\"!  ！\"\n"))
	write(filepath.Join(root, ".env"), []byte("# 已有的\nOTHER=1\n"))
	// v2 和 v1 的测速设置都在时用 v2 的。
	write(filepath.Join(mibox, "assets/speedtest/v2-config.json"), []byte(`{"default_server_id": 222}`))
	write(filepath.Join(mibox, "assets/speedtest/speedtest.json"), []byte(`{"default_server_id": 111}`))

	var log strings.Builder
	if err := ImportMiBox(mibox, filepath.Join(root, "data"), &log); err != nil {
		t.Fatal(err)
	}
	var aliases struct {
		Aliases map[string]string `json:"aliases"`
	}
	raw, _ := os.ReadFile(filepath.Join(root, "data", "alias.json"))
	if err := json.Unmarshal(raw, &aliases); err != nil || aliases.Aliases["测速"] != "speedtest 12345" || len(aliases.Aliases) != 403 {
		t.Errorf("别名没迁对：%v %d 条\n%s", err, len(aliases.Aliases), log.String())
	}
	if raw, _ := os.ReadFile(filepath.Join(root, "data", "dme.json")); string(raw) != `{"x":1}` {
		t.Errorf("原样复制的文件不对：%q", raw)
	}
	if raw, _ := os.ReadFile(filepath.Join(root, "data", "speedtest.json")); !strings.Contains(string(raw), "222") {
		t.Errorf("测速设置应取 v2 的：%s", raw)
	}
	env, _ := os.ReadFile(filepath.Join(root, ".env"))
	if !strings.Contains(string(env), "MIBOT_PREFIX=! ！") || !strings.Contains(string(env), "OTHER=1") {
		t.Errorf(".env 不对：%q", env)
	}

	// 再跑一次：都已存在，一个也不覆盖。
	log.Reset()
	if err := ImportMiBox(mibox, filepath.Join(root, "data"), &log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "0 file(s) imported") || !strings.Contains(log.String(), "MIBOT_PREFIX already set") {
		t.Errorf("第二次不该覆盖：\n%s", log.String())
	}
}

// snapshot 记下目录下每个文件的内容和修改时间。
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[path] = info.ModTime().String() + "\n" + string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// --check 是只读的：.update run 在服务运行中用新版本跑它，注册命令时往 data/ 写东西，
// 就会从正在运行的服务底下改它的文件（da 会把正在跑的任务标成暂停）。
func TestReadOnlyRegistrationWritesNothing(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, "data"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "data", name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("da.json", `{"tasks":[{"chatId":"-1001","isRunning":true,"errors":[]}],"imported":true}`)
	write("acn.json", `{"users":{}}`)
	before := snapshot(t, root)

	a := &app.App{Root: root, ReadOnly: true, Registry: command.New([]string{"."}, slog.New(slog.NewTextHandler(io.Discard, nil)))}
	RegisterAll(a)

	after := snapshot(t, root)
	for path, was := range before {
		if after[path] != was {
			t.Errorf("只读注册改了 %s", path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("只读注册新建了 %s", path)
		}
	}
}
