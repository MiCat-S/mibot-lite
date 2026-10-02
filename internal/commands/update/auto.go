package update

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// autoConfig 是自动更新的设置，存在 data/update.json。
type autoConfig struct {
	Enabled bool `json:"enabled"`
	// Time 是每天检查的时间，北京时间 HH:MM。
	Time string `json:"time"`
	// CheckedDate 是最近一次到点检查的日期（北京时间 2006-01-02）。同一天只查一次，
	// 存下来是为了自动更新重启回来之后不再查一遍。
	CheckedDate string `json:"checkedDate,omitempty"`
}

func autoDefaults() autoConfig { return autoConfig{Enabled: true, Time: "04:00"} }

func autoSummary(cfg autoConfig) string {
	if !cfg.Enabled {
		return "关闭"
	}
	return "开启，每天 " + command.Code(cfg.Time) + "（北京时间）检查"
}

// shanghai 是自动更新看时间用的时区，和 .checkin 一样固定是北京时间。
var shanghai = func() *time.Location {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		panic(err)
	}
	return location
}()

// maxWait 是到点后等命令跑完的最长时间。还不空就改天：自动更新不该把正在跑的
// .da、.sum 之类掐断。
const maxWait = 2 * time.Hour

type autoAction int

const (
	autoIdle   autoAction = iota // 没开、今天查过或者还没到点
	autoWait                     // 到点了，但还有命令在跑
	autoGiveUp                   // 等了 maxWait 还不空，今天不查了
	autoCheck                    // 现在检查
)

// decide 看这一分钟该做什么，返回动作和北京时间的今天。过了检查时间才启动（比如服务
// 停过）的，当天补查一次。
func decide(cfg autoConfig, now time.Time, running int) (autoAction, string) {
	local := now.In(shanghai)
	today := local.Format("2006-01-02")
	if !cfg.Enabled || cfg.CheckedDate == today {
		return autoIdle, today
	}
	at, ok := parseClock(cfg.Time)
	if !ok {
		at = 4 * 60
	}
	minute := local.Hour()*60 + local.Minute()
	switch {
	case minute < at:
		return autoIdle, today
	case running == 0:
		return autoCheck, today
	case time.Duration(minute-at)*time.Minute >= maxWait:
		return autoGiveUp, today
	}
	return autoWait, today
}

// strictlyNewer 判断 latest 是不是比 current 新。自动更新只往前走：本地构建的 dev 版、
// 或者发布页上的版本比现在的还旧，都不动。手动的 .update run 仍按「不一样就装」。
func strictlyNewer(current, latest string) bool {
	have, ok := versionParts(current)
	if !ok {
		return false
	}
	want, ok := versionParts(latest)
	if !ok {
		return false
	}
	for index := range have {
		if want[index] != have[index] {
			return want[index] > have[index]
		}
	}
	return false
}

// versionParts 读 v1.2.3 或 1.2.3（后面带 -rc1 之类的不认）。
func versionParts(value string) ([3]int, bool) {
	var parts [3]int
	fields := strings.Split(normalizeVersion(value), ".")
	if len(fields) != 3 {
		return parts, false
	}
	for index, field := range fields {
		number, err := strconv.Atoi(field)
		if err != nil || number < 0 {
			return parts, false
		}
		parts[index] = number
	}
	return parts, true
}

// parseClock 读 HH:MM（小时可以是一位），返回一天里的第几分钟。
func parseClock(value string) (int, bool) {
	hours, minutes, ok := strings.Cut(strings.ReplaceAll(strings.TrimSpace(value), "：", ":"), ":")
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

// configureAuto 处理 .update auto：看设置、开关、改时间。
func (u *updater) configureAuto(ctx context.Context, inv *command.Invocation) error {
	switch action := strings.ToLower(inv.Arg(1)); action {
	case "":
		cfg, err := u.settings.Read()
		if err != nil {
			return err
		}
		return inv.Edit(ctx, "⬆️ <b>自动更新</b>\n"+autoSummary(cfg))
	case "on", "off":
		if err := u.settings.Update(func(cfg *autoConfig) error { cfg.Enabled = action == "on"; return nil }); err != nil {
			return err
		}
		return inv.EditText(ctx, "✅ 自动更新已"+kit.OnOffText(action == "on"))
	case "time":
		at, ok := parseClock(inv.Arg(2))
		if !ok {
			return kit.Fail("时间要写成 HH:MM，例如 04:00")
		}
		value := fmt.Sprintf("%02d:%02d", at/60, at%60)
		if err := u.settings.Update(func(cfg *autoConfig) error { cfg.Time = value; return nil }); err != nil {
			return err
		}
		return inv.EditText(ctx, "✅ 自动更新的检查时间已设为 "+value+"（北京时间）；今天已经查过的话，从明天开始")
	}
	return kit.Usage(inv.Prefix, "update auto [on|off|time HH:MM]")
}

// schedule 每到整分钟看一次该不该自动更新。
func (u *updater) schedule(ctx context.Context, client *bot.Client) {
	for {
		now := time.Now()
		if kit.Sleep(ctx, now.Truncate(time.Minute).Add(time.Minute).Sub(now)) != nil {
			return
		}
		u.tick(ctx, client, time.Now())
	}
}

func (u *updater) tick(ctx context.Context, client *bot.Client, now time.Time) {
	cfg, err := u.settings.Read()
	if err != nil {
		return
	}
	action, today := decide(cfg, now, u.a.Registry.Running())
	switch action {
	case autoIdle, autoWait:
		return
	case autoGiveUp:
		client.Logger().Info("update.auto_skipped", "reason", "commands still running", "waited", maxWait.String())
		u.markChecked(today)
		return
	}
	// 手动的 .update 正在跑：下一分钟再看。
	if !busy.TryLock() {
		return
	}
	defer busy.Unlock()
	u.markChecked(today)
	latest, err := fetchRelease(ctx, u.repo)
	if err != nil {
		client.Logger().Warn("update.auto_check_failed", "error", err.Error())
		return
	}
	if !strictlyNewer(u.a.Version, latest.TagName) {
		client.Logger().Info("update.auto_up_to_date", "current", kit.Version(u.a), "latest", latest.TagName)
		return
	}
	u.autoInstall(ctx, client, latest)
}

func (u *updater) markChecked(today string) {
	kit.Warn(u.a, "update.save_failed", u.settings.Update(func(cfg *autoConfig) error { cfg.CheckedDate = today; return nil }))
}

// autoInstall 先在收藏夹发一条消息说要更新，进度和结果都改在这条上；装好后重启，
// 重启回来由 restart 的回执把它改成「已更新到 vX」。
func (u *updater) autoInstall(ctx context.Context, client *bot.Client, latest *release) {
	logger := client.Logger()
	if u.restarter == nil {
		return
	}
	binary, err := executablePath()
	if err != nil {
		logger.Warn("update.locate_failed", "error", err.Error())
		return
	}
	self := &tg.InputPeerSelf{}
	const header = "⬆️ <b>自动更新</b>\n"
	id, err := client.SendHTML(ctx, self, header+"⏳ 正在从 "+command.Code(kit.Version(u.a))+" 更新到 "+command.Code(latest.TagName)+"…", bot.SendOptions{})
	if err != nil {
		// 没法告诉本人，就不悄悄重启。
		logger.Warn("update.auto_notice_failed", "error", err.Error())
		return
	}
	logger.Info("update.auto_start", "current", kit.Version(u.a), "latest", latest.TagName)
	show := func(text string) error { return client.EditMessage(ctx, self, id, header+text, false) }
	progress := func(text string) error { return show("⏳ " + text) }
	if err := u.install(ctx, logger, latest, binary, progress); err != nil {
		reason, ok := kit.IsUserError(err)
		if !ok {
			reason = command.Brief(err)
		}
		logger.Warn("update.auto_failed", "latest", latest.TagName, "error", err.Error())
		_ = show("❌ " + command.Escape(reason))
		return
	}
	if err := progress("已安装 " + command.Code(latest.TagName) + "，正在重启…"); err != nil {
		logger.Warn("update.auto_notice_failed", "error", err.Error())
	}
	if err := u.restarter.Schedule(ctx, self, strconv.FormatInt(client.SelfID(), 10), id, "auto-update"); err != nil {
		logger.Error("update.auto_restart_failed", "error", err.Error())
		_ = show("❌ 已安装 " + command.Code(latest.TagName) + "，但重启没提交成功，可以手动重启服务\n状态：\n" + u.restarter.Status(ctx))
	}
}
