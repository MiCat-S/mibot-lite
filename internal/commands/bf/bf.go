// Package bf 实现 .bf：把配置打包发到收藏夹，重装后用它恢复。打包和恢复本身在 internal/backup。
package bf

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/backup"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// installURL 是恢复说明里让人运行的地址。它直接写在备份文件自己的说明
// 文字里，因为真正用到它的时候，人在一台全新的机器上，手边没有别的线索。
const installURL = "https://raw.githubusercontent.com/MiCat-S/mibot-lite/main/scripts/install.sh"

// captionFileLimit 让文件列表不超出 Telegram 说明文字的 1024 字符上限；
// 再多的话，给出总数就够了。
const captionFileLimit = 12

func backupCaption(version string, names []string, size int) string {
	listed := names
	more := ""
	if len(listed) > captionFileLimit {
		listed, more = listed[:captionFileLimit], fmt.Sprintf(" 等 %d 个", len(names))
	}
	quoted := make([]string, len(listed))
	for index, name := range listed {
		quoted[index] = command.Escape(name)
	}
	return "📦 <b>mibot-lite 配置备份</b> · " + command.Escape(version) + "\n" +
		strconv.Itoa(len(names)) + " 个文件，" + command.Escape(kit.FormatBytes(size)) + "：" +
		strings.Join(quoted, "、") + more + "\n\n" +
		"⚠️ <b>这个文件就是你的账号</b>：里面有登录会话和各命令的 API 密钥。不要转发给任何人。\n\n" +
		"<b>在新机器上恢复</b>\n" +
		"<code>bash &lt;(curl -fsSL " + installURL + ") --restore 备份文件路径</code>\n" +
		"恢复前先停掉旧机器上的服务：同一个账号不能在两处同时在线。"
}

func backupHelp(prefix string) string {
	p := command.Escape(prefix)
	return "📦 <b>配置备份</b>\n\n把这个部署的配置打包，发到本账号的收藏夹。重装系统或换机器时用它恢复，不用重新登录，也不用重新配各命令。\n\n" +
		"• <code>" + p + "bf</code> 打包并发到收藏夹\n\n" +
		"<b>包含</b>\n登录会话（config.json、gotd-session.json）、.env，以及 data/ 下每个命令的配置。\n\n" +
		"<b>不包含</b>\neatgif 素材和测速 CLI 这类缓存，用到时会自己重新下载；也不含程序本身。\n\n" +
		"<b>恢复</b>\n在新机器上把备份文件传上去，然后运行：\n" +
		"<code>bash &lt;(curl -fsSL " + installURL + ") --restore 备份文件路径</code>\n" +
		"已经装好的机器可以用 <code>mibot-lite --restore 文件 --root 部署目录</code>，" +
		"要覆盖已有账号得加 <code>--force</code>，并且先停掉服务。\n\n" +
		"⚠️ 无论在哪个对话里执行，备份都只发到收藏夹，不会出现在当前对话。" +
		"这个文件等同于你的账号，不要转发给别人——要给别人看问题，用 <code>" + p + "log</code>，那个是脱敏的。"
}

// Register 注册 .bf。
func Register(a *app.App) {
	handle := func(ctx context.Context, inv *command.Invocation) error {
		switch strings.ToLower(inv.Arg(0)) {
		case "help", "h":
			return inv.Edit(ctx, backupHelp(inv.Prefix))
		}
		if err := inv.Edit(ctx, "📦 正在打包配置…"); err != nil {
			return err
		}
		archive, names, err := backup.Create(a.Root, a.Version, time.Now())
		if err != nil {
			return inv.EditText(ctx, "❌ 备份失败："+err.Error())
		}
		// 不管命令是在哪个对话里发的，一律发到收藏夹。这个压缩包就等于
		// 账号本身；只因为有人碰巧在群里敲了 .bf 就把它发进群，等于把
		// 账号交给了群里所有人。
		name := "mibot-lite-backup-" + time.Now().Format("20060102-1504") + ".tar.gz"
		if err := inv.Client.SendDocument(ctx, &tg.InputPeerSelf{}, name, "application/gzip", archive,
			backupCaption(a.Version, names, len(archive)), 0); err != nil {
			return err
		}
		inv.Log.Info("backup.sent", "files", len(names), "bytes", len(archive))
		// 在收藏夹以外的对话里，确认消息只说文件去了哪，不提里面有什么。
		return inv.Edit(ctx, "✅ 备份已发到收藏夹（"+strconv.Itoa(len(names))+" 个文件）")
	}
	a.Registry.Register(&command.Command{
		Name: "bf", Description: "备份配置到收藏夹，重装后可恢复",
		Help: backupHelp, Timeout: 2 * time.Minute, Handle: handle,
	})
}
