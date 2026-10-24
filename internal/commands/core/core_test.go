package core

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
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
	names, folded, ok := strings.Cut(list, "<blockquote expandable>")
	if !ok || !strings.HasSuffix(folded, "</blockquote>") {
		t.Fatalf("说明应折在最后的引用里：\n%s", list)
	}
	for _, want := range []string{"<b>运行与维护</b>\n<code>.ping</code>", "<b>查询与工具</b>\n<code>.speedtest</code>", "<code>.help 命令</code>"} {
		if !strings.Contains(names, want) {
			t.Errorf("总览缺 %q：\n%s", want, names)
		}
	}
	if strings.Contains(names, "测试网络延迟") || strings.Contains(names, ".st") || strings.Contains(list, "账号") {
		t.Errorf("引用外面不该有说明、简写，空的分组不列：\n%s", list)
	}
	if !strings.Contains(folded, "<b>查询与工具</b>\n<code>.speedtest</code>（<code>.st</code>） — 测量主机网络速度") {
		t.Errorf("折叠的说明不对：\n%s", folded)
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

// Rich Message 版的总览和分组列表：总览露出组名和命令名，说明是详情块里的一张表格，
// 组名占一整行；分组列表直接是表格。都要能按 MTProto 编码、在上限之内。
func TestHelpRich(t *testing.T) {
	registry := command.New([]string{".", "$"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	registry.Register(
		&command.Command{Name: "ping", Group: command.GroupSystem, Description: "测试网络延迟"},
		&command.Command{Name: "speedtest", Aliases: []string{"st"}, Group: command.GroupTools, Description: "测量主机网络速度"},
	)
	list := helpListRich(registry, ".")
	if len(list) != 5 {
		t.Fatalf("总览应是标题、两组、提示和详情块：%d 块", len(list))
	}
	if got := bot.RichMessageText(richMessage(list[:4])); got != "📖 命令列表\n运行与维护\n.ping\n查询与工具\n.speedtest\n.help 命令 看用法 · 前缀：. $" {
		t.Errorf("总览的文字不对：%q", got)
	}
	details, ok := list[4].(*tg.PageBlockDetails)
	if !ok || details.Open {
		t.Fatalf("说明应收在收起的详情块里：%T", list[4])
	}
	table := details.Blocks[0].(*tg.PageBlockTable)
	if len(table.Rows) != 5 || !table.Rows[1].Cells[0].Header || table.Rows[1].Cells[0].Colspan != 2 {
		t.Fatalf("表格应是表头、组名行和命令行：%+v", table.Rows)
	}
	if got := bot.RichMessageText(richMessage([]tg.PageBlockClass{table})); !strings.Contains(got, ".speedtest（.st） | 测量主机网络速度") {
		t.Errorf("命令行不对：%q", got)
	}
	group := groupHelpRich(registry, ".", command.GroupTools)
	if got := bot.RichMessageText(richMessage(group)); got != "📖 查询与工具\n命令 | 说明\n.speedtest（.st） | 测量主机网络速度\n.help 命令 看用法" {
		t.Errorf("分组列表不对：%q", got)
	}
	for _, blocks := range [][]tg.PageBlockClass{list, group} {
		var buffer bin.Buffer
		if err := (&tg.InputRichMessage{Blocks: blocks}).Encode(&buffer); err != nil || !bot.RichFits(blocks) {
			t.Fatalf("编码失败或超限：%v", err)
		}
	}
}

func richMessage(blocks []tg.PageBlockClass) *tg.Message {
	message := &tg.Message{}
	message.SetRichMessage(tg.RichMessage{Blocks: blocks})
	return message
}
