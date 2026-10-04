// Package trace 实现 .trace：追踪指定的用户或关键词，自动给命中的消息点表情回应。
// 数据格式和 MiBox V2 的 trace 插件（assets/trace/db.json）一样，--import-mibox 原样搬过来。
package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// reaction 是一个表情回应：普通表情（Emoticon）或者会员的自定义表情（DocumentID）。
// 存成 {"emoticon": "👍"} 或 {"documentId": "123"}；V2 早期的数据直接存字符串，读的时候也认。
type reaction struct {
	Emoticon   string `json:"emoticon,omitempty"`
	DocumentID string `json:"documentId,omitempty"`
}

func (r *reaction) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if kit.IsDigits(text) && text[0] != '0' {
			*r = reaction{DocumentID: text}
		} else {
			*r = reaction{Emoticon: text}
		}
		return nil
	}
	type plain reaction
	var value plain
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*r = reaction(value)
	return nil
}

func (r reaction) display() string {
	if r.DocumentID != "" {
		return "自定义：" + r.DocumentID
	}
	return r.Emoticon
}

func (r reaction) tl() (tg.ReactionClass, bool) {
	if r.DocumentID != "" {
		id, err := strconv.ParseInt(r.DocumentID, 10, 64)
		if err != nil || id <= 0 {
			return nil, false
		}
		return &tg.ReactionCustomEmoji{DocumentID: id}, true
	}
	if strings.TrimSpace(r.Emoticon) == "" {
		return nil, false
	}
	return &tg.ReactionEmoji{Emoticon: r.Emoticon}, true
}

type config struct {
	// Users 的键是发送者 ID（用户是正数，以频道身份发言的是 -100 开头）。
	Users    map[string][]reaction `json:"users"`
	Keywords map[string][]reaction `json:"keywords"`
	Config   struct {
		// KeepLog 为假时，命令的回执 10 秒后删掉。
		KeepLog bool `json:"keepLog"`
		// Big 是大号表情动画。
		Big bool `json:"big"`
	} `json:"config"`
}

func defaults() config {
	cfg := config{Users: map[string][]reaction{}, Keywords: map[string][]reaction{}}
	cfg.Config.KeepLog, cfg.Config.Big = true, true
	return cfg
}

// available 是不需要会员就能用的表情，按 Telegram 里的顺序，说明里照这个列。
var available = []string{"👍", "👎", "❤️", "🔥", "🥰", "👏", "😁", "🤔", "🤯", "😱", "🤬", "😢", "🎉", "🤩", "🤮", "💩", "🙏", "👌",
	"🕊", "🤡", "🥱", "🥴", "😍", "🐳", "❤️‍🔥", "🌚", "🌭", "💯", "🤣", "⚡️", "🍌", "🏆", "💔", "🤨", "😐", "🍓", "🍾", "💋",
	"🖕", "😈", "😎", "😇", "😤"}

// standard 是匹配用的同一份表情，长的排在前面：❤️‍🔥 要先于 ❤️ 匹配。
var standard = func() []string {
	list := append([]string(nil), available...)
	sort.SliceStable(list, func(a, b int) bool { return len(list[a]) > len(list[b]) })
	return list
}()

type job struct {
	peer      tg.InputPeerClass
	message   int
	reactions []reaction
	big       bool
}

type service struct {
	a      *app.App
	store  *store.Store[config]
	cached atomic.Pointer[config]
	queue  chan job
}

// Register 注册 .trace、看别人消息的监听者和发回应的后台任务。数据存在 data/trace.json。
func Register(a *app.App) {
	s := &service{a: a, store: kit.NewStore(a, "trace.json", defaults), queue: make(chan job, 128)}
	a.OnForeign(s.offer)
	a.Registry.AddJob(s.work)
	a.Registry.Register(&command.Command{Name: "trace", Group: command.GroupMedia, Description: "自动给指定用户点表情",
		Usage: "[表情…|kw|status|clean|reset]", Help: help, Handle: s.handle})
}

func help(prefix string) string {
	c := func(text string) string { return command.Code(prefix + "trace" + text) }
	return "👀 <b>自动回应</b>\n\n追踪指定的用户或关键词，他们发消息时账号自动点上表情回应。\n\n" +
		"<b>用户</b>\n" +
		"• 回复一条消息发 " + c(" 👍🔥") + " 追踪发这条消息的人，并马上给这条点上\n" +
		"• 回复一条消息发 " + c("") + " 取消追踪这个人\n\n" +
		"<b>关键词</b>\n" +
		"• " + c(" kw add 关键词 👍🔥") + " 消息里有这个词就点上\n" +
		"• " + c(" kw del 关键词") + " 删除\n\n" +
		"<b>管理</b>\n" +
		"• " + c(" status") + " 查看追踪的用户和关键词\n" +
		"• " + c(" clean") + " 清空所有追踪，" + c(" reset") + " 连设置一起重置\n" +
		"• " + c(" log on|off") + " 保留命令回执（关闭时 10 秒后删掉）\n" +
		"• " + c(" big on|off") + " 大号表情动画\n\n" +
		"可用的表情：" + strings.Join(available, "") + "。会员可以用自定义表情；不是会员的账号一条消息只能点一个表情。"
}

func (s *service) current() *config {
	if loaded := s.cached.Load(); loaded != nil {
		return loaded
	}
	cfg, err := s.store.Read()
	if err != nil {
		cfg = defaults()
	}
	s.cached.Store(&cfg)
	return &cfg
}

func (s *service) update(mutate func(*config)) error {
	return s.store.Update(func(cfg *config) error {
		if cfg.Users == nil {
			cfg.Users = map[string][]reaction{}
		}
		if cfg.Keywords == nil {
			cfg.Keywords = map[string][]reaction{}
		}
		mutate(cfg)
		copied := *cfg
		copied.Users, copied.Keywords = make(map[string][]reaction, len(cfg.Users)), make(map[string][]reaction, len(cfg.Keywords))
		for key, value := range cfg.Users {
			copied.Users[key] = append([]reaction(nil), value...)
		}
		for key, value := range cfg.Keywords {
			copied.Keywords[key] = append([]reaction(nil), value...)
		}
		s.cached.Store(&copied)
		return nil
	})
}

// match 是一条消息要点的表情：先看发送者，再按关键词（按字典序找第一个命中的，结果固定）。
func (cfg *config) match(sender, text string) []reaction {
	if items := cfg.Users[sender]; len(items) > 0 {
		return items
	}
	keys := make([]string, 0, len(cfg.Keywords))
	for keyword := range cfg.Keywords {
		keys = append(keys, keyword)
	}
	sort.Strings(keys)
	for _, keyword := range keys {
		if keyword != "" && strings.Contains(text, keyword) {
			return cfg.Keywords[keyword]
		}
	}
	return nil
}

// offer 看别人的消息，命中就排进队列。只查内存里的设置，不占用消息。
func (s *service) offer(ctx context.Context, client *bot.Client, message *bot.Message) bool {
	if message.Sender == nil || message.Saved {
		return false
	}
	cfg := s.current()
	if len(cfg.Users) == 0 && len(cfg.Keywords) == 0 {
		return false
	}
	items := cfg.match(bot.PeerID(message.Sender), message.Text)
	if len(items) == 0 {
		return false
	}
	peer, err := client.InputPeer(message.Peer)
	if err != nil {
		return false
	}
	select {
	case s.queue <- job{peer: peer, message: message.ID, reactions: items, big: cfg.Config.Big}:
	default:
		client.Logger().Warn("trace.queue_full", "chat", message.ChatID)
	}
	return false
}

func (s *service) work(ctx context.Context, client *bot.Client) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-s.queue:
			callCtx, cancel := context.WithTimeout(ctx, time.Minute)
			if err := react(callCtx, client, item); err != nil && ctx.Err() == nil {
				client.Logger().Debug("trace.reaction_failed", slog.String("error", err.Error()))
			}
			cancel()
		}
	}
}

// react 给一条消息点上表情。
func react(ctx context.Context, client *bot.Client, item job) error {
	values := make([]tg.ReactionClass, 0, len(item.reactions))
	for _, entry := range item.reactions {
		if value, ok := entry.tl(); ok {
			values = append(values, value)
		}
	}
	if len(values) == 0 {
		return nil
	}
	request := &tg.MessagesSendReactionRequest{Peer: item.peer, MsgID: item.message, Big: item.big}
	request.SetReaction(values)
	_, err := client.API().MessagesSendReaction(ctx, request)
	return err
}

// parse 从 text 里读出表情：普通表情按 standard 认，会员的自定义表情从消息的实体里取。
// start 是 text 在整条消息里的起点（UTF-16 单位），实体的位置按它对齐。按出现的顺序排，去掉重复的。
func parse(text string, start int, entities []tg.MessageEntityClass) []reaction {
	type found struct {
		at    int
		value reaction
	}
	var items []found
	var spans [][2]int
	for _, entity := range entities {
		custom, ok := entity.(*tg.MessageEntityCustomEmoji)
		if !ok || custom.Offset < start || custom.Offset+custom.Length > start+command.UTF16Len(text) {
			continue
		}
		items = append(items, found{custom.Offset - start, reaction{DocumentID: strconv.FormatInt(custom.DocumentID, 10)}})
		spans = append(spans, [2]int{custom.Offset - start, custom.Offset - start + custom.Length})
	}
	inCustom := func(at int) bool {
		for _, span := range spans {
			if at >= span[0] && at < span[1] {
				return true
			}
		}
		return false
	}
	units := 0
	for index := 0; index < len(text); {
		matched := ""
		for _, emoji := range standard {
			if strings.HasPrefix(text[index:], emoji) {
				matched = emoji
				break
			}
		}
		if matched == "" {
			_, size := utf8.DecodeRuneInString(text[index:])
			units += command.UTF16Len(text[index : index+size])
			index += size
			continue
		}
		if !inCustom(units) {
			items = append(items, found{units, reaction{Emoticon: matched}})
		}
		units += command.UTF16Len(matched)
		index += len(matched)
	}
	sort.SliceStable(items, func(a, b int) bool { return items[a].at < items[b].at })
	seen := map[reaction]bool{}
	var result []reaction
	for _, item := range items {
		if !seen[item.value] {
			seen[item.value] = true
			result = append(result, item.value)
		}
	}
	return result
}

func display(items []reaction) string {
	parts := make([]string, len(items))
	for index, item := range items {
		parts[index] = item.display()
	}
	return strings.Join(parts, " ")
}

func (s *service) handle(ctx context.Context, inv *command.Invocation) error {
	switch sub := strings.ToLower(inv.Arg(0)); sub {
	case "status":
		return s.status(ctx, inv)
	case "clean", "reset":
		if err := s.update(func(cfg *config) {
			if sub == "reset" {
				*cfg = defaults()
				return
			}
			cfg.Users, cfg.Keywords = map[string][]reaction{}, map[string][]reaction{}
		}); err != nil {
			return err
		}
		if sub == "reset" {
			return s.receipt(ctx, inv, "✅ 已重置自动回应的追踪和设置")
		}
		return s.receipt(ctx, inv, "✅ 已清空所有追踪")
	case "log", "big":
		value, err := onOff(inv.Arg(1))
		if err != nil {
			return kit.Usage(inv.Prefix, "trace "+sub+" on|off")
		}
		if err := s.update(func(cfg *config) {
			if sub == "log" {
				cfg.Config.KeepLog = value
			} else {
				cfg.Config.Big = value
			}
		}); err != nil {
			return err
		}
		label := "保留回执"
		if sub == "big" {
			label = "大号表情动画"
		}
		return s.receipt(ctx, inv, "✅ "+label+"已"+kit.OnOffText(value))
	case "kw":
		return s.keyword(ctx, inv)
	}
	return s.user(ctx, inv)
}

// onOff 认 on/off，也认 V2 用的 true/false。
func onOff(value string) (bool, error) {
	switch strings.ToLower(value) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return kit.OnOff(value)
}

// reactionsAfter 读命令里第 skip 个词之后的表情。
func reactionsAfter(inv *command.Invocation, skip int) []reaction {
	raw := inv.RawAfter(skip)
	start := command.UTF16Len(strings.TrimSuffix(inv.Text, raw))
	var entities []tg.MessageEntityClass
	if inv.Message.Raw != nil {
		entities = inv.Message.Raw.Entities
	}
	return parse(raw, start, entities)
}

func (s *service) keyword(ctx context.Context, inv *command.Invocation) error {
	action, word := strings.ToLower(inv.Arg(1)), inv.Arg(2)
	if word == "" || action != "add" && action != "del" {
		return kit.Usage(inv.Prefix, "trace kw add 关键词 表情|kw del 关键词")
	}
	var items []reaction
	if action == "add" {
		items = reactionsAfter(inv, 3)
		if len(items) == 0 {
			return kit.Fail("没找到能用的表情，" + inv.Prefix + "help trace 看可用的表情")
		}
		if err := s.needPremium(inv, items); err != nil {
			return err
		}
	}
	found := true
	if err := s.update(func(cfg *config) {
		if action == "add" {
			cfg.Keywords[word] = items
			return
		}
		_, found = cfg.Keywords[word]
		delete(cfg.Keywords, word)
	}); err != nil {
		return err
	}
	if !found {
		return kit.Failf("没有追踪关键词 %s", word)
	}
	if action == "add" {
		return s.receipt(ctx, inv, "✅ 已追踪关键词 "+word+"："+display(items))
	}
	return s.receipt(ctx, inv, "✅ 已取消追踪关键词 "+word)
}

func (s *service) user(ctx context.Context, inv *command.Invocation) error {
	reply, err := kit.Reply(ctx, inv)
	if err != nil {
		return err
	}
	if reply == nil || reply.Sender == nil {
		return inv.Edit(ctx, help(inv.Prefix))
	}
	id := bot.PeerID(reply.Sender)
	if id == "" || reply.SenderID() == inv.Client.SelfID() {
		return kit.Fail("不能追踪自己")
	}
	if len(inv.Args) == 0 {
		found := false
		if err := s.update(func(cfg *config) {
			_, found = cfg.Users[id]
			delete(cfg.Users, id)
		}); err != nil {
			return err
		}
		if !found {
			return kit.Failf("没有在追踪 %s", id)
		}
		return s.receipt(ctx, inv, "✅ 已取消追踪 "+id)
	}
	items := reactionsAfter(inv, 0)
	if len(items) == 0 {
		return kit.Fail("没找到能用的表情，" + inv.Prefix + "help trace 看可用的表情")
	}
	if err := s.needPremium(inv, items); err != nil {
		return err
	}
	if err := s.update(func(cfg *config) { cfg.Users[id] = items }); err != nil {
		return err
	}
	if peer, err := inv.Client.InputPeer(reply.Peer); err == nil {
		if err := react(ctx, inv.Client, job{peer: peer, message: reply.ID, reactions: items, big: s.current().Config.Big}); err != nil {
			return kit.FailWith("已追踪 "+id+"，但这次没能点上表情", err)
		}
	}
	return s.receipt(ctx, inv, "✅ 已追踪 "+id+"："+display(items))
}

// needPremium 用自定义表情时确认账号是会员：不是会员的话 Telegram 会拒绝。
func (s *service) needPremium(inv *command.Invocation, items []reaction) error {
	for _, item := range items {
		if item.DocumentID != "" && !inv.Client.Self().Premium {
			return kit.Fail("自定义表情要会员才能用")
		}
	}
	return nil
}

// receipt 回执；关掉保留回执时 10 秒后删掉命令消息，不在对话里留痕。
func (s *service) receipt(ctx context.Context, inv *command.Invocation, text string) error {
	if err := inv.EditText(ctx, text); err != nil {
		return err
	}
	if !s.current().Config.KeepLog && kit.Sleep(ctx, 10*time.Second) == nil {
		_ = inv.Client.DeleteMessage(ctx, inv.Message)
	}
	return nil
}

func (s *service) status(ctx context.Context, inv *command.Invocation) error {
	cfg := s.current()
	lines := []string{"👀 <b>自动回应</b>", fmt.Sprintf("追踪 %d 个用户、%d 个关键词", len(cfg.Users), len(cfg.Keywords)), "", "<b>用户</b>"}
	users := make([]string, 0, len(cfg.Users))
	for id := range cfg.Users {
		users = append(users, id)
	}
	sort.Strings(users)
	for _, id := range users {
		name := id
		if peer, ok := bot.PeerFromID(id); ok {
			if title := inv.Client.Peers().Title(peer); title != "" {
				name = title + "（" + id + "）"
			}
		}
		lines = append(lines, "• "+command.Escape(name)+"："+command.Escape(display(cfg.Users[id])))
	}
	if len(users) == 0 {
		lines = append(lines, "无")
	}
	lines = append(lines, "", "<b>关键词</b>")
	keywords := make([]string, 0, len(cfg.Keywords))
	for word := range cfg.Keywords {
		keywords = append(keywords, word)
	}
	sort.Strings(keywords)
	for _, word := range keywords {
		lines = append(lines, "• "+command.Code(word)+"："+command.Escape(display(cfg.Keywords[word])))
	}
	if len(keywords) == 0 {
		lines = append(lines, "无")
	}
	lines = append(lines, "", "保留回执："+kit.OnOffText(cfg.Config.KeepLog)+" · 大号动画："+kit.OnOffText(cfg.Config.Big))
	return inv.EditPages(ctx, command.HTMLPages(strings.Join(lines, "\n"), command.PageLimit))
}
