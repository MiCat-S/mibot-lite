// Package keyword 实现 .keyword：别人在对话里发的消息命中关键词时自动回复，还可以删掉
// 原消息、封禁或禁言发送者。任务按对话保存在 data/keyword.json，格式和 MiBox v2 的
// keyword 插件（assets/keyword/config.json）一样，原样搬过来就能用。
package keyword

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// Register 注册 .keyword，并挂上看别人消息的监听者。
func Register(a *app.App) {
	s := newService(kit.NewStore(a, "keyword.json", defaults), a.Logger)
	a.OnForeign(s.offer)
	a.Registry.Register(&command.Command{Name: "keyword", Group: command.GroupAdmin, Description: "设置关键词自动回复",
		Usage: "[任务|list|rm|alias]", Help: help, FreeText: true, Handle: s.command})
}

func help(prefix string) string {
	p := command.Escape(prefix)
	c := func(text string) string { return "<code>" + p + "keyword" + text + "</code>" }
	return "💬 <b>关键词自动回复</b>\n\n" +
		"别人在对话里发的消息命中关键词时，账号自动回复，还可以删掉原消息、封禁或禁言发送者。" +
		"任务在哪个对话里添加，就只在哪个对话里生效。\n\n" +
		"<b>管理</b>\n" +
		"• " + c(" list") + " 当前对话的任务\n" +
		"• " + c(" list all") + " 所有对话的任务\n" +
		"• " + c(" rm 1,2,3") + " 按编号删除任务\n" +
		"• " + c(" alias") + " 查看当前对话继承了哪个对话\n" +
		"• " + c(" alias -1001234567890") + " 当前对话也用另一个对话的任务（对话 ID 可以用 " + p + "ids 查）\n" +
		"• " + c(" alias rm") + " 取消继承\n\n" +
		"<b>添加任务</b>\n" +
		"各段之间用单独一行 <code>+++</code> 隔开，前两段必填，后面的可以省略或留空：\n" +
		"<pre>" + p + "keyword 关键词\n+++\n回复内容\n+++\n匹配选项\n+++\n动作\n+++\n回复几秒后删除\n+++\n原消息几秒后删除</pre>\n\n" +
		"<b>匹配选项</b>（空格分隔）\n" +
		"• <code>include</code> 消息里包含关键词就算（默认）\n" +
		"• <code>exact</code> 整条消息和关键词一致\n" +
		"• <code>regexp</code> 关键词是正则表达式（JavaScript 语法）\n" +
		"• <code>case</code> 区分大小写\n" +
		"• <code>ignore_forward</code> 不理会转发的消息（为兼容 MiBox 保留；目前转发来的消息本来就不触发）\n\n" +
		"<b>动作</b>（空格分隔）\n" +
		"• <code>reply</code> 回复时引用原消息（默认）\n" +
		"• <code>delete</code> 删除原消息，第六段可以设延迟\n" +
		"• <code>ban3600</code> 封禁发送者 3600 秒\n" +
		"• <code>restrict600</code> 禁言发送者 600 秒\n" +
		"少于 30 秒或超过 366 天的，Telegram 按永久处理。\n\n" +
		"<b>回复里的变量</b>\n" +
		"• <code>$mention</code> 提及发送者\n" +
		"• <code>$code_id</code> 发送者 ID\n" +
		"• <code>$code_name</code> 发送者名字\n" +
		"• <code>$delay_delete</code> 回复几秒后删除\n" +
		"回复按 HTML 发送，可以用 &lt;b&gt;、&lt;i&gt;、&lt;code&gt; 这些标签。\n\n" +
		"<b>示例</b>\n" +
		"<pre>" + p + "keyword 你好\n+++\n欢迎，$mention</pre>\n" +
		"<pre>" + p + "keyword \\d{11}\n+++\n请勿发送手机号码\n+++\nregexp\n+++\ndelete\n+++\n10</pre>\n" +
		"<pre>" + p + "keyword 广告\n+++\n检测到广告，已封禁 $code_name\n+++\n\n+++\ndelete ban3600</pre>\n\n" +
		"<b>注意</b>\n" +
		"• 删除别人的消息、封禁、禁言都要管理员权限\n" +
		"• 只看别人发的消息，自己发的、转发来的都不会触发\n" +
		"• 继承只看一层：当前对话先查继承来的任务，再查自己的\n" +
		"• 任务编号在所有对话里唯一，删掉的编号不再使用"
}

func (s *service) command(ctx context.Context, inv *command.Invocation) error {
	// 正文照原样取：多行格式要保留换行，拆成参数再拼回去就丢了。
	body := strings.TrimLeftFunc(inv.RawAfter(0), unicode.IsSpace)
	// 有 +++ 就是添加任务，即使关键词恰好是 list、rm 这些词。v2 先看第一个词，
	// 结果关键词为 list 的任务加不进去。
	if hasSeparator(body) {
		return s.add(ctx, inv, body)
	}
	switch strings.ToLower(inv.Arg(0)) {
	case "", "h", "help":
		return inv.EditPages(ctx, command.HTMLPages(help(inv.Prefix), command.PageLimit))
	case "list":
		return s.list(ctx, inv, strings.EqualFold(inv.Arg(1), "all"))
	case "rm":
		return s.remove(ctx, inv)
	case "alias":
		return s.alias(ctx, inv)
	}
	return s.add(ctx, inv, body)
}

// add 添加一条任务，归当前对话。编号取 nextId 和现有最大编号加一里大的那个：
// v2 只看 nextId，文件被手改过时会发出重复的编号。
func (s *service) add(ctx context.Context, inv *command.Invocation, body string) error {
	parsed, err := parseTask(body)
	if err != nil {
		if text, ok := kit.IsUserError(err); ok && !hasSeparator(body) {
			return kit.Failf("%s，%skeyword help 看格式", text, inv.Prefix)
		}
		return err
	}
	parsed.ChatID = inv.Message.ChatID
	if err := s.update(func(doc *document) error {
		id := max(doc.NextID, 1)
		for _, existing := range doc.Tasks {
			id = max(id, existing.ID+1)
		}
		parsed.ID = id
		doc.NextID = id + 1
		doc.Tasks = append(doc.Tasks, parsed)
		return nil
	}); err != nil {
		return err
	}
	return inv.Edit(ctx, "✅ 已添加关键词任务 "+command.Code(strconv.Itoa(parsed.ID)))
}

// line 是列表里的一条任务：编号、关键词、回复，和默认不同的设置另起一行。
func line(entry *compiled) string {
	text := command.Code(strconv.Itoa(entry.ID)) + " " + command.Code(entry.Key) + " → " + command.Escape(entry.Response)
	extra := summary(entry.task)
	if entry.Regexp && entry.pattern == nil {
		// 搬过来的正则 regexp2 编译不了，这条任务不会命中，得让人知道。
		extra = strings.TrimPrefix(extra+" · ⚠️ 正则无法使用，不会命中", " · ")
	}
	if extra != "" {
		text += "\n　" + extra
	}
	return text
}

// list 列出当前对话的任务，all 为真时按对话分组列出全部。
func (s *service) list(ctx context.Context, inv *command.Invocation, all bool) error {
	if _, err := s.store.Read(); err != nil {
		return err
	}
	v := s.current()
	here := inv.Message.ChatID
	if !all {
		own := v.chats[here]
		var lines []string
		if len(own) == 0 {
			lines = append(lines, "当前对话没有关键词任务")
		} else {
			lines = append(lines, "💬 <b>当前对话的关键词任务</b>（"+strconv.Itoa(len(own))+" 个）")
			for _, entry := range own {
				lines = append(lines, line(entry))
			}
		}
		if from := v.aliases[here]; from != "" && from != here {
			lines = append(lines, "", "另外继承 "+command.Code(from)+"（"+command.Escape(title(inv.Client, from))+"）的 "+
				strconv.Itoa(len(v.chats[from]))+" 个任务")
		}
		return inv.EditPages(ctx, command.HTMLPages(strings.Join(lines, "\n"), command.PageLimit))
	}
	chats := make([]string, 0, len(v.chats))
	total := 0
	for chat, entries := range v.chats {
		chats = append(chats, chat)
		total += len(entries)
	}
	if total == 0 {
		return inv.Edit(ctx, "还没有任何关键词任务")
	}
	// 按每个对话最早那条任务的编号排，和 v2 按编号列出的顺序大致一样，又能按对话分组。
	sort.Slice(chats, func(i, j int) bool { return v.chats[chats[i]][0].ID < v.chats[chats[j]][0].ID })
	lines := []string{"💬 <b>全部关键词任务</b>（" + strconv.Itoa(total) + " 个）"}
	for _, chat := range chats {
		lines = append(lines, "", "<b>"+command.Escape(title(inv.Client, chat))+"</b> "+command.Code(chat))
		for _, entry := range v.chats[chat] {
			lines = append(lines, line(entry))
		}
	}
	return inv.EditPages(ctx, command.HTMLPages(strings.Join(lines, "\n"), command.PageLimit))
}

// title 是对话的显示名，缓存里没有就是 ID 本身。
func title(client *bot.Client, chatID string) string {
	peer, ok := bot.PeerFromID(chatID)
	if !ok {
		return chatID
	}
	return client.Peers().Title(peer)
}

// parseIDs 读要删除的编号：逗号或空格分隔都行（v2 只认逗号）。
func parseIDs(args []string) ([]int, error) {
	var ids []int
	for _, field := range strings.FieldsFunc(strings.Join(args, " "), func(r rune) bool { return r == ',' || r == '，' || unicode.IsSpace(r) }) {
		id, err := strconv.Atoi(field)
		if err != nil || id < 1 {
			return nil, kit.Failf("任务编号要是正整数，%s 不是", field)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// remove 按编号删除任务。编号在所有对话里唯一，所以不限于当前对话（同 v2）。
func (s *service) remove(ctx context.Context, inv *command.Invocation) error {
	ids, err := parseIDs(inv.Args[1:])
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return kit.Usage(inv.Prefix, "keyword rm 编号[,编号…]")
	}
	removed := map[int]bool{}
	if err := s.update(func(doc *document) error {
		kept := doc.Tasks[:0]
		for _, t := range doc.Tasks {
			if containsID(ids, t.ID) {
				removed[t.ID] = true
				continue
			}
			kept = append(kept, t)
		}
		doc.Tasks = kept
		return nil
	}); err != nil {
		return err
	}
	var missing []string
	for _, id := range ids {
		if !removed[id] {
			missing = append(missing, strconv.Itoa(id))
		}
	}
	if len(removed) == 0 {
		return kit.Failf("没有编号为 %s 的任务，%skeyword list all 可以看全部编号", strings.Join(missing, "、"), inv.Prefix)
	}
	text := "✅ 已删除 " + strconv.Itoa(len(removed)) + " 个任务"
	if len(missing) > 0 {
		text += "（没有编号 " + command.Escape(strings.Join(missing, "、")) + "）"
	}
	return inv.Edit(ctx, text)
}

func containsID(ids []int, id int) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

// alias 查看、设置或取消当前对话的继承。v2 什么都收，写错了就是悄悄不生效；
// 这里要求是对话 ID，也不许继承自己（那样每条任务会触发两次）。
func (s *service) alias(ctx context.Context, inv *command.Invocation) error {
	here := inv.Message.ChatID
	target := inv.Arg(1)
	switch {
	case target == "":
		current, err := s.store.Read()
		if err != nil {
			return err
		}
		from := current.Aliases[here]
		if from == "" {
			return inv.Edit(ctx, "当前对话没有继承别的对话")
		}
		return inv.Edit(ctx, "当前对话继承自 "+command.Code(from)+"（"+command.Escape(title(inv.Client, from))+"）")
	case strings.EqualFold(target, "rm"):
		existed := false
		if err := s.update(func(doc *document) error {
			_, existed = doc.Aliases[here]
			delete(doc.Aliases, here)
			return nil
		}); err != nil {
			return err
		}
		if !existed {
			return kit.Fail("当前对话没有继承别的对话")
		}
		return inv.Edit(ctx, "✅ 已取消继承")
	case !kit.IsNumericID(target):
		return kit.Failf("要写对话 ID，如 -1001234567890，%sids 可以查", inv.Prefix)
	case target == here:
		return kit.Fail("不能继承当前对话自己")
	}
	count := 0
	if err := s.update(func(doc *document) error {
		if doc.Aliases == nil {
			doc.Aliases = map[string]string{}
		}
		doc.Aliases[here] = target
		for _, t := range doc.Tasks {
			if t.ChatID == target {
				count++
			}
		}
		return nil
	}); err != nil {
		return err
	}
	text := "✅ 已继承 " + command.Code(target) + "（" + command.Escape(title(inv.Client, target)) + "）的任务"
	if count == 0 {
		text += "\n⚠️ 那个对话现在还没有任务"
	}
	return inv.Edit(ctx, text)
}
