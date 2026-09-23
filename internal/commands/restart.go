package commands

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// receipt 是能撑过 systemd 重启的记录，让重启回来的进程能编辑
// 发起重启的那条消息。
type receipt struct {
	ChatID      string `json:"chatId"`
	MessageID   int    `json:"messageId"`
	RequestedAt int64  `json:"requestedAt"`
	BootID      string `json:"bootId"`
	Kind        string `json:"kind"`
}

type receiptDocument struct {
	Pending *receipt `json:"pending"`
}

var chatIDPattern = regexp.MustCompile(`^-?[0-9]+$`)

// restarter 向 systemd 提交重启，并留下回执。
type restarter struct {
	a       *app.App
	service string
	store   *store.Store[receiptDocument]
	mu      sync.Mutex
	pending bool
}

var serviceRestarter *restarter

func systemctl() string {
	if _, err := os.Stat("/usr/bin/systemctl"); err == nil {
		return "/usr/bin/systemctl"
	}
	return "systemctl"
}

// Restart 注册 .restart 和处理回执的钩子。
func Restart(a *app.App) {
	r := &restarter{a: a, service: a.Env.Get("MIBOT_SERVICE", "mibot-lite.service"),
		store: store.New(filepath.Join(a.Root, "restart-receipt.json"), func() receiptDocument { return receiptDocument{} })}
	serviceRestarter = r
	a.Registry.Register(&command.Command{Name: "restart", Description: "重启 systemd 服务", Handle: func(ctx context.Context, inv *command.Invocation) error {
		return r.command(ctx, inv, "restart", "<b>MiBot Lite 重启</b>\n正在提交重启请求…", "服务重启命令执行失败。")
	}})
	a.Registry.AddJob(r.notifyReady)
}

func (r *restarter) command(ctx context.Context, inv *command.Invocation, kind, progress, failure string) error {
	r.mu.Lock()
	if r.pending {
		r.mu.Unlock()
		return inv.EditText(ctx, "重启请求已提交，请稍候")
	}
	r.pending = true
	r.mu.Unlock()
	release := func() { r.mu.Lock(); r.pending = false; r.mu.Unlock() }

	if err := inv.Edit(ctx, progress); err != nil {
		release()
		return err
	}
	note := receipt{ChatID: inv.Message.ChatID, MessageID: inv.Message.ID, RequestedAt: time.Now().UnixMilli(), BootID: r.a.BootID, Kind: kind}
	if err := r.store.Update(func(doc *receiptDocument) error { doc.Pending = &note; return nil }); err != nil {
		release()
		return err
	}
	if err := r.trigger(ctx); err != nil {
		release()
		_ = r.store.Update(func(doc *receiptDocument) error { doc.Pending = nil; return nil })
		status := r.status(ctx)
		return inv.Edit(ctx, command.Escape(failure)+"\n状态：\n"+status+"\n"+command.Escape(ownerHint())+
			"\n\n可执行 "+command.Code("systemctl status "+r.service+" --no-pager")+" 查看详情。")
	}
	return nil
}

func (r *restarter) trigger(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, systemctl(), "--no-block", "restart", r.service).Run()
}

func (r *restarter) status(ctx context.Context) string {
	rows := []string{}
	for _, field := range []string{"LoadState", "ActiveState", "SubState", "FragmentPath"} {
		ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		out, err := exec.CommandContext(ctx, systemctl(), "show", "--value", "-p", field, r.service).Output()
		cancel()
		value := "unavailable"
		if err == nil {
			if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
				value = trimmed
			} else {
				value = "unknown"
			}
		}
		rows = append(rows, command.Escape(field)+": "+command.Escape(value))
	}
	return strings.Join(rows, "\n")
}

func ownerHint() string {
	uid := os.Getuid()
	if uid == 0 {
		return "当前运行在 root 用户。"
	}
	return "当前运行用户 UID=" + strconv.Itoa(uid) + "，通常需要 root 或 systemd 管理权限。"
}

// notifyReady 回应发起重启的那条消息，只回应一次。
func (r *restarter) notifyReady(ctx context.Context, client *bot.Client) {
	doc, err := r.store.Read()
	if err != nil || doc.Pending == nil || doc.Pending.BootID == r.a.BootID {
		return
	}
	note := *doc.Pending
	clear := func() {
		_ = r.store.Update(func(doc *receiptDocument) error {
			if doc.Pending != nil && doc.Pending.BootID == note.BootID && doc.Pending.RequestedAt == note.RequestedAt {
				doc.Pending = nil
			}
			return nil
		})
	}
	age := time.Now().UnixMilli() - note.RequestedAt
	if !chatIDPattern.MatchString(note.ChatID) || note.MessageID <= 0 || age < 0 || age > 10*60*1000 {
		clear()
		return
	}
	peer, err := client.InputPeerFromChatID(note.ChatID)
	if err != nil {
		client.Logger().Warn("restart.receipt_unaddressable", slog.String("chat", note.ChatID))
		clear()
		return
	}
	text := "<b>MiBot Lite 重启成功</b>\n服务已就绪"
	if note.Kind == "update" {
		text = "<b>MiBot Lite 更新完成</b>\n新版本已就绪"
	}
	if err := client.EditMessage(ctx, peer, note.MessageID, text, false); err != nil {
		client.Logger().Warn("restart.receipt_failed", slog.String("error", err.Error()))
	}
	clear()
}
