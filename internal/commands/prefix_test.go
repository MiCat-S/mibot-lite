package commands

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/config"
)

func TestCheckPrefix(t *testing.T) {
	for _, good := range []string{".", "。", "！", "~", "$", "!!", "喵"} {
		if problem := checkPrefix(good); problem != "" {
			t.Errorf("%q 被拒了：%s", good, problem)
		}
	}
	for _, bad := range []string{"a", "1", "_", ".a", `"`, "'", "~~~~~~~~~", "\t"} {
		if checkPrefix(bad) == "" {
			t.Errorf("%q 应该被拒", bad)
		}
	}
}

func TestNextPrefixes(t *testing.T) {
	current := []string{".", "。", "$"}
	for _, c := range []struct {
		action string
		tokens []string
		want   []string
	}{
		{"set", []string{"!", "！", "!"}, []string{"!", "！"}},
		{"add", []string{"~", "."}, []string{".", "。", "$", "~"}},
		{"del", []string{"$", "。"}, []string{"."}},
		{"del", []string{".", "。", "$"}, nil},
	} {
		if got := nextPrefixes(c.action, current, c.tokens); !slices.Equal(got, c.want) {
			t.Errorf("%s %v = %v，应为 %v", c.action, c.tokens, got, c.want)
		}
	}
}

// 走一遍真实的处理流程：改完之后新前缀立刻能用、旧前缀失效、.env 写对了。
func TestPrefixCommandEndToEnd(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".env"), []byte("# 设置\nMIBOT_PREFIX=. 。\n"), 0o600)
	a := &app.App{Root: root, Registry: command.New([]string{".", "。"}, slog.New(slog.NewTextHandler(io.Discard, nil)))}
	a.Registry.Register(&command.Command{Name: "ping"})
	Prefix(a)
	prefix, _ := a.Registry.Lookup("prefix")

	fake := newFakeTelegram(t, dmeSelf)
	client := fakeClient(fake, otherUser, nil)
	run := func(text string) string {
		message := &bot.Message{ID: dmeCommand + len(fake.calls), Peer: dmePrivate, ChatID: bot.PeerID(dmePrivate), Out: true}
		route, ok := a.Registry.Parse(text)
		if !ok || route.Command != "prefix" {
			t.Fatalf("%q 没有解析成 prefix 命令", text)
		}
		inv := fakeInvocation(client, message, route.Args...)
		inv.Text, inv.Prefix = route.Text, route.Prefix
		if err := prefix.Handle(context.Background(), inv); err != nil {
			t.Fatal(err)
		}
		return fake.calls[len(fake.calls)-1]
	}

	if reply := run(".prefix"); !strings.Contains(reply, "当前前缀") {
		t.Errorf("查看前缀的回复不对：%s", reply)
	}
	if reply := run(".prefix set a"); !strings.Contains(reply, "分不开") {
		t.Errorf("字母前缀应该被拒：%s", reply)
	}
	if reply := run(".prefix set ! ！\n这一行是说明，不算前缀"); !strings.Contains(reply, "已改为") {
		t.Errorf("设置前缀的回复不对：%s", reply)
	}
	if got := a.Registry.Prefixes(); !slices.Equal(got, []string{"!", "！"}) {
		t.Fatalf("前缀是 %v，应为 [! ！]（第二行不该算进去）", got)
	}
	if _, ok := a.Registry.Parse(".ping"); ok {
		t.Error("旧前缀 . 还能用")
	}
	if route, ok := a.Registry.Parse("!ping"); !ok || route.Command != "ping" {
		t.Error("新前缀 ! 不能用")
	}
	raw, _ := os.ReadFile(filepath.Join(root, ".env"))
	if string(raw) != "# 设置\nMIBOT_PREFIX=! ！\n" {
		t.Errorf(".env 写成了 %q", raw)
	}
	if got := config.ReadEnv(root, nil).Prefixes(); !slices.Equal(got, []string{"!", "！"}) {
		t.Errorf("重启后会读到 %v", got)
	}
	run("!prefix add ~")
	run("！prefix del !")
	if got := a.Registry.Prefixes(); !slices.Equal(got, []string{"！", "~"}) {
		t.Errorf("add、del 之后前缀是 %v", got)
	}
	if reply := run("~prefix del ！ ~"); !strings.Contains(reply, "至少要保留") {
		t.Errorf("全删掉应该被拒：%s", reply)
	}
}
