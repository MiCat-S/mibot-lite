package keyword

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dlclark/regexp2"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// compiled 是一条任务和它编译好的正则（只有正则任务、而且编译成功时才有）。
type compiled struct {
	task
	pattern *regexp2.Regexp
}

// view 是按对话分好组、正则已经编译好的任务表。别人的每条消息都要查一遍，
// 不能每次都读文件、重新编译；任务改了就整个换一份新的。
type view struct {
	chats   map[string][]*compiled
	aliases map[string]string
}

// tasksFor 是一个对话要检查的任务：先是它继承的那个对话的，再是它自己的，和 v2 的顺序一样。
// 继承只看一层，继承自己的旧数据不重复算。
func (v *view) tasksFor(chatID string) []*compiled {
	own := v.chats[chatID]
	inherited := v.aliases[chatID]
	if inherited == "" || inherited == chatID {
		return own
	}
	from := v.chats[inherited]
	if len(from) == 0 {
		return own
	}
	return append(append(make([]*compiled, 0, len(from)+len(own)), from...), own...)
}

// build 编译一份任务表。搬过来的正则编译不了（多半是 JS 特有、regexp2 不认的写法）时
// 记一条警告，这条任务从此不命中，.keyword list 里会标出来。
func build(doc document, logger *slog.Logger) *view {
	v := &view{chats: map[string][]*compiled{}, aliases: doc.Aliases}
	if v.aliases == nil {
		v.aliases = map[string]string{}
	}
	for _, t := range doc.Tasks {
		entry := &compiled{task: t}
		if t.Regexp {
			pattern, err := compilePattern(t.Key, t.CaseSensitive)
			if err != nil && logger != nil {
				logger.Warn("keyword.regexp_invalid", slog.Int("task", t.ID), slog.String("error", err.Error()))
			}
			entry.pattern = pattern
		}
		v.chats[t.ChatID] = append(v.chats[t.ChatID], entry)
	}
	return v
}

// service 管任务表的读写，以及别人的消息命中之后要做的事。
type service struct {
	store  *store.Store[document]
	logger *slog.Logger
	// mu 让「改文件、换任务表」成为一步，两个同时进行的修改不会互相覆盖对方换上的表。
	mu     sync.Mutex
	cached atomic.Pointer[view]
	// slots 限制同时在执行动作的消息数。占不到就丢掉这次触发：群里刷屏、账号又在等限流时，
	// 排队只会越积越多，等轮到时刷屏早过去了。
	slots chan struct{}
	// warnMu 和 warned 让「忙不过来、丢掉了」的警告每个对话每分钟只记一次，刷屏时不刷日志。
	warnMu sync.Mutex
	warned map[string]time.Time
	// background 让动作离开更新处理的路径；测试里换成直接执行。
	background func(func())
	// later 安排延时删除，测试里换成立即记录。
	later func(time.Duration, func())
	now   func() time.Time
}

func newService(saved *store.Store[document], logger *slog.Logger) *service {
	return &service{store: saved, logger: logger, slots: make(chan struct{}, 4), warned: map[string]time.Time{},
		background: func(work func()) { go work() },
		later:      func(delay time.Duration, run func()) { time.AfterFunc(delay, run) },
		now:        time.Now}
}

// current 返回任务表，第一次用到时从文件读。文件读不了就当没有任务，记一条错误：
// 这时 .keyword 的每条子命令都会报出同一个错误，用户看得到。
func (s *service) current() *view {
	if loaded := s.cached.Load(); loaded != nil {
		return loaded
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if loaded := s.cached.Load(); loaded != nil {
		return loaded
	}
	doc, err := s.store.Read()
	if err != nil {
		if s.logger != nil {
			s.logger.Error("keyword.load_failed", slog.String("error", err.Error()))
		}
		doc = defaults()
	}
	loaded := build(doc, s.logger)
	s.cached.Store(loaded)
	return loaded
}

// update 修改文件，并换上新的任务表。
func (s *service) update(mutate func(*document) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.Update(mutate); err != nil {
		return err
	}
	doc, err := s.store.Read()
	if err != nil {
		return err
	}
	s.cached.Store(build(doc, s.logger))
	return nil
}

// applyTimeout 是执行一条任务的时限。监听者的 ctx 永远不会取消，没有这个时限，
// 一个卡住的请求会一直占着名额。
const applyTimeout = 2 * time.Minute

// offer 是看别人消息的监听者。先在这里把任务匹配完：普通关键词是一次字符串比较，
// 正则有 100 毫秒的时限和 4096 字的上限，都很快。只有命中了才去占一个名额、到后台执行动作；
// 名额都被占着就丢掉这次，记一条警告。以前先占名额再匹配，占不到就一直等，
// 刷屏加上限流等待时，等着的协程会无限堆积。
//
// 永远返回 false，也就是不认领这条消息：关键词回复只是顺带做的事，同一条消息
// 还得交给后面的监听者——名单里的人发的 .sudo 命令碰巧含关键词，照样要执行。
func (s *service) offer(ctx context.Context, client *bot.Client, message *bot.Message) bool {
	if message.Text == "" || message.Out {
		return false
	}
	tasks := s.current().tasksFor(message.ChatID)
	if len(tasks) == 0 {
		return false
	}
	hits := hitsOf(client, message, tasks)
	if len(hits) == 0 {
		return false
	}
	select {
	case s.slots <- struct{}{}:
	default:
		s.warnDropped(message.ChatID)
		return false
	}
	s.background(func() {
		defer func() { <-s.slots }()
		// 同一条消息命中几条就做几次，和 v2 一样。
		for _, t := range hits {
			applyCtx, cancel := context.WithTimeout(ctx, applyTimeout)
			s.apply(applyCtx, client, message, t)
			cancel()
		}
	})
	return false
}

// hitsOf 按顺序挑出命中的任务。
func hitsOf(client *bot.Client, message *bot.Message, tasks []*compiled) []task {
	var hits []task
	for _, entry := range tasks {
		hit, err := matches(entry.task, entry.pattern, message.Text, message.Forward)
		if err != nil {
			// regexp2 超时：这条正则回溯得太厉害，这次按没命中算，和 v2 一样。
			client.Logger().Warn("keyword.regexp_failed", slog.Int("task", entry.ID), slog.String("error", err.Error()))
			continue
		}
		if hit {
			hits = append(hits, entry.task)
		}
	}
	return hits
}

// warnDropped 记一条「忙不过来、丢掉了一次触发」的警告，同一个对话一分钟内只记一次。
func (s *service) warnDropped(chatID string) {
	now := s.now()
	s.warnMu.Lock()
	if last, ok := s.warned[chatID]; ok && now.Sub(last) < time.Minute {
		s.warnMu.Unlock()
		return
	}
	for chat, at := range s.warned {
		if now.Sub(at) >= time.Minute {
			delete(s.warned, chat)
		}
	}
	s.warned[chatID] = now
	s.warnMu.Unlock()
	if s.logger != nil {
		s.logger.Warn("keyword.dropped_busy", slog.String("chat", chatID), slog.Int("slots", cap(s.slots)))
	}
}

// senderOf 取回复变量要用的发送者信息。名字从更新带来的实体里查；没有名字（注销的账号）时写「用户」。
func senderOf(client *bot.Client, message *bot.Message) sender {
	switch peer := message.Sender.(type) {
	case *tg.PeerUser:
		info, _ := client.Peers().User(peer.UserID)
		name := strings.TrimSpace(info.FirstName)
		if name == "" {
			name = "用户"
		}
		return sender{id: strconv.FormatInt(peer.UserID, 10), name: name, user: true}
	case *tg.PeerChannel:
		return sender{id: strconv.FormatInt(peer.ChannelID, 10), name: client.Peers().Title(peer)}
	}
	return sender{}
}

// apply 执行一条命中的任务：回复、处置发送者、删原消息、到时删回复，顺序同 v2。
// ctx 由调用方设好时限。
//
// 和 v2 不同的是某一步失败不拦住后面几步：v2 回复发不出去就整个中止，
// 可「删掉广告」不该因为账号在这个群里不能发言就不做了。
func (s *service) apply(ctx context.Context, client *bot.Client, message *bot.Message, t task) {
	logger := client.Logger().With(slog.String("chat", message.ChatID), slog.Int("task", t.ID))
	peer, err := client.InputPeer(message.Peer)
	if err != nil {
		logger.Warn("keyword.unaddressable", slog.String("error", err.Error()))
		return
	}
	sent := 0
	if text := render(t, senderOf(client, message)); strings.TrimSpace(text) != "" {
		// 不引用原消息时也留在同一个话题里；v2 这时会把回复发到论坛的默认话题。
		options := bot.SendOptions{Topic: message.TopicID}
		if t.Reply {
			options.ReplyTo = message.ID
		}
		sent, err = client.SendHTML(ctx, peer, text, options)
		if err != nil {
			logger.Warn("keyword.reply_failed", slog.String("error", err.Error()))
		}
	}
	if err := s.moderate(ctx, client, peer, message, t); err != nil {
		logger.Warn("keyword.moderate_failed", slog.String("error", err.Error()))
	}
	if t.DeleteSource {
		s.deleteAfter(client, peer, message.ID, duration(t.DeleteSourceAfter), logger)
	}
	if t.DeleteReplyAfter > 0 && sent > 0 {
		s.deleteAfter(client, peer, sent, duration(t.DeleteReplyAfter), logger)
	}
	logger.Info("keyword.triggered", slog.Int("message", message.ID))
}

// deleteAfter 在 delay 之后删掉一条消息，0 表示马上删。计时不占协程；进程退出时
// 还没到点的删除随之放弃（v2 也一样，任务不落盘）。
func (s *service) deleteAfter(client *bot.Client, peer tg.InputPeerClass, id int, delay time.Duration, logger *slog.Logger) {
	run := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// 已经被别人删掉的消息不算失败。
		if err := client.Delete(ctx, peer, []int{id}); err != nil && !tgerr.Is(err, "MESSAGE_ID_INVALID") {
			logger.Warn("keyword.delete_failed", slog.Int("message", id), slog.String("error", err.Error()))
		}
	}
	if delay <= 0 {
		run()
		return
	}
	s.later(delay, run)
}

// banRights 是完全封禁：看不到也发不了，到 until 为止。
func banRights(until int) tg.ChatBannedRights {
	return tg.ChatBannedRights{UntilDate: until, ViewMessages: true, SendMessages: true, SendMedia: true,
		SendStickers: true, SendGifs: true, SendGames: true, SendInline: true, EmbedLinks: true}
}

// muteRights 是 restrict：留在群里，但什么都发不了。v2 只设了 sendMessages 一项，
// 新版 Telegram 里图片、贴纸这些要分别禁，只禁文字等于没禁住。
func muteRights(until int) tg.ChatBannedRights {
	return tg.ChatBannedRights{UntilDate: until, SendMessages: true, SendMedia: true, SendStickers: true,
		SendGifs: true, SendGames: true, SendInline: true, EmbedLinks: true, SendPolls: true,
		SendPhotos: true, SendVideos: true, SendRoundvideos: true, SendAudios: true, SendVoices: true,
		SendDocs: true, SendPlain: true}
}

// moderate 按任务封禁或禁言发送者，两个都设了时以封禁为准（同 v2）。
// 超级群组走 channels.editBanned；基本群组没有限时封禁，封禁改成移出、禁言做不到。
// 匿名管理员（发送者就是群组本身）和账号自己不处置。
func (s *service) moderate(ctx context.Context, client *bot.Client, chat tg.InputPeerClass, message *bot.Message, t task) error {
	seconds, ban := t.BanSeconds, true
	if seconds <= 0 {
		seconds, ban = t.RestrictSeconds, false
	}
	if seconds <= 0 || message.Sender == nil || bot.PeerID(message.Sender) == message.ChatID {
		return nil
	}
	if user, ok := message.Sender.(*tg.PeerUser); ok && user.UserID == client.SelfID() {
		return nil
	}
	target, err := client.InputPeer(message.Sender)
	if err != nil {
		return err
	}
	until := int(s.now().Add(duration(seconds)).Unix())
	rights := muteRights(until)
	if ban {
		rights = banRights(until)
	}
	if channel, ok := bot.InputChannel(chat); ok {
		_, err := client.API().ChannelsEditBanned(ctx, &tg.ChannelsEditBannedRequest{Channel: channel, Participant: target, BannedRights: rights})
		return err
	}
	group, ok := chat.(*tg.InputPeerChat)
	if !ok || !ban {
		// 私聊里没有封禁可言；基本群组不能禁言。都只记日志，不打扰对话。
		client.Logger().Info("keyword.moderate_unsupported", slog.String("chat", message.ChatID), slog.Bool("ban", ban))
		return nil
	}
	user, ok := bot.InputUser(target)
	if !ok {
		return nil
	}
	_, err = client.API().MessagesDeleteChatUser(ctx, &tg.MessagesDeleteChatUserRequest{ChatID: group.ChatID, UserID: user})
	return err
}
