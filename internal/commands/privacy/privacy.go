// Package privacy 实现 .privacy：设置发出去的消息里 IP 地址怎么显示，同 MiBox v2。
// 打码本身在连接层（bot.IPRedactor）做，所有命令的输出都经过它。
package privacy

import (
	"context"
	"strconv"
	"strings"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// describe 用一句话说明当前设置。
func describe(policy bot.IPPolicy) string {
	if policy.Mode == "hide" {
		return "完全隐藏"
	}
	return "IPv4 末尾 " + strconv.Itoa(policy.IPv4) + " 段、IPv6 末尾 " + strconv.Itoa(policy.IPv6) + " 段打码"
}

func usage(prefix string) string {
	p := command.Escape(prefix)
	return "<code>" + p + "privacy ip mask 2 4</code> IPv4 遮后 2 段、IPv6 遮后 4 段（IPv4 1–4，IPv6 1–8，IPv6 可省略）\n" +
		"<code>" + p + "privacy ip hide</code> 整个地址换成「[IP已隐藏]」"
}

func help(prefix string) string {
	return "🛡 <b>IP 显示</b>\n\n账号发出和编辑的每条消息，里面的 IP 地址都按这里的设置打码，文件名里的也一样；" +
		"指向 IP 的链接会去掉。默认 IPv4 遮后 2 段、IPv6 遮后 4 段。\n\n" + usage(prefix) + "\n\n" +
		"只影响之后发出的消息，已经发出去的不会改。图片、附件内容不在范围内。替别人代发的命令（.sudo、.sure）不打码，" +
		"那些地址本来就是对方自己打出来的。"
}

// Register 注册 .privacy，并把保存的设置应用到连接层。
func Register(a *app.App) {
	saved := kit.NewStore(a, "privacy.json", func() bot.IPPolicy { return bot.DefaultIPPolicy })
	if current, err := saved.Read(); err == nil && current.Validate() == nil {
		_ = bot.SetIPPolicy(current)
	}
	apply := func(ctx context.Context, inv *command.Invocation, next bot.IPPolicy) error {
		if err := next.Validate(); err != nil {
			return inv.Edit(ctx, "用法：\n"+usage(inv.Prefix))
		}
		if err := saved.Update(func(value *bot.IPPolicy) error { *value = next; return nil }); err != nil {
			return err
		}
		_ = bot.SetIPPolicy(next)
		return inv.EditText(ctx, "✅ IP 显示已更新："+describe(next))
	}
	a.Registry.Register(&command.Command{Name: "privacy", Description: "设置输出里 IP 地址的打码方式", Usage: "[ip mask 2 4|ip hide]", Help: help,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			scope, action := inv.Arg(0), strings.ToLower(inv.Arg(1))
			switch {
			case scope == "":
				return inv.Edit(ctx, "🛡 IP 显示："+command.Escape(describe(bot.CurrentIPPolicy()))+"\n\n"+usage(inv.Prefix))
			case strings.EqualFold(scope, "help") || strings.EqualFold(scope, "h"):
				return inv.Edit(ctx, help(inv.Prefix))
			case !strings.EqualFold(scope, "ip"):
				return inv.Edit(ctx, "用法：\n"+usage(inv.Prefix))
			case action == "hide" && len(inv.Args) == 2:
				next := bot.CurrentIPPolicy()
				next.Mode = "hide"
				return apply(ctx, inv, next)
			case action == "mask" && (len(inv.Args) == 3 || len(inv.Args) == 4):
				next := bot.CurrentIPPolicy()
				next.Mode = "mask"
				var err error
				if next.IPv4, err = strconv.Atoi(inv.Arg(2)); err != nil {
					return inv.Edit(ctx, "用法：\n"+usage(inv.Prefix))
				}
				if inv.Arg(3) != "" {
					if next.IPv6, err = strconv.Atoi(inv.Arg(3)); err != nil {
						return inv.Edit(ctx, "用法：\n"+usage(inv.Prefix))
					}
				}
				return apply(ctx, inv, next)
			}
			return inv.Edit(ctx, "用法：\n"+usage(inv.Prefix))
		}})
}
