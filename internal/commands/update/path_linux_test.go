//go:build linux

package update

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/restart"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

// 用测试进程自己的副本执行真实改名，验证延期重启后命令仍操作原安装位置。
func TestUpdateKeepsInstallationPathAfterRename(t *testing.T) {
	if os.Getenv("MIBOT_UPDATE_PATH_HELPER") != "1" {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(binary)
		if err != nil {
			t.Fatal(err)
		}
		helper := filepath.Join(t.TempDir(), "mibot-lite")
		if err := os.WriteFile(helper, body, 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(helper, "-test.run=^TestUpdateKeepsInstallationPathAfterRename$", "-test.v")
		cmd.Env = append(os.Environ(), "MIBOT_UPDATE_PATH_HELPER=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("helper: %v\n%s", err, out)
		}
		return
	}
	binary, err := executablePath()
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	registry := command.New([]string{"."}, logger)
	defer registry.Abort()
	a := &app.App{Root: t.TempDir(), Registry: registry, Logger: logger}
	Register(a, &restart.Restarter{})
	registered, _ := registry.Lookup("update")
	if err := os.WriteFile(binary+".download", []byte("new candidate"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := swapBinary(os.Rename, binary, binary+".download", binary+".previous"); err != nil {
		t.Fatal(err)
	}
	current, err := executablePath()
	if err != nil || current != binary+".previous" {
		t.Fatalf("Linux rename precondition: %q %v", current, err)
	}
	var shown string
	stopped := errors.New("offline edit failed; never request restart")
	fake := telegram.InvokeFunc(func(_ context.Context, input bin.Encoder, _ bin.Decoder) error {
		request, ok := input.(*tg.MessagesEditMessageRequest)
		if !ok {
			t.Fatalf("unexpected RPC %T", input)
		}
		shown = request.Message
		return stopped
	})
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 1}, logger)
	inv := &command.Invocation{Prefix: ".", Client: client, Log: logger, Message: &bot.Message{ID: 1, ChatID: "1", Peer: &tg.PeerUser{UserID: 1}}}
	_ = registered.Handle(context.Background(), inv)
	if !strings.Contains(shown, binary) || strings.Contains(shown, binary+".previous") {
		t.Fatalf("status lost installation path: %s", shown)
	}
	inv.Args = []string{"rollback"}
	if err := registered.Handle(context.Background(), inv); !errors.Is(err, stopped) {
		t.Fatalf("rollback did not reach progress edit: %v", err)
	}
	previous, err := os.ReadFile(binary + ".previous")
	if err != nil || string(previous) != "new candidate" {
		t.Fatalf("rollback targeted wrong file: %q %v", previous, err)
	}
	installed, err := executablePath()
	if err != nil || installed != binary {
		t.Fatalf("rollback did not restore running inode to installation path: %s %v", installed, err)
	}
}
