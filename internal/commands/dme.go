package commands

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
)

type dmeConfig struct {
	BatchSize     int `json:"batchSize"`
	SearchLimit   int `json:"searchLimit"`
	RetryAttempts int `json:"retryAttempts"`
}

func dmeDefaults() dmeConfig { return dmeConfig{BatchSize: 50, SearchLimit: 100, RetryAttempts: 3} }

// placeholderPNG 是防撤回模式用来替换媒体的图片。
var placeholderPNG = sync.OnceValue(func() []byte {
	canvas := image.NewRGBA(image.Rect(0, 0, 256, 256))
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			canvas.Set(x, y, color.RGBA{R: 40, G: 40, B: 40, A: 255})
		}
	}
	var buffer bytes.Buffer
	_ = png.Encode(&buffer, canvas)
	return buffer.Bytes()
})

func dmeHelp(prefix string) string {
	p := command.Escape(prefix)
	return "🗑️ <b>智能防撤回删除</b>\n\n<code>" + p + "dme 数量</code> 快速删除自己的消息\n<code>" + p + "dme -f 数量</code> 替换文本和媒体后删除\n<code>" + p +
		"dme 999999</code> 删除全部可见的自己的消息\n普通数量单次最多 2000 条；仅处理命令之前的消息及当前话题。\n收藏夹直接删除；-f 模式下广播频道主直接按数量删除。\n防撤回编辑可能因消息类型、编辑时限或权限失败，不保证第三方副本被删除。"
}

// Dme 注册 .dme。
func Dme(a *app.App) {
	cfgStore := newStore(a, "dme.json", dmeDefaults)
	var mu sync.Mutex
	active := map[string]bool{}
	a.Registry.Register(&command.Command{Name: "dme", Description: "删除自己的消息，支持防撤回模式", Usage: "[-f] 数量", Help: dmeHelp, Timeout: -1,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			sub := strings.ToLower(inv.Arg(0))
			if sub == "" || sub == "help" || sub == "h" {
				return inv.Edit(ctx, dmeHelp(inv.Prefix))
			}
			anti := sub == "-f"
			token := inv.Arg(0)
			if anti {
				token = inv.Arg(1)
			}
			count, err := strconv.Atoi(token)
			if token == "" || !regexp.MustCompile(`^\d+$`).MatchString(token) || err != nil || count <= 0 {
				return inv.EditText(ctx, "参数错误：请指定正整数删除数量")
			}
			mu.Lock()
			if active[inv.Message.ChatID] {
				mu.Unlock()
				return inv.EditText(ctx, "当前会话已有 DME 删除任务正在执行，请等待任务完成")
			}
			active[inv.Message.ChatID] = true
			mu.Unlock()
			defer func() { mu.Lock(); delete(active, inv.Message.ChatID); mu.Unlock() }()
			cfg, err := cfgStore.Read()
			if err != nil {
				return err
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
			if err := dmeExecute(ctx, inv, count, anti, cfg); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				inv.Log.Error("dme.failed", slog.String("error", err.Error()))
				_, _ = inv.Client.SendSelf(context.WithoutCancel(ctx), command.Escape("DME "+inv.Message.ChatID+" 操作失败，请检查权限、网络和 Telegram 限制后重试。"))
			}
			return nil
		}})
}

// dmeRun 是一次 .dme 的全部状态：要删哪里、谁算自己、翻到了哪一页、删了多少。
type dmeRun struct {
	ctx     context.Context
	inv     *command.Invocation
	client  *bot.Client
	api     *tg.Client
	message *bot.Message
	peer    tg.InputPeerClass
	cfg     dmeConfig
	anti    bool

	isChannel bool
	// direct 表示不必判断是谁发的，按数量直接删：收藏夹，或者 -f 模式下自己是广播频道的创建者。
	direct bool
	// identities 是在这个频道里能拿来发言的身份（以频道身份发言），它们发的也算自己的。
	identities map[string]bool
	topicID    int

	// 翻页状态。history 为真时翻完整历史，为假时用「只看自己发的」搜索。
	history  bool
	offsetID int
	empty    int
	seen     map[int]bool

	matched, deleted, failed, edited, failedEdits int
}

// newDmeRun 弄清楚在哪删、哪些消息算自己的、从哪种翻页方式开始。
func newDmeRun(ctx context.Context, inv *command.Invocation, anti bool, cfg dmeConfig) (*dmeRun, error) {
	client, message := inv.Client, inv.Message
	peer, err := client.InputPeer(message.Peer)
	if err != nil {
		return nil, err
	}
	run := &dmeRun{ctx: ctx, inv: inv, client: client, api: client.API(), message: message, peer: peer, cfg: cfg, anti: anti,
		identities: map[string]bool{}, topicID: message.TopicID, offsetID: message.ID, seen: map[int]bool{}}
	channelID, isChannel := message.Channel()
	run.isChannel = isChannel
	var channelInfo bot.ChannelInfo
	if isChannel {
		channelInfo, _ = client.Peers().Channel(channelID)
	}
	noforwards := channelInfo.Noforwards
	if chat, ok := message.Peer.(*tg.PeerChat); ok {
		if info, known := client.Peers().Chat(chat.ChatID); known {
			noforwards = info.Noforwards
		}
	}
	run.direct = message.Saved
	inputChannel, _ := bot.InputChannel(peer)
	if anti && isChannel && channelInfo.Broadcast && inputChannel != nil {
		result, err := run.api.ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{Channel: inputChannel, Participant: &tg.InputPeerSelf{}})
		if err == nil {
			_, run.direct = result.Participant.(*tg.ChannelParticipantCreator)
		}
	}
	if !run.direct && isChannel {
		if result, err := run.api.ChannelsGetSendAs(ctx, &tg.ChannelsGetSendAsRequest{Peer: peer}); err == nil {
			for _, item := range result.Peers {
				if key := peerKey(item.Peer); key != "" {
					run.identities[key] = true
				}
			}
		}
	}
	// 按发送者搜索在频道和禁止转发的对话里不可靠，那些地方一开始就翻历史。
	run.history = run.direct || isChannel || noforwards
	return run, nil
}

func (r *dmeRun) retry(fn func() error) error { return retryFlood(r.ctx, r.cfg.RetryAttempts, fn) }

// mine 判断一条消息是不是自己发的：自己账号发的，或者以自己能用的频道身份发的。
func (r *dmeRun) mine(m *tg.Message) bool {
	if m.Out {
		return true
	}
	if from, ok := m.GetFromID(); ok {
		if user, isUser := from.(*tg.PeerUser); isUser && user.UserID == r.client.SelfID() {
			return true
		}
		return r.identities[peerKey(from)]
	}
	return false
}

// topicOf 读出一条消息所在的论坛话题，不在话题里返回 0。
func topicOf(m *tg.Message) int {
	if header, ok := m.GetReplyTo(); ok {
		if reply, ok := header.(*tg.MessageReplyHeader); ok {
			if top, ok := reply.GetReplyToTopID(); ok && top > 0 {
				return top
			}
			if reply.ForumTopic && reply.ReplyToMsgID > 0 {
				return reply.ReplyToMsgID
			}
		}
	}
	return 0
}

// eligible 判断一条消息该不该删：在命令之前、在当前话题里、并且是自己的。
func (r *dmeRun) eligible(m *tg.Message) bool {
	return m.ID > 0 && m.ID < r.message.ID && (r.topicID == 0 || topicOf(m) == r.topicID) && (r.direct || r.mine(m))
}

// scrub 是防撤回模式：删之前先把文字改成「占位符」、把媒体换成一张灰图，
// 这样对方即使存了消息的副本，看到的也是改过的内容。
// 两天前的消息已经过了编辑时限；链接预览和贴纸换不了，都跳过。
func (r *dmeRun) scrub(m *tg.Message) {
	if m.Date > 0 && time.Now().Unix()-int64(m.Date) > 172800 {
		return
	}
	if m.Media == nil {
		if m.Message != "占位符" {
			request := &tg.MessagesEditMessageRequest{Peer: r.peer, ID: m.ID}
			request.SetMessage("占位符")
			if _, err := r.api.MessagesEditMessage(r.ctx, request); err != nil {
				r.failedEdits++
				return
			}
		}
		r.edited++
		return
	}
	switch media := m.Media.(type) {
	case *tg.MessageMediaWebPage:
		return
	case *tg.MessageMediaDocument:
		if document, ok := media.Document.(*tg.Document); ok {
			for _, attribute := range document.Attributes {
				if _, sticker := attribute.(*tg.DocumentAttributeSticker); sticker {
					return
				}
			}
		}
	}
	file, err := r.client.Uploader().FromBytes(r.ctx, "dme.png", placeholderPNG())
	if err != nil {
		r.failedEdits++
		return
	}
	request := &tg.MessagesEditMessageRequest{Peer: r.peer, ID: m.ID}
	request.SetMessage("")
	request.SetMedia(&tg.InputMediaUploadedPhoto{File: file})
	if _, err := r.api.MessagesEditMessage(r.ctx, request); err != nil {
		r.failedEdits++
		return
	}
	r.edited++
}

// deleteBatch 分批删除。一批删失败就对半拆开再试，拆到只剩一条还删不掉，就记为失败跳过；
// 顺利的话每批加大 5 条，最多 100。
func (r *dmeRun) deleteBatch(ids []int) error {
	size := r.cfg.BatchSize
	for offset := 0; offset < len(ids); {
		end := min(len(ids), offset+size)
		batch := ids[offset:end]
		if err := r.retry(func() error { return r.client.Delete(r.ctx, r.peer, batch) }); err != nil {
			if r.ctx.Err() != nil {
				return r.ctx.Err()
			}
			if len(batch) == 1 {
				r.failed++
				offset++
			} else {
				size = max(1, len(batch)/2)
			}
		} else {
			r.deleted += len(batch)
			offset += len(batch)
			size = min(100, size+5)
			_, _ = r.api.UpdatesGetState(r.ctx)
		}
		if err := sleepCtx(r.ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

// pageLimit 决定这一页取多少条。翻历史时一页取满 100；搜索时不多取超过还差的条数。
func (r *dmeRun) pageLimit(target int, unlimited bool) int {
	if r.history {
		return 100
	}
	if unlimited {
		return min(100, r.cfg.SearchLimit)
	}
	return min(100, r.cfg.SearchLimit, target-r.matched)
}

func (r *dmeRun) fetch(limit int) ([]tg.MessageClass, error) {
	var result tg.MessagesMessagesClass
	var err error
	if r.history {
		result, err = r.api.MessagesGetHistory(r.ctx, &tg.MessagesGetHistoryRequest{Peer: r.peer, OffsetID: r.offsetID, Limit: limit, MaxID: r.message.ID})
	} else {
		request := &tg.MessagesSearchRequest{Peer: r.peer, Q: "", Filter: &tg.InputMessagesFilterEmpty{}, OffsetID: r.offsetID, Limit: limit, MaxID: r.message.ID}
		request.SetFromID(&tg.InputPeerSelf{})
		if r.topicID != 0 {
			request.SetTopMsgID(r.topicID)
		}
		result, err = r.api.MessagesSearch(r.ctx, request)
	}
	if err != nil {
		return nil, err
	}
	page, _ := r.client.Unpack(result)
	return page, nil
}

// pageOutcome 是取一页之后该怎么走。
type pageOutcome int

const (
	pageReady     pageOutcome = iota // 拿到了一页
	pageSwitched                     // 搜索走不通，已改成翻历史，从头再来
	pageExhausted                    // 历史翻完了
)

// nextPage 取下一页并把游标往前推。搜索失败或者搜不到时退回翻历史；
// 历史也翻完了就结束。游标没有前进说明服务器返回有问题，当错误处理，免得死循环。
func (r *dmeRun) nextPage(limit int) ([]tg.MessageClass, pageOutcome, error) {
	var page []tg.MessageClass
	err := retryFlood(r.ctx, 2, func() error {
		var err error
		page, err = r.fetch(limit)
		return err
	})
	if err != nil {
		if r.ctx.Err() != nil || r.history {
			return nil, 0, err
		}
		r.history, r.offsetID = true, r.message.ID
		return nil, pageSwitched, nil
	}
	if len(page) == 0 {
		if !r.history {
			r.history, r.offsetID = true, r.message.ID
			return nil, pageSwitched, nil
		}
		return nil, pageExhausted, nil
	}
	next := 0
	for _, item := range page {
		if id := messageID(item); id > 0 && (next == 0 || id < next) {
			next = id
		}
	}
	if next == 0 || next >= r.offsetID {
		return nil, 0, fmt.Errorf("DME history cursor did not advance")
	}
	r.offsetID = next
	return page, pageReady, nil
}

// pick 从一页里挑出该删、还没处理过的消息，不超过还差的条数。
func (r *dmeRun) pick(page []tg.MessageClass, target int, unlimited bool) []*tg.Message {
	var batch []*tg.Message
	for _, item := range page {
		m, ok := item.(*tg.Message)
		if ok && r.eligible(m) && !r.seen[m.ID] {
			batch = append(batch, m)
		}
	}
	if !unlimited && len(batch) > target-r.matched {
		batch = batch[:target-r.matched]
	}
	return batch
}

// giveUp 在连续多页都没有可删的消息时放弃，免得在别人的长历史里一直翻下去。
// 频道里别人的消息往往成片出现，所以给得多一些。
func (r *dmeRun) giveUp() bool {
	r.empty++
	emptyLimit := 3
	if r.isChannel {
		emptyLimit = 300
	}
	return r.history && !r.direct && r.empty >= emptyLimit
}

// process 删掉一批；防撤回模式下先逐条改掉内容，改过的话等一秒再删，让编辑先生效。
func (r *dmeRun) process(batch []*tg.Message) error {
	r.empty = 0
	ids := make([]int, 0, len(batch))
	for _, m := range batch {
		r.seen[m.ID] = true
		ids = append(ids, m.ID)
	}
	r.matched += len(batch)
	if r.anti && !r.direct {
		before := r.edited
		for _, m := range batch {
			if err := r.ctx.Err(); err != nil {
				return err
			}
			r.scrub(m)
		}
		if r.edited > before {
			if err := sleepCtx(r.ctx, time.Second); err != nil {
				return err
			}
		}
	}
	return r.deleteBatch(ids)
}

// forgetPassed 丢掉游标之后的去重记录：翻历史时那些编号再也不会出现，留着只占内存。
func (r *dmeRun) forgetPassed() {
	if !r.history {
		return
	}
	for id := range r.seen {
		if id >= r.offsetID {
			delete(r.seen, id)
		}
	}
}

// report 写日志；有删不掉或改不了的，再给收藏夹发一条说明。
func (r *dmeRun) report(requested int) {
	r.inv.Log.Info("dme.complete", slog.Int("requested", requested), slog.Int("matched", r.matched), slog.Int("deleted", r.deleted), slog.Int("edited", r.edited), slog.Int("failed", r.failed), slog.Int("failed_edits", r.failedEdits))
	if r.failed > 0 || r.failedEdits > 0 {
		_, _ = r.client.SendSelf(r.ctx, command.Escape(fmt.Sprintf("DME %s：已删除 %d 条；删除失败 %d 条；防撤回编辑失败 %d 条。", r.message.ChatID, r.deleted, r.failed, r.failedEdits)))
	}
}

// dmeExecute 删掉命令之前自己的 count 条消息。999999 表示全部，其余最多 2000。
func dmeExecute(ctx context.Context, inv *command.Invocation, count int, anti bool, cfg dmeConfig) error {
	run, err := newDmeRun(ctx, inv, anti, cfg)
	if err != nil {
		return err
	}
	if err := run.retry(func() error { return run.client.Delete(ctx, run.peer, []int{run.message.ID}) }); err != nil {
		inv.Log.Warn("dme.command_delete", slog.String("error", err.Error()))
	}
	target, unlimited := count, count == 999999
	if !unlimited && target > 2000 {
		target = 2000
	}
	for unlimited || run.matched < target {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, outcome, err := run.nextPage(run.pageLimit(target, unlimited))
		if err != nil {
			return err
		}
		if outcome == pageSwitched {
			continue
		}
		if outcome == pageExhausted {
			break
		}
		if batch := run.pick(page, target, unlimited); len(batch) == 0 {
			if run.giveUp() {
				break
			}
		} else if err := run.process(batch); err != nil {
			return err
		}
		run.forgetPassed()
		if err := sleepCtx(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	run.report(count)
	return nil
}
