// Package guestban 实现 .guestban：在开启的群组里自动清理访客机器人（guest bot）发的广告。
//
// 访客机器人是 Telegram 的新玩法：机器人带 bot_guestchat 标志，群里有人提到它，它就能把结果
// 贴进这个群，自己不用是成员。消息的发送者是机器人，guestchat_via_from 是叫它出来的人
// （客户端显示成「来自 某人」）。广告号就拿这个刷屏：机器人的名字就是广告词，叫它的是
// 一次性的小号。这里看到群里有带 guestchat_via_from 的消息，就删掉它、在这个群里封禁那个
// 机器人——封了之后它再也贴不进来，这才是治本的；严格模式下连叫它的人一起封。
package guestban

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// group 是一个开启了清理的群组。
type group struct {
	Enabled bool   `json:"enabled"`
	Title   string `json:"title,omitempty"`
	// Strict 为真时，叫出机器人的人也一起封禁。
	Strict bool `json:"strict,omitempty"`
	// Bots 是在这个群里封过的机器人数，Messages 是删掉的消息数。
	Bots     int `json:"bots,omitempty"`
	Messages int `json:"messages,omitempty"`
}

// config 存在 data/guestban.json。
type config struct {
	Groups map[string]*group `json:"groups"`
	// Allow 是放行的机器人：数字 ID 或者用户名（不带 @，小写）。正经的访客机器人
	// （翻译、搜图之类）加进来就不会被清理。
	Allow []string `json:"allow"`
}

func defaults() config { return config{Groups: map[string]*group{}, Allow: []string{}} }

// event 是一条要处理的访客机器人消息。
type event struct {
	chatID  string
	channel *tg.InputChannel
	message int
	bot     int64
	via     tg.PeerClass
	strict  bool
}

// queueSize 是排队等处理的消息上限。刷屏时一秒几十条，处理一条要发两三个请求；
// 排不下的只记日志，不能让监听者卡住更新的路。
const queueSize = 256

type service struct {
	a      *app.App
	store  *store.Store[config]
	cached atomic.Pointer[config]
	queue  chan event

	mu sync.Mutex
	// banned 记下最近在某个群里封过的机器人（「群/机器人」→ 时间），一阵刷屏只封一次，
	// 后面的消息只删不再封。
	banned map[string]time.Time
	// warned 记下没有管理权限的群，每个群一小时只记一次日志。
	warned map[string]time.Time
}

// Register 注册 .guestban、看别人消息的监听者，以及处理队列的后台任务。
func Register(a *app.App) {
	s := &service{a: a, store: kit.NewStore(a, "guestban.json", defaults), queue: make(chan event, queueSize),
		banned: map[string]time.Time{}, warned: map[string]time.Time{}}
	a.OnForeign(s.offer)
	a.Registry.AddJob(s.work)
	a.Registry.Register(&command.Command{Name: "guestban", Group: command.GroupAdmin, Description: "自动清理访客机器人广告",
		Usage: "[on|off|strict|allow|list]", Help: help, Handle: s.handle})
}

func help(prefix string) string {
	c := func(text string) string { return command.Code(prefix + "guestban" + text) }
	return "🚫 <b>访客机器人清理</b>\n\n" +
		"访客机器人不用进群，群里有人提到它就能把消息贴进来，显示成「机器人名 来自 某人」。" +
		"广告号拿它刷屏：机器人的名字就是广告词。开启后，在这个群组里看到这种消息就删掉，" +
		"并在本群封禁那个机器人，之后它再也贴不进来。\n\n" +
		"<b>本群</b>\n" +
		"• " + c("") + " 查看本群的状态\n" +
		"• " + c(" on") + "、" + c(" off") + " 在本群开启或关闭\n" +
		"• " + c(" strict on|off") + " 严格模式：叫出机器人的人也一起封禁（默认关闭，对方多半是一次性小号）\n\n" +
		"<b>放行和总览</b>\n" +
		"• " + c(" allow @机器人") + " 放行正经的访客机器人，" + c(" allow rm @机器人") + " 取消，" + c(" allow") + " 查看\n" +
		"• " + c(" list") + " 开启了的群组和清理次数\n\n" +
		"账号要是本群的管理员，有封禁成员和删除消息的权限。只用于超级群组。"
}

// current 是缓存的设置。别人的每条消息都要先看一眼，不能每次都读文件。
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

func (s *service) update(mutate func(*config) error) error {
	return s.store.Update(func(cfg *config) error {
		if cfg.Groups == nil {
			cfg.Groups = map[string]*group{}
		}
		if err := mutate(cfg); err != nil {
			return err
		}
		copied := cloneConfig(*cfg)
		s.cached.Store(&copied)
		return nil
	})
}

// cloneConfig 深拷贝一份给缓存：Update 里的 cfg 之后还会被改。
func cloneConfig(cfg config) config {
	copied := config{Groups: make(map[string]*group, len(cfg.Groups)), Allow: append([]string(nil), cfg.Allow...)}
	for id, value := range cfg.Groups {
		entry := *value
		copied.Groups[id] = &entry
	}
	return copied
}

// allowed 判断机器人在不在放行名单里（按 ID 或用户名）。
func (cfg *config) allowed(id int64, username string) bool {
	username = strings.ToLower(username)
	for _, entry := range cfg.Allow {
		if entry == strconv.FormatInt(id, 10) || username != "" && entry == username {
			return true
		}
	}
	return false
}

// offer 看一条别人发的消息：是访客机器人发的、在开启了的群里、机器人没被放行，就排进队列。
// 总是返回 false：不占用消息，.sure、.sudo 照样看得到。
func (s *service) offer(ctx context.Context, client *bot.Client, message *bot.Message) bool {
	raw := message.Raw
	if raw == nil || raw.GuestchatViaFrom == nil {
		return false
	}
	cfg := s.current()
	settings := cfg.Groups[message.ChatID]
	if settings == nil || !settings.Enabled {
		return false
	}
	sender, ok := raw.FromID.(*tg.PeerUser)
	if !ok {
		return false
	}
	info, _ := client.Peers().User(sender.UserID)
	if cfg.allowed(sender.UserID, info.Username) {
		return false
	}
	peer, err := client.InputPeer(message.Peer)
	if err != nil {
		return false
	}
	channel, ok := bot.InputChannel(peer)
	if !ok {
		return false
	}
	select {
	case s.queue <- event{chatID: message.ChatID, channel: channel, message: message.ID, bot: sender.UserID, via: raw.GuestchatViaFrom, strict: settings.Strict}:
	default:
		client.Logger().Warn("guestban.queue_full", "chat", message.ChatID, "message", message.ID)
	}
	return false
}

// work 一条一条处理队列里的消息。
func (s *service) work(ctx context.Context, client *bot.Client) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-s.queue:
			s.process(ctx, client, item)
		}
	}
}

// process 删掉消息；这阵子没在这个群封过这个机器人的话就封禁它，严格模式下再封叫它的人。
func (s *service) process(ctx context.Context, client *bot.Client, item event) {
	logger := client.Logger()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	deleted := 0
	if _, err := client.API().ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{Channel: item.channel, ID: []int{item.message}}); err != nil {
		s.warnOnce(logger, item.chatID, "delete", err)
	} else {
		deleted = 1
	}
	bots := 0
	if s.firstBan(item.chatID, item.bot) {
		if target, err := client.InputPeer(&tg.PeerUser{UserID: item.bot}); err == nil {
			if err := ban(ctx, client, item.channel, target); err != nil {
				s.warnOnce(logger, item.chatID, "ban_bot", err)
			} else {
				bots = 1
				logger.Info("guestban.banned", "chat", item.chatID, "bot", item.bot)
			}
		}
		if item.strict {
			if target, err := client.InputPeer(item.via); err == nil {
				if err := ban(ctx, client, item.channel, target); err != nil {
					s.warnOnce(logger, item.chatID, "ban_invoker", err)
				} else {
					logger.Info("guestban.banned_invoker", "chat", item.chatID, "invoker", bot.PeerID(item.via))
				}
			}
		}
	}
	if deleted+bots > 0 {
		kit.Warn(s.a, "guestban.save_failed", s.update(func(cfg *config) error {
			if entry := cfg.Groups[item.chatID]; entry != nil {
				entry.Messages += deleted
				entry.Bots += bots
			}
			return nil
		}))
	}
}

// ban 在超级群组里永久封禁一个用户或频道。不在群里的也能封：访客机器人本来就不是成员。
func ban(ctx context.Context, client *bot.Client, channel *tg.InputChannel, target tg.InputPeerClass) error {
	_, err := client.API().ChannelsEditBanned(ctx, &tg.ChannelsEditBannedRequest{Channel: channel, Participant: target,
		BannedRights: tg.ChatBannedRights{ViewMessages: true, SendMessages: true, SendMedia: true, SendStickers: true,
			SendGifs: true, SendGames: true, SendInline: true, EmbedLinks: true}})
	return err
}

// firstBan 判断这阵子（10 分钟内）是不是第一次在这个群看到这个机器人，是的话记下。
func (s *service) firstBan(chatID string, botID int64) bool {
	key := chatID + "/" + strconv.FormatInt(botID, 10)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for entry, at := range s.banned {
		if now.Sub(at) > 10*time.Minute {
			delete(s.banned, entry)
		}
	}
	if _, seen := s.banned[key]; seen {
		return false
	}
	s.banned[key] = now
	return true
}

// warnOnce 记下失败，每个群每种操作一小时最多一条：没有管理权限时每条广告都会失败。
func (s *service) warnOnce(logger *slog.Logger, chatID, action string, err error) {
	key := chatID + "/" + action
	s.mu.Lock()
	last, seen := s.warned[key]
	quiet := seen && time.Since(last) < time.Hour
	if !quiet {
		s.warned[key] = time.Now()
	}
	s.mu.Unlock()
	if !quiet {
		logger.Warn("guestban.failed", "chat", chatID, "action", action, "error", err.Error())
	}
}

func (s *service) handle(ctx context.Context, inv *command.Invocation) error {
	switch action := strings.ToLower(inv.Arg(0)); action {
	case "":
		return s.status(ctx, inv)
	case "on", "off":
		return s.toggle(ctx, inv, action == "on")
	case "strict":
		return s.strict(ctx, inv)
	case "allow":
		return s.allow(ctx, inv)
	case "list", "ls":
		return s.list(ctx, inv)
	}
	return kit.Usage(inv.Prefix, "guestban [on|off|strict on|off|allow|list]")
}

// here 是命令所在的超级群组；不在超级群组里时报错。
func here(client *bot.Client, message *bot.Message) (*tg.PeerChannel, error) {
	peer, ok := message.Peer.(*tg.PeerChannel)
	if !ok {
		return nil, kit.Fail("要在超级群组里用")
	}
	if info, known := client.Peers().Channel(peer.ChannelID); known && info.Broadcast {
		return nil, kit.Fail("要在超级群组里用，频道里没有访客机器人")
	}
	return peer, nil
}

func (s *service) status(ctx context.Context, inv *command.Invocation) error {
	if _, err := here(inv.Client, inv.Message); err != nil {
		return s.list(ctx, inv)
	}
	entry := s.current().Groups[inv.Message.ChatID]
	if entry == nil || !entry.Enabled {
		return inv.Edit(ctx, "🚫 <b>访客机器人清理</b>\n本群：关闭，"+command.Code(inv.Prefix+"guestban on")+" 开启")
	}
	return inv.Edit(ctx, "🚫 <b>访客机器人清理</b>\n本群：开启"+strictNote(entry.Strict)+
		fmt.Sprintf("\n已封禁 %d 个机器人，删除 %d 条消息", entry.Bots, entry.Messages))
}

func strictNote(strict bool) string {
	if strict {
		return "，严格模式（叫它的人一起封）"
	}
	return ""
}

func (s *service) toggle(ctx context.Context, inv *command.Invocation, enable bool) error {
	peer, err := here(inv.Client, inv.Message)
	if err != nil {
		return err
	}
	if enable {
		info, known := inv.Client.Peers().Channel(peer.ChannelID)
		if !known || !info.Creator && !(info.AdminRights.BanUsers && info.AdminRights.DeleteMessages) {
			return kit.Fail("账号在本群没有封禁成员和删除消息的权限，开启了也清理不了")
		}
		if err := s.update(func(cfg *config) error {
			entry := cfg.Groups[inv.Message.ChatID]
			if entry == nil {
				entry = &group{}
				cfg.Groups[inv.Message.ChatID] = entry
			}
			entry.Enabled, entry.Title = true, info.Title
			return nil
		}); err != nil {
			return err
		}
		return inv.EditText(ctx, "✅ 本群已开启访客机器人清理")
	}
	if err := s.update(func(cfg *config) error {
		if entry := cfg.Groups[inv.Message.ChatID]; entry != nil {
			entry.Enabled = false
		}
		return nil
	}); err != nil {
		return err
	}
	return inv.EditText(ctx, "✅ 本群已关闭访客机器人清理")
}

func (s *service) strict(ctx context.Context, inv *command.Invocation) error {
	if _, err := here(inv.Client, inv.Message); err != nil {
		return err
	}
	value, err := kit.OnOff(inv.Arg(1))
	if err != nil {
		return kit.Usage(inv.Prefix, "guestban strict on|off")
	}
	entry := s.current().Groups[inv.Message.ChatID]
	if entry == nil || !entry.Enabled {
		return kit.Failf("本群还没开启，先发 %sguestban on", inv.Prefix)
	}
	if err := s.update(func(cfg *config) error {
		if entry := cfg.Groups[inv.Message.ChatID]; entry != nil {
			entry.Strict = value
		}
		return nil
	}); err != nil {
		return err
	}
	return inv.EditText(ctx, "✅ 严格模式已"+kit.OnOffText(value))
}

// allow 管理放行名单：allow 查看，allow @机器人 添加，allow rm @机器人 删除。
func (s *service) allow(ctx context.Context, inv *command.Invocation) error {
	remove := strings.EqualFold(inv.Arg(1), "rm") || strings.EqualFold(inv.Arg(1), "del")
	name := inv.Arg(1)
	if remove {
		name = inv.Arg(2)
	}
	entry := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "@"))
	if entry == "" {
		cfg := s.current()
		if len(cfg.Allow) == 0 {
			return inv.Edit(ctx, "🚫 <b>放行的访客机器人</b>\n还没有，"+command.Code(inv.Prefix+"guestban allow @机器人")+" 添加")
		}
		lines := []string{"🚫 <b>放行的访客机器人</b>"}
		for _, item := range cfg.Allow {
			if kit.IsDigits(item) {
				lines = append(lines, "• "+command.Code(item))
			} else {
				lines = append(lines, "• "+command.Code("@"+item))
			}
		}
		return inv.Edit(ctx, strings.Join(lines, "\n"))
	}
	if !kit.IsDigits(entry) && !validUsername(entry) {
		return kit.Fail("写机器人的 @用户名 或者数字 ID")
	}
	changed := false
	if err := s.update(func(cfg *config) error {
		index := -1
		for i, item := range cfg.Allow {
			if item == entry {
				index = i
			}
		}
		switch {
		case remove && index >= 0:
			cfg.Allow = append(cfg.Allow[:index], cfg.Allow[index+1:]...)
			changed = true
		case !remove && index < 0:
			cfg.Allow = append(cfg.Allow, entry)
			changed = true
		}
		return nil
	}); err != nil {
		return err
	}
	label := entry
	if !kit.IsDigits(entry) {
		label = "@" + entry
	}
	switch {
	case remove && changed:
		return inv.EditText(ctx, "✅ 已取消放行 "+label)
	case remove:
		return kit.Failf("放行名单里没有 %s", label)
	case changed:
		return inv.EditText(ctx, "✅ 已放行 "+label)
	}
	return inv.EditText(ctx, "✅ "+label+" 已经在放行名单里")
}

func validUsername(name string) bool {
	if len(name) < 4 || len(name) > 32 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

func (s *service) list(ctx context.Context, inv *command.Invocation) error {
	cfg := s.current()
	var lines []string
	for id, entry := range cfg.Groups {
		if !entry.Enabled {
			continue
		}
		title := kit.OrDefault(entry.Title, id)
		lines = append(lines, "• "+command.Escape(title)+strictNote(entry.Strict)+fmt.Sprintf("：封禁 %d 个机器人，删除 %d 条", entry.Bots, entry.Messages))
	}
	if len(lines) == 0 {
		return inv.Edit(ctx, "🚫 <b>访客机器人清理</b>\n还没有群组开启，在群里发 "+command.Code(inv.Prefix+"guestban on"))
	}
	header := fmt.Sprintf("🚫 <b>访客机器人清理</b>（%d 个群组，放行 %d 个机器人）", len(lines), len(cfg.Allow))
	return inv.EditPages(ctx, command.HTMLPages(header+"\n"+strings.Join(lines, "\n"), command.PageLimit))
}
