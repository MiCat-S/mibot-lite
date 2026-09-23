// Package alias 实现 .alias：给命令起别名。
package alias

import (
	"context"
	"sort"
	"strings"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// aliasDocument 对应 data/alias.json。
type aliasDocument struct {
	Aliases map[string]string `json:"aliases"`
}

func aliasHelp(prefix string) string {
	p := command.Escape(prefix)
	return "🔀 <b>命令别名</b>\n\n给命令起个顺手的名字，可以连参数一起。\n\n" +
		"• <code>" + p + "alias</code> 列出所有别名\n" +
		"• <code>" + p + "alias set 别名 原命令 [参数]</code> 添加或修改\n" +
		"• <code>" + p + "alias del 别名</code> 删除\n\n" +
		"<b>例子</b>\n" +
		"<code>" + p + "alias set 测速 speedtest 48463</code>\n之后发 <code>" + p + "测速</code> 就等于 <code>" + p + "speedtest 48463</code>。\n\n" +
		"<code>" + p + "alias set 译 gt</code>\n之后 <code>" + p + "译 你好</code> 等于 <code>" + p + "gt 你好</code>，别名后面的内容原样带过去，换行也保留。\n\n" +
		"<b>规则</b>\n" +
		"• 原命令写名字就行，不用带前缀。\n" +
		"• 别名可以是中文，也可以是几个词。\n" +
		"• 不能给别名再起别名；和现有命令同名的单字别名不会生效，所以不让设。\n" +
		"• 同一条原命令只留一个别名，设新的会替换旧的。"
}

// splitAlias 找出别名在哪里结束、命令从哪里开始：从第二个词起，第一个
// 是命令名的词就是分界。这样别名可以由几个词组成，后面的命令也能带上
// 自己的参数。
func splitAlias(tokens []string, isCommand func(string) bool) (alias, target string, ok bool) {
	for index := 1; index < len(tokens); index++ {
		if isCommand(tokens[index]) {
			return strings.Join(tokens[:index], " "), strings.Join(tokens[index:], " "), true
		}
	}
	return "", "", false
}

func renderAliases(aliases map[string]string, prefix string) string {
	p := command.Escape(prefix)
	if len(aliases) == 0 {
		return "🔀 还没有别名。\n\n例如 <code>" + p + "alias set 测速 speedtest</code>，之后发 <code>" + p + "测速</code> 就能测速。"
	}
	names := make([]string, 0, len(aliases))
	for name := range aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	lines := []string{"🔀 <b>命令别名</b>", ""}
	for _, name := range names {
		lines = append(lines, command.Code(prefix+name)+" → "+command.Code(prefix+aliases[name]))
	}
	lines = append(lines, "", "<i>"+command.Escape(prefix+"alias set 别名 原命令")+" 添加，"+
		command.Escape(prefix+"alias del 别名")+" 删除</i>")
	return strings.Join(lines, "\n")
}

// Register 注册 .alias，并把保存的别名表加载进注册表。
func Register(a *app.App) {
	saved := kit.NewStore(a, "alias.json", func() aliasDocument { return aliasDocument{Aliases: map[string]string{}} })
	if current, err := saved.Read(); err != nil {
		a.Logger.Warn("alias.load_failed", "error", err.Error())
	} else {
		a.Registry.SetAliases(current.Aliases)
	}
	// publish 把保存好的别名表交给注册表。它在写入成功之后才运行，并且
	// 把表重新读回来，而不是直接用刚构造的那份：写入失败时，正在生效的
	// 别名不能被动到；两个 .alias 命令乱序完成时，也不能让旧的那份表
	// 盖掉新的。
	publish := func() {
		if current, err := saved.Read(); err == nil {
			a.Registry.SetAliases(current.Aliases)
		}
	}
	isCommand := func(name string) bool {
		_, ok := a.Registry.Lookup(name)
		return ok
	}

	handle := func(ctx context.Context, inv *command.Invocation) error {
		switch strings.ToLower(inv.Arg(0)) {
		case "", "ls", "list":
			return inv.Edit(ctx, renderAliases(a.Registry.Aliases(), inv.Prefix))
		case "help", "h":
			return inv.Edit(ctx, aliasHelp(inv.Prefix))
		case "del", "rm", "delete":
			name := inv.Rest(1)
			if name == "" {
				return inv.EditText(ctx, "用法："+inv.Prefix+"alias del 别名")
			}
			removed := false
			if err := saved.Update(func(document *aliasDocument) error {
				_, removed = document.Aliases[name]
				delete(document.Aliases, name)
				return nil
			}); err != nil {
				return err
			}
			publish()
			if !removed {
				return inv.EditText(ctx, "没有叫 "+name+" 的别名，"+inv.Prefix+"alias 可以看全部")
			}
			return inv.Edit(ctx, "✅ 已删除别名 "+command.Code(inv.Prefix+name))
		case "set", "add":
		default:
			return inv.Edit(ctx, aliasHelp(inv.Prefix))
		}

		tokens := inv.Args[1:]
		if len(tokens) < 2 {
			return inv.EditText(ctx, "用法："+inv.Prefix+"alias set 别名 原命令 [参数]")
		}
		name, target, ok := splitAlias(tokens, isCommand)
		if !ok {
			hint := "后面没有找到存在的命令"
			if strings.HasPrefix(tokens[len(tokens)-1], inv.Prefix) || strings.HasPrefix(tokens[1], inv.Prefix) {
				hint += "；原命令不用带前缀"
			}
			return inv.EditText(ctx, "❌ "+hint+"。例如 "+inv.Prefix+"alias set 测速 speedtest")
		}
		if isCommand(name) {
			return inv.EditText(ctx, "❌ "+name+" 本身就是一个命令，同名的别名永远不会生效")
		}
		first := strings.Fields(target)[0]
		if _, aliased := a.Registry.Aliases()[first]; aliased && !isCommand(first) {
			return inv.EditText(ctx, "❌ "+first+" 是别名，不能再给别名起别名")
		}

		replaced := ""
		if err := saved.Update(func(document *aliasDocument) error {
			if document.Aliases == nil {
				document.Aliases = map[string]string{}
			}
			// 每条展开内容只对应一个别名，和 MiBox 的做法一致。
			for other, expansion := range document.Aliases {
				if expansion == target && other != name {
					replaced = other
					delete(document.Aliases, other)
				}
			}
			document.Aliases[name] = target
			return nil
		}); err != nil {
			return err
		}
		publish()
		text := "✅ " + command.Code(inv.Prefix+name) + " → " + command.Code(inv.Prefix+target)
		if replaced != "" {
			text += "\n<i>原来的别名 " + command.Escape(inv.Prefix+replaced) + " 已替换</i>"
		}
		return inv.Edit(ctx, text)
	}
	a.Registry.Register(&command.Command{
		Name: "alias", Description: "给命令起别名", Usage: "[set 别名 原命令|del 别名]",
		Help: aliasHelp, Handle: handle,
	})
}
