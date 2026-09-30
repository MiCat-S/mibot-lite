// Package cleanmember 实现 .clean_member：按最后上线时间、发言情况或账号状态搜索群组成员，
// 出一份 CSV 报告，或者直接移出。搜索结果缓存 24 小时，每份一个文件，放在 data/clean_member/ 下。
//
// 包名不带下划线（Go 的习惯），命令名照 v2 还是 clean_member。
package cleanmember

import (
	"context"
	"math/rand/v2"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// service 是 .clean_member 的状态。等待和时间都可以换掉，测试里不用真等。
type service struct {
	cache cache
	now   func() time.Time
	// pause 是移出过程里的等待（封禁和解封之间、两次移出之间、限流），ctx 结束时提前返回。
	pause func(ctx context.Context, d time.Duration) error
	// gap 是两次移出之间的间隔，同 v2 是 1 到 1.5 秒。
	gap func() time.Duration
	// progressEvery 是进度提示的最短间隔。v2 每翻一页改一次消息，只搜上线时间时几秒就翻几十页，
	// 改得太勤会被限流。
	progressEvery time.Duration
}

// Register 注册 .clean_member。扫大群、逐个搜发言可能要几个小时，所以不设时限；重启会中断它。
func Register(a *app.App) {
	s := &service{cache: cache{dir: filepath.Join(a.DataDir(), "clean_member")}, now: time.Now, pause: kit.Sleep,
		gap:           func() time.Duration { return time.Second + rand.N(500*time.Millisecond) },
		progressEvery: 5 * time.Second}
	a.Registry.Register(&command.Command{Name: "clean_member", Group: command.GroupAdmin, Description: "搜索或清理群组成员",
		Usage: "模式 [参数] [chat:对话 ID] [limit:数量] [search]", Help: help, Timeout: command.NoTimeout, Handle: s.handle})
}

func help(prefix string) string {
	p := command.Escape(prefix)
	c := func(text string) string { return "<code>" + p + "clean_member" + text + "</code>" }
	return "🧽 <b>群成员搜索与清理</b>\n\n" +
		"按最后上线时间、发言情况或账号状态找出群组成员，生成 CSV 报告，或者直接移出。\n\n" +
		"<b>格式</b>\n" + c(" 模式 [参数] [chat:对话 ID] [limit:数量] [search]") + "\n\n" +
		"<b>模式</b>\n" +
		"• <code>1 天数</code> 最后上线超过这么多天；看不到上线时间的跳过\n" +
		"• <code>2 天数</code> 这么多天里没有发言\n" +
		"• <code>3 条数</code> 能搜到的发言少于这么多条\n" +
		"• <code>4</code> 已注销的账号\n" +
		"• <code>5</code> 所有普通成员\n" +
		"模式 1、2 的天数少于 7 时按 7 天算。\n\n" +
		"<b>选项</b>\n" +
		"• <code>search</code> 只搜索、出报告，不移出；不写就直接移出\n" +
		"• <code>limit:50</code> 最多移出 50 人，只对移出起作用\n" +
		"• <code>chat:-1001234567890</code> 指定群组，也可以写 @用户名；不写就是当前群组\n\n" +
		"<b>示例</b>\n" +
		"• " + c(" 1 30 search") + " 看看超过 30 天没上线的成员\n" +
		"• " + c(" 1 30 limit:10") + " 重新查一遍，移出其中 10 人\n" +
		"• " + c(" 4 chat:-1001234567890 search") + " 搜索指定群组里的注销账号\n" +
		"• " + c(" 3 5 search") + " 搜索发言少于 5 条的成员\n\n" +
		"<b>说明</b>\n" +
		"• 移出要群主身份或封禁成员权限；搜索只要看得到成员列表\n" +
		"• 管理员和你自己不会被列入\n" +
		"• 移出是封禁后马上解封，对方还能重新加入\n" +
		"• 同一群组、模式和参数的搜索结果缓存 24 小时；移出总是重新查，查完清掉这个群组的缓存\n" +
		"• 报告以 CSV 文件发到收藏夹，不会出现在群组里\n" +
		"• 上线时间和发言统计受对方隐私设置和 Telegram 的可见范围限制，成员很多时 Telegram 可能只给出一部分\n" +
		"• 频道的订阅者不能发言，模式 2、3 只能用于群组\n" +
		"• 参数写错会直接报错，不会当成移出执行"
}

// modeName 是模式的说法，报告和提示里用。
func modeName(mode string, n int) string {
	count := strconv.Itoa(n)
	switch mode {
	case "1":
		return "未上线超过 " + count + " 天的用户"
	case "2":
		return "未发言超过 " + count + " 天的用户"
	case "3":
		return "发言少于 " + count + " 条的用户"
	case "4":
		return "已注销的账号"
	case "5":
		return "所有普通成员"
	}
	return "未知"
}

// request 是解析好的一次调用。
type request struct {
	mode   string
	day    int
	search bool
	// limit 是最多移出的人数，0 表示不限。
	limit int
	// chat 是 chat: 指定的群组，空表示当前对话。
	chat string
}

// option 认 key:value 形式的选项，冒号全角半角都行：中文输入法常打出全角的。
func option(arg, key string) (string, bool) {
	lower := strings.ToLower(arg)
	for _, colon := range []string{":", "："} {
		if strings.HasPrefix(lower, key+colon) {
			return arg[len(key)+len(colon):], true
		}
	}
	return "", false
}

// parseRequest 解析参数，规则同 v2：第一个是模式，模式 1、2、3 的第二个是天数或条数，
// 其余是 search、limit:数量、chat:对话。
//
// 和 v2 不同，不认识的参数直接报错。v2 忽略它们，于是把 search 拼错成 serach 时，
// 本来只想搜一下，结果真把人移出去了。
func parseRequest(args []string, prefix string) (request, error) {
	req := request{mode: strings.ToLower(args[0])}
	if len(req.mode) != 1 || req.mode < "1" || req.mode > "5" {
		return request{}, kit.Failf("没有模式 %s，模式是 1 到 5，%sclean_member help 看说明", args[0], prefix)
	}
	rest := args[1:]
	if req.mode <= "3" {
		what := "天数"
		if req.mode == "3" {
			what = "发言条数"
		}
		if len(rest) == 0 {
			return request{}, kit.Failf("模式 %s 后面要写%s", req.mode, what)
		}
		day, err := strconv.Atoi(rest[0])
		if err != nil || day < 1 || !kit.IsDigits(rest[0]) {
			return request{}, kit.Failf("%s要是正整数，%s 不是", what, rest[0])
		}
		if req.mode != "3" {
			day = max(day, 7)
		}
		req.day, rest = day, rest[1:]
	}
	for _, arg := range rest {
		if strings.EqualFold(arg, "search") {
			req.search = true
			continue
		}
		if value, ok := option(arg, "limit"); ok {
			limit, err := strconv.Atoi(value)
			if err != nil || limit < 1 || !kit.IsDigits(value) {
				return request{}, kit.Fail("limit 要是正整数，如 limit:50")
			}
			req.limit = limit
			continue
		}
		if value, ok := option(arg, "chat"); ok {
			if value == "" {
				return request{}, kit.Fail("chat: 后面要写对话 ID 或 @用户名")
			}
			req.chat = value
			continue
		}
		return request{}, kit.Failf("不认识的参数 %s，%sclean_member help 看说明", arg, prefix)
	}
	return req, nil
}

func (s *service) handle(ctx context.Context, inv *command.Invocation) error {
	if len(inv.Args) == 0 {
		return inv.EditPages(ctx, command.HTMLPages(help(inv.Prefix), command.PageLimit))
	}
	req, err := parseRequest(inv.Args, inv.Prefix)
	if err != nil {
		return err
	}
	client := inv.Client
	g, err := resolveGroup(ctx, client, inv.Message, req.chat)
	if err != nil {
		return err
	}
	if g.broadcast && (req.mode == "2" || req.mode == "3") {
		return kit.Fail("频道的订阅者不能发言，模式 2、3 只能用于群组")
	}
	if !req.search {
		allowed, err := s.canBan(ctx, client, g)
		if err != nil {
			return kit.FailWith("查不到你在这个群组的权限", err)
		}
		if !allowed {
			return kit.Fail("权限不足：移出成员要群主身份或封禁成员权限")
		}
	}
	key := cacheKey(g.chatID, req.mode, req.day)
	if req.search {
		if cached, ok := s.cached(key); ok {
			return s.finishCached(ctx, inv, cached)
		}
	}
	if err := inv.EditText(ctx, kit.Working("正在"+verb(req)+" "+g.title+" 里"+modeName(req.mode, req.day))); err != nil {
		return err
	}
	admins, err := s.admins(ctx, client, g)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// 搜索时少跳过几个管理员无妨，报告里多几行；移出时不行，同 v2。
		if !req.search {
			return kit.FailWith("读不到完整的管理员列表，已停止清理", err)
		}
		inv.Log.Warn("clean_member.admins_failed", "error", err.Error())
		admins = map[int64]bool{}
	}
	out, err := s.scan(ctx, inv, g, req, admins)
	if err != nil {
		return err
	}
	now := s.now()
	data := cacheData{ChatID: g.chatID, ChatTitle: g.title, Mode: req.mode, Day: req.day, SearchTime: isoTime(now),
		TotalFound: len(out.found), Users: out.found, ExpiresAt: now.Add(cacheTTL).UnixMilli()}
	if data.Users == nil {
		data.Users = []userInfo{}
	}
	s.remember(inv, key, data, req.search)
	return s.finish(ctx, inv, data, req, out)
}

func verb(req request) string {
	if req.search {
		return "搜索"
	}
	return "清理"
}

// cached 取一份还没过期的搜索结果。
func (s *service) cached(key string) (cacheData, bool) {
	return s.cache.load(key, s.now())
}

// remember 存下这次的结果。v2 移出之后也把名单存成缓存，24 小时内再搜会拿到一份
// 已经移出去的人；这里只存搜索的结果，移出之后把这个群组的缓存都清掉——人少了，
// 别的模式的结果也跟着变了。存不进去不影响这次的结果，只记日志。
func (s *service) remember(inv *command.Invocation, key string, data cacheData, search bool) {
	var err error
	if search {
		err = s.cache.save(key, data, s.now())
	} else {
		err = s.cache.clearChat(data.ChatID)
	}
	if err != nil {
		inv.Log.Warn("clean_member.cache_write_failed", "error", err.Error())
	}
}

// outcome 是一次扫描的结果。
type outcome struct {
	scanned int
	found   []userInfo
	failed  []userInfo
	removed int
	// skipped 是模式 2、3 里查不到发言记录、只好跳过的人数。
	skipped int
	// limited 表示移出的人数到了 limit，后面的成员没有再查。
	limited bool
	reasons map[string]int
}

// scan 翻完成员列表，找出符合条件的人；不是搜索时顺手移出。
func (s *service) scan(ctx context.Context, inv *command.Invocation, g group, req request, admins map[int64]bool) (outcome, error) {
	client := inv.Client
	out := outcome{reasons: map[string]int{}}
	seen := map[int64]bool{}
	lastProgress := s.now()
	progress := func() {
		if s.now().Sub(lastProgress) < s.progressEvery {
			return
		}
		lastProgress = s.now()
		_ = inv.EditText(ctx, progressText(g, req, out))
	}
	for offset := 0; offset <= maxOffset; {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		members, count, err := s.page(ctx, client, g, offset)
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			return out, kit.FailWith("读取成员列表失败", err)
		}
		removedHere := 0
		for _, m := range members {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			if seen[m.user.ID] {
				continue
			}
			seen[m.user.ID] = true
			out.scanned++
			if m.admin || admins[m.user.ID] || m.user.ID == client.SelfID() || m.user.Self {
				continue
			}
			hit, err := s.matches(ctx, client, g, req, m.user)
			if err != nil {
				if ctx.Err() != nil {
					return out, ctx.Err()
				}
				out.skipped++
				continue
			}
			if !hit {
				continue
			}
			info := infoOf(m.user)
			out.found = append(out.found, info)
			if !req.search {
				if err := s.remove(ctx, client, g, m.user); err != nil {
					if ctx.Err() != nil {
						return out, ctx.Err()
					}
					info.ErrorMessage = failureReason(err)
					out.failed = append(out.failed, info)
					out.reasons[info.ErrorMessage]++
				} else {
					out.removed++
					removedHere++
					if req.limit > 0 && out.removed >= req.limit {
						out.limited = true
						return out, nil
					}
					if err := s.pause(ctx, s.gap()); err != nil {
						return out, err
					}
				}
			}
			progress()
		}
		if count < pageSize || g.channel == nil {
			break
		}
		// 移出的人不在列表里了，后面的人往前挪了这么多位。v2 照样加 200，会漏掉这么多人。
		// 挪过头读到重复的人也没关系，seen 会跳过。
		offset += count - removedHere
		progress()
	}
	return out, nil
}

// progressText 是进行中的提示。
func progressText(g group, req request, out outcome) string {
	text := kit.Working("正在"+verb(req)+" "+g.title+" 里"+modeName(req.mode, req.day)) +
		"\n已扫描 " + strconv.Itoa(out.scanned) + " 人，符合条件 " + strconv.Itoa(len(out.found)) + " 人"
	if !req.search {
		text += "，已移出 " + strconv.Itoa(out.removed) + " 人"
	}
	return text
}

// sendReport 把报告发到收藏夹。不管在哪个对话里执行都只发到收藏夹：名单里有成员的用户名和
// 上线时间，不该出现在群里。v2 把文件存在插件目录、回执里写文件名，这边主机上的路径不进聊天。
func (s *service) sendReport(ctx context.Context, client *bot.Client, data cacheData, failed bool) error {
	caption := "🧽 <b>群组清理报告</b>\n"
	if failed {
		caption = "🧽 <b>清理失败名单</b>\n"
	}
	caption += command.Escape(data.ChatTitle) + "\n" + command.Escape(modeName(data.Mode, data.Day)) + "：" + strconv.Itoa(data.TotalFound) + " 人"
	return client.SendDocument(ctx, &tg.InputPeerSelf{}, csvReport(data, failed),
		bot.MediaOptions{Name: reportName(data, failed, s.now()), MimeType: "text/csv", Caption: caption, ForceDocument: true})
}

// reportLine 发报告，返回结果里说明报告去向的那一行。没有符合条件的人时不发空报告。
func (s *service) reportLine(ctx context.Context, inv *command.Invocation, data cacheData) string {
	if data.TotalFound == 0 {
		return "没有符合条件的成员，没有生成报告"
	}
	if err := s.sendReport(ctx, inv.Client, data, false); err != nil {
		if ctx.Err() == nil {
			inv.Log.Warn("clean_member.report_failed", "error", err.Error())
		}
		return "⚠️ 报告没能发到收藏夹（" + command.Escape(command.Brief(err)) + "）"
	}
	return "报告已发到收藏夹"
}

// finishCached 用缓存的搜索结果作答，同 v2。
func (s *service) finishCached(ctx context.Context, inv *command.Invocation, data cacheData) error {
	when := data.SearchTime
	if parsed, err := time.Parse(time.RFC3339, data.SearchTime); err == nil {
		when = parsed.UTC().Format("2006-01-02 15:04") + " UTC"
	}
	lines := []string{
		"✅ 已搜索完 " + command.Escape(data.ChatTitle) + " 里" + command.Escape(modeName(data.Mode, data.Day)) + "（用的是 " + command.Escape(when) + " 的缓存）",
		"符合条件 " + strconv.Itoa(data.TotalFound) + " 人",
		s.reportLine(ctx, inv, data),
	}
	return inv.Edit(ctx, strings.Join(lines, "\n"))
}

// finish 发报告并给出结果。
func (s *service) finish(ctx context.Context, inv *command.Invocation, data cacheData, req request, out outcome) error {
	lines := []string{"✅ 已" + verb(req) + "完 " + command.Escape(data.ChatTitle) + " 里" + command.Escape(modeName(req.mode, req.day)),
		"扫描 " + strconv.Itoa(out.scanned) + " 人，符合条件 " + strconv.Itoa(len(out.found)) + " 人"}
	if !req.search {
		removed := "已移出 " + strconv.Itoa(out.removed) + " 人"
		if len(out.found) > 0 {
			removed += "（成功率 " + strconv.FormatFloat(float64(out.removed)*100/float64(len(out.found)), 'f', 1, 64) + "%）"
		}
		lines = append(lines, removed)
	}
	if out.limited {
		lines = append(lines, "已达到 limit:"+strconv.Itoa(req.limit)+"，后面的成员没有再查")
	}
	if out.skipped > 0 {
		lines = append(lines, "⚠️ "+strconv.Itoa(out.skipped)+" 人查不到发言记录，已跳过")
	}
	lines = append(lines, s.reportLine(ctx, inv, data))
	if len(out.failed) > 0 {
		lines = append(lines, "⚠️ 移出失败 "+strconv.Itoa(len(out.failed))+" 人（"+command.Escape(topReasons(out.reasons, 3))+"）")
		failed := data
		failed.TotalFound, failed.Users = len(out.failed), out.failed
		if err := s.sendReport(ctx, inv.Client, failed, true); err != nil {
			if ctx.Err() == nil {
				inv.Log.Warn("clean_member.failed_report_failed", "error", err.Error())
			}
		} else {
			lines = append(lines, "失败名单另发了一份到收藏夹")
		}
	}
	inv.Log.Info("clean_member.done", "chat", data.ChatID, "mode", req.mode, "search", req.search,
		"scanned", out.scanned, "found", len(out.found), "removed", out.removed, "failed", len(out.failed))
	return inv.Edit(ctx, strings.Join(lines, "\n"))
}

// topReasons 把失败原因按次数从多到少排，取前 n 个，写成「原因×次数」。
func topReasons(reasons map[string]int, n int) string {
	keys := make([]string, 0, len(reasons))
	for reason := range reasons {
		keys = append(keys, reason)
	}
	sort.Slice(keys, func(i, j int) bool {
		if reasons[keys[i]] != reasons[keys[j]] {
			return reasons[keys[i]] > reasons[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	for index, reason := range keys {
		keys[index] = reason + "×" + strconv.Itoa(reasons[reason])
	}
	return strings.Join(keys, "、")
}
