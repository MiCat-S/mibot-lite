package dme

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// dmeTrigger 是一个对话的触发词：别人在 ChatID 里发的消息包含 Keyword 时，删掉自己在这里、
// 那条消息之前的 Count 条消息（999999 表示全部）。Anti 表示先替换内容再删，同 .dme -f。
type dmeTrigger struct {
	ChatID  string `json:"chatId"`
	Keyword string `json:"keyword"`
	Count   int    `json:"count"`
	Anti    bool   `json:"anti,omitempty"`
}

const (
	// maxKeywordRunes 和 maxChatTriggers 限制触发词的长度和每个对话的个数：别人的每条消息
	// 都要和这个对话的每个触发词比一遍。
	maxKeywordRunes = 64
	maxChatTriggers = 20
	// triggerRuns 是同时在跑的触发删除个数上限。
	triggerRuns = 2
	// allMessages 是「全部」的条数，同 .dme 999999。
	allMessages = 999999
)

// indexed 是一个触发词和它的小写形式。
type indexed struct {
	dmeTrigger
	lower string
}

// triggerIndex 是按对话分好的触发词。
type triggerIndex map[string][]indexed

func buildIndex(triggers []dmeTrigger) triggerIndex {
	index := triggerIndex{}
	for _, t := range triggers {
		index[t.ChatID] = append(index[t.ChatID], indexed{dmeTrigger: t, lower: strings.ToLower(t.Keyword)})
	}
	return index
}

// match 找出 text 包含的、这个对话的第一个触发词，不区分大小写。
func (index triggerIndex) match(chatID, text string) (dmeTrigger, bool) {
	list := index[chatID]
	if len(list) == 0 {
		return dmeTrigger{}, false
	}
	lower := strings.ToLower(text)
	for _, t := range list {
		if strings.Contains(lower, t.lower) {
			return t.dmeTrigger, true
		}
	}
	return dmeTrigger{}, false
}

// service 是 .dme 的全部状态：设置文件、正在删的对话、触发词表。命令和看别人消息的监听者共用它。
type service struct {
	store  *store.Store[dmeConfig]
	logger *slog.Logger
	// mu 让「改文件、换触发词表」成为一步，两个同时进行的修改不会互相覆盖对方换上的表。
	mu sync.Mutex
	// cached 是触发词表。别人的每条消息都要查一遍，不能每次都读文件；改了就整份换掉。
	cached atomic.Pointer[triggerIndex]
	// runMu 管 active：正在删的对话。同一个对话同时只跑一个删除，命令和触发词都算。
	runMu  sync.Mutex
	active map[string]bool
	// slots 限制同时在跑的触发删除，占不到就丢掉这次触发：别人刷屏时不能一直排队。
	slots chan struct{}
	// track 把一次触发删除算进 Registry.Busy；维护或退出期间返回 false，这次就不删了。
	track func() (func(), bool)
	// background 让删除离开更新处理的路径，sweep 真正去删。测试里都换掉。
	background func(func())
	sweep      func(ctx context.Context, client *bot.Client, message *bot.Message, t dmeTrigger)
}

func newService(saved *store.Store[dmeConfig], logger *slog.Logger, track func() (func(), bool)) *service {
	s := &service{store: saved, logger: logger, active: map[string]bool{}, slots: make(chan struct{}, triggerRuns), track: track}
	s.background = func(work func()) {
		go func() {
			// 单独开的协程里 panic 没人接住会带倒整个进程。
			defer func() {
				if recovered := recover(); recovered != nil {
					logger.Error("dme.panic", slog.Any("panic", recovered), slog.String("stack", string(debug.Stack())))
				}
			}()
			work()
		}()
	}
	s.sweep = s.sweepTriggered
	return s
}

// claim 占下一个对话；这个对话已经在删时返回 false。
func (s *service) claim(chatID string) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.active[chatID] {
		return false
	}
	s.active[chatID] = true
	return true
}

func (s *service) release(chatID string) {
	s.runMu.Lock()
	delete(s.active, chatID)
	s.runMu.Unlock()
}

// config 读设置，超出范围的值换回默认。
func (s *service) config() (dmeConfig, error) {
	cfg, err := s.store.Read()
	if err != nil {
		return cfg, err
	}
	if cfg.BatchSize < 5 || cfg.BatchSize > 100 {
		cfg.BatchSize = 50
	}
	if cfg.SearchLimit < 10 || cfg.SearchLimit > 500 {
		cfg.SearchLimit = 100
	}
	if cfg.RetryAttempts < 0 || cfg.RetryAttempts > 10 {
		cfg.RetryAttempts = 3
	}
	return cfg, nil
}

// current 返回触发词表，第一次用到时从文件读。文件读不了就当没有触发词，记一条错误：
// 这时 .dme kw 的每条子命令都会报出同一个错误，用户看得到。
func (s *service) current() triggerIndex {
	if loaded := s.cached.Load(); loaded != nil {
		return *loaded
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if loaded := s.cached.Load(); loaded != nil {
		return *loaded
	}
	cfg, err := s.store.Read()
	if err != nil {
		if s.logger != nil {
			s.logger.Error("dme.load_failed", slog.String("error", err.Error()))
		}
		cfg = dmeDefaults()
	}
	index := buildIndex(cfg.Triggers)
	s.cached.Store(&index)
	return index
}

// update 修改文件，并换上新的触发词表。
func (s *service) update(mutate func(*dmeConfig) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.Update(mutate); err != nil {
		return err
	}
	cfg, err := s.store.Read()
	if err != nil {
		return err
	}
	index := buildIndex(cfg.Triggers)
	s.cached.Store(&index)
	return nil
}

// offer 是看别人消息的监听者：消息包含这个对话的触发词，就到后台删掉自己在这里、这条消息
// 之前的消息。比较只是几次字符串查找，很快；命中了才去占名额。这个对话已经在删，或者同时
// 在跑的触发删除满了，就丢掉这次。
//
// 永远返回 false，也就是不认领这条消息：同一条消息还得交给后面的监听者（关键词回复、.sudo）。
func (s *service) offer(ctx context.Context, client *bot.Client, message *bot.Message) bool {
	if message.Text == "" || message.Out {
		return false
	}
	hit, ok := s.current().match(message.ChatID, message.Text)
	if !ok {
		return false
	}
	select {
	case s.slots <- struct{}{}:
	default:
		s.logger.Warn("dme.trigger_dropped", slog.String("chat", message.ChatID), slog.String("reason", "too many running"))
		return false
	}
	if !s.claim(message.ChatID) {
		<-s.slots
		s.logger.Debug("dme.trigger_dropped", slog.String("chat", message.ChatID), slog.String("reason", "already deleting here"))
		return false
	}
	done, ok := s.track()
	if !ok {
		s.release(message.ChatID)
		<-s.slots
		return false
	}
	s.background(func() {
		defer func() {
			done()
			s.release(message.ChatID)
			<-s.slots
		}()
		s.sweep(ctx, client, message, hit)
	})
	return false
}

// sweepTriggered 删掉 message 之前自己的消息。删除是别人触发的，账号主人未必在看，所以删掉了
// 或者失败了都在收藏夹留一条说明。
func (s *service) sweepTriggered(ctx context.Context, client *bot.Client, message *bot.Message, t dmeTrigger) {
	title := client.Peers().Title(message.Peer)
	s.logger.Info("dme.triggered", slog.String("chat", message.ChatID), slog.Int("message", message.ID),
		slog.Int("count", t.Count), slog.Bool("anti", t.Anti))
	fail := func(err error) {
		s.logger.Error("dme.trigger_failed", slog.String("chat", message.ChatID), slog.String("error", err.Error()))
		_, _ = client.SendSelf(ctx, command.Escape("❌ 在「"+title+"」按触发词删除自己的消息失败，请检查权限、网络和 Telegram 限制"))
	}
	cfg, err := s.config()
	if err != nil {
		fail(err)
		return
	}
	run, err := newDmeRun(ctx, client, message, s.logger, t.Anti, cfg)
	if err != nil {
		fail(err)
		return
	}
	if err := run.sweep(t.Count); err != nil {
		if ctx.Err() == nil {
			fail(err)
		}
		return
	}
	if run.deleted > 0 {
		_, _ = client.SendSelf(ctx, command.Escape(fmt.Sprintf("✅ 「%s」里有人发了触发词「%s」，已删除自己的 %d 条消息", title, t.Keyword, run.deleted)))
	}
}

// keywordCommand 处理 .dme kw：看、加、删触发词。回复都发到收藏夹、命令消息随即删掉，
// 免得对话里的人看到触发词；在收藏夹里发的就地回复。
func (s *service) keywordCommand(ctx context.Context, inv *command.Invocation) error {
	sub := strings.ToLower(inv.Arg(1))
	switch {
	case sub == "all" || (sub == "" && inv.Message.Saved):
		text, err := s.listAll(inv.Client)
		if err != nil {
			return err
		}
		return respond(ctx, inv, text)
	case sub == "":
		return respond(ctx, inv, s.listChat(inv.Client, inv.Message))
	case inv.Message.Saved:
		return kit.Fail("收藏夹里没有别人发消息，到要生效的对话里添加或删除触发词")
	case sub == "rm":
		keyword := strings.TrimSpace(inv.RawAfter(2))
		if keyword == "" {
			return kit.Usage(inv.Prefix, "dme kw rm 关键词")
		}
		removed := false
		if err := s.update(func(cfg *dmeConfig) error {
			cfg.Triggers = slices.DeleteFunc(cfg.Triggers, func(t dmeTrigger) bool {
				hit := t.ChatID == inv.Message.ChatID && strings.EqualFold(t.Keyword, keyword)
				removed = removed || hit
				return hit
			})
			return nil
		}); err != nil {
			return err
		}
		if !removed {
			// 不复述触发词：失败提示留在对话里，别人看得到。
			return kit.Fail("这个对话没有这个触发词，" + inv.Prefix + "dme kw 可以看全部")
		}
		return respond(ctx, inv, command.Escape("✅ 已删除「"+inv.Client.Peers().Title(inv.Message.Peer)+"」的触发词「"+keyword+"」"))
	}
	trigger, err := parseTrigger(inv)
	if err != nil {
		return err
	}
	if err := s.update(func(cfg *dmeConfig) error {
		for i := range cfg.Triggers {
			if cfg.Triggers[i].ChatID == trigger.ChatID && strings.EqualFold(cfg.Triggers[i].Keyword, trigger.Keyword) {
				cfg.Triggers[i] = trigger
				return nil
			}
		}
		chatCount := 0
		for _, t := range cfg.Triggers {
			if t.ChatID == trigger.ChatID {
				chatCount++
			}
		}
		if chatCount >= maxChatTriggers {
			return kit.Failf("一个对话最多 %d 个触发词，先用 %sdme kw rm 删掉不用的", maxChatTriggers, inv.Prefix)
		}
		cfg.Triggers = append(cfg.Triggers, trigger)
		return nil
	}); err != nil {
		return err
	}
	return respond(ctx, inv, command.Escape("✅ 已在「"+inv.Client.Peers().Title(inv.Message.Peer)+"」添加触发词「"+trigger.Keyword+
		"」：别人发的消息包含它时，"+describe(trigger)))
}

// parseTrigger 读 .dme kw [-f] 条数 关键词。关键词取原文，可以带空格。
func parseTrigger(inv *command.Invocation) (dmeTrigger, error) {
	usage := kit.Usage(inv.Prefix, "dme kw [-f] 条数 关键词")
	anti := strings.EqualFold(inv.Arg(1), "-f")
	index := 1
	if anti {
		index = 2
	}
	token := inv.Arg(index)
	count, err := strconv.Atoi(token)
	if !kit.IsDigits(token) || err != nil || count <= 0 {
		return dmeTrigger{}, usage
	}
	if count > 2000 && count != allMessages {
		count = 2000
	}
	keyword := strings.TrimSpace(inv.RawAfter(index + 1))
	if keyword == "" {
		return dmeTrigger{}, usage
	}
	if utf8.RuneCountInString(keyword) > maxKeywordRunes {
		return dmeTrigger{}, kit.Failf("触发词最多 %d 个字", maxKeywordRunes)
	}
	return dmeTrigger{ChatID: inv.Message.ChatID, Keyword: keyword, Count: count, Anti: anti}, nil
}

// describe 说一个触发词触发后删什么。
func describe(t dmeTrigger) string {
	what := "删掉自己在这里之前的 " + strconv.Itoa(t.Count) + " 条消息"
	if t.Count == allMessages {
		what = "删掉自己在这里之前的全部消息"
	}
	if t.Anti {
		what += "（先替换内容再删）"
	}
	return what
}

// listChat 列出一个对话的触发词。
func (s *service) listChat(client *bot.Client, message *bot.Message) string {
	title := "🗑️ <b>" + command.Escape(client.Peers().Title(message.Peer)) + " 的触发词</b>\n"
	list := s.current()[message.ChatID]
	if len(list) == 0 {
		return title + "这个对话还没有触发词"
	}
	rows := make([]string, 0, len(list))
	for _, t := range list {
		rows = append(rows, "• "+command.Code(t.Keyword)+" "+command.Escape(describe(t.dmeTrigger)))
	}
	return title + strings.Join(rows, "\n")
}

// listAll 按对话列出全部触发词，顺序同添加的顺序。
func (s *service) listAll(client *bot.Client) (string, error) {
	cfg, err := s.store.Read()
	if err != nil {
		return "", err
	}
	if len(cfg.Triggers) == 0 {
		return "🗑️ <b>触发词</b>\n还没有触发词", nil
	}
	var chats []string
	byChat := map[string][]dmeTrigger{}
	for _, t := range cfg.Triggers {
		if _, seen := byChat[t.ChatID]; !seen {
			chats = append(chats, t.ChatID)
		}
		byChat[t.ChatID] = append(byChat[t.ChatID], t)
	}
	sections := []string{"🗑️ <b>触发词</b>"}
	for _, chat := range chats {
		name := chat
		if peer, ok := bot.PeerFromID(chat); ok {
			name = client.Peers().Title(peer)
		}
		rows := []string{"<b>" + command.Escape(name) + "</b>"}
		for _, t := range byChat[chat] {
			rows = append(rows, "• "+command.Code(t.Keyword)+" "+command.Escape(describe(t)))
		}
		sections = append(sections, strings.Join(rows, "\n"))
	}
	return strings.Join(sections, "\n\n"), nil
}

// respond 给 .dme kw 回话：在收藏夹里就地改命令消息；在别的对话里把结果发到收藏夹，
// 再删掉命令消息。
func respond(ctx context.Context, inv *command.Invocation, html string) error {
	if inv.Message.Saved {
		return inv.Edit(ctx, html)
	}
	if _, err := inv.Client.SendSelf(ctx, html); err != nil {
		return err
	}
	return inv.Client.DeleteMessage(ctx, inv.Message)
}
