// Package checkin 实现 .checkin：每天在设定的时间向机器人或群组发签到命令，需要时再点一下
// 回调按钮，最后发一份汇总。数据格式和 TeleBox 的 checkin 插件（assets/checkin/checkin_config.json）
// 一样，--import-mibox 原样搬过来。
package checkin

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// signTarget 是一个签到目标：往 Target 发 Command；CallbackData 或 ButtonText 不为空时，
// 再在对方的回复里找到这个回调按钮点一下。
type signTarget struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Target       string `json:"target"`
	Command      string `json:"command"`
	CallbackData string `json:"callbackData,omitempty"`
	ButtonText   string `json:"buttonText,omitempty"`
	Enabled      bool   `json:"enabled"`
}

// matcher 说明怎么找要点的按钮，列表和设置里显示用。
func (t signTarget) matcher() string {
	switch {
	case t.CallbackData != "":
		return "回调数据 " + command.Code(t.CallbackData)
	case t.ButtonText != "":
		return "按钮文字 " + command.Code(t.ButtonText)
	}
	return ""
}

type config struct {
	// RunTime 是开始时间；RunTimeEnd 不为空时，每天在 RunTime 到 RunTimeEnd 之间随机挑一分钟，
	// 结束早于开始表示跨午夜。都是北京时间 HH:MM。
	RunTime    string `json:"runTime"`
	RunTimeEnd string `json:"runTimeEnd"`
	// LogChat 是汇总发往的对话；BotToken 和 PushChatID 都设了时改用机器人推送。
	LogChat    string `json:"logChat"`
	BotToken   string `json:"botToken"`
	PushChatID string `json:"pushChatId"`
	// RandomDelay 是到点之后再随机多等的分钟数上限。
	RandomDelay int `json:"randomDelay"`
	// LastRunDate 是最近一次执行全部签到的日期，格式和原插件一样是 2006/1/2。
	LastRunDate string       `json:"lastRunDate"`
	Targets     []signTarget `json:"targets"`
	// CurrentRunTime 是给 PlannedDate 那天挑好的执行时刻。存下来，重启后不会重挑。
	CurrentRunTime string `json:"currentRunTime,omitempty"`
	PlannedDate    string `json:"plannedDate,omitempty"`
}

func defaults() config {
	return config{RunTime: "10:00", RunTimeEnd: "11:30", Targets: []signTarget{}}
}

func (c config) enabled() []signTarget {
	var targets []signTarget
	for _, target := range c.Targets {
		if target.Enabled {
			targets = append(targets, target)
		}
	}
	return targets
}

func (c config) find(id string) (int, bool) {
	for index, target := range c.Targets {
		if target.ID == id {
			return index, true
		}
	}
	return -1, false
}

// shanghai 是签到用的时区，和原插件一样固定是北京时间。
var shanghai = mustLocation("Asia/Shanghai")

func mustLocation(name string) *time.Location {
	location, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return location
}

// dateOf 是 now 在北京时间的日期，写法和原插件存的 lastRunDate 一样。
func dateOf(now time.Time) string { return now.In(shanghai).Format("2006/1/2") }

type service struct {
	a     *app.App
	store *store.Store[config]
	// running 让手动和定时的签到不同时跑：同一个机器人同时收到两条签到命令，
	// 读回复时会认错。
	running sync.Mutex
}

// Register 注册 .checkin 和每分钟看一次该不该签到的后台任务。设置和目标存在 data/checkin.json。
func Register(a *app.App) {
	s := &service{a: a, store: kit.NewStore(a, "checkin.json", defaults)}
	a.Registry.Register(&command.Command{Name: "checkin", Group: command.GroupAccount, Description: "每天定时向机器人签到", Usage: "[子命令]",
		Help: help, Timeout: 30 * time.Minute, Handle: s.handle})
	a.Registry.AddJob(s.schedule)
}

func help(prefix string) string {
	p := command.Escape(prefix)
	c := func(text string) string { return "<code>" + p + "checkin" + text + "</code>" }
	return "📅 <b>定时签到</b>\n\n每天在设定的时间（北京时间）向机器人或群组发签到命令，需要时再点一下回调按钮，最后发一份汇总。\n\n" +
		"<b>签到</b>\n" +
		"• " + c("") + " 立即签到全部开启的目标，当天不再定时签\n" +
		"• " + c(" test ID") + " 只试一个\n" +
		"• " + c(" reset") + " 清掉今天的执行记录，到时间会再签一次\n\n" +
		"<b>目标</b>\n" +
		"• " + c(" add ID 名称 目标 [data:回调数据|text:按钮文字]") + "，签到命令写在第二行，可以带空格\n" +
		"• " + c(" list") + "、" + c(" del ID") + "、" + c(" toggle ID") + "（开启或关闭）\n\n" +
		"<b>设置</b>\n" +
		"• " + c(" set time 10:00") + " 开始时间\n" +
		"• " + c(" set range 11:30") + " 在开始时间到这个时间之间随机挑一分钟签，可以跨午夜；不写时间就改回固定时间\n" +
		"• " + c(" set delay 分钟数") + " 到点后再随机多等 0 到这么多分钟，最多 60\n" +
		"• " + c(" set bot Token 对话 ID|off") + " 汇总改用机器人推送\n" +
		"• " + c(" set log 对话|off") + " 汇总发到这个对话\n" +
		"• " + c(" config") + " 查看设置\n\n" +
		"汇总默认发回执行命令的对话，定时签到时发到收藏夹。错过了时间（比如服务停着），当天稍后补上。" +
		"涉及 Bot Token 的命令请在收藏夹里执行。\n\n" +
		"<b>示例</b>\n<pre>" + p + "checkin add storm Storm签到 @storm_bot data:checkin\n/sign 123456</pre>"
}

func (s *service) handle(ctx context.Context, inv *command.Invocation) error {
	switch action := strings.ToLower(inv.Arg(0)); action {
	case "":
		return s.runNow(ctx, inv)
	case "add":
		return s.add(ctx, inv)
	case "del", "delete", "rm":
		return s.remove(ctx, inv)
	case "list", "ls":
		return s.list(ctx, inv)
	case "toggle":
		return s.toggle(ctx, inv)
	case "test":
		return s.test(ctx, inv)
	case "set":
		return s.set(ctx, inv)
	case "config", "settings", "info":
		return s.show(ctx, inv)
	case "reset":
		if err := s.store.Update(func(cfg *config) error {
			cfg.LastRunDate, cfg.PlannedDate, cfg.CurrentRunTime = "", "", ""
			return nil
		}); err != nil {
			return err
		}
		return inv.EditText(ctx, "✅ 已清掉今天的执行记录，到时间会再签一次")
	}
	return kit.Failf("没有这个子命令：%s，%shelp checkin 看用法", inv.Arg(0), inv.Prefix)
}

// runNow 立即签到全部开启的目标。签过之后当天不再定时签：刚手动签完，
// 到点再签一遍只会收到一堆「今天已经签过了」。
func (s *service) runNow(ctx context.Context, inv *command.Invocation) error {
	cfg, err := s.store.Read()
	if err != nil {
		return err
	}
	if len(cfg.Targets) == 0 {
		return inv.Edit(ctx, help(inv.Prefix))
	}
	targets := cfg.enabled()
	if len(targets) == 0 {
		return kit.Failf("签到目标都关着，%scheckin toggle ID 可以打开", inv.Prefix)
	}
	if !s.running.TryLock() {
		return kit.Fail("正在签到，请稍候")
	}
	defer s.running.Unlock()
	if err := inv.EditText(ctx, kit.Working(fmt.Sprintf("正在签到 %d 个目标", len(targets)))); err != nil {
		return err
	}
	today := dateOf(time.Now())
	summary := runAll(ctx, inv.Client, targets, "手动")
	kit.Warn(s.a, "checkin.save_failed", s.store.Update(func(cfg *config) error { cfg.LastRunDate = today; return nil }))
	return deliver(ctx, inv.Client, cfg, summary, inv)
}

// add 处理「.checkin add ID 名称 目标 [data:…|text:…]」，签到命令在第二行。
// 原插件要回复它发的提示消息再写命令；写在第二行一步就完，重启了也不会丢半截。
func (s *service) add(ctx context.Context, inv *command.Invocation) error {
	head, sign, _ := strings.Cut(inv.RawAfter(0), "\n")
	fields := strings.Fields(head)
	sign = strings.TrimSpace(sign)
	if len(fields) < 4 || sign == "" {
		return kit.Failf("用法：%scheckin add ID 名称 目标 [data:回调数据|text:按钮文字]，签到命令写在第二行", inv.Prefix)
	}
	next := signTarget{ID: fields[1], Name: fields[2], Target: fields[3], Command: sign, Enabled: true}
	next.CallbackData, next.ButtonText = parseMatcher(strings.Join(fields[4:], " "))
	verb := "添加"
	if err := s.store.Update(func(cfg *config) error {
		if index, ok := cfg.find(next.ID); ok {
			cfg.Targets[index] = next
			verb = "更新"
			return nil
		}
		cfg.Targets = append(cfg.Targets, next)
		return nil
	}); err != nil {
		return err
	}
	lines := []string{"✅ 已" + verb + "签到目标 " + command.Bold(next.Name) + "（" + command.Code(next.ID) + "）",
		"目标：" + command.Code(next.Target), "命令：" + command.Code(next.Command)}
	if matcher := next.matcher(); matcher != "" {
		lines = append(lines, "然后点："+matcher)
	}
	return inv.Edit(ctx, strings.Join(lines, "\n"))
}

// parseMatcher 读按钮的写法：data:回调数据、text:按钮文字，什么前缀都不带的当回调数据，
// 和原插件一样。
func parseMatcher(raw string) (callbackData, buttonText string) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "":
		return "", ""
	case strings.HasPrefix(raw, "data:"):
		return strings.TrimSpace(raw[len("data:"):]), ""
	case strings.HasPrefix(raw, "text:"):
		return "", strings.TrimSpace(raw[len("text:"):])
	}
	return raw, ""
}

func (s *service) remove(ctx context.Context, inv *command.Invocation) error {
	id := inv.Arg(1)
	if id == "" {
		return kit.Usage(inv.Prefix, "checkin del ID")
	}
	found := false
	if err := s.store.Update(func(cfg *config) error {
		index, ok := cfg.find(id)
		if ok {
			cfg.Targets = append(cfg.Targets[:index], cfg.Targets[index+1:]...)
		}
		found = ok
		return nil
	}); err != nil {
		return err
	}
	if !found {
		return notFound(inv, id)
	}
	return inv.Edit(ctx, "✅ 已删除签到目标 "+command.Code(id))
}

func notFound(inv *command.Invocation, id string) error {
	return kit.Failf("没有 ID 是 %s 的签到目标，%scheckin list 可以看全部", id, inv.Prefix)
}

func (s *service) toggle(ctx context.Context, inv *command.Invocation) error {
	id := inv.Arg(1)
	if id == "" {
		return kit.Usage(inv.Prefix, "checkin toggle ID")
	}
	var target signTarget
	found := false
	if err := s.store.Update(func(cfg *config) error {
		index, ok := cfg.find(id)
		if ok {
			cfg.Targets[index].Enabled = !cfg.Targets[index].Enabled
			target = cfg.Targets[index]
		}
		found = ok
		return nil
	}); err != nil {
		return err
	}
	if !found {
		return notFound(inv, id)
	}
	return inv.Edit(ctx, "✅ "+command.Bold(target.Name)+" 已"+kit.OnOffText(target.Enabled))
}

func (s *service) list(ctx context.Context, inv *command.Invocation) error {
	cfg, err := s.store.Read()
	if err != nil {
		return err
	}
	if len(cfg.Targets) == 0 {
		return inv.Edit(ctx, "📅 <b>签到目标</b>\n\n还没有，"+command.Code(inv.Prefix+"checkin add")+" 添加")
	}
	lines := []string{fmt.Sprintf("📅 <b>签到目标</b>（%d/%d 个开启）", len(cfg.enabled()), len(cfg.Targets))}
	for index, target := range cfg.Targets {
		title := fmt.Sprintf("<b>%d. %s</b>", index+1, command.Escape(target.Name))
		if !target.Enabled {
			title += "（关闭）"
		}
		lines = append(lines, "", title, "ID："+command.Code(target.ID)+" · 目标："+command.Code(target.Target),
			"命令："+command.Code(target.Command))
		if matcher := target.matcher(); matcher != "" {
			lines = append(lines, "然后点："+matcher)
		}
	}
	return inv.EditPages(ctx, command.HTMLPages(strings.Join(lines, "\n"), command.PageLimit))
}

func (s *service) test(ctx context.Context, inv *command.Invocation) error {
	id := inv.Arg(1)
	if id == "" {
		return kit.Usage(inv.Prefix, "checkin test ID")
	}
	cfg, err := s.store.Read()
	if err != nil {
		return err
	}
	index, ok := cfg.find(id)
	if !ok {
		return notFound(inv, id)
	}
	target := cfg.Targets[index]
	if !s.running.TryLock() {
		return kit.Fail("正在签到，请稍候")
	}
	defer s.running.Unlock()
	if err := inv.EditText(ctx, kit.Working("正在试签 "+target.Name)); err != nil {
		return err
	}
	reply, err := signOne(ctx, inv.Client, target)
	if err != nil {
		return kit.Fail(target.Name + " 签到失败：" + reason(err))
	}
	return inv.Edit(ctx, "✅ 已签到 "+command.Bold(target.Name)+"\n回复："+command.Escape(brief(reply)))
}

var clockPattern = strings.NewReplacer("：", ":")

// parseClock 读 HH:MM（小时可以是一位，冒号也可以是全角），返回一天里的第几分钟。
func parseClock(value string) (int, bool) {
	hours, minutes, ok := strings.Cut(clockPattern.Replace(strings.TrimSpace(value)), ":")
	if !ok || len(hours) < 1 || len(hours) > 2 || len(minutes) != 2 || !kit.IsDigits(hours) || !kit.IsDigits(minutes) {
		return 0, false
	}
	h, _ := strconv.Atoi(hours)
	m, _ := strconv.Atoi(minutes)
	if h > 23 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

func formatClock(minutes int) string { return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60) }

func (s *service) set(ctx context.Context, inv *command.Invocation) error {
	value := inv.Arg(2)
	switch strings.ToLower(inv.Arg(1)) {
	case "time", "t":
		at, ok := parseClock(value)
		if !ok {
			return kit.Fail("时间要写成 HH:MM，例如 10:30")
		}
		return s.update(ctx, inv, func(cfg *config) { cfg.RunTime = formatClock(at) }, "开始时间已设为 "+formatClock(at))
	case "range", "r":
		if value == "" {
			return s.update(ctx, inv, func(cfg *config) { cfg.RunTimeEnd = "" }, "已改回固定时间签到")
		}
		end, ok := parseClock(value)
		if !ok {
			return kit.Fail("时间要写成 HH:MM，例如 11:30")
		}
		return s.update(ctx, inv, func(cfg *config) { cfg.RunTimeEnd = formatClock(end) }, "结束时间已设为 "+formatClock(end)+"，每天在这段时间里随机挑一分钟签")
	case "delay", "d":
		minutes, err := strconv.Atoi(value)
		if err != nil || minutes < 0 || minutes > 60 {
			return kit.Fail("随机延迟要写 0 到 60 之间的分钟数")
		}
		return s.update(ctx, inv, func(cfg *config) { cfg.RandomDelay = minutes }, "随机延迟已设为 "+strconv.Itoa(minutes)+" 分钟")
	case "bot", "b":
		if strings.EqualFold(value, "off") {
			return s.update(ctx, inv, func(cfg *config) { cfg.BotToken, cfg.PushChatID = "", "" }, "已关闭机器人推送")
		}
		chat := inv.Arg(3)
		if value == "" || chat == "" {
			return kit.Usage(inv.Prefix, "checkin set bot Token 对话 ID|off")
		}
		// 命令消息会被改成下面这句，Token 不会留在对话里。
		return s.update(ctx, inv, func(cfg *config) { cfg.BotToken, cfg.PushChatID = value, chat }, "机器人推送已设置：Token "+mask(value)+"，发到 "+chat)
	case "log", "l":
		if value == "" {
			return kit.Usage(inv.Prefix, "checkin set log 对话|off")
		}
		if strings.EqualFold(value, "off") {
			value = ""
		}
		note := "汇总对话已设为 " + value
		if value == "" {
			note = "已取消汇总对话"
		}
		return s.update(ctx, inv, func(cfg *config) { cfg.LogChat = value }, note)
	}
	return kit.Failf("能设的有 time、range、delay、bot、log，%shelp checkin 看用法", inv.Prefix)
}

// update 改设置并回一句完成提示。改了时间的话，今天挑好的时刻作废，下一分钟按新设置重挑。
func (s *service) update(ctx context.Context, inv *command.Invocation, mutate func(*config), done string) error {
	if err := s.store.Update(func(cfg *config) error {
		before := cfg.RunTime + cfg.RunTimeEnd
		mutate(cfg)
		if cfg.RunTime+cfg.RunTimeEnd != before {
			cfg.PlannedDate, cfg.CurrentRunTime = "", ""
		}
		return nil
	}); err != nil {
		return err
	}
	return inv.EditText(ctx, "✅ "+done)
}

func mask(secret string) string {
	if len(secret) <= 5 {
		return "***"
	}
	return secret[:5] + "…"
}

func (s *service) show(ctx context.Context, inv *command.Invocation) error {
	cfg, err := s.store.Read()
	if err != nil {
		return err
	}
	schedule := "每天 " + command.Code(cfg.RunTime) + "（北京时间）"
	if cfg.RunTimeEnd != "" {
		schedule = "每天 " + command.Code(cfg.RunTime+" ~ "+cfg.RunTimeEnd) + " 之间随机挑一分钟（北京时间）"
	}
	today := dateOf(time.Now())
	status := "今天还没签"
	switch {
	case cfg.LastRunDate == today:
		status = "今天已经签过"
	case cfg.PlannedDate == today && cfg.CurrentRunTime != "":
		status = "今天在 " + command.Code(cfg.CurrentRunTime) + " 签"
	}
	push := "未设置"
	if cfg.BotToken != "" && cfg.PushChatID != "" {
		push = "Token " + command.Code(mask(cfg.BotToken)) + "，发到 " + command.Code(cfg.PushChatID)
	}
	lines := []string{"📅 <b>签到设置</b>", "",
		"时间：" + schedule,
		"状态：" + status,
		"随机延迟：" + command.Code(strconv.Itoa(cfg.RandomDelay)+" 分钟"),
		"机器人推送：" + push,
		"汇总对话：" + command.Code(kit.OrDefault(cfg.LogChat, "未设置")),
		fmt.Sprintf("签到目标：%d/%d 个开启", len(cfg.enabled()), len(cfg.Targets)),
	}
	return inv.Edit(ctx, strings.Join(lines, "\n"))
}

// pick 挑某一天的执行时刻：没设结束时间就是开始时间；设了就在开始到结束之间随机挑一分钟，
// 结束早于开始表示跨午夜。random(n) 返回 [0, n) 里的一个数。
func pick(cfg config, random func(int) int) string {
	start, ok := parseClock(cfg.RunTime)
	if !ok {
		start = 10 * 60
	}
	end, ok := parseClock(cfg.RunTimeEnd)
	if !ok {
		return formatClock(start)
	}
	if end <= start {
		end += 24 * 60
	}
	return formatClock((start + random(end-start)) % (24 * 60))
}

// schedule 每到整分钟看一次该不该签到。
//
// 原插件每分钟都重挑一次随机时刻，再看现在是不是刚好是那一分钟：等于每分钟抽一次签，
// 一段时间抽下来一次都没中的机率大约三分之一，那天就没签。这里每天只挑一次、存下来，
// 到了那一刻或者更晚（服务停过）就签。
func (s *service) schedule(ctx context.Context, client *bot.Client) {
	for {
		now := time.Now()
		if kit.Sleep(ctx, now.Truncate(time.Minute).Add(time.Minute).Sub(now)) != nil {
			return
		}
		s.tick(ctx, client, time.Now())
	}
}

func (s *service) tick(ctx context.Context, client *bot.Client, now time.Time) {
	today := dateOf(now)
	cfg, err := s.store.Read()
	if err != nil || cfg.LastRunDate == today || len(cfg.enabled()) == 0 {
		return
	}
	if cfg.PlannedDate != today || cfg.CurrentRunTime == "" {
		cfg.PlannedDate, cfg.CurrentRunTime = today, pick(cfg, rand.IntN)
		planned := cfg
		kit.Warn(s.a, "checkin.save_failed", s.store.Update(func(stored *config) error {
			stored.PlannedDate, stored.CurrentRunTime = planned.PlannedDate, planned.CurrentRunTime
			return nil
		}))
	}
	at, _ := parseClock(cfg.CurrentRunTime)
	local := now.In(shanghai)
	if local.Hour()*60+local.Minute() < at {
		return
	}
	if cfg.RandomDelay > 0 && kit.Sleep(ctx, time.Duration(rand.Int64N(int64(cfg.RandomDelay)*int64(time.Minute)))) != nil {
		return
	}
	if !s.running.TryLock() {
		return
	}
	defer s.running.Unlock()
	// 等延迟的时候可能有人手动签过、改过设置，按最新的来。
	if cfg, err = s.store.Read(); err != nil || cfg.LastRunDate == today {
		return
	}
	targets := cfg.enabled()
	if len(targets) == 0 {
		return
	}
	client.Logger().Info("checkin.start", "targets", len(targets), "planned", cfg.CurrentRunTime)
	summary := runAll(ctx, client, targets, "定时")
	kit.Warn(s.a, "checkin.save_failed", s.store.Update(func(cfg *config) error { cfg.LastRunDate = today; return nil }))
	if err := deliver(ctx, client, cfg, summary, nil); err != nil {
		client.Logger().Warn("checkin.report_failed", "error", err.Error())
	}
}
