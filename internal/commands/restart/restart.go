// Package restart 实现 .restart：通过 systemd 重启服务，回来之后报告结果。
package restart

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
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
	// PeerType 和 AccessHash 一起让重启回来的进程直接寻址那条消息，不必先
	// 靠缓存或 updates.json 认出对话。旧格式的回执没有这两个字段。
	PeerType   string `json:"peerType,omitempty"`
	AccessHash int64  `json:"accessHash,omitempty"`
}

type receiptDocument struct {
	Pending *receipt `json:"pending"`
}

// Restarter 向 systemd 提交重启，并留下回执。.update 装好新版本后也靠它重启。
type Restarter struct {
	a       *app.App
	service string
	store   *store.Store[receiptDocument]
	mu      sync.Mutex
	pending bool
}

// Now 提交重启并把命令消息改成 progress；kind 写进回执，重启回来后据此报告。
func (r *Restarter) Now(ctx context.Context, inv *command.Invocation, kind, progress, failure string) error {
	return r.command(ctx, inv, kind, progress, failure)
}

func systemctl() string {
	if _, err := os.Stat("/usr/bin/systemctl"); err == nil {
		return "/usr/bin/systemctl"
	}
	return "systemctl"
}

// Register 注册 .restart 和处理回执的钩子，返回的 Restarter 交给 .update。
func Register(a *app.App) *Restarter {
	r := &Restarter{a: a, service: a.Env.Get("MIBOT_SERVICE", "mibot-lite.service"),
		store: store.New(filepath.Join(a.Root, "restart-receipt.json"), func() receiptDocument { return receiptDocument{} })}
	a.Registry.Register(&command.Command{Name: "restart", Group: command.GroupSystem, Description: "重启 systemd 服务", Help: restartHelp, Handle: func(ctx context.Context, inv *command.Invocation) error {
		// 重启不收参数；带了参数多半是打错了，别真的重启。只带 help 的由派发器显示说明。
		if len(inv.Args) > 0 {
			return kit.Usage(inv.Prefix, "restart（不带参数）重启服务，重启完成后这条消息会改成「重启成功」")
		}
		return r.command(ctx, inv, "restart", "🔄 <b>重启服务</b>\n⏳ 正在提交重启请求…", "服务重启命令执行失败。")
	}})
	a.Registry.AddJob(r.notifyReady)
	return r
}

// ErrPending 表示已经有一个重启请求在等 systemd 处理。
var ErrPending = errors.New("重启请求已提交，请稍候")

// triggerError 是 systemctl 没能提交重启。回执已经撤掉，服务还在跑原来的版本。
type triggerError struct{ err error }

func (e triggerError) Error() string { return "systemctl restart: " + e.err.Error() }
func (e triggerError) Unwrap() error { return e.err }

// Schedule 留下回执再向 systemd 提交重启。回执让重启回来的进程把 chatID 对话里的
// messageID 那条消息改成结果；kind 决定改成什么（restart、update、rollback、auto-update）。
// peer 是那条消息的对话，连同 access hash 一起存进回执；拿不到就传 nil，只存 chatID。
// 没有命令消息的调用方（自动更新）先自己发一条，再把它交给这里。
func (r *Restarter) Schedule(ctx context.Context, peer tg.InputPeerClass, chatID string, messageID int, kind string) error {
	r.mu.Lock()
	if r.pending {
		r.mu.Unlock()
		return ErrPending
	}
	r.pending = true
	r.mu.Unlock()
	release := func() { r.mu.Lock(); r.pending = false; r.mu.Unlock() }

	note := receipt{ChatID: chatID, MessageID: messageID, RequestedAt: time.Now().UnixMilli(), BootID: r.a.BootID, Kind: kind}
	note.PeerType, note.AccessHash = receiptPeer(peer)
	if err := r.store.Update(func(doc *receiptDocument) error { doc.Pending = &note; return nil }); err != nil {
		release()
		return err
	}
	if err := triggerRestart(ctx, r.service); err != nil {
		if interrupted(ctx, err) {
			// 重启已经在进行，本进程马上就退出：回执留着，新进程回来改那条消息。
			return nil
		}
		release()
		kit.Warn(r.a, "restart.receipt_clear_failed", r.store.Update(func(doc *receiptDocument) error { doc.Pending = nil; return nil }))
		return triggerError{err}
	}
	return nil
}

// receiptPeer 把 input peer 折成回执里能存的类型和 access hash。普通群和 self 没有
// hash；认不出的类型（或 nil）返回空，回执只留 chatID。
func receiptPeer(peer tg.InputPeerClass) (string, int64) {
	switch value := peer.(type) {
	case *tg.InputPeerSelf:
		return "self", 0
	case *tg.InputPeerUser:
		return "user", value.AccessHash
	case *tg.InputPeerChat:
		return "chat", 0
	case *tg.InputPeerChannel:
		return "channel", value.AccessHash
	}
	return "", 0
}

// receiptInputPeer 用回执里的 peer 类型和 access hash 直接构造 input peer。旧格式的回执
// 没有这两个字段，或者和 chatID 的标记对不上时返回 false，调用方再退回按 chatID 解析。
func receiptInputPeer(note receipt, selfID int64) (tg.InputPeerClass, bool) {
	peer, ok := bot.PeerFromID(note.ChatID)
	if !ok {
		return nil, false
	}
	switch note.PeerType {
	case "self":
		// 只认本账号自己的对话：回执要是写错了，不能拿 InputPeerSelf 去改收藏夹里同编号的消息。
		if value, ok := peer.(*tg.PeerUser); !ok || value.UserID != selfID {
			return nil, false
		}
		return &tg.InputPeerSelf{}, true
	case "user":
		value, ok := peer.(*tg.PeerUser)
		if !ok || note.AccessHash == 0 {
			return nil, false
		}
		return &tg.InputPeerUser{UserID: value.UserID, AccessHash: note.AccessHash}, true
	case "chat":
		value, ok := peer.(*tg.PeerChat)
		if !ok {
			return nil, false
		}
		return &tg.InputPeerChat{ChatID: value.ChatID}, true
	case "channel":
		value, ok := peer.(*tg.PeerChannel)
		if !ok || note.AccessHash == 0 {
			return nil, false
		}
		return &tg.InputPeerChannel{ChannelID: value.ChannelID, AccessHash: note.AccessHash}, true
	}
	return nil, false
}

// errTriggerTimeout 是 systemctl 5 秒没返回：重启请求多半没提交上。
var errTriggerTimeout = errors.New("systemctl 5 秒没有返回")

// triggerRestart 让 systemd 重启 service，不等它完成。测试里换掉。
var triggerRestart = func(ctx context.Context, service string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := exec.CommandContext(ctx, systemctl(), "--no-block", "restart", service).Run()
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", errTriggerTimeout, err)
	}
	return err
}

// interrupted 判断 systemctl 是不是被这次重启本身打断的。
//
// systemd 一收到重启请求就开始停本服务：给本进程发 SIGTERM，连同同一个 cgroup 里还没退出
// 的 systemctl 一起结束；本进程退出时也会取消正在跑的命令的 ctx。两种情况下 Run 都返回
// 错误，可重启已经在进行。以前把这当成失败撤掉了回执，结果重启回来没人改那条
// 「正在重启…」的消息。真的失败是 systemctl 非零退出，或者 5 秒没返回。
func interrupted(ctx context.Context, err error) bool {
	if errors.Is(err, errTriggerTimeout) {
		return false
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return true
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return true
		}
	}
	return false
}

// Status 是 systemd 眼里这个服务的状态，重启没提交成功时给人看。
func (r *Restarter) Status(ctx context.Context) string {
	return r.status(ctx) + "\n" + command.Escape(ownerHint())
}

func (r *Restarter) command(ctx context.Context, inv *command.Invocation, kind, progress, failure string) error {
	if err := inv.Edit(ctx, progress); err != nil {
		return err
	}
	peer, _ := inv.Client.InputPeer(inv.Message.Peer)
	err := r.Schedule(ctx, peer, inv.Message.ChatID, inv.Message.ID, kind)
	var failed triggerError
	switch {
	case errors.Is(err, ErrPending):
		return inv.EditText(ctx, ErrPending.Error())
	case errors.As(err, &failed):
		return inv.Edit(ctx, command.Escape(failure)+"\n状态：\n"+r.Status(ctx)+
			"\n\n可执行 "+command.Code("systemctl status "+r.service+" --no-pager")+" 查看详情。")
	}
	return err
}

func (r *Restarter) status(ctx context.Context) string {
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
func (r *Restarter) notifyReady(ctx context.Context, client *bot.Client) {
	doc, err := r.store.Read()
	if err != nil || doc.Pending == nil || doc.Pending.BootID == r.a.BootID {
		return
	}
	note := *doc.Pending
	forget := func() {
		kit.Warn(r.a, "restart.receipt_clear_failed", r.store.Update(func(doc *receiptDocument) error {
			if doc.Pending != nil && doc.Pending.BootID == note.BootID && doc.Pending.RequestedAt == note.RequestedAt {
				doc.Pending = nil
			}
			return nil
		}))
	}
	age := time.Now().UnixMilli() - note.RequestedAt
	if !kit.IsNumericID(note.ChatID) || note.MessageID <= 0 || age < 0 || age > 10*60*1000 {
		forget()
		return
	}
	peer, ok := receiptInputPeer(note, client.SelfID())
	if !ok {
		var err error
		peer, err = client.InputPeerFromChatID(note.ChatID)
		if err != nil {
			client.Logger().Warn("restart.receipt_unaddressable", slog.String("chat", note.ChatID))
			forget()
			return
		}
	}
	text := "🔄 <b>重启服务</b>\n✅ 重启成功，服务已就绪"
	switch note.Kind {
	case "update":
		text = "⬆️ <b>程序更新</b>\n✅ 更新完成，新版本已就绪"
	case "rollback":
		text = "⬆️ <b>程序更新</b>\n✅ 回滚完成，已换回上一版本"
	case "auto-update":
		text = "⬆️ <b>自动更新</b>\n✅ 已更新到 " + command.Code(kit.Version(r.a))
	}
	if err := client.EditMessage(ctx, peer, note.MessageID, text, false); err != nil {
		client.Logger().Warn("restart.receipt_failed", slog.String("error", err.Error()))
	}
	forget()
}

func restartHelp(prefix string) string {
	return "🔄 <b>重启服务</b>\n\n" + command.Code(prefix+"restart") + " 通过 systemd 重启服务。正在跑的命令会被中断，重启前发的命令重启后也不会补跑；" +
		"重启完成后这条消息会改成「重启成功」。"
}
