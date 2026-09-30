// Package acron 实现 .acron：按六段 Cron（带秒，北京时间）定时发送、复制、转发、删除、
// 置顶或取消置顶消息，或者定时执行一条命令。移植自 MiBox V2 的 acron 插件，数据格式和它的
// assets/acron/acron_config.json 一样，--import-mibox 原样搬过来就能用。
package acron

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/gotd/td/tg"
	"github.com/robfig/cron/v3"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

type service struct {
	a     *app.App
	log   *slog.Logger
	store *store.Store[state]
	cron  *cron.Cron
	// registry 用来按当前的前缀和别名认出 cmd 任务里的命令是不是 .acron 本身，见 invokesSelf。
	registry *command.Registry
	// dispatch 把一条消息交给命令注册表，cmd 任务靠它执行命令；正式运行时是 a.DispatchMessage，
	// 测试里换成假的。
	dispatch func(ctx context.Context, message *tg.Message) bool

	mu      sync.Mutex
	entries map[string]cron.EntryID
	running map[string]bool
	// client 和 ctx 是账号连上后后台任务拿到的，定时触发时用它们。
	client *bot.Client
	ctx    context.Context
	// serial 让任务一个接一个执行，见 fire。
	serial sync.Mutex
}

// commandName 是本命令的名字，cmd 任务靠它认出自己。
const commandName = "acron"

func newService(a *app.App, logger *slog.Logger, documents *store.Store[state], registry *command.Registry,
	dispatch func(context.Context, *tg.Message) bool) *service {
	// robfig/cron 在单独的协程里跑任务，任务里 panic 了没人接住就会带倒整个进程；
	// Recover 接住后记一条日志，别的任务照常。
	jobs := cron.New(cron.WithParser(cronParser), cron.WithLocation(shanghai),
		cron.WithLogger(cronLogger{logger}), cron.WithChain(cron.Recover(cronLogger{logger})))
	return &service{a: a, log: logger, store: documents, registry: registry, dispatch: dispatch, cron: jobs,
		entries: map[string]cron.EntryID{}, running: map[string]bool{}}
}

// cronLogger 把 robfig/cron 的日志接到 slog：它自己的例行日志（开始、调度）只进 debug，
// 错误（主要是接住的 panic，带调用栈）进 error。
type cronLogger struct{ log *slog.Logger }

func (l cronLogger) Info(message string, keysAndValues ...any) {
	l.log.Debug("acron.cron_"+message, keysAndValues...)
}

func (l cronLogger) Error(err error, message string, keysAndValues ...any) {
	l.log.Error("acron.cron_"+message, append([]any{"error", err.Error()}, keysAndValues...)...)
}

// invokesSelf 判断 text 按当前的前缀和别名解析出来是不是 .acron 本身。这样的 cmd 任务
// 每执行一次就再添加一个任务，嵌套的还会成倍增长。用注册表解析，起个别名绕不过去。
func (s *service) invokesSelf(text string) bool {
	if s.registry == nil {
		return false
	}
	route, ok := s.registry.Parse(text)
	return ok && route.Command == commandName
}

// Register 注册 .acron 和到点执行任务的后台调度。任务存在 data/acron.json。
func Register(a *app.App) {
	logger := a.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := newService(a, logger, kit.NewStore(a, "acron.json", defaults), a.Registry, a.DispatchMessage)
	a.Registry.Register(&command.Command{Name: commandName, Group: command.GroupMedia, Description: "定时发送、删除消息或执行命令",
		Usage: "类型 Cron 对话 [参数]", Help: help, Handle: s.handle})
	a.Registry.AddJob(s.start)
}

func help(prefix string) string {
	p := command.Escape(prefix)
	c := func(text string) string { return "<code>" + p + "acron " + command.Escape(text) + "</code>" }
	return "⏰ <b>定时任务</b>\n\n" +
		"按六段 Cron 定时发送、复制、转发、删除、置顶消息，或者执行命令。Cron 依次是 秒 分 时 日 月 星期，按北京时间；" +
		"下面的 " + command.Code("0 0 2 * * *") + " 是每天 2 点。\n\n" +
		"对话写数字 ID、@用户名或 me（收藏夹）。send、copy、cmd 可以在对话后面加「|消息 ID」回复那条消息，" +
		"论坛里写话题 ID 就发进那个话题；forward 后面只能写话题 ID。末尾都可以加备注。\n\n" +
		"<b>发送</b>\n" +
		"• " + c("send 0 0 2 * * * 对话 [备注]") + " 回复一条文字消息，到时间把这段文字连同格式发到对话\n" +
		"• " + c("copy 0 0 2 * * * 对话 [备注]") + " 回复一条消息，到时间复制过去，不带转发来源，图片和文件也行\n" +
		"• " + c("forward 0 0 2 * * * 对话 [备注]") + " 回复一条消息，到时间转发过去\n\n" +
		"<b>执行命令</b>\n" +
		"命令写在第二行，到时间发到对话，再按当前的前缀和别名执行。比如每天 2 点备份：\n" +
		"<pre>" + p + "acron cmd 0 0 2 * * * me 定时备份\n" + p + "bf</pre>\n" +
		"列表里的结果写明是「已执行命令」还是「已发送命令（未执行：原因）」。\n\n" +
		"<b>删除与置顶</b>\n" +
		"• " + c("del 0 0 2 * * * 对话 消息 ID [备注]") + " 删除一条消息\n" +
		"• " + c("del_re 0 0 2 * * * 对话 100 /^test/i [备注]") + " 在最近 100 条消息里删除文字匹配正则的；" +
		"条数 1–1000，正则写成 /模式/标志 或只写模式，标志可用 i、m、s，不支持环视和反向引用\n" +
		"• " + c("pin 0 0 2 * * * 对话 消息 ID 1 0 [备注]") + " 置顶；两个数字依次是发不发通知、是不是只对自己置顶（私聊里），" +
		"写 1|0，也收 true|false、yes|no\n" +
		"• " + c("unpin 0 0 2 * * * 对话 消息 ID [备注]") + " 取消置顶\n\n" +
		"<b>管理</b>\n" +
		"• " + c("ls [all] [类型]") + " 当前对话的任务；all 看全部，写类型（如 del）只看这一种；" + c("la") + " 同 ls all\n" +
		"• " + c("rm ID") + " 删除任务\n" +
		"• " + c("off ID") + "、" + c("on ID") + " 关闭、开启任务（也收 disable、enable）"
}

func (s *service) handle(ctx context.Context, inv *command.Invocation) error {
	switch sub := strings.ToLower(inv.Arg(0)); sub {
	case "":
		return inv.EditPages(ctx, command.HTMLPages(help(inv.Prefix), command.PageLimit))
	case "list", "ls", "la":
		return s.list(ctx, inv, sub == "la")
	case "rm", "remove", "del_task":
		return s.remove(ctx, inv, sub)
	case "disable", "off":
		return s.disable(ctx, inv, sub)
	case "enable", "on":
		return s.enable(ctx, inv, sub)
	default:
		if isTaskType(sub) {
			return s.add(ctx, inv, sub)
		}
	}
	return kit.Failf("没有这个子命令：%s，%shelp acron 看用法", inv.Arg(0), inv.Prefix)
}

func (s *service) read() (state, error) {
	st, err := s.store.Read()
	st.normalize()
	return st, err
}

// updateTask 改一个任务再写回；任务已经被删了就什么也不改。
func (s *service) updateTask(id string, mutate func(*task)) error {
	return s.store.Update(func(st *state) error {
		if index := st.find(id); index >= 0 {
			mutate(&st.Tasks[index])
		}
		return nil
	})
}

func typeLabel(kind string) string {
	if label, ok := typeLabels[kind]; ok {
		return label
	}
	return kind
}

// addedTitles 是添加成功时的第一行，照 V2 的说法。
var addedTitles = map[string]string{
	"send": "已添加定时发送任务", "cmd": "已添加定时命令任务", "copy": "已添加定时复制任务", "forward": "已添加定时转发任务",
	"del": "已添加删除消息的定时任务", "del_re": "已添加正则删除的定时任务", "pin": "已添加置顶消息的定时任务", "unpin": "已添加取消置顶的定时任务",
}

// add 处理「.acron 类型 秒 分 时 日 月 星期 对话[|消息 ID] [参数] [备注]」，cmd 的命令写在第二行。
func (s *service) add(ctx context.Context, inv *command.Invocation, kind string) error {
	// 第一行按空白切词：[acron, 类型, 六段 Cron, 对话, 参数…]。别名展开后 Text 也以正式命令名开头。
	head, body, _ := strings.Cut(strings.TrimPrefix(inv.Text, inv.Prefix), "\n")
	fields := strings.Fields(head)
	if len(fields) < 8 {
		return kit.Fail("Cron 表达式要写 6 段：秒 分 时 日 月 星期")
	}
	expression := strings.Join(fields[2:8], " ")
	schedule, err := parseCron(expression)
	if err != nil {
		return err
	}
	var chat, replyTo string
	if len(fields) > 8 {
		chat, replyTo = splitTarget(fields[8])
	}
	if chat == "" {
		return kit.Fail("请写上对话：数字 ID、@用户名或 me")
	}
	if replyTo != "" && messageID(replyTo) == 0 {
		return kit.Fail("对话后面「|」之后要写消息 ID 或话题 ID")
	}
	rest := fields[9:]
	arg := func(index int) string {
		if index < len(rest) {
			return rest[index]
		}
		return ""
	}
	now := time.Now()
	t := task{Type: kind, Cron: expression, Chat: chat, ReplyTo: replyTo, CreatedAt: millis(now), Delivery: deliveryPending}
	// used 是第一行里对话之后被参数占掉的词数，再往后的是备注。
	used := 0
	switch kind {
	case "send":
		if err := fillSend(ctx, inv, &t); err != nil {
			return err
		}
	case "cmd":
		// V2 只取第二行；这里取第一行之后的全部，多行的命令（比如 .checkin add）也能定时执行。
		t.Message = strings.TrimSpace(body)
		if t.Message == "" {
			return kit.Failf("要执行的命令写在第二行，%shelp acron 有例子", inv.Prefix)
		}
		if s.invokesSelf(t.Message) {
			return kit.Failf("定时执行的命令不能是 %sacron：每执行一次就会再添加一个任务", inv.Prefix)
		}
	case "copy", "forward":
		reply, err := kit.Reply(ctx, inv)
		if err != nil {
			return err
		}
		if reply == nil {
			return kit.Fail("请回复一条要复制或转发的消息")
		}
		t.FromChatID, t.FromMsgID = reply.ChatID, strconv.Itoa(reply.ID)
	case "del", "unpin", "pin":
		if messageID(arg(0)) == 0 {
			return kit.Fail("请写上消息 ID（正整数）")
		}
		t.MsgID, used = arg(0), 1
		if kind == "pin" {
			if arg(1) == "" || arg(2) == "" {
				return kit.Fail("请写上发不发通知、是不是只对自己置顶，如 1 0 或 true false")
			}
			t.Notify, t.PmOneSide, used = parseFlag(arg(1)), parseFlag(arg(2)), 3
		}
	case "del_re":
		limit, err := strconv.Atoi(arg(0))
		if err != nil || limit < 1 || limit > maxScan {
			return kit.Failf("请写上 1–%d 之间的整数条数", maxScan)
		}
		if arg(1) == "" {
			return kit.Fail("请写上消息的正则表达式")
		}
		if _, err := compileRegex(arg(1)); err != nil {
			return err
		}
		t.Limit, t.Regex, used = strconv.Itoa(limit), arg(1), 2
	}
	// 9 是命令名、类型、六段 Cron 和对话。
	t.Remark = remarkAfter(head, 9+used)

	// 先解析一次对话：解析得出来就记下 ID，列表按它筛选当前对话；解析不出来也照样添加，
	// 到时间再解析一次，同 V2（比如对话的 access hash 这时还没见过）。
	resolved := false
	if peer, err := inv.Client.ResolveTarget(ctx, chat); err == nil {
		t.ChatID, t.ResolvedPeer, resolved = bot.PeerID(peerOf(peer, inv.Client.SelfID())), true, true
	} else {
		inv.Log.Info("acron.resolve_failed", "chat", chat, "error", err.Error())
	}
	t.Display = chatLabel(inv.Client, t)

	if err := s.store.Update(func(st *state) error {
		st.normalize()
		t.ID = st.nextID()
		st.Seq, st.SchemaVersion = t.ID, 1
		st.Tasks = append(st.Tasks, t)
		return nil
	}); err != nil {
		return err
	}
	s.schedule(t.ID, schedule)

	display := t.Display
	if !resolved {
		display += "（现在找不到这个对话，到时间再找）"
	}
	lines := []string{"✅ " + addedTitles[kind], "ID：" + command.Code(t.ID), "对话：" + display}
	switch {
	case t.ReplyTo != "" && kind == "forward":
		lines = append(lines, "话题："+command.Code(t.ReplyTo))
	case t.ReplyTo != "":
		lines = append(lines, "回复："+command.Code(t.ReplyTo))
	}
	if t.MsgID != "" {
		lines = append(lines, "消息 ID："+command.Code(t.MsgID))
	}
	if kind == "del_re" {
		lines = append(lines, "最近条数："+command.Code(t.Limit), "匹配："+command.Code(t.Regex))
	}
	if kind == "pin" {
		lines = append(lines, "通知："+kit.OnOffText(t.Notify), "只对自己置顶："+kit.OnOffText(t.PmOneSide))
	}
	if t.Remark != "" {
		lines = append(lines, "备注："+command.Escape(t.Remark))
	}
	if next, ok := nextRun(t.Cron, now); ok {
		lines = append(lines, "下次执行："+formatTime(next))
	}
	lines = append(lines, "复制："+copyHTML(t, inv.Prefix))
	return inv.Edit(ctx, strings.Join(lines, "\n"))
}

// fillSend 存下 send 要发的文字和格式。只存文字：带媒体（链接预览不算）或按钮的消息，
// V2 也不收，请用 copy 或 forward。
func fillSend(ctx context.Context, inv *command.Invocation, t *task) error {
	reply, err := kit.Reply(ctx, inv)
	if err != nil {
		return err
	}
	if reply == nil || reply.Raw == nil {
		return kit.Fail("请回复一条文字消息，到时间发的就是它的文字")
	}
	raw := reply.Raw
	if media, ok := raw.GetMedia(); ok {
		switch media.(type) {
		case *tg.MessageMediaWebPage, *tg.MessageMediaEmpty:
		default:
			return kit.Fail("定时发送只存文字，带媒体的消息请用 copy 或 forward")
		}
	}
	if raw.ReplyMarkup != nil {
		return kit.Fail("定时发送只存文字，带按钮的消息请用 copy 或 forward")
	}
	if strings.TrimSpace(raw.Message) == "" {
		return kit.Fail("被回复的消息里没有文字")
	}
	t.Message, t.Entities = raw.Message, encodeEntities(raw.Entities)
	return nil
}

// list 列出任务：「ls」是当前对话的，「ls all」「la」是全部的，后面再写类型只看这一种。
// V2 的「ls del」把 del 当成了范围参数，结果列出的是当前对话的全部任务；这里 all 和类型
// 不分先后，都认。
func (s *service) list(ctx context.Context, inv *command.Invocation, all bool) error {
	kind := ""
	for _, value := range inv.Args[1:] {
		switch value = strings.ToLower(value); {
		case value == "all":
			all = true
		case isTaskType(value) && kind == "":
			kind = value
		default:
			return kit.Failf("列表只能按 all 和任务类型筛选，%shelp acron 看用法", inv.Prefix)
		}
	}
	st, err := s.read()
	if err != nil {
		return err
	}
	var tasks []task
	for _, t := range st.Tasks {
		if (all || t.ChatID == inv.Message.ChatID) && (kind == "" || t.Type == kind) {
			tasks = append(tasks, t)
		}
	}
	html := renderList(tasks, all, kind, inv.Prefix, func(t task) string { return chatLabel(inv.Client, t) }, time.Now())
	return inv.EditPages(ctx, command.HTMLPages(html, command.PageLimit))
}

// renderList 生成任务列表，开启的在前、关闭的在后，各自保持添加的顺序。
func renderList(tasks []task, all bool, kind, prefix string, chat func(task) string, now time.Time) string {
	title := "当前对话的定时任务"
	if all {
		title = "全部定时任务"
	}
	if kind != "" {
		title += " · " + typeLabel(kind)
	}
	if len(tasks) == 0 {
		hint := "还没有，" + command.Code(prefix+"acron la") + " 看全部"
		if all {
			hint = "还没有，" + command.Code(prefix+"help acron") + " 看怎么添加"
		}
		return "⏰ <b>" + title + "</b>\n\n" + hint
	}
	lines := []string{fmt.Sprintf("⏰ <b>%s</b>（%d 个）", title, len(tasks))}
	for _, disabled := range []bool{false, true} {
		group := slices.DeleteFunc(slices.Clone(tasks), func(t task) bool { return t.Disabled != disabled })
		if len(group) == 0 {
			continue
		}
		lines = append(lines, "", "<b>"+kit.OnOffText(!disabled)+"</b>")
		for _, t := range group {
			lines = append(lines, "")
			lines = append(lines, taskLines(t, prefix, chat(t), now)...)
		}
	}
	return strings.Join(lines, "\n")
}

// taskLines 是列表里的一个任务。关闭的任务不写消息、回复和下次执行时间，同 V2。
func taskLines(t task, prefix, chat string, now time.Time) []string {
	head := command.Code(t.ID) + " · " + command.Escape(typeLabel(t.Type))
	if t.Remark != "" {
		head += " · " + command.Escape(t.Remark)
	}
	lines := []string{head, "对话：" + chat}
	if !t.Disabled {
		chatID := kit.OrDefault(t.ChatID, t.Chat)
		if t.MsgID != "" {
			lines = append(lines, "消息："+messageLink(chatID, t.MsgID))
		}
		if t.FromChatID != "" && t.FromMsgID != "" {
			lines = append(lines, "源消息："+messageLink(t.FromChatID, t.FromMsgID))
		}
		switch {
		case t.ReplyTo != "" && t.Type == "forward":
			lines = append(lines, "话题："+messageLink(chatID, t.ReplyTo))
		case t.ReplyTo != "" && slices.Contains([]string{"send", "cmd", "copy"}, t.Type):
			lines = append(lines, "回复："+messageLink(chatID, t.ReplyTo))
		}
		if next, ok := nextRun(t.Cron, now); ok {
			lines = append(lines, "下次执行："+formatTime(next))
		}
	}
	if at, ok := parseMillis(t.LastRunAt); ok {
		lines = append(lines, "上次执行："+formatTime(at))
	}
	if t.LastResult != "" {
		lines = append(lines, "结果："+command.Escape(t.LastResult))
	}
	if t.LastError != "" {
		lines = append(lines, "错误："+command.Escape(t.LastError))
	}
	return append(lines, "复制："+copyHTML(t, prefix))
}

func notFound(inv *command.Invocation, id string) error {
	return kit.Failf("没有 ID 是 %s 的定时任务，%sacron la 可以看全部", id, inv.Prefix)
}

func (s *service) remove(ctx context.Context, inv *command.Invocation, sub string) error {
	id := inv.Arg(1)
	if id == "" {
		return kit.Usage(inv.Prefix, "acron "+sub+" ID")
	}
	found := false
	if err := s.store.Update(func(st *state) error {
		index := st.find(id)
		if found = index >= 0; found {
			st.Tasks = slices.Delete(st.Tasks, index, index+1)
		}
		return nil
	}); err != nil {
		return err
	}
	if !found {
		return notFound(inv, id)
	}
	s.unschedule(id)
	return inv.Edit(ctx, "✅ 已删除定时任务 "+command.Code(id))
}

func (s *service) disable(ctx context.Context, inv *command.Invocation, sub string) error {
	id := inv.Arg(1)
	if id == "" {
		return kit.Usage(inv.Prefix, "acron "+sub+" ID")
	}
	found := false
	if err := s.store.Update(func(st *state) error {
		index := st.find(id)
		if found = index >= 0; found {
			st.Tasks[index].Disabled = true
		}
		return nil
	}); err != nil {
		return err
	}
	if !found {
		return notFound(inv, id)
	}
	s.unschedule(id)
	return inv.Edit(ctx, "✅ 已关闭定时任务 "+command.Code(id))
}

// enable 先检查 Cron、挂上调度器，成功了才把任务记成开启，同 V2：Cron 坏了的任务留在关闭状态。
func (s *service) enable(ctx context.Context, inv *command.Invocation, sub string) error {
	id := inv.Arg(1)
	if id == "" {
		return kit.Usage(inv.Prefix, "acron "+sub+" ID")
	}
	st, err := s.read()
	if err != nil {
		return err
	}
	index := st.find(id)
	if index < 0 {
		return notFound(inv, id)
	}
	t := st.Tasks[index]
	schedule, err := parseCron(t.Cron)
	if err != nil {
		return kit.Failf("定时任务 %s 的 Cron 表达式无效，没法开启", id)
	}
	s.schedule(id, schedule)
	if err := s.updateTask(id, func(t *task) { t.Disabled = false }); err != nil {
		s.unschedule(id)
		return err
	}
	t.Disabled = false
	lines := []string{"✅ 已开启定时任务 " + command.Code(id)}
	if next, ok := nextRun(t.Cron, time.Now()); ok {
		lines = append(lines, "下次执行："+formatTime(next))
	}
	lines = append(lines, "复制："+copyHTML(t, inv.Prefix))
	return inv.Edit(ctx, strings.Join(lines, "\n"))
}
