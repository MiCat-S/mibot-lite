package commands

import (
	"context"
	"regexp"
	"strings"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
)

var leadingToken = regexp.MustCompile(`^\S+\s*`)

// Gt registers .gt: translation through the ai command's current chat
// model.
func Gt(a *app.App) {
	help := func(prefix string) string {
		p := command.Escape(prefix)
		return "📘 <b>AI 翻译</b>\n\n• <code>" + p + "gt 文本</code> - 翻译为简体中文\n• <code>" + p + "gt en 文本</code> - 翻译为英文\n• 回复消息后使用 <code>" + p + "gt</code> 或 <code>" + p +
			"gt en</code>\n\n使用 ai 的当前聊天 API、模型及超时设置，请先用 <code>" + p + "ai config add</code> 和 <code>" + p + "ai model chat</code> 配置。单次最多 5000 字符，长译文自动分段发送。"
	}
	a.Registry.Register(&command.Command{Name: "gt", Description: "AI 翻译", Usage: "[en] 文本", Help: help, Timeout: 15 * 60 * 1e9,
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
				return inv.EditText(ctx, "❌ 请提供要翻译的文本或回复一条文字消息")
			}
			if utf16Len(text) > 5000 {
				return inv.EditText(ctx, "❌ 文本过长，请保持在5000字符以内")
			}
			if aiShared == nil {
				return inv.EditText(ctx, "❌ AI 组件不可用")
			}
			if err := inv.Edit(ctx, "🔄 <b>AI 翻译中...</b>"); err != nil {
				return err
			}
			translated, err := aiShared.translate(ctx, text, target)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				detail := chatHTMLError(err)
				if detail == "" {
					detail = "请检查 ai 聊天配置、API 可用性及超时设置后重试"
				}
				return inv.Edit(ctx, "❌ AI 翻译失败，"+detail)
			}
			preview := truncateRunes(text, 50)
			suffix := ""
			if len([]rune(text)) > 50 {
				suffix = "..."
			}
			language := "中文"
			if target == "en" {
				language = "英文"
			}
			pages := command.EscapedPages(translated, 3000)
			pages[0] = "🌐 <b>AI 翻译结果</b> (→ " + language + ")\n\n<b>原文:</b>\n<code>" + command.Escape(preview) + suffix + "</code>\n\n<b>译文:</b>\n" + pages[0]
			return sendPages(ctx, inv, pages)
		}})
}
