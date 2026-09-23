package commands

import (
	"io"
	"log/slog"
	"slices"
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
			t.Errorf("delegable 里的 %q 不是已注册的命令", name)
		}
	}
	for _, ownerOnly := range []string{"sudo", "sure", "dme", "da", "acn", "autochangename", "prefix", "alias", "bf", "log", "save", "restart", "update", "sb", "unsb", "sysinfo", "refresh", "aban"} {
		if slices.Contains(sudo.Delegable(), ownerOnly) {
			t.Errorf("%q 不该能借出去", ownerOnly)
		}
	}
}
