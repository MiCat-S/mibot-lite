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

// placeholderPNG is the image anti-recall mode swaps media for.
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

// Dme registers .dme.
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

func dmeExecute(ctx context.Context, inv *command.Invocation, count int, anti bool, cfg dmeConfig) error {
	client := inv.Client
	api := client.API()
	message := inv.Message
	peer, err := client.InputPeer(message.Peer)
	if err != nil {
		return err
	}
	channelID, isChannel := message.Channel()
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
	retry := func(fn func() error) error { return retryFlood(ctx, cfg.RetryAttempts, fn) }

	topicID := message.TopicID
	saved := message.Saved
	direct := saved
	inputChannel, _ := bot.InputChannel(peer)
	if anti && isChannel && channelInfo.Broadcast && inputChannel != nil {
		result, err := api.ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{Channel: inputChannel, Participant: &tg.InputPeerSelf{}})
		if err == nil {
			_, direct = result.Participant.(*tg.ChannelParticipantCreator)
		}
	}
	identities := map[string]bool{}
	if !direct && isChannel {
		if result, err := api.ChannelsGetSendAs(ctx, &tg.ChannelsGetSendAsRequest{Peer: peer}); err == nil {
			for _, item := range result.Peers {
				if key := peerKey(item.Peer); key != "" {
					identities[key] = true
				}
			}
		}
	}
	selfID := client.SelfID()
	mine := func(m *tg.Message) bool {
		if m.Out {
			return true
		}
		if from, ok := m.GetFromID(); ok {
			if user, isUser := from.(*tg.PeerUser); isUser && user.UserID == selfID {
				return true
			}
			return identities[peerKey(from)]
		}
		return false
	}
	topicOf := func(m *tg.Message) int {
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
	eligible := func(m *tg.Message) bool {
		return m.ID > 0 && m.ID < message.ID && (topicID == 0 || topicOf(m) == topicID) && (direct || mine(m))
	}

	if err := retry(func() error { return client.Delete(ctx, peer, []int{message.ID}) }); err != nil {
		inv.Log.Warn("dme.command_delete", slog.String("error", err.Error()))
	}

	edited, failedEdits, deleted, matched, failed := 0, 0, 0, 0, 0
	edit := func(m *tg.Message) {
		if m.Date > 0 && time.Now().Unix()-int64(m.Date) > 172800 {
			return
		}
		if m.Media == nil {
			if m.Message != "占位符" {
				request := &tg.MessagesEditMessageRequest{Peer: peer, ID: m.ID}
				request.SetMessage("占位符")
				if _, err := api.MessagesEditMessage(ctx, request); err != nil {
					failedEdits++
					return
				}
			}
			edited++
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
		file, err := client.Uploader().FromBytes(ctx, "dme.png", placeholderPNG())
		if err != nil {
			failedEdits++
			return
		}
		request := &tg.MessagesEditMessageRequest{Peer: peer, ID: m.ID}
		request.SetMessage("")
		request.SetMedia(&tg.InputMediaUploadedPhoto{File: file})
		if _, err := api.MessagesEditMessage(ctx, request); err != nil {
			failedEdits++
			return
		}
		edited++
	}
	deleteBatch := func(ids []int) error {
		size := cfg.BatchSize
		for offset := 0; offset < len(ids); {
			end := min(len(ids), offset+size)
			batch := ids[offset:end]
			if err := retry(func() error { return client.Delete(ctx, peer, batch) }); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if len(batch) == 1 {
					failed++
					offset++
				} else {
					size = max(1, len(batch)/2)
				}
			} else {
				deleted += len(batch)
				offset += len(batch)
				size = min(100, size+5)
				_, _ = api.UpdatesGetState(ctx)
			}
			if err := sleepCtx(ctx, 200*time.Millisecond); err != nil {
				return err
			}
		}
		return nil
	}

	target := count
	unlimited := count == 999999
	if !unlimited && target > 2000 {
		target = 2000
	}
	history := direct || isChannel || noforwards
	offsetID := message.ID
	empty := 0
	seen := map[int]bool{}
	for unlimited || matched < target {
		if err := ctx.Err(); err != nil {
			return err
		}
		limit := 100
		if !history {
			limit = min(100, cfg.SearchLimit, target-matched)
			if unlimited {
				limit = min(100, cfg.SearchLimit)
			}
		}
		var page []tg.MessageClass
		fetch := func() error {
			var result tg.MessagesMessagesClass
			var err error
			if history {
				result, err = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, OffsetID: offsetID, Limit: limit, MaxID: message.ID})
			} else {
				request := &tg.MessagesSearchRequest{Peer: peer, Q: "", Filter: &tg.InputMessagesFilterEmpty{}, OffsetID: offsetID, Limit: limit, MaxID: message.ID}
				request.SetFromID(&tg.InputPeerSelf{})
				if topicID != 0 {
					request.SetTopMsgID(topicID)
				}
				result, err = api.MessagesSearch(ctx, request)
			}
			if err != nil {
				return err
			}
			page, _ = client.Unpack(result)
			return nil
		}
		if err := retryFlood(ctx, 2, fetch); err != nil {
			if ctx.Err() != nil {
				return err
			}
			if history {
				return err
			}
			history, offsetID = true, message.ID
			continue
		}
		if len(page) == 0 {
			if !history {
				history, offsetID = true, message.ID
				continue
			}
			break
		}
		next := 0
		for _, item := range page {
			if id := messageID(item); id > 0 && (next == 0 || id < next) {
				next = id
			}
		}
		if next == 0 || next >= offsetID {
			return fmt.Errorf("DME history cursor did not advance")
		}
		offsetID = next
		var batch []*tg.Message
		for _, item := range page {
			m, ok := item.(*tg.Message)
			if ok && eligible(m) && !seen[m.ID] {
				batch = append(batch, m)
			}
		}
		if !unlimited && len(batch) > target-matched {
			batch = batch[:target-matched]
		}
		if len(batch) == 0 {
			empty++
			emptyLimit := 3
			if isChannel {
				emptyLimit = 300
			}
			if history && !direct && empty >= emptyLimit {
				break
			}
		} else {
			empty = 0
			ids := make([]int, 0, len(batch))
			for _, m := range batch {
				seen[m.ID] = true
				ids = append(ids, m.ID)
			}
			matched += len(batch)
			if anti && !direct {
				before := edited
				for _, m := range batch {
					if err := ctx.Err(); err != nil {
						return err
					}
					edit(m)
				}
				if edited > before {
					if err := sleepCtx(ctx, time.Second); err != nil {
						return err
					}
				}
			}
			if err := deleteBatch(ids); err != nil {
				return err
			}
		}
		if history {
			for id := range seen {
				if id >= offsetID {
					delete(seen, id)
				}
			}
		}
		if err := sleepCtx(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	inv.Log.Info("dme.complete", slog.Int("requested", count), slog.Int("matched", matched), slog.Int("deleted", deleted), slog.Int("edited", edited), slog.Int("failed", failed), slog.Int("failed_edits", failedEdits))
	if failed > 0 || failedEdits > 0 {
		_, _ = client.SendSelf(ctx, command.Escape(fmt.Sprintf("DME %s：已删除 %d 条；删除失败 %d 条；防撤回编辑失败 %d 条。", message.ChatID, deleted, failed, failedEdits)))
	}
	return nil
}

func messageID(item tg.MessageClass) int {
	switch value := item.(type) {
	case *tg.Message:
		return value.ID
	case *tg.MessageService:
		return value.ID
	case *tg.MessageEmpty:
		return value.ID
	}
	return 0
}
