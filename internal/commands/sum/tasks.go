package sum

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/robfig/cron/v3"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// sumCronParser 同时接受五字段（分 时 日 月 周）和六字段（秒 分 时 日 月 周）的
// Cron，MiBox 生成的间隔任务就是六字段的，比如 0 */30 * * * *。
var sumCronParser = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

var sumIntervalPattern = regexp.MustCompile(`^(\d+)(m|h|d)$`)

// sumIntervalCron 把间隔换成 Cron：30m、2h、1d，或者原样接受五、六字段的 Cron。
func sumIntervalCron(interval string) (string, error) {
	if fields := strings.Fields(interval); len(fields) == 5 || len(fields) == 6 {
		spec := strings.Join(fields, " ")
		if _, err := sumCronParser.Parse(spec); err != nil {
			return "", kit.Fail("定时表达式无效")
		}
		return spec, nil
	}
	match := sumIntervalPattern.FindStringSubmatch(strings.ToLower(interval))
	if match == nil {
		return "", kit.Fail("间隔格式示例：30m、2h、1d，或五、六字段的 Cron")
	}
	value, err := strconv.Atoi(match[1])
	if err != nil || value <= 0 {
		return "", kit.Fail("间隔必须为正整数")
	}
	switch match[2] {
	case "m":
		if value > 59 || 60%value != 0 {
			return "", kit.Fail("分钟间隔须为 1-59 且能整除 60")
		}
		return fmt.Sprintf("*/%d * * * *", value), nil
	case "h":
		if value > 23 || 24%value != 0 {
			return "", kit.Fail("小时间隔须为 1-23 且能整除 24")
		}
		return fmt.Sprintf("0 */%d * * *", value), nil
	}
	if value != 1 {
		return "", kit.Fail("当前按天间隔仅支持 1d")
	}
	return "0 0 * * *", nil
}

// sumNextRun 是按 spec 在 now 之后的下一次执行时间。
func sumNextRun(spec string, now time.Time) (time.Time, bool) {
	schedule, err := sumCronParser.Parse(spec)
	if err != nil {
		return time.Time{}, false
	}
	return schedule.Next(now), true
}

// sumParseTime 读取 lastRunAt：新版存 RFC3339，旧版存毫秒时间戳。
func sumParseTime(value string) (time.Time, bool) {
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, true
	}
	if millis, err := strconv.ParseInt(value, 10, 64); err == nil && millis > 0 {
		return time.UnixMilli(millis), true
	}
	return time.Time{}, false
}

func sumFormatTime(value time.Time) string { return value.In(time.Local).Format("2006-01-02 15:04") }

var (
	sumSimpleInterval = regexp.MustCompile(`(?i)^\d+[mhd]$`)
	sumCronToken      = regexp.MustCompile(`^[\w*?,/\-#]+$`)
	sumDowToken       = regexp.MustCompile(`(?i)^(?:[0-9*?,/#-]+|(?:SUN|MON|TUE|WED|THU|FRI|SAT)(?:[-,/#](?:SUN|MON|TUE|WED|THU|FRI|SAT|[0-7*]))*)$`)
	sumDigits         = regexp.MustCompile(`\d+`)
	sumQuotedToken    = regexp.MustCompile(`"([^"\\]*(?:\\.[^"\\]*)*)"|'([^'\\]*(?:\\.[^'\\]*)*)'|(\S+)`)
	sumEscapedQuote   = regexp.MustCompile(`\\(["'])`)
)

// sumAddArguments 取 .sum add 之后的参数。命令第一行按引号重新切词，
// 这样 "30 */2 * * *" 这样带空格的 Cron 能作为一个参数。
func sumAddArguments(inv *command.Invocation) []string {
	line, _, _ := strings.Cut(inv.Text, "\n")
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), inv.Prefix))
	var values []string
	for _, match := range sumQuotedToken.FindAllStringSubmatch(line, -1) {
		value := match[3]
		if match[1] != "" || strings.HasPrefix(match[0], "\"") {
			value = match[1]
		} else if match[2] != "" || strings.HasPrefix(match[0], "'") {
			value = match[2]
		}
		values = append(values, sumEscapedQuote.ReplaceAllString(value, "$1"))
	}
	if len(values) >= 2 && strings.EqualFold(values[1], "add") {
		return values[2:]
	}
	if len(inv.Args) > 1 {
		return inv.Args[1:]
	}
	return nil
}

func sumCronLike(value string) bool {
	return !strings.HasPrefix(value, "--") && sumCronToken.MatchString(value)
}

func sumDayOfWeek(value string) bool {
	if !sumDowToken.MatchString(value) {
		return false
	}
	for _, number := range sumDigits.FindAllString(value, -1) {
		if parsed, _ := strconv.Atoi(number); parsed > 7 {
			return false
		}
	}
	return true
}

// sumAddSyntax 拆出目标、间隔和其余参数。间隔可以是 30m 这样的简写、带引号的 Cron，
// 也可以是不带引号直接写开的五、六字段 Cron（六字段时最后一位必须像星期）。
func sumAddSyntax(args []string) (target, interval string, rest []string) {
	if len(args) == 0 {
		return "", "", nil
	}
	target = args[0]
	first := ""
	if len(args) > 1 {
		first = args[1]
	}
	if sumSimpleInterval.MatchString(first) || strings.Contains(first, " ") {
		return target, first, args[min(2, len(args)):]
	}
	if len(args) >= 7 {
		six := args[1:7]
		if sumAllCron(six[:5]) && sumDayOfWeek(six[5]) {
			return target, strings.Join(six, " "), args[7:]
		}
	}
	if len(args) >= 6 && sumAllCron(args[1:6]) {
		return target, strings.Join(args[1:6], " "), args[6:]
	}
	return target, first, args[min(2, len(args)):]
}

func sumAllCron(values []string) bool {
	for _, value := range values {
		if !sumCronLike(value) {
			return false
		}
	}
	return true
}

// sumAddOptions 是 .sum add 间隔之后的可选参数。
type sumAddOptions struct {
	count     int
	timeRange int
	provider  string
	spoiler   *bool
	remark    string
}

func sumParseAddOptions(rest []string) (sumAddOptions, error) {
	options := sumAddOptions{count: 100}
	var remark []string
	for index := 0; index < len(rest); index++ {
		value := rest[index]
		switch {
		case value == "--time" || value == "--provider":
			if index+1 >= len(rest) || rest[index+1] == "" {
				return options, kit.Fail("请提供 " + value + " 的值")
			}
			index++
			if value == "--provider" {
				options.provider = rest[index]
				continue
			}
			hours, err := strconv.Atoi(rest[index])
			if err != nil || hours <= 0 || hours > 720 {
				return options, kit.Fail("时间范围须为 1-720 小时")
			}
			options.timeRange = hours
		case value == "--spoiler" || value == "--no-spoiler":
			enabled := value == "--spoiler"
			options.spoiler = &enabled
		case regexp.MustCompile(`^\d+$`).MatchString(value):
			options.count, _ = strconv.Atoi(value)
		default:
			remark = append(remark, value)
		}
	}
	options.remark = strings.Join(remark, " ")
	return options, nil
}

var (
	sumInviteLink  = regexp.MustCompile(`(?i)^(?:https?://)?t\.me/(?:\+|joinchat/)([A-Za-z0-9_-]+)`)
	sumPrivateLink = regexp.MustCompile(`(?i)^(?:https?://)?t\.me/c/(\d+)`)
	sumPublicLink  = regexp.MustCompile(`(?i)^(?:https?://)?t\.me/([A-Za-z0-9_]+)`)
	sumNumericID   = regexp.MustCompile(`^-?\d+$`)
)

// sumTargetKind 把用户写的目标规整成要解析的形式：here、带标记的数字 ID、@用户名，
// 或邀请链接的 hash（invite 为真）。
func sumTargetKind(target string) (normalized string, invite bool) {
	switch {
	case strings.EqualFold(target, "here"):
		return "here", false
	case sumNumericID.MatchString(target):
		return target, false
	}
	if match := sumInviteLink.FindStringSubmatch(target); match != nil {
		return match[1], true
	}
	if match := sumPrivateLink.FindStringSubmatch(target); match != nil {
		return "-100" + match[1], false
	}
	if match := sumPublicLink.FindStringSubmatch(target); match != nil {
		return "@" + match[1], false
	}
	return "@" + strings.TrimPrefix(target, "@"), false
}

// chatIDOf 是对话的带标记 ID。
func chatIDOf(chat tg.ChatClass) (string, string, string) {
	switch value := chat.(type) {
	case *tg.Channel:
		return "-100" + strconv.FormatInt(value.ID, 10), value.Title, value.Username
	case *tg.Chat:
		return "-" + strconv.FormatInt(value.ID, 10), value.Title, ""
	}
	return "", "", ""
}

// joinInvite 通过邀请链接找到群组：已在群里就直接用；还没加入就先加入，
// 和 MiBox 的做法一样（要读历史消息，本来就得是成员）。
func joinInvite(ctx context.Context, client *bot.Client, hash string) (tg.ChatClass, error) {
	invite, err := client.API().MessagesCheckChatInvite(ctx, hash)
	if err != nil {
		return nil, kit.Fail("无法处理邀请链接：" + command.Brief(err))
	}
	switch value := invite.(type) {
	case *tg.ChatInviteAlready:
		client.Peers().RememberChats([]tg.ChatClass{value.Chat})
		return value.Chat, nil
	case *tg.ChatInvite, *tg.ChatInvitePeek:
		result, err := client.API().MessagesImportChatInvite(ctx, hash)
		if err != nil {
			if tgerr.Is(err, "INVITE_REQUEST_SENT") {
				return nil, kit.Fail("已提交入群申请，通过后再添加任务")
			}
			return nil, kit.Fail("加入群组失败：" + command.Brief(err))
		}
		var chats []tg.ChatClass
		if joined, ok := result.(*tg.MessagesChatInviteJoinResultOk); ok {
			switch updates := joined.Updates.(type) {
			case *tg.Updates:
				chats = updates.Chats
			case *tg.UpdatesCombined:
				chats = updates.Chats
			}
		}
		if len(chats) == 0 {
			return nil, kit.Fail("加入群组后未拿到群组信息")
		}
		client.Peers().RememberChats(chats)
		return chats[0], nil
	}
	return nil, kit.Fail("无法处理邀请链接")
}

// resolveChat 把 .sum add 的目标解析成要存下的 chatId 和显示名，同时确认它可以访问。
// 公开群存 @用户名：以后每次都按用户名解析，不依赖本地记住的 access hash。
func (s *sumService) resolveChat(ctx context.Context, inv *command.Invocation, target string) (string, string, error) {
	client := inv.Client
	normalized, invite := sumTargetKind(target)
	if invite {
		chat, err := joinInvite(ctx, client, normalized)
		if err != nil {
			return "", "", err
		}
		chatID, title, username := chatIDOf(chat)
		if chatID == "" {
			return "", "", kit.Fail("邀请链接指向的不是群组")
		}
		if username != "" {
			chatID = "@" + username
		}
		return chatID, sumDisplay(title, username), nil
	}
	chatID := normalized
	if normalized == "here" {
		chatID = inv.Message.ChatID
	}
	input, err := client.ResolveTarget(ctx, chatID)
	if err != nil {
		return "", "", kit.Fail("无法获取群组信息：" + command.Brief(err))
	}
	title, username := chatTitle(client, peerOfInput(input, client.SelfID()))
	return chatID, sumDisplay(title, username), nil
}

func sumDisplay(title, username string) string {
	display := strings.TrimSpace(title)
	if username != "" {
		display = strings.TrimSpace(display + " @" + username)
	}
	return display
}

// add 创建定时任务：.sum add 目标 间隔 [消息数] [--time 小时] [--provider 名称] [--spoiler|--no-spoiler] [备注]。
func (s *sumService) add(ctx context.Context, inv *command.Invocation) error {
	target, interval, rest := sumAddSyntax(sumAddArguments(inv))
	options, err := sumParseAddOptions(rest)
	if err != nil {
		return err
	}
	if target == "" || interval == "" || options.count < 10 || options.count > 500 {
		return kit.Fail("用法：sum add here|群组 2h 100")
	}
	spec, err := sumIntervalCron(interval)
	if err != nil {
		return err
	}
	db, err := s.read()
	if err != nil {
		return err
	}
	if options.provider != "" && !providerKnown(db, options.provider) {
		return kit.Fail("未找到 AI 配置：" + options.provider)
	}
	chatID, display, err := s.resolveChat(ctx, inv, target)
	if err != nil {
		return err
	}
	spoiler := db.AIConfig.DefaultSpoiler
	if options.spoiler != nil {
		spoiler = *options.spoiler
	}
	var task sumTask
	var previous json.Number
	if err := s.update(func(db *sumDB) error {
		previous = db.Seq
		last, _ := strconv.ParseInt(string(db.Seq), 10, 64)
		id := strconv.FormatInt(last+1, 10)
		task = sumTask{ID: id, Cron: spec, ChatID: chatID, ChatDisplay: display, Interval: interval, MessageCount: options.count,
			TimeRange: options.timeRange, PushTarget: db.DefaultPushTarget, AIProvider: options.provider, UseSpoiler: spoiler,
			CreatedAt: time.Now().UTC().Format(time.RFC3339), Remark: options.remark}
		db.Seq = json.Number(id)
		db.Tasks = append(db.Tasks, task)
		return nil
	}); err != nil {
		return err
	}
	if err := s.register(task); err != nil {
		_ = s.update(func(db *sumDB) error {
			db.Tasks = slicesDeleteTask(db.Tasks, task.ID)
			db.Seq = previous
			return nil
		})
		return err
	}
	lines := []string{"✅ 已创建摘要任务 " + command.Code(task.ID), "群组：" + command.Escape(kit.OrDefault(display, chatID)), "间隔：" + command.Code(interval)}
	if task.TimeRange > 0 {
		lines = append(lines, fmt.Sprintf("时间范围：过去 %d 小时（最多 %d 条）", task.TimeRange, task.MessageCount))
	} else {
		lines = append(lines, fmt.Sprintf("消息数：%d", task.MessageCount))
	}
	if task.AIProvider != "" {
		lines = append(lines, "AI 配置："+command.Code(task.AIProvider))
	}
	if task.UseSpoiler {
		lines = append(lines, "折叠：是")
	}
	lines = append(lines, "推送："+command.Code(pushTarget(db, task)))
	if task.Remark != "" {
		lines = append(lines, "备注："+command.Escape(task.Remark))
	}
	if next, ok := sumNextRun(spec, time.Now()); ok {
		lines = append(lines, "下次执行："+sumFormatTime(next))
	}
	return inv.Edit(ctx, strings.Join(lines, "\n"))
}

func slicesDeleteTask(tasks []sumTask, id string) []sumTask {
	kept := tasks[:0:0]
	for _, task := range tasks {
		if task.ID != id {
			kept = append(kept, task)
		}
	}
	return kept
}

// register 把任务挂到调度器上。触发时按 ID 重新读取任务，这样 edit 改过的
// 服务商、提示词、折叠设置在下一次执行时就生效。
func (s *sumService) register(task sumTask) error {
	if task.Disabled {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[task.ID]; exists {
		return nil
	}
	id := task.ID
	entry, err := s.cron.AddFunc(task.Cron, func() { s.fire(id) })
	if err != nil {
		return kit.Fail("定时表达式无效")
	}
	s.entries[id] = entry
	return nil
}

// fire 执行一次定时任务，并把结果或错误记进任务。
func (s *sumService) fire(id string) {
	s.mu.Lock()
	client := s.client
	if s.running[id] || client == nil {
		s.mu.Unlock()
		return
	}
	s.running[id] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.running, id); s.mu.Unlock() }()
	db, err := s.read()
	if err != nil {
		s.a.Logger.Error("sum.scheduled_failed", "task", id, "error", err.Error())
		return
	}
	var task *sumTask
	for index := range db.Tasks {
		if db.Tasks[index].ID == id {
			task = &db.Tasks[index]
		}
	}
	if task == nil || task.Disabled {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	err = s.push(ctx, client, *task)
	target := pushTarget(db, *task)
	_ = s.update(func(db *sumDB) error {
		for index := range db.Tasks {
			if db.Tasks[index].ID != id {
				continue
			}
			db.Tasks[index].LastRunAt = time.Now().UTC().Format(time.RFC3339)
			if err != nil {
				db.Tasks[index].LastError = sumErrorText(err)
			} else {
				db.Tasks[index].LastResult, db.Tasks[index].LastError = "总结完成，已推送到 "+target, ""
			}
		}
		return nil
	})
	if err != nil {
		s.a.Logger.Error("sum.scheduled_failed", "task", id, "error", err.Error())
	}
}

func (s *sumService) unregister(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.entries[id]; ok {
		s.cron.Remove(entry)
		delete(s.entries, id)
	}
}

func sumTaskIndex(db sumDB, id string) int {
	for index := range db.Tasks {
		if db.Tasks[index].ID == id {
			return index
		}
	}
	return -1
}

// list 列出任务：间隔、范围、AI 配置、推送目标、下次和上次执行时间、结果和错误。
func (s *sumService) list(ctx context.Context, inv *command.Invocation) error {
	db, err := s.read()
	if err != nil {
		return err
	}
	if len(db.Tasks) == 0 {
		return inv.Edit(ctx, "<b>摘要任务</b>\n暂无定时任务")
	}
	tasks := append([]sumTask(nil), db.Tasks...)
	sort.SliceStable(tasks, func(a, b int) bool {
		left, _ := strconv.Atoi(tasks[a].ID)
		right, _ := strconv.Atoi(tasks[b].ID)
		return left < right
	})
	now := time.Now()
	blocks := []string{"📋 <b>摘要任务</b>"}
	for _, task := range tasks {
		lines := []string{command.Code(task.ID) + " • " + command.Escape(kit.OrDefault(task.Remark, kit.OrDefault(task.ChatDisplay, task.ChatID)))}
		lines = append(lines, "群组："+command.Escape(kit.OrDefault(task.ChatDisplay, task.ChatID)), "间隔："+command.Code(task.Interval))
		if task.TimeRange > 0 {
			lines = append(lines, fmt.Sprintf("时间范围：过去 %d 小时", task.TimeRange))
		} else {
			lines = append(lines, fmt.Sprintf("消息数：%d", task.MessageCount))
		}
		switch {
		case task.AIProvider != "":
			lines = append(lines, "AI 配置："+command.Code(task.AIProvider))
		case db.AIConfig.DefaultProvider != "":
			lines = append(lines, "AI 配置：默认（"+command.Escape(db.AIConfig.DefaultProvider)+"）")
		default:
			lines = append(lines, "AI 配置：默认（ai 聊天模型）")
		}
		if task.AIPrompt != "" {
			lines = append(lines, "提示词："+command.Escape(kit.TruncateRunes(task.AIPrompt, 30)))
		}
		if task.UseSpoiler {
			lines = append(lines, "折叠：是")
		}
		lines = append(lines, "推送："+command.Code(pushTarget(db, task)))
		switch next, ok := sumNextRun(task.Cron, now); {
		case task.Disabled:
			lines = append(lines, "状态：⏹ 已停用")
		case !ok:
			lines = append(lines, "状态：定时表达式无效")
		default:
			lines = append(lines, "下次："+sumFormatTime(next))
		}
		if task.LastRunAt != "" {
			if last, ok := sumParseTime(task.LastRunAt); ok {
				lines = append(lines, "上次："+sumFormatTime(last))
			}
		}
		if task.LastResult != "" {
			lines = append(lines, "结果："+command.Escape(task.LastResult))
		}
		if task.LastError != "" {
			lines = append(lines, "错误："+command.Escape(task.LastError))
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	return kit.SendPages(ctx, inv, command.HTMLPages(strings.Join(blocks, "\n\n"), 3800))
}

// runNow 立即执行一个任务并推送。
func (s *sumService) runNow(ctx context.Context, inv *command.Invocation) error {
	db, err := s.read()
	if err != nil {
		return err
	}
	index := sumTaskIndex(db, inv.Arg(1))
	if index < 0 {
		return kit.Fail("摘要任务不存在")
	}
	task := db.Tasks[index]
	s.mu.Lock()
	if s.running == nil {
		s.running = map[string]bool{}
	}
	if s.running[task.ID] {
		s.mu.Unlock()
		return kit.Fail("该任务正在执行，请稍后再试")
	}
	s.running[task.ID] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.running, task.ID); s.mu.Unlock() }()
	if err := inv.EditText(ctx, "📝 正在生成摘要..."); err != nil {
		return err
	}
	if err := s.push(ctx, inv.Client, task); err != nil {
		return err
	}
	return inv.EditText(ctx, "✅ 摘要已推送到 "+pushTarget(db, task))
}

// toggle 处理 del、disable、enable。
func (s *sumService) toggle(ctx context.Context, inv *command.Invocation, sub string) error {
	id := inv.Arg(1)
	db, err := s.read()
	if err != nil {
		return err
	}
	index := sumTaskIndex(db, id)
	if index < 0 {
		return kit.Fail("摘要任务不存在")
	}
	task := db.Tasks[index]
	if sub != "enable" {
		s.unregister(id)
	}
	if err := s.update(func(db *sumDB) error {
		for index := range db.Tasks {
			if db.Tasks[index].ID != id {
				continue
			}
			if sub == "del" || sub == "rm" {
				db.Tasks = append(db.Tasks[:index], db.Tasks[index+1:]...)
			} else {
				db.Tasks[index].Disabled = sub == "disable"
			}
			break
		}
		return nil
	}); err != nil {
		return err
	}
	if sub == "enable" {
		task.Disabled = false
		if err := s.register(task); err != nil {
			return err
		}
	}
	return inv.EditText(ctx, "✅ 摘要任务已更新")
}

// edit 修改任务：spoiler on|off、provider [名称]、prompt [内容]；名称和内容留空表示改回全局默认。
func (s *sumService) edit(ctx context.Context, inv *command.Invocation) error {
	id, property, value := inv.Arg(1), strings.ToLower(inv.Arg(2)), inv.Rest(3)
	if id == "" || property == "" {
		return kit.Fail("用法：sum edit ID spoiler on|off | provider [名称] | prompt [内容]")
	}
	db, err := s.read()
	if err != nil {
		return err
	}
	if sumTaskIndex(db, id) < 0 {
		return kit.Fail("摘要任务不存在")
	}
	var mutate func(task *sumTask)
	var message string
	switch property {
	case "spoiler":
		var enabled bool
		switch strings.ToLower(value) {
		case "":
			return kit.Fail("请提供值：on 或 off")
		case "on", "true", "1":
			enabled, message = true, "已启用任务 "+id+" 的折叠显示"
		case "off", "false", "0":
			message = "已关闭任务 " + id + " 的折叠显示"
		default:
			return kit.Fail("无效的值，请使用 on 或 off")
		}
		mutate = func(task *sumTask) { task.UseSpoiler = enabled }
	case "provider":
		if value != "" && !providerKnown(db, value) {
			return kit.Fail("未找到 AI 配置：" + value)
		}
		message = "已设置任务 " + id + " 的 AI 配置为 " + value
		if value == "" {
			message = "已清空任务 " + id + " 的 AI 配置，将使用全局默认配置"
		}
		mutate = func(task *sumTask) { task.AIProvider = value }
	case "prompt":
		message = "已设置任务 " + id + " 的提示词"
		if value == "" {
			message = "已清空任务 " + id + " 的提示词，将使用全局默认提示词"
		}
		mutate = func(task *sumTask) { task.AIPrompt = value }
	default:
		return kit.Fail("未知属性：" + property + "，支持 spoiler、provider、prompt")
	}
	if err := s.update(func(db *sumDB) error {
		index := sumTaskIndex(*db, id)
		if index < 0 {
			return kit.Fail("摘要任务不存在")
		}
		mutate(&db.Tasks[index])
		return nil
	}); err != nil {
		return err
	}
	return inv.EditText(ctx, "✅ "+message)
}

// reorder 按任务当前的顺序重新编号为 1..n，并重新挂到调度器上。
func (s *sumService) reorder(ctx context.Context, inv *command.Invocation) error {
	db, err := s.read()
	if err != nil {
		return err
	}
	if len(db.Tasks) == 0 {
		return kit.Fail("暂无任务需要重排序")
	}
	for _, task := range db.Tasks {
		s.unregister(task.ID)
	}
	var mapping []string
	var tasks []sumTask
	if err := s.update(func(db *sumDB) error {
		mapping = mapping[:0]
		for index := range db.Tasks {
			next := strconv.Itoa(index + 1)
			mapping = append(mapping, command.Escape(db.Tasks[index].ID)+" → "+next)
			db.Tasks[index].ID = next
		}
		db.Seq = json.Number(strconv.Itoa(len(db.Tasks)))
		tasks = append([]sumTask(nil), db.Tasks...)
		return nil
	}); err != nil {
		return err
	}
	for _, task := range tasks {
		if err := s.register(task); err != nil {
			inv.Log.Warn("sum.register_failed", "task", task.ID, "error", err.Error())
		}
	}
	return inv.Edit(ctx, fmt.Sprintf("✅ 已重新排序 %d 个任务\n\n%s", len(tasks), strings.Join(mapping, ", ")))
}
