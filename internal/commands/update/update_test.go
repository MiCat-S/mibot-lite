package update

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

func TestNewerVersion(t *testing.T) {
	if !newer("", "v1.0.0") || !newer("dev", "v1.0.0") || !newer("0.1.0", "v0.2.0") {
		t.Error("a new release should be detected")
	}
	if newer("v1.2.3", "1.2.3") {
		t.Error("the same version with a different v prefix is not newer")
	}
}

// swapBinary 的每一步失败，都要让程序文件的位置上还有一个能跑的版本；实在恢复不了，
// 也要告诉人原来的版本在哪里。rename 换成在第 fail 次调用时失败的假版本。
func TestSwapBinary(t *testing.T) {
	setup := func(t *testing.T) (string, string, string) {
		dir := t.TempDir()
		binary, incoming, aside := filepath.Join(dir, "mibot-lite"), filepath.Join(dir, "mibot-lite.download"), filepath.Join(dir, "mibot-lite.previous")
		os.WriteFile(binary, []byte("old"), 0o755)
		os.WriteFile(incoming, []byte("new"), 0o755)
		return binary, incoming, aside
	}
	failing := func(fail ...int) func(from, to string) error {
		calls := 0
		return func(from, to string) error {
			calls++
			for _, n := range fail {
				if calls == n {
					return errors.New("rename failed")
				}
			}
			return os.Rename(from, to)
		}
	}
	read := func(path string) string {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "<missing>"
		}
		return string(raw)
	}

	binary, incoming, aside := setup(t)
	if err := swapBinary(os.Rename, binary, incoming, aside); err != nil || read(binary) != "new" || read(aside) != "old" {
		t.Fatalf("swap: err=%v binary=%s aside=%s", err, read(binary), read(aside))
	}

	// 挪不开当前的：什么都没动。
	binary, incoming, aside = setup(t)
	err := swapBinary(failing(1), binary, incoming, aside)
	if text, ok := kit.IsUserError(err); !ok || !strings.Contains(text, "什么都没改") || read(binary) != "old" {
		t.Errorf("first step: %v binary=%s", err, read(binary))
	}

	// 新的放不进去：换回原来的。
	binary, incoming, aside = setup(t)
	err = swapBinary(failing(2), binary, incoming, aside)
	if text, ok := kit.IsUserError(err); !ok || !strings.Contains(text, "已恢复") || read(binary) != "old" {
		t.Errorf("second step: %v binary=%s", err, read(binary))
	}

	// 放不进去、也换不回来：程序文件的位置是空的，提示里要写明原来的版本在哪、该改成什么名字。
	binary, incoming, aside = setup(t)
	err = swapBinary(failing(2, 3), binary, incoming, aside)
	text, ok := kit.IsUserError(err)
	if !ok || !strings.Contains(text, aside) || !strings.Contains(text, binary) || read(aside) != "old" {
		t.Errorf("restore failed: %q (aside=%s)", text, read(aside))
	}
	if !strings.Contains(err.Error(), "rename failed") {
		t.Errorf("the log line lost the cause: %v", err)
	}
}

// 到点前不动；到点了有命令在跑就等，等满两小时改天；没命令在跑就查；今天查过或者关了就不动。
// 过了检查时间才启动的，当天补查。
func TestDecide(t *testing.T) {
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, shanghai)
	on := autoConfig{Enabled: true, Time: "04:00"}
	for name, c := range map[string]struct {
		cfg     autoConfig
		at      time.Duration
		running int
		want    autoAction
	}{
		"还没到点":      {on, 3*time.Hour + 59*time.Minute, 0, autoIdle},
		"到点、空闲":     {on, 4 * time.Hour, 0, autoCheck},
		"到点、有命令在跑":  {on, 4*time.Hour + 30*time.Minute, 2, autoWait},
		"等满两小时":     {on, 6 * time.Hour, 1, autoGiveUp},
		"下午才启动，补查":  {on, 15 * time.Hour, 0, autoCheck},
		"今天查过":      {autoConfig{Enabled: true, Time: "04:00", CheckedDate: "2026-10-01"}, 5 * time.Hour, 0, autoIdle},
		"关着":        {autoConfig{Time: "04:00"}, 5 * time.Hour, 0, autoIdle},
		"时间写坏了按四点算": {autoConfig{Enabled: true, Time: "25:99"}, 4 * time.Hour, 0, autoCheck},
	} {
		got, today := decide(c.cfg, day.Add(c.at), c.running)
		if got != c.want || today != "2026-10-01" {
			t.Errorf("%s：%v %s，应为 %v", name, got, today, c.want)
		}
	}
	// 北京时间的日期，不是 UTC 的：UTC 前一天 20:00 就是北京时间今天 04:00。
	if got, today := decide(on, time.Date(2026, 9, 30, 20, 0, 0, 0, time.UTC), 0); got != autoCheck || today != "2026-10-01" {
		t.Errorf("时区：%v %s", got, today)
	}
}

// 自动更新只往前走：dev 版、旧版本、看不懂的版本号都不装。
func TestStrictlyNewer(t *testing.T) {
	for _, c := range []struct {
		current, latest string
		want            bool
	}{
		{"0.3.0", "v0.3.1", true}, {"v0.3.9", "v0.4.0", true}, {"0.9.0", "v1.0.0", true}, {"0.3.10", "v0.3.9", false},
		{"0.3.0", "v0.3.0", false}, {"0.3.1", "v0.3.0", false}, {"dev", "v0.3.1", false}, {"", "v0.3.1", false},
		{"0.3.0", "v0.4.0-rc1", false}, {"0.3.0", "latest", false},
	} {
		if got := strictlyNewer(c.current, c.latest); got != c.want {
			t.Errorf("strictlyNewer(%q, %q) = %v", c.current, c.latest, got)
		}
	}
}

// tick：到点查到的已是最新，只记下今天查过，不发消息、不重启；等满两小时也记下，改天再查。
func TestTick(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"tag_name":"v0.3.0","assets":[]}`))
	}))
	defer server.Close()
	githubAPI = server.URL
	defer func() { githubAPI = "https://api.github.com" }()

	registry := command.New([]string{"."}, slog.New(slog.DiscardHandler))
	a := &app.App{Root: t.TempDir(), Version: "0.3.0", Registry: registry}
	u := &updater{a: a, repo: "x/y", settings: store.New(filepath.Join(t.TempDir(), "update.json"), autoDefaults)}
	client := bot.FromAPI(tg.NewClient(nil), bot.NewPeerCache(), &tg.User{ID: 1}, slog.New(slog.DiscardHandler))
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, shanghai)

	u.tick(context.Background(), client, day.Add(3*time.Hour))
	if requests != 0 {
		t.Fatal("没到点就查了")
	}
	u.tick(context.Background(), client, day.Add(4*time.Hour))
	cfg, _ := u.settings.Read()
	if requests != 1 || cfg.CheckedDate != "2026-10-01" {
		t.Fatalf("到点应查一次并记下：%d 次，%+v", requests, cfg)
	}
	u.tick(context.Background(), client, day.Add(4*time.Hour+time.Minute))
	if requests != 1 {
		t.Error("同一天查了两次")
	}
}
