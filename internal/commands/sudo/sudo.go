// Package sudo 实现 .sudo 和 .sure：让名单里的人借用账号执行命令。
package sudo

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// 借用账号（.sudo 和 .sure）的做法照 MiBox：名单里的人发了符合条件的消息，
// 账号就以自己的身份在同一个对话里把它（或者重定向后的命令）发出去，
// 回复同一条消息，再当作自己的命令执行。
//
// 和 MiBox 不同的是能借出去的范围。MiBox 让名单里的人执行任何命令，包括
// .sudo add 本身——被授权的人可以再去授权别人。这里改成白名单：只有下面表里
// 列出的命令能借出去，其余只限本人；以后新加的命令默认也不能借，要借得在表里写明。

// delegable 是能借出去的命令，值是其中只限本人的子命令（第一个参数）。
//
// 不在表里的，按类别：
//   - 管授权的：sudo、sure。
//   - 删消息的：dme、da。
//   - 改账号或行为的：acn（改昵称）、prefix、alias。
//   - 导出东西的：bf（备份里有登录会话）、log、save（能把账号看得到的私密内容转给任何人）。
//   - 管进程的：restart、update。
//   - 跨群的：sb、unsb（在账号管理的所有群里封禁），借用应该只作用于当前这个群。
//   - 会暴露主机信息的：sysinfo（显示主机名）。
var delegable = map[string][]string{
	"ping": nil, "help": nil, "h": nil, "version": nil, "ver": nil, "status": nil, "memory": nil,
	"calc": nil, "rate": nil, "whois": nil, "tr": nil, "gt": nil, "ip": nil, "bin": nil, "ids": nil, "dc": nil,
	"re":        nil,
	"speedtest": {"set", "clear", "auto", "自动"},
	"st":        {"set", "clear", "auto", "自动"},
	"yvlu":      {"config"},
	"eatgif":    {"clear"},
	"ai":        {"config", "model", "reasoning", "service", "prompt", "collapse", "timeout", "telegraph"},
	"sum":       {"config", "list", "run", "del", "disable", "enable", "add"},
	"ban":       nil, "unban": nil, "kick": nil, "mute": nil, "unmute": nil,
}

// delegationAllowed 判断一条命令能不能借出去。按别名展开后的真实命令判断。
func delegationAllowed(route command.Route) bool {
	ownerOnly, ok := delegable[route.Command]
	if !ok {
		return false
	}
	return len(route.Args) == 0 || !slices.Contains(ownerOnly, strings.ToLower(route.Args[0]))
}

// Delegable 按字母顺序列出能借出去的命令，包括简写。
func Delegable() []string {
	names := make([]string, 0, len(delegable))
	for name := range delegable {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// delegableList 按字母顺序列出能借出去的命令，写进帮助里，和上面的表保持一致。
func delegableList(prefix string) string {
	names := make([]string, 0, len(delegable))
	for name := range delegable {
		if name != "h" && name != "ver" && name != "st" {
			names = append(names, prefix+name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

// delegateEntry 是名单里的一个用户或对话，Name 只用于显示。
type delegateEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// sureRule 是一条消息规则。Msg 以 _command: 开头时按命令前缀匹配，否则要求整条消息一致。
type sureRule struct {
	ID       int    `json:"id"`
	Msg      string `json:"msg"`
	Redirect string `json:"redirect,omitempty"`
}

// delegateDocument 是 data/sudo.json 和 data/sure.json 的格式；sudo 用不到 Messages。
type delegateDocument struct {
	Users    []delegateEntry `json:"users"`
	Chats    []delegateEntry `json:"chats"`
	Messages []sureRule      `json:"messages,omitempty"`
	NextID   int             `json:"next_id,omitempty"`
}

func (d delegateDocument) hasUser(id string) bool {
	for _, user := range d.Users {
		if user.ID == id {
			return true
		}
	}
	return false
}

// allowsChat 没设对话名单时处处可用，设了就只在名单里的对话可用。
func (d delegateDocument) allowsChat(id string) bool {
	if len(d.Chats) == 0 {
		return true
	}
	for _, chat := range d.Chats {
		if chat.ID == id {
			return true
		}
	}
	return false
}

// match 找到第一条匹配的规则，返回要以账号身份发出去的文字。
func (d delegateDocument) match(text string) (string, bool) {
	for _, rule := range d.Messages {
		if prefix, ok := strings.CutPrefix(rule.Msg, "_command:"); ok {
			rest, found := strings.CutPrefix(text, prefix)
			if !found || (rest != "" && !strings.HasPrefix(rest, " ")) {
				continue
			}
			if rule.Redirect != "" {
				return rule.Redirect + rest, true
			}
			return text, true
		}
		if rule.Msg == text {
			if rule.Redirect != "" {
				return rule.Redirect, true
			}
			return text, true
		}
	}
	return "", false
}

// senderUserID 是发消息的用户 ID；以频道身份发的、匿名管理员发的都不算，返回空。
func senderUserID(message *bot.Message) string {
	if user, ok := message.Sender.(*tg.PeerUser); ok {
		return strconv.FormatInt(user.UserID, 10)
	}
	return ""
}

// relay 以账号身份在触发消息所在的对话里发出 text，回复同一个目标、留在同一个话题里；
// text 是命令的话接着执行。能不能借出去在发之前就判断，不许的命令不会以账号身份出现在群里。
func relay(ctx context.Context, a *app.App, client *bot.Client, trigger *bot.Message, text string, entities []tg.MessageEntityClass) {
	logger := client.Logger().With("from", senderUserID(trigger), "chat", trigger.ChatID)
	route, isCommand := a.Registry.Parse(text)
	if isCommand {
		if _, known := a.Registry.Lookup(route.Command); !known {
			isCommand = false
		}
	}
	peer, err := client.InputPeer(trigger.Peer)
	if err != nil {
		logger.Warn("delegate.unaddressable", "error", err.Error())
		return
	}
	if isCommand && !delegationAllowed(route) {
		logger.Info("delegate.refused", "command", route.Command)
		_, _ = client.SendRaw(ctx, peer, "⛔ "+route.Prefix+route.Command+" 只有账号本人能用", nil, trigger.ID, trigger.TopicID)
		return
	}
	id, err := client.SendRaw(ctx, peer, text, entities, trigger.ReplyToID, trigger.TopicID)
	if err != nil {
		logger.Warn("delegate.send_failed", "error", err.Error())
		return
	}
	logger.Info("delegate.relayed", "command", route.Command, "message", id)
	if !isCommand {
		return
	}
	// 读回刚发的那条，当作账号自己的命令执行；处理函数编辑的就是这一条。
	sent, err := client.GetMessages(ctx, peer, []int{id})
	if err != nil || len(sent) == 0 {
		logger.Warn("delegate.read_back_failed")
		return
	}
	envelope, ok := bot.Envelope(sent[0], client.SelfID(), false, client.Peers())
	if !ok {
		return
	}
	a.Registry.DispatchFor(ctx, client, envelope, trigger, delegationAllowed)
}

// resolveUser 找出要加进名单的用户：参数是数字 ID 或 @用户名，没有参数就取被回复消息的发送者。
func resolveUser(ctx context.Context, inv *command.Invocation, argument string) (delegateEntry, error) {
	if argument != "" {
		if id, err := strconv.ParseInt(argument, 10, 64); err == nil {
			if id <= 0 {
				return delegateEntry{}, fmt.Errorf("%s 不是用户 ID", argument)
			}
			return delegateEntry{ID: argument, Name: inv.Client.Peers().Title(&tg.PeerUser{UserID: id})}, nil
		}
		peer, err := inv.Client.ResolveUsername(ctx, strings.TrimPrefix(argument, "@"))
		if err != nil {
			return delegateEntry{}, fmt.Errorf("找不到 %s", argument)
		}
		user, ok := peer.(*tg.InputPeerUser)
		if !ok {
			return delegateEntry{}, fmt.Errorf("%s 不是用户", argument)
		}
		return delegateEntry{ID: strconv.FormatInt(user.UserID, 10), Name: "@" + strings.TrimPrefix(argument, "@")}, nil
	}
	if inv.Message.ReplyToID == 0 {
		return delegateEntry{}, fmt.Errorf("回复对方的消息，或者带上数字 ID、@用户名")
	}
	reply, err := inv.Client.GetReply(ctx, inv.Message)
	if err != nil || reply == nil {
		return delegateEntry{}, fmt.Errorf("读不到被回复的消息")
	}
	id := senderUserID(reply)
	if id == "" {
		return delegateEntry{}, fmt.Errorf("被回复的消息不是用户发的（频道身份或匿名管理员不能授权）")
	}
	return delegateEntry{ID: id, Name: inv.Client.Peers().Title(reply.Sender)}, nil
}

// resolveChat 找出要加进名单的对话：参数是对话 ID 或 @名称，没有参数就是当前对话。
func resolveChat(ctx context.Context, inv *command.Invocation, argument string) (delegateEntry, error) {
	if argument == "" {
		return delegateEntry{ID: inv.Message.ChatID, Name: inv.Client.Peers().Title(inv.Message.Peer)}, nil
	}
	if peer, ok := bot.PeerFromID(argument); ok {
		return delegateEntry{ID: argument, Name: inv.Client.Peers().Title(peer)}, nil
	}
	peer, err := inv.Client.ResolveTarget(ctx, argument)
	if err != nil {
		return delegateEntry{}, fmt.Errorf("找不到 %s", argument)
	}
	var id string
	switch value := peer.(type) {
	case *tg.InputPeerChannel:
		id = bot.PeerID(&tg.PeerChannel{ChannelID: value.ChannelID})
	case *tg.InputPeerChat:
		id = bot.PeerID(&tg.PeerChat{ChatID: value.ChatID})
	case *tg.InputPeerUser:
		id = bot.PeerID(&tg.PeerUser{UserID: value.UserID})
	default:
		return delegateEntry{}, fmt.Errorf("找不到 %s", argument)
	}
	return delegateEntry{ID: id, Name: argument}, nil
}

func renderEntries(title string, entries []delegateEntry, empty string) string {
	if len(entries) == 0 {
		return empty
	}
	lines := []string{"<b>" + command.Escape(title) + "</b>"}
	for _, entry := range entries {
		name := entry.Name
		if name == "" || name == entry.ID {
			name = "（未知名称）"
		}
		lines = append(lines, "• "+command.Escape(name)+" "+command.Code(entry.ID))
	}
	return strings.Join(lines, "\n")
}

func addEntry(entries []delegateEntry, entry delegateEntry) ([]delegateEntry, bool) {
	for index, existing := range entries {
		if existing.ID == entry.ID {
			entries[index] = entry
			return entries, false
		}
	}
	return append(entries, entry), true
}

func removeEntry(entries []delegateEntry, id string) ([]delegateEntry, bool) {
	for index, existing := range entries {
		if existing.ID == id {
			return append(entries[:index:index], entries[index+1:]...), true
		}
	}
	return entries, false
}

// manageLists 处理 sudo 和 sure 共有的用户、对话名单子命令。第一个返回值表示参数是不是这些子命令。
func manageLists(ctx context.Context, inv *command.Invocation, saved *store.Store[delegateDocument], name string) (bool, error) {
	action := strings.ToLower(inv.Arg(0))
	target := inv.Arg(1)
	if action == "chat" {
		sub := strings.ToLower(inv.Arg(1))
		target = inv.Arg(2)
		switch sub {
		case "ls", "list", "":
			current, err := saved.Read()
			if err != nil {
				return true, err
			}
			return true, inv.Edit(ctx, renderEntries("对话名单", current.Chats, "⚠️ 没有设对话名单，所有对话里都能用"))
		case "add", "del":
			chat, err := resolveChat(ctx, inv, target)
			if err != nil {
				return true, inv.EditText(ctx, "❌ "+err.Error())
			}
			changed := false
			if err := saved.Update(func(document *delegateDocument) error {
				if sub == "add" {
					document.Chats, changed = addEntry(document.Chats, chat)
				} else {
					document.Chats, changed = removeEntry(document.Chats, chat.ID)
				}
				return nil
			}); err != nil {
				return true, err
			}
			if sub == "del" && !changed {
				return true, inv.EditText(ctx, "对话名单里没有 "+chat.ID)
			}
			verb := map[string]string{"add": "已加入", "del": "已移出"}[sub]
			return true, inv.Edit(ctx, "✅ "+verb+" "+command.Escape(name)+" 对话名单："+command.Escape(chat.Name)+" "+command.Code(chat.ID))
		}
		return false, nil
	}
	switch action {
	case "ls", "list":
		current, err := saved.Read()
		if err != nil {
			return true, err
		}
		return true, inv.Edit(ctx, renderEntries(name+" 用户名单", current.Users, "当前没有任何用户"))
	case "add", "del":
		user, err := resolveUser(ctx, inv, target)
		if err != nil {
			return true, inv.EditText(ctx, "❌ "+err.Error())
		}
		if user.ID == strconv.FormatInt(inv.Client.SelfID(), 10) {
			return true, inv.EditText(ctx, "❌ 不能把账号自己加进名单")
		}
		changed := false
		if err := saved.Update(func(document *delegateDocument) error {
			if action == "add" {
				document.Users, changed = addEntry(document.Users, user)
			} else {
				document.Users, changed = removeEntry(document.Users, user.ID)
			}
			return nil
		}); err != nil {
			return true, err
		}
		if action == "del" && !changed {
			return true, inv.EditText(ctx, name+" 名单里没有 "+user.ID)
		}
		verb := map[string]string{"add": "已授权", "del": "已取消授权"}[action]
		return true, inv.Edit(ctx, "✅ "+verb+"："+command.Escape(user.Name)+" "+command.Code(user.ID))
	}
	return false, nil
}

func sudoHelp(prefix string) string {
	p := command.Escape(prefix)
	return "🔐 <b>sudo：让别人用你的账号执行命令</b>\n\n" +
		"名单里的人发一条命令（用你的前缀），账号就以你的身份在同一个对话里发出这条命令并执行，回复同一个目标。\n\n" +
		"• <code>" + p + "sudo add</code> 回复对方的消息，或带上数字 ID、@用户名\n" +
		"• <code>" + p + "sudo del</code> 同上\n" +
		"• <code>" + p + "sudo ls</code> 用户名单\n" +
		"• <code>" + p + "sudo chat add</code> 把当前对话（或带上 ID、@名称）加进对话名单\n" +
		"• <code>" + p + "sudo chat del</code> / <code>chat ls</code>\n\n" +
		"⚠️ 没设对话名单时，名单里的人在所有有你的对话里都能用。\n\n" +
		"<b>能借出去的命令</b>\n" + command.Escape(delegableList(prefix)) + "\n" +
		"其中改设置的子命令（如 ai config、sum config、speedtest set）不行。\n\n" +
		"<b>只限你本人</b>\n授权管理、删消息、改昵称或前缀别名、备份与日志、转存消息、重启更新、跨所有群的封禁、看主机信息。" +
		"名单里的人发这些命令，账号只会回一句没有权限。"
}

func sureHelp(prefix string) string {
	p := command.Escape(prefix)
	return "✅ <b>sure：让别人触发指定的消息或命令</b>\n\n" +
		"比 sudo 窄：名单里的人发的消息要和规则对上，账号才会以你的身份发出去；规则可以重定向成别的命令。\n\n" +
		"<b>用户和对话</b>\n" +
		"• <code>" + p + "sure add</code> / <code>del</code> / <code>ls</code> 和 sudo 一样\n" +
		"• <code>" + p + "sure chat add</code> / <code>del</code> / <code>ls</code> 对话名单，没设就处处可用\n\n" +
		"<b>消息规则（至少要有一条才会生效）</b>\n" +
		"• <code>" + p + "sure msg add 消息原文</code> 整条消息一致才算\n" +
		"• <code>" + p + "sure msg add _command:/sb</code> 以 /sb 开头的都算，如 /sb 和 /sb 123\n" +
		"• <code>" + p + "sure msg redirect 编号 " + p + "ban</code> 重定向：/sb 123 会变成 " + p + "ban 123；不写目标就是清除重定向\n" +
		"• <code>" + p + "sure msg del 编号</code> / <code>msg ls</code>\n\n" +
		"<b>典型用法</b>\n规则 <code>_command:/sb</code> 重定向到 <code>" + p + "ban</code>，再把群成员加进用户名单。他们回复某人发 /sb，就会以你的身份执行 <code>" + p + "ban</code>。" +
		"触发的那条消息 5 秒后删掉（需要管理员权限）。\n\n" +
		"⚠️ 重定向出来的命令和 sudo 一样受限，只限本人的命令不会被执行。"
}

// manageRules 处理 .sure msg 的子命令。
func manageRules(ctx context.Context, inv *command.Invocation, saved *store.Store[delegateDocument]) error {
	action := strings.ToLower(inv.Arg(1))
	// 规则原文可以有空格，按原样取：命令之后跳过 "msg add" 这两个词。
	raw := func(skip int) string {
		fields := strings.Fields(strings.SplitN(inv.Text, "\n", 2)[0])
		if len(fields) <= skip {
			return ""
		}
		line := strings.SplitN(inv.Text, "\n", 2)[0]
		for _, field := range fields[:skip] {
			line = strings.TrimLeft(line, " \t")
			line = strings.TrimPrefix(line, field)
		}
		return strings.TrimSpace(line)
	}
	switch action {
	case "ls", "list", "":
		current, err := saved.Read()
		if err != nil {
			return err
		}
		if len(current.Messages) == 0 {
			return inv.Edit(ctx, "⚠️ 还没有消息规则，sure 不会生效。\n"+command.Code(inv.Prefix+"sure msg add _command:/sb")+" 添加一条")
		}
		lines := []string{"<b>消息规则</b>"}
		for _, rule := range current.Messages {
			line := command.Code(strconv.Itoa(rule.ID)) + " " + command.Code(rule.Msg)
			if rule.Redirect != "" {
				line += " → " + command.Code(rule.Redirect)
			}
			lines = append(lines, line)
		}
		return inv.Edit(ctx, strings.Join(lines, "\n"))
	case "add":
		text := raw(3)
		if text == "" {
			return inv.EditText(ctx, "用法："+inv.Prefix+"sure msg add 消息原文")
		}
		var id int
		if err := saved.Update(func(document *delegateDocument) error {
			document.NextID++
			id = document.NextID
			document.Messages = append(document.Messages, sureRule{ID: id, Msg: text})
			return nil
		}); err != nil {
			return err
		}
		return inv.Edit(ctx, "✅ 已添加规则 "+command.Code(strconv.Itoa(id))+"："+command.Code(text))
	case "redirect", "del":
		id, err := strconv.Atoi(inv.Arg(2))
		if err != nil {
			return inv.EditText(ctx, "请给出规则编号，"+inv.Prefix+"sure msg ls 可以看")
		}
		target := raw(4)
		found := false
		if err := saved.Update(func(document *delegateDocument) error {
			for index := range document.Messages {
				if document.Messages[index].ID != id {
					continue
				}
				found = true
				if action == "del" {
					document.Messages = append(document.Messages[:index:index], document.Messages[index+1:]...)
				} else {
					document.Messages[index].Redirect = target
				}
				return nil
			}
			return nil
		}); err != nil {
			return err
		}
		if !found {
			return inv.EditText(ctx, "没有编号为 "+strconv.Itoa(id)+" 的规则")
		}
		switch {
		case action == "del":
			return inv.Edit(ctx, "✅ 已删除规则 "+command.Code(strconv.Itoa(id)))
		case target == "":
			return inv.Edit(ctx, "✅ 已清除规则 "+command.Code(strconv.Itoa(id))+" 的重定向")
		}
		return inv.Edit(ctx, "✅ 规则 "+command.Code(strconv.Itoa(id))+" 重定向到 "+command.Code(target))
	}
	return inv.Edit(ctx, sureHelp(inv.Prefix))
}

// sureDelay 是触发消息被删掉前等待的时间，照 MiBox 是 5 秒；测试里改短。
var sureDelay = 5 * time.Second

// inBackground 让代发在后台进行，不耽误处理后面的更新；测试里换成直接执行。
var inBackground = func(work func()) { go work() }

// Register 注册 .sudo 和 .sure，并在应用里挂上看别人消息的监听者。
// 先问 sure 再问 sudo：sure 的规则更具体，还可能带重定向。
func Register(a *app.App) {
	sudoList := kit.NewStore(a, "sudo.json", func() delegateDocument { return delegateDocument{} })
	sureList := kit.NewStore(a, "sure.json", func() delegateDocument { return delegateDocument{} })

	a.OnForeign(func(ctx context.Context, client *bot.Client, message *bot.Message) bool {
		user := senderUserID(message)
		if user == "" {
			return false
		}
		current, err := sureList.Read()
		if err != nil || !current.hasUser(user) || !current.allowsChat(message.ChatID) {
			return false
		}
		text, ok := current.match(message.Text)
		if !ok {
			return false
		}
		inBackground(func() {
			relay(ctx, a, client, message, text, nil)
			if kit.Sleep(ctx, sureDelay) == nil {
				_ = client.DeleteMessage(ctx, message)
			}
		})
		return true
	})
	a.OnForeign(func(ctx context.Context, client *bot.Client, message *bot.Message) bool {
		user := senderUserID(message)
		if user == "" {
			return false
		}
		current, err := sudoList.Read()
		if err != nil || !current.hasUser(user) || !current.allowsChat(message.ChatID) {
			return false
		}
		// 只转发认得的命令。前缀开头但不是命令的消息如果也照发，名单里的人
		// 就能让账号说任何话。
		route, ok := a.Registry.Parse(message.Text)
		if !ok {
			return false
		}
		if _, known := a.Registry.Lookup(route.Command); !known {
			return false
		}
		var entities []tg.MessageEntityClass
		if message.Raw != nil {
			entities = message.Raw.Entities
		}
		inBackground(func() { relay(ctx, a, client, message, message.Text, entities) })
		return true
	})

	a.Registry.Register(
		&command.Command{Name: "sudo", Description: "让名单里的人用你的账号执行命令", Usage: "[add|del|ls|chat]", Help: sudoHelp,
			Handle: func(ctx context.Context, inv *command.Invocation) error {
				if handled, err := manageLists(ctx, inv, sudoList, "sudo"); handled {
					return err
				}
				return inv.Edit(ctx, sudoHelp(inv.Prefix))
			}},
		&command.Command{Name: "sure", Description: "让名单里的人触发指定的消息或命令", Usage: "[add|del|ls|chat|msg]", Help: sureHelp,
			Handle: func(ctx context.Context, inv *command.Invocation) error {
				if strings.EqualFold(inv.Arg(0), "msg") {
					return manageRules(ctx, inv, sureList)
				}
				if handled, err := manageLists(ctx, inv, sureList, "sure"); handled {
					return err
				}
				return inv.Edit(ctx, sureHelp(inv.Prefix))
			}},
	)
}
