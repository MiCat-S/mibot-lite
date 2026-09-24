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
