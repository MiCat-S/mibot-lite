// Package search 实现 .so（也叫 .search）：在添加的频道、群组里按关键词找视频，
// 或者随机挑一段 20 秒到 3 分钟的视频（kkp），转发到当前对话；禁止转发或要防剧透时
// 下载后重新上传。频道源、默认频道和广告过滤词存在 data/search.json。
package search

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// channel 是一个频道源。Handle 是添加时写的原样（@name、链接或 ID），搜索时按它重新解析；
// LinkedGroup 是频道绑定的讨论组（@用户名），视频常常发在评论里。
type channel struct {
	Title       string `json:"title"`
	Handle      string `json:"handle"`
	LinkedGroup string `json:"linkedGroup,omitempty"`
}

// config 是 data/search.json，沿用 MiBox search 插件 v2 的 assets/search/channel_search_config.json，
// 可以原样复制过来。DefaultChannel 没有时是 null。
type config struct {
	SchemaVersion  int       `json:"schemaVersion"`
	DefaultChannel *string   `json:"defaultChannel"`
	ChannelList    []channel `json:"channelList"`
	AdFilters      []string  `json:"adFilters"`
}

// defaultFilters 是没有配置文件时的广告过滤词，与 MiBox 一致。
var defaultFilters = []string{"广告", "推广", "赞助", "合作", "代理", "招商", "加盟", "投资", "理财", "贷款", "借钱", "网贷",
	"信用卡", "pos机", "刷单", "兼职", "副业", "微商", "代购", "淘宝", "拼多多", "京东", "直播带货", "优惠券", "返利", "红包",
	"现金", "提现", "充值", "游戏币", "点卡", "彩票", "博彩", "赌博", "六合彩", "时时彩", "北京赛车", "股票", "期货", "外汇",
	"数字货币", "比特币", "挖矿", "保险", "医疗", "整容", "减肥", "丰胸", "壮阳", "药品", "假货", "高仿", "A货", "精仿", "原单",
	"尾单", "办证", "刻章", "发票", "学历", "文凭", "证书", "黑客", "破解", "外挂", "木马", "病毒", "盗号", "vpn", "翻墙",
	"代理ip", "科学上网", "梯子"}

func defaults() config {
	return config{SchemaVersion: 1, ChannelList: []channel{}, AdFilters: slices.Clone(defaultFilters)}
}

// maxImportBytes 是导入的备份文件上限，与 MiBox 一致。
const maxImportBytes = 256 << 10

type service struct {
	store   *store.Store[config]
	sources resolver
	partial string
	gap     time.Duration
	pick    func(n int) int
	// lookupGap 是 add 时两次（没缓存的）频道源解析之间的间隔。
	lookupGap time.Duration
	resolve   func(ctx context.Context, client *bot.Client, handle string) (*source, error)
	upload    func(ctx context.Context, inv *command.Invocation, picked candidate, text string, spoiler bool) error
	linkedOf  func(ctx context.Context, client *bot.Client, channel *source) string
}

func help(prefix string) string {
	p := command.Escape(prefix)
	item := func(args, text string) string {
		return "• <code>" + p + "so " + command.Escape(args) + "</code> " + text + "\n"
	}
	return "🎞 <b>频道视频搜索</b>\n\n在添加的频道里找视频，发到当前对话。\n\n" +
		"<b>搜索</b>\n" +
		item("关键词", "按关键词搜索，不限大小和时长，先搜默认频道") +
		item("kkp", "随机挑一段 20 秒到 3 分钟的视频") +
		"• 加 <code>-s</code> 下载后以防剧透方式发送，加 <code>-r</code> 从匹配结果里随机选\n\n" +
		"<b>频道源</b>\n" +
		item("add 频道链接或@用户名…", "添加频道、群组或讨论组，多个用空格或 \\ 分隔") +
		item("del 频道|序号…", "移除频道源，del all 全部移除") +
		item("default 频道", "设为默认频道，default d 取消默认") +
		item("list", "查看频道源") +
		item("export", "导出频道源文件") +
		item("import", "回复备份文件导入") +
		"\n<b>广告过滤</b>\n" +
		item("ad add 关键词…", "添加过滤词") +
		item("ad del 关键词…", "删除过滤词") +
		item("ad list", "查看过滤词") +
		"\n先转发原消息；对话禁止转发或用了 -s 时下载再发送，上限 2 GB。<code>" + p + "search</code> 与 <code>" + p + "so</code> 用法相同。"
}

// Register 注册 .so，别名 .search。
func Register(a *app.App) {
	s := &service{store: kit.NewStore(a, "search.json", defaults), partial: filepath.Join(a.Root, "search", ".partial"),
		gap: 750 * time.Millisecond, lookupGap: time.Second, pick: rand.IntN, linkedOf: linkedGroup}
	s.resolve = s.sources.resolve
	s.upload = func(ctx context.Context, inv *command.Invocation, picked candidate, text string, spoiler bool) error {
		return reupload(ctx, inv, s.partial, picked, text, spoiler)
	}
	a.Registry.Register(&command.Command{Name: "so", Aliases: []string{"search"}, Group: command.GroupMedia,
		Description: "从频道搜索并发送视频", Usage: "关键词|kkp [-s] [-r]", Help: help, FreeText: true,
		// 下载再上传一个 2 GB 的视频要很久，默认的 5 分钟不够。
		Timeout: 30 * time.Minute,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			return s.handle(ctx, inv)
		}})
}

func (s *service) read() (config, error) {
	cfg, err := s.store.Read()
	if cfg.ChannelList == nil {
		cfg.ChannelList = []channel{}
	}
	return cfg, err
}

func (s *service) handle(ctx context.Context, inv *command.Invocation) error {
	sub := strings.ToLower(inv.Arg(0))
	switch sub {
	case "":
		return inv.EditPages(ctx, command.HTMLPages(help(inv.Prefix), command.PageLimit))
	case "help", "h":
		if len(inv.Args) == 1 {
			return inv.EditPages(ctx, command.HTMLPages(help(inv.Prefix), command.PageLimit))
		}
	case "add":
		return s.add(ctx, inv, splitHandles(inv.Rest(1)))
	case "del":
		return s.remove(ctx, inv)
	case "default":
		return s.setDefault(ctx, inv)
	case "list":
		return s.list(ctx, inv)
	case "export":
		return s.export(ctx, inv)
	case "import":
		return s.importFile(ctx, inv)
	case "ad":
		return s.ad(ctx, inv)
	case "kkp":
		return s.search(ctx, inv, parseOptions(inv.Args[1:], true))
	}
	return s.search(ctx, inv, parseOptions(inv.Args, false))
}

var handleSeparators = regexp.MustCompile(`[\s\p{Z}\x{FEFF}\\]+`)

// splitHandles 把 add 的参数拆成频道源。MiBox 只认 \ 分隔；频道源里不会有空格，
// 这里空格也算分隔。
func splitHandles(raw string) []string {
	var list []string
	for _, item := range handleSeparators.Split(raw, -1) {
		if item = strings.TrimSpace(item); item != "" {
			list = append(list, item)
		}
	}
	return list
}

const (
	// maxAddPerCall 是一次 add 或 import 最多处理的频道源数，多出的跳过并说明。
	// 每个没缓存的频道源都要调一次 contacts.resolveUsername，它的限流很严。
	maxAddPerCall = 50
	// listedLines 是结果里最多列出的频道名或失败原因条数，免得一条消息超长。
	listedLines = 20
)

// clipLines 最多留 limit 行，多出的写成「…还有 N 个」。
func clipLines(lines []string, limit int) []string {
	if len(lines) <= limit {
		return lines
	}
	return append(lines[:limit:limit], fmt.Sprintf("…还有 %d 个", len(lines)-limit))
}

// add 解析并添加频道源：频道要是有公开的讨论组，一并记下。第一个添加的频道在还没有
// 默认频道时成为默认频道。格式不对的不去解析；没缓存的两次解析之间停 lookupGap。
func (s *service) add(ctx context.Context, inv *command.Invocation, values []string) error {
	if len(values) == 0 {
		return kit.Usage(inv.Prefix, "so add 频道链接或@用户名…")
	}
	skipped := 0
	if len(values) > maxAddPerCall {
		skipped = len(values) - maxAddPerCall
		values = values[:maxAddPerCall]
	}
	cfg, err := s.read()
	if err != nil {
		return err
	}
	if err := inv.EditText(ctx, kit.Working("正在添加频道")); err != nil {
		return err
	}
	var added []channel
	var failures []string
	exists := func(handle string) bool {
		return slices.ContainsFunc(cfg.ChannelList, func(item channel) bool { return item.Handle == handle }) ||
			slices.ContainsFunc(added, func(item channel) bool { return item.Handle == handle })
	}
	lookups := 0
	for _, value := range values {
		if exists(value) {
			failures = append(failures, value+"：已存在")
			continue
		}
		if !validHandle(value) {
			failures = append(failures, value+"："+errBadHandleText)
			continue
		}
		if !s.sources.has(value) {
			if lookups > 0 {
				if err := kit.Sleep(ctx, s.lookupGap); err != nil {
					return err
				}
			}
			lookups++
		}
		found, err := s.resolve(ctx, inv.Client, value)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			reason := "无法访问"
			if text, ok := kit.IsUserError(err); ok {
				reason = text
			}
			inv.Log.Info("search.add_failed", "handle", value, "error", err.Error())
			failures = append(failures, value+"："+reason)
			continue
		}
		item := channel{Title: kit.OrDefault(found.title, value), Handle: value}
		if found.broadcast && !found.megagroup {
			item.LinkedGroup = s.linkedOf(ctx, inv.Client, found)
		}
		added = append(added, item)
	}
	applied := 0
	if err := s.store.Update(func(cfg *config) error {
		for _, item := range added {
			if slices.ContainsFunc(cfg.ChannelList, func(existing channel) bool { return existing.Handle == item.Handle }) {
				continue
			}
			cfg.ChannelList = append(cfg.ChannelList, item)
			if cfg.DefaultChannel == nil {
				handle := item.Handle
				cfg.DefaultChannel = &handle
			}
			applied++
		}
		cfg.SchemaVersion = 1
		return nil
	}); err != nil {
		return err
	}
	skippedNote := ""
	if skipped > 0 {
		skippedNote = fmt.Sprintf("一次最多处理 %d 个，另外 %d 个没有处理", maxAddPerCall, skipped)
	}
	if applied == 0 {
		reason := "没有添加任何频道：" + strings.Join(clipLines(failures, 5), "；")
		if skippedNote != "" {
			reason += "；" + skippedNote
		}
		return kit.Fail(reason)
	}
	text := kit.Feedback("success", fmt.Sprintf("已添加 %d 个频道", applied), "")
	if len(failures) > 0 {
		text += "\n\n⚠️ 以下频道没有添加\n" + command.Escape(strings.Join(clipLines(failures, listedLines), "\n"))
	}
	if skippedNote != "" {
		text += "\n\n⚠️ " + command.Escape(skippedNote)
	}
	return inv.EditPages(ctx, command.HTMLPages(text, command.PageLimit))
}

// remove 按频道源或序号移除，del all 全部移除。移除的是默认频道时，剩下的第一个成为默认。
func (s *service) remove(ctx context.Context, inv *command.Invocation) error {
	raw := inv.Rest(1)
	if raw == "" {
		return kit.Usage(inv.Prefix, "so del 频道|序号…")
	}
	var removed []string
	if err := s.store.Update(func(cfg *config) error {
		targets := map[string]bool{}
		if strings.EqualFold(raw, "all") {
			for _, item := range cfg.ChannelList {
				targets[item.Handle] = true
			}
		} else {
			for _, token := range splitHandles(raw) {
				if number, err := strconv.Atoi(token); err == nil && number > 0 && number <= len(cfg.ChannelList) {
					token = cfg.ChannelList[number-1].Handle
				}
				targets[token] = true
			}
		}
		kept := make([]channel, 0, len(cfg.ChannelList))
		for _, item := range cfg.ChannelList {
			if targets[item.Handle] {
				removed = append(removed, item.Title)
				s.sources.forget(item.Handle)
				continue
			}
			kept = append(kept, item)
		}
		if len(removed) == 0 {
			return kit.Fail("列表里没有指定的频道或序号")
		}
		cfg.ChannelList = kept
		if cfg.DefaultChannel != nil && targets[*cfg.DefaultChannel] {
			cfg.DefaultChannel = nil
			if len(kept) > 0 {
				handle := kept[0].Handle
				cfg.DefaultChannel = &handle
			}
		}
		return nil
	}); err != nil {
		return err
	}
	text := kit.Feedback("success", fmt.Sprintf("已移除 %d 个频道", len(removed)), "") + "\n" +
		command.Escape("• "+strings.Join(clipLines(removed, listedLines), "\n• "))
	return inv.EditPages(ctx, command.HTMLPages(text, command.PageLimit))
}

// setDefault 设默认频道；default d 取消默认。默认频道要先添加。
func (s *service) setDefault(ctx context.Context, inv *command.Invocation) error {
	raw := inv.Rest(1)
	if raw == "" {
		return kit.Usage(inv.Prefix, "so default 频道|d")
	}
	if err := s.store.Update(func(cfg *config) error {
		if raw == "d" {
			cfg.DefaultChannel = nil
			return nil
		}
		if !slices.ContainsFunc(cfg.ChannelList, func(item channel) bool { return item.Handle == raw }) {
			return kit.Failf("请先用 %sso add 添加这个频道", inv.Prefix)
		}
		cfg.DefaultChannel = &raw
		return nil
	}); err != nil {
		return err
	}
	if raw == "d" {
		return inv.Edit(ctx, kit.Feedback("success", "已取消默认频道", ""))
	}
	return inv.Edit(ctx, kit.Feedback("success", "默认频道已设为 "+raw, ""))
}

func renderList(cfg config, prefix string) string {
	if len(cfg.ChannelList) == 0 {
		return "🎞 <b>搜索频道</b>\n\n还没有添加频道，用 " + command.Code(prefix+"so add") + " 添加"
	}
	lines := make([]string, 0, len(cfg.ChannelList))
	for index, item := range cfg.ChannelList {
		line := strconv.Itoa(index+1) + ". " + command.Escape(item.Title) + " · " + command.Code(item.Handle)
		if cfg.DefaultChannel != nil && *cfg.DefaultChannel == item.Handle {
			line += "（默认）"
		}
		lines = append(lines, line)
	}
	return "🎞 <b>搜索频道</b>\n\n" + strings.Join(lines, "\n")
}

func (s *service) list(ctx context.Context, inv *command.Invocation) error {
	cfg, err := s.read()
	if err != nil {
		return err
	}
	return inv.EditPages(ctx, command.HTMLPages(renderList(cfg, inv.Prefix), command.PageLimit))
}

// export 把频道源（一行一个）作为文件发到当前对话，import 能原样读回。
func (s *service) export(ctx context.Context, inv *command.Invocation) error {
	cfg, err := s.read()
	if err != nil {
		return err
	}
	if len(cfg.ChannelList) == 0 {
		return kit.Fail("没有可导出的频道")
	}
	handles := make([]string, len(cfg.ChannelList))
	for index, item := range cfg.ChannelList {
		handles[index] = item.Handle
	}
	peer, err := inv.Client.InputPeer(inv.Message.Peer)
	if err != nil {
		return err
	}
	if err := inv.Client.SendDocument(ctx, peer, []byte(strings.Join(handles, "\n")), bot.MediaOptions{Name: "search-channels.txt",
		MimeType: "text/plain", Caption: fmt.Sprintf("✅ 已导出 %d 个频道源", len(handles)), ForceDocument: true}); err != nil {
		return kit.FailWith("导出失败", err)
	}
	deleteCommand(ctx, inv)
	return nil
}

// importFile 读回复的备份文件（一行一个频道源，export 导出的格式），按 add 的规则添加。
// 只收文件：回复一张图或一段文字不算，免得把一篇文章当成几千个频道去解析。
// 和 MiBox 一样按行拆，一行里有空格的整行都不是频道源。
func (s *service) importFile(ctx context.Context, inv *command.Invocation) error {
	reply, err := kit.Reply(ctx, inv)
	if err != nil {
		return err
	}
	if reply == nil || reply.Raw == nil {
		return kit.Fail("请回复备份文件")
	}
	if _, isDocument := reply.Raw.Media.(*tg.MessageMediaDocument); !isDocument {
		return kit.Fail("请回复备份文件（.txt 文件）")
	}
	file, err := inv.Client.DownloadMedia(ctx, reply.Raw, maxImportBytes)
	switch {
	case err == nil:
	case errors.Is(err, bot.ErrTooLarge):
		return kit.Fail("备份文件超过 256 KB")
	case errors.Is(err, bot.ErrNoMedia):
		return kit.Fail("请回复备份文件")
	default:
		return kit.FailWith("读取备份文件失败", err)
	}
	var handles []string
	for _, line := range strings.Split(string(file.Data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			handles = append(handles, line)
		}
	}
	if len(handles) == 0 {
		return kit.Fail("备份文件无效")
	}
	return s.add(ctx, inv, handles)
}

// ad 管理广告过滤词：add、del、list。
func (s *service) ad(ctx context.Context, inv *command.Invocation) error {
	action := strings.ToLower(inv.Arg(1))
	words := inv.Args[min(2, len(inv.Args)):]
	switch action {
	case "list":
		cfg, err := s.read()
		if err != nil {
			return err
		}
		if len(cfg.AdFilters) == 0 {
			return inv.Edit(ctx, "🎞 <b>广告过滤词</b>\n\n还没有过滤词")
		}
		return inv.EditPages(ctx, command.HTMLPages("🎞 <b>广告过滤词</b>\n\n"+command.Escape(strings.Join(cfg.AdFilters, "\n")), command.PageLimit))
	case "add", "del":
		if len(words) == 0 {
			return kit.Usage(inv.Prefix, "so ad "+action+" 关键词…")
		}
	default:
		return kit.Usage(inv.Prefix, "so ad add|del|list [关键词…]")
	}
	changed := 0
	if err := s.store.Update(func(cfg *config) error {
		if action == "add" {
			for _, word := range words {
				if !slices.Contains(cfg.AdFilters, word) {
					cfg.AdFilters = append(cfg.AdFilters, word)
					changed++
				}
			}
			return nil
		}
		kept := cfg.AdFilters[:0:0]
		for _, word := range cfg.AdFilters {
			if slices.Contains(words, word) {
				changed++
				continue
			}
			kept = append(kept, word)
		}
		cfg.AdFilters = kept
		return nil
	}); err != nil {
		return err
	}
	verb := "添加"
	if action == "del" {
		verb = "删除"
	}
	return inv.Edit(ctx, kit.Feedback("success", fmt.Sprintf("已%s %d 个广告过滤词", verb, changed), ""))
}

// candidate 是找到的一条视频，连同它所在对话的 peer（转发要用）。
type candidate struct {
	message *tg.Message
	peer    tg.InputPeerClass
}

func (c candidate) key() string {
	return bot.PeerID(c.message.PeerID) + ":" + strconv.Itoa(c.message.ID)
}

// search 按顺序搜各个频道源（默认频道在前，两个频道之间停 0.75 秒），挑一条视频发出去。
// 关键词搜索在某个频道找到就停，加 -r 或 kkp 时搜完全部再随机挑。
func (s *service) search(ctx context.Context, inv *command.Invocation, opts options) error {
	if !opts.kkp && opts.query == "" {
		return kit.Fail("请输入搜索关键词")
	}
	cfg, err := s.read()
	if err != nil {
		return err
	}
	if len(cfg.ChannelList) == 0 {
		return kit.Failf("请先用 %sso add 添加至少一个搜索频道", inv.Prefix)
	}
	progress := "正在搜索视频"
	if opts.kkp {
		progress = "正在随机挑选视频"
	}
	if err := inv.EditText(ctx, kit.Working(progress)); err != nil {
		return err
	}
	var order []string
	if cfg.DefaultChannel != nil && *cfg.DefaultChannel != "" {
		order = append(order, *cfg.DefaultChannel)
	}
	for _, item := range cfg.ChannelList {
		if !slices.Contains(order, item.Handle) {
			order = append(order, item.Handle)
		}
	}
	var found []candidate
	albums := map[int64]bool{}
	for index, handle := range order {
		if index > 0 {
			if err := kit.Sleep(ctx, s.gap); err != nil {
				return err
			}
		}
		position := slices.IndexFunc(cfg.ChannelList, func(item channel) bool { return item.Handle == handle })
		if position < 0 {
			continue
		}
		videos, err := s.channelVideos(ctx, inv, cfg.ChannelList[position], opts, cfg.AdFilters, albums)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			inv.Log.Warn("search.source_failed", "source", handle, "error", err.Error())
			s.pruneIfGone(handle, err)
			continue
		}
		found = append(found, videos...)
		if len(videos) > 0 && !opts.kkp && !opts.random {
			break
		}
	}
	unique := dedupe(found)
	if len(unique) == 0 {
		if opts.kkp {
			return kit.Fail("没有找到合适的视频")
		}
		return kit.Fail("所有频道里都没有找到匹配的视频")
	}
	picked := choose(unique, opts, s.pick)
	if err := inv.EditText(ctx, kit.Working("正在发送")); err != nil {
		return err
	}
	return s.deliver(ctx, inv, picked, opts)
}

// pruneIfGone 在 Telegram 明确说用户名不存在时移除这个频道源，和 MiBox 一样；
// 连不上、限流之类的暂时错误不动它。
func (s *service) pruneIfGone(handle string, err error) {
	if !tgerr.Is(err, "USERNAME_NOT_OCCUPIED", "USERNAME_INVALID") {
		return
	}
	s.sources.forget(handle)
	_ = s.store.Update(func(cfg *config) error {
		cfg.ChannelList = slices.DeleteFunc(cfg.ChannelList, func(item channel) bool { return item.Handle == handle })
		if cfg.DefaultChannel != nil && *cfg.DefaultChannel == handle {
			cfg.DefaultChannel = nil
		}
		return nil
	})
}

// dedupe 按「对话:编号」去重，保持先后顺序。
func dedupe(list []candidate) []candidate {
	seen := map[string]bool{}
	var unique []candidate
	for _, item := range list {
		if key := item.key(); !seen[key] {
			seen[key] = true
			unique = append(unique, item)
		}
	}
	return unique
}

// choose 挑出要发的一条：随机或 kkp 时随机，否则按匹配分数、再按时长从高到低取第一条。
func choose(list []candidate, opts options, pick func(int) int) candidate {
	if opts.random || opts.kkp {
		return list[pick(len(list))]
	}
	sorted := slices.Clone(list)
	sort.SliceStable(sorted, func(a, b int) bool {
		left, right := score(sorted[a].message, opts.query), score(sorted[b].message, opts.query)
		if left != right {
			return left > right
		}
		return duration(sorted[a].message) > duration(sorted[b].message)
	})
	return sorted[0]
}

// channelVideos 在一个频道源里找视频。
//   - kkp：取最近的视频（超级群 200 条，其余 100 条），留下 20 秒到 3 分钟的。
//   - 关键词：先看讨论组——找到匹配的帖子就读它的评论，评论里有视频就用；没有再在讨论组里
//     按关键词搜视频。然后在频道里按关键词搜 200 条，相册里有一条匹配就把整个相册的视频都算上。
func (s *service) channelVideos(ctx context.Context, inv *command.Invocation, item channel, opts options, filters []string, albums map[int64]bool) ([]candidate, error) {
	client := inv.Client
	found, err := s.resolve(ctx, client, item.Handle)
	if err != nil {
		return nil, err
	}
	pure := func(message *tg.Message) bool {
		_, _, ok := videoDocument(message)
		return ok && !isAd(message, filters)
	}
	wrap := func(peer tg.InputPeerClass, messages []*tg.Message) []candidate {
		list := make([]candidate, 0, len(messages))
		for _, message := range messages {
			list = append(list, candidate{message: message, peer: peer})
		}
		return list
	}
	if opts.kkp {
		limit := 100
		if found.megagroup {
			limit = 200
		}
		messages, err := searchMessages(ctx, client, found.peer, "", &tg.InputMessagesFilterVideo{}, limit)
		if err != nil {
			return nil, err
		}
		var videos []*tg.Message
		for _, message := range messages {
			if length := duration(message); pure(message) && length >= 20 && length <= 180 {
				videos = append(videos, message)
			}
		}
		return wrap(found.peer, videos), nil
	}
	var result []candidate
	if item.LinkedGroup != "" {
		result = append(result, s.linkedVideos(ctx, inv, item.LinkedGroup, opts.query, pure)...)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	messages, err := searchMessages(ctx, client, found.peer, opts.query, nil, 200)
	if err != nil {
		return nil, err
	}
	for _, message := range messages {
		if !matches(message, opts.query) {
			continue
		}
		group, grouped := message.GetGroupedID()
		if !grouped {
			if pure(message) {
				result = append(result, candidate{message: message, peer: found.peer})
			}
			continue
		}
		if albums[group] {
			continue
		}
		around, err := history(ctx, client, found.peer, message.ID+10, 20)
		if err != nil {
			return nil, err
		}
		var album []*tg.Message
		for _, member := range around {
			if id, ok := member.GetGroupedID(); ok && id == group && pure(member) {
				album = append(album, member)
			}
		}
		if len(album) > 0 {
			result = append(result, wrap(found.peer, album)...)
			albums[group] = true
		}
	}
	return result, nil
}

// linkedVideos 在讨论组里找视频。讨论组读不了不影响搜频道本身，只记日志。
func (s *service) linkedVideos(ctx context.Context, inv *command.Invocation, handle, query string, pure func(*tg.Message) bool) []candidate {
	client := inv.Client
	linked, err := s.resolve(ctx, client, handle)
	if err != nil {
		inv.Log.Info("search.linked_failed", "group", handle, "error", err.Error())
		return nil
	}
	posts, err := searchMessages(ctx, client, linked.peer, query, nil, 100)
	if err != nil {
		inv.Log.Info("search.linked_failed", "group", handle, "error", err.Error())
		return nil
	}
	var videos []candidate
	for _, post := range posts {
		if _, hasReplies := post.GetReplies(); !hasReplies || !matches(post, query) {
			continue
		}
		comments, err := replies(ctx, client, linked.peer, post.ID, 100)
		if err != nil {
			inv.Log.Info("search.linked_failed", "group", handle, "error", err.Error())
			return nil
		}
		for _, comment := range comments {
			if pure(comment) {
				videos = append(videos, candidate{message: comment, peer: linked.peer})
			}
		}
		if len(videos) > 0 {
			return videos
		}
	}
	messages, err := searchMessages(ctx, client, linked.peer, query, &tg.InputMessagesFilterVideo{}, 100)
	if err != nil {
		inv.Log.Info("search.linked_failed", "group", handle, "error", err.Error())
		return nil
	}
	for _, message := range messages {
		if pure(message) {
			videos = append(videos, candidate{message: message, peer: linked.peer})
		}
	}
	return videos
}

// deliver 发出选中的视频：不要防剧透时先转发。只有来源禁止转发（消息带 noforwards 标记，
// 或者转发被 CHAT_FORWARDS_RESTRICTED 拒绝）才下载上传，与 .save 的判断一样；
// 别的转发错误照实报告，不为一个限流或网络错误去下载 2 GB 的视频。发出后删掉命令消息。
func (s *service) deliver(ctx context.Context, inv *command.Invocation, picked candidate, opts options) error {
	if !opts.spoiler && !picked.message.Noforwards {
		target, err := inv.Client.InputPeer(inv.Message.Peer)
		if err != nil {
			return err
		}
		err = inv.Client.ForwardToTopic(ctx, picked.peer, target, []int{picked.message.ID}, forumTopic(inv.Message))
		if err == nil {
			deleteCommand(ctx, inv)
			return nil
		}
		if !tgerr.Is(err, "CHAT_FORWARDS_RESTRICTED") {
			return kit.FailWith("转发视频失败", err)
		}
	}
	text := opts.query
	if text == "" {
		text = picked.message.Message
	}
	if err := s.upload(ctx, inv, picked, text, opts.spoiler); err != nil {
		return err
	}
	deleteCommand(ctx, inv)
	return nil
}
