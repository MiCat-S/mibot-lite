package core

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/command"
)

func TestLookupForHelp(t *testing.T) {
	registry := command.New([]string{".", "。"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	registry.Register(&command.Command{Name: "speedtest"}, &command.Command{Name: "ping"})
	registry.SetAliases(map[string]string{"测速": "speedtest 12345"})
	for name, want := range map[string]string{
		"ping": "ping", ".ping": "ping", "。ping": "ping", "PING": "ping", "测速": "speedtest",
	} {
		cmd, ok := lookupForHelp(registry, name)
		if !ok || cmd.Name != want {
			t.Errorf("%q → %v %v，应为 %s", name, cmd, ok, want)
		}
	}
	if _, ok := lookupForHelp(registry, "nosuch"); ok {
		t.Error("不存在的命令不该找到")
	}
}

// 无效目标在发请求之前就拒掉。
func TestProbeRejectsBadTargets(t *testing.T) {
	for _, bad := range []string{"a b", "http://x", "x/y", "a;b", strings.Repeat("a", 254)} {
		if got := probe(context.Background(), bad); !strings.Contains(got, "无效的目标") {
			t.Errorf("%q 应该被拒：%s", bad, got)
		}
	}
}

// 总览只列名字；.help 分组 才带说明和简写；.help ai 是命令不是「AI」那一组。
func TestHelpListAndGroups(t *testing.T) {
	registry := command.New([]string{"."}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	registry.Register(
		&command.Command{Name: "ping", Group: command.GroupSystem, Description: "测试网络延迟"},
		&command.Command{Name: "speedtest", Aliases: []string{"st"}, Group: command.GroupTools, Description: "测量主机网络速度"},
		&command.Command{Name: "ai", Group: command.GroupAI, Description: "与 AI 对话", Help: func(string) string { return "ai 的帮助" }},
	)
	list := renderHelpList(registry, ".")
	for _, want := range []string{"<b>运行与维护</b>\n<code>.ping</code>", "<b>查询与工具</b>\n<code>.speedtest</code>", "<code>.help 分组</code>"} {
		if !strings.Contains(list, want) {
			t.Errorf("总览缺 %q：\n%s", want, list)
		}
	}
	if strings.Contains(list, "测试网络延迟") || strings.Contains(list, ".st") || strings.Contains(list, "账号") {
		t.Errorf("总览不该有说明、简写和空的分组：\n%s", list)
	}
	group := renderCommandHelp(registry, ".", "工具")
	if !strings.Contains(group, "<code>.speedtest</code>（<code>.st</code>） — 测量主机网络速度") || strings.Contains(group, ".ping") {
		t.Errorf("分组列表不对：\n%s", group)
	}
	if got := renderCommandHelp(registry, ".", "ai"); got != "ai 的帮助" {
		t.Errorf(".help ai 应是命令的帮助：%q", got)
	}
	if got := renderCommandHelp(registry, ".", "nosuch"); !strings.HasPrefix(got, "❌ 没有这个命令或分组") {
		t.Errorf("不存在时：%q", got)
	}
	for name, want := range map[string]string{"群组": command.GroupAdmin, "消息": command.GroupMedia, "运行": command.GroupSystem, "账号": command.GroupAccount, "查询与工具": command.GroupTools} {
		if got, ok := findGroup(name); !ok || got != want {
			t.Errorf("findGroup(%q) = %q %v，应为 %s", name, got, ok, want)
		}
	}
	if _, ok := findGroup("与"); ok {
		t.Error("落在两个组名里的一段不该算找到")
	}
}
