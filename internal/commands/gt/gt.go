// Package gt 实现 .gt：借 .ai 的模型翻译。
package gt

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/ai"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

var leadingToken = regexp.MustCompile(`^\S+\s*`)

// Register 注册 .gt：借 ai 命令当前的对话模型来翻译。
func Register(a *app.App) {
	help := func(prefix string) string {
		p := command.Escape(prefix)
		return "📘 <b>AI 翻译</b>\n\n• <code>" + p + "gt 文本</code> 翻译为简体中文\n• <code>" + p + "gt en 文本</code> 翻译为英文\n• 回复消息后使用 <code>" + p + "gt</code> 或 <code>" + p +
			"gt en</code>\n\n使用 ai 的当前聊天 API、模型及超时设置，请先用 <code>" + p + "ai config add</code> 和 <code>" + p + "ai model chat</code> 配置。单次最多 5000 字符，长译文自动分段发送。"
	}
	a.Registry.Register(&command.Command{Name: "gt", Group: command.GroupTools, Description: "用 AI 翻译文本", Usage: "[en] 文本", Help: help, Timeout: 15 * time.Minute,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			text := leadingToken.ReplaceAllString(inv.Text, "")
			first := strings.ToLower(regexp.MustCompile(`^\S+`).FindString(text))
			if first == "help" || first == "h" {
				return inv.Edit(ctx, help(inv.Prefix))
			}
			target := "zh-CN"
			if first == "en" {
				target = "en"
				text = leadingToken.ReplaceAllString(text, "")
			}
			if strings.TrimSpace(text) == "" {
				reply, err := inv.Client.GetReply(ctx, inv.Message)
				if err != nil {
					return err
				}
				if reply != nil {
					text = reply.Text
				}
			}
			if strings.TrimSpace(text) == "" {
				return kit.Fail("请提供要翻译的文本，或回复一条文字消息")
			}
			if kit.UTF16Len(text) > 5000 {
				return kit.Fail("文本过长，请保持在 5000 字以内")
			}
			if !ai.Available() {
				return kit.Fail("AI 组件不可用")
			}
			if err := inv.EditText(ctx, kit.Working("正在翻译")); err != nil {
				return err
			}
			translated, err := ai.Translate(ctx, text, target)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return kit.FailWith("AI 翻译失败", err)
			}
			preview := kit.TruncateRunes(text, 50)
			suffix := ""
			if len([]rune(text)) > 50 {
				suffix = "…"
			}
			language := "中文"
			if target == "en" {
				language = "英文"
			}
			pages := command.EscapedPages(translated, command.PageLimit)
			pages[0] = "📘 <b>AI 翻译结果</b>（→ " + language + "）\n\n<b>原文</b>\n<code>" + command.Escape(preview) + suffix + "</code>\n\n<b>译文</b>\n" + pages[0]
			return kit.SendPages(ctx, inv, pages)
		}})
}
