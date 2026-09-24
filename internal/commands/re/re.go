// Package re 实现 .re：把被回复的消息（连同它之后的几条）重复转发。
package re

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/commands/save"
)

// Register 注册 .re：把被回复的消息以及它之后的几条转发到当前对话，可以转发多次。
func Register(a *app.App) {
	help := func(prefix string) string {
		p := command.Escape(prefix)
		return "🔁 <b>消息复读</b>\n\n回复一条消息，把从它开始往后的若干条消息转发到当前会话。\n\n• 回复消息发送 <code>" + p + "re</code> - 转发一条，一次\n• <code>" + p +
			"re 3</code> - 转发被回复的消息和它之后的 2 条，已删除的消息不算数\n• <code>" + p + "re 3 2</code> - 将这 3 条消息重复转发 2 次\n\n" +
			"消息数最多 20，复读次数最多 10。对话禁止转发时，改为把内容重新发一遍。"
	}
	a.Registry.Register(&command.Command{Name: "re", Description: "回复消息后重复转发，可指定数量和次数", Usage: "[消息数] [复读次数]", Help: help,
		// 禁止转发的对话里要把媒体下载再上传，最多 20 条乘 10 次，默认的 5 分钟不够。
		Timeout: 30 * time.Minute,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			return handle(ctx, a.Root, inv)
		}})
}

// handle 执行一次 .re。别人借用账号发起的，结束后和 MiBox 一样把对方那条命令也删掉；
// 没有管理权限删不了，不算失败。
func handle(ctx context.Context, root string, inv *command.Invocation) error {
	err := repeat(ctx, root, inv)
	if inv.Trigger != nil {
		_ = inv.Client.DeleteMessage(ctx, inv.Trigger)
	}
	return err
}

// repeat 读出要复读的消息，转发 times 次；对话禁止转发时改为复制。
func repeat(ctx context.Context, root string, inv *command.Invocation) error {
	reply, err := inv.Client.GetReply(ctx, inv.Message)
	if err != nil {
		return err
	}
	count, _ := strconv.Atoi(inv.Arg(0))
	times, _ := strconv.Atoi(inv.Arg(1))
	count, times = kit.Clamp(count, 1, 20), kit.Clamp(times, 1, 10)
	if reply == nil {
		return inv.EditText(ctx, "请回复一条消息使用 "+inv.Prefix+"re [消息数] [复读次数]")
	}
	source, err := inv.Client.InputPeer(reply.Peer)
	if err != nil {
		return err
	}
	target, err := inv.Client.InputPeer(inv.Message.Peer)
	if err != nil {
		return err
	}
	messages, err := fetchFrom(ctx, inv.Client, source, reply.ID, count)
	if err != nil {
		return err
	}
	if len(messages) == 0 {
		return inv.EditText(ctx, "❌ 被回复的消息已经不在了")
	}
	ids := make([]int, len(messages))
	for index, message := range messages {
		ids[index] = message.ID
	}
	topic := forumTopic(inv.Message)
	forwarded := 0
	for ; forwarded < times; forwarded++ {
		err := inv.Client.ForwardToTopic(ctx, source, target, ids, topic)
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !tgerr.Is(err, "CHAT_FORWARDS_RESTRICTED") {
			return inv.EditText(ctx, "❌ 复读失败："+command.Brief(err))
		}
		break
	}
	// 对话禁止转发：剩下的次数改为把内容重新发一遍，和 MiBox 一样。
	for ; forwarded < times; forwarded++ {
		for _, message := range messages {
			err := save.CopyMessage(ctx, inv.Client, root, message, target, topic)
			if err == nil || errors.Is(err, save.ErrNothingToCopy) {
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return inv.EditText(ctx, "❌ 复读失败："+command.Brief(err))
		}
	}
	return inv.Client.DeleteMessage(ctx, inv.Message)
}

// forumTopic 返回命令消息所在的论坛话题，转发和复制都发到这里；借用账号时，代发的
// 命令也发在对方所在的话题里。不在论坛话题里返回 0：普通超级群里回复链的根消息
// 同样记在 reply_to_top_id 里，那不是话题。
func forumTopic(message *bot.Message) int {
	if message.Raw == nil {
		return 0
	}
	header, ok := message.Raw.ReplyTo.(*tg.MessageReplyHeader)
	if !ok || !header.ForumTopic {
		return 0
	}
	return message.TopicID
}

// fetchFrom 读出编号从 first 开始（含 first）往后的 count 条消息，按编号从小到大排。
//
// 做法和 MiBox 一样：offset_id 取 first、add_offset 取 -count，Telegram 返回的就是
// 编号不小于 first 的最早 count 条。已删除的编号不占名额；私聊和普通群的编号是
// 整个账号共用的，中间隔着别的对话的编号，也不会因此少转。服务消息（进群、置顶等）
// 占名额但转发不了，不转。
func fetchFrom(ctx context.Context, client *bot.Client, peer tg.InputPeerClass, first, count int) ([]*tg.Message, error) {
	result, err := client.API().MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, OffsetID: first, AddOffset: -count, Limit: count})
	if err != nil {
		return nil, err
	}
	page, _ := client.Unpack(result)
	var messages []*tg.Message
	for _, item := range page {
		if message, ok := item.(*tg.Message); ok && message.ID >= first {
			messages = append(messages, message)
		}
	}
	sort.Slice(messages, func(a, b int) bool { return messages[a].ID < messages[b].ID })
	return messages, nil
}
