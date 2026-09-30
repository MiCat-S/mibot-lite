package bot

import (
	"context"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

// SentRecorder 是连接中间件：本进程每发出一条新消息，都把它所在的对话和编号交给 Mark。
//
// 发消息的 RPC 结果不经过更新引擎（gotd 不分发它们），引擎的 pts 于是落后一步；下一条推送
// 到来时，它以为中间缺了更新，去补抓，把我们刚发的消息当成新消息再送一遍。那时它是「本账号
// 写的」，要是看上去像命令就会被执行：引用了别人名字的关键词回复、.save 复制来的别人的文字、
// 长结果的续页，开头都可能恰好是命令前缀，别人就能借此让账号执行只限本人的命令。
// App 用 Mark 把这些消息记进去重表，补抓回来时直接跳过。
type SentRecorder struct {
	// Self 返回本账号的 ID，把发往收藏夹（InputPeerSelf）的消息换算成对话 ID。
	Self func() int64
	Mark func(peer tg.PeerClass, id int)
}

// Handle 实现 telegram.Middleware。
func (r SentRecorder) Handle(next tg.Invoker) telegram.InvokeFunc {
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		err := next.Invoke(ctx, input, output)
		if err == nil && r.Mark != nil {
			r.record(input, output)
		}
		return err
	}
}

// record 从应答里找出新发出的消息。发消息、发媒体、转发、编辑的应答都装在 UpdatesBox 里；
// 私聊和基础群里发文字只回一个 updateShortSentMessage，没有对话，对话要从请求里取。
func (r SentRecorder) record(input bin.Encoder, output bin.Decoder) {
	box, ok := output.(*tg.UpdatesBox)
	if !ok {
		return
	}
	switch updates := box.Updates.(type) {
	case *tg.UpdateShortSentMessage:
		if request, ok := input.(*tg.MessagesSendMessageRequest); ok {
			if peer, ok := r.peerOf(request.Peer); ok {
				r.Mark(peer, updates.ID)
			}
		}
	case *tg.Updates:
		r.markNew(updates.Updates)
	case *tg.UpdatesCombined:
		r.markNew(updates.Updates)
	case *tg.UpdateShort:
		r.markNew([]tg.UpdateClass{updates.Update})
	}
}

// markNew 记下应答里的新消息。编辑过的消息不用记：派发器不执行带编辑的消息。
func (r SentRecorder) markNew(list []tg.UpdateClass) {
	for _, update := range list {
		var message tg.MessageClass
		switch value := update.(type) {
		case *tg.UpdateNewMessage:
			message = value.Message
		case *tg.UpdateNewChannelMessage:
			message = value.Message
		default:
			continue
		}
		if plain, ok := message.(*tg.Message); ok && plain.PeerID != nil {
			r.Mark(plain.PeerID, plain.ID)
		}
	}
}

func (r SentRecorder) peerOf(input tg.InputPeerClass) (tg.PeerClass, bool) {
	switch peer := input.(type) {
	case *tg.InputPeerSelf:
		if r.Self == nil || r.Self() == 0 {
			return nil, false
		}
		return &tg.PeerUser{UserID: r.Self()}, true
	case *tg.InputPeerUser:
		return &tg.PeerUser{UserID: peer.UserID}, true
	case *tg.InputPeerChat:
		return &tg.PeerChat{ChatID: peer.ChatID}, true
	case *tg.InputPeerChannel:
		return &tg.PeerChannel{ChannelID: peer.ChannelID}, true
	}
	return nil, false
}
