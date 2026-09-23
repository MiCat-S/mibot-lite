package commands

import (
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/config"
)

// prefixLimit 是单个前缀最多几个字符。前缀是每条命令都要打的，太长没有意义。
const prefixLimit = 8

func prefixHelp(prefix string) string {
	p := command.Escape(prefix)
	return "🛠 <b>命令前缀</b>\n\n" +
		"• <code>" + p + "prefix</code> 查看当前前缀\n" +
		"• <code>" + p + "prefix set . ！</code> 换成这几个\n" +
		"• <code>" + p + "prefix add ~</code> 追加\n" +
		"• <code>" + p + "prefix del $</code> 删除\n\n" +
		"<b>规则</b>\n" +
		"• 可以同时有几个前缀，至少留一个；重复的只算一次。\n" +
		"• 前缀不能含英文字母、数字、下划线：前缀后面紧跟命令名，没有分隔，含这些字符就分不清前缀到哪里结束。\n" +
		"• 前缀不能含引号，最长 8 个字符。\n" +
		"• 改完立刻生效，并写进 <code>.env</code>，重启后仍然有效；写不进去的话只在本次运行里生效。\n" +
		"• 只看消息的第一行。"
}

// checkPrefix 说明一个前缀为什么不能用，能用就返回空字符串。
func checkPrefix(prefix string) string {
	if len([]rune(prefix)) > prefixLimit {
		return "「" + prefix + "」太长，前缀最多 8 个字符"
	}
	for _, r := range prefix {
		switch {
		case r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'):
			return "「" + prefix + "」含英文字母、数字或下划线，会和后面的命令名连在一起分不开"
		case r == '"' || r == '\'':
			return "「" + prefix + "」含引号，写进 .env 后读回来会变"
		case unicode.IsSpace(r) || unicode.IsControl(r):
			return "前缀里不能有空白或控制字符"
		}
	}
	return ""
}

// nextPrefixes 按 set、add、del 算出新的前缀列表，去重并保持顺序。
func nextPrefixes(action string, current, tokens []string) []string {
	var candidates []string
	switch action {
	case "set":
		candidates = tokens
	case "add":
		candidates = append(append([]string{}, current...), tokens...)
	default:
		for _, prefix := range current {
			if !slices.Contains(tokens, prefix) {
				candidates = append(candidates, prefix)
			}
		}
	}
	var result []string
	for _, prefix := range candidates {
		if !slices.Contains(result, prefix) {
			result = append(result, prefix)
		}
	}
	return result
}

func quotePrefixes(prefixes []string) string {
	quoted := make([]string, len(prefixes))
	for index, prefix := range prefixes {
		quoted[index] = command.Code(prefix)
	}
	return strings.Join(quoted, " • ")
}

// Prefix 注册 .prefix。
func Prefix(a *app.App) {
	// 同时来两条 .prefix 时一条一条改，免得后改完的用旧列表盖掉先改完的。
	var mu sync.Mutex
	handle := func(ctx context.Context, inv *command.Invocation) error {
		mu.Lock()
		defer mu.Unlock()
		// 只看第一行，和 MiBox 一样：后面几行可能是随手带上的说明文字。
		fields := strings.Fields(strings.SplitN(inv.Text, "\n", 2)[0])
		args := fields[min(1, len(fields)):]
		current := a.Registry.Prefixes()
		action := ""
		if len(args) > 0 {
			action = strings.ToLower(args[0])
		}
		switch action {
		case "":
			return inv.Edit(ctx, "🛠 当前前缀："+quotePrefixes(current)+
				"\n<i>"+command.Escape(inv.Prefix+"prefix set . ！")+" 修改，"+command.Escape(inv.Prefix+"prefix help")+" 看说明</i>")
		case "set", "add", "del":
		default:
			return inv.Edit(ctx, prefixHelp(inv.Prefix))
		}
		tokens := args[1:]
		if len(tokens) == 0 {
			return inv.EditText(ctx, "❌ "+action+" 后面要跟前缀，例如 "+inv.Prefix+"prefix "+action+" ！")
		}
		if action != "del" {
			for _, token := range tokens {
				if problem := checkPrefix(token); problem != "" {
					return inv.EditText(ctx, "❌ "+problem)
				}
			}
		}
		next := nextPrefixes(action, current, tokens)
		if len(next) == 0 {
			return inv.EditText(ctx, "❌ 至少要保留一个前缀")
		}
		a.Registry.SetPrefixes(next)

		note := "已写入 .env，重启后仍然有效"
		if err := config.SetEnv(a.Root, "MIBOT_PREFIX", strings.Join(next, " ")); err != nil {
			inv.Log.Error("prefix.persist_failed", "error", err.Error())
			note = "⚠️ 写入 .env 失败，只在本次运行里生效"
		} else if value, set := os.LookupEnv("MIBOT_PREFIX"); set && value != "" {
			// 进程环境变量里的 MIBOT_* 会盖过 .env，重启后生效的会是它。
			note = "⚠️ 已写入 .env，但服务的环境变量里也设了 MIBOT_PREFIX，重启后会以那个为准"
		}
		return inv.Edit(ctx, "✅ 前缀已改为："+quotePrefixes(next)+"\n<i>"+command.Escape(note)+"</i>\n\n以后这样用："+
			command.Code(next[0]+"help"))
	}
	a.Registry.Register(&command.Command{
		Name: "prefix", Description: "查看或修改命令前缀", Usage: "[set|add|del 前缀…]",
		Help: prefixHelp, Handle: handle,
	})
}
