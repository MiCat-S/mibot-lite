// Package re 实现 .re：把被回复的消息重复转发。
package re

import (
	"context"
	"strconv"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// Register 注册 .re：把被回复的消息（以及它之前的几条）转发到当前对话，
// 可以转发多次。
func Register(a *app.App) {
	help := func(prefix string) string {
		p := command.Escape(prefix)
		return "🔁 <b>消息复读</b>\n\n回复一条消息，将包含该消息在内的最近若干条消息转发到当前会话。\n\n• 回复消息发送 <code>" + p + "re</code> - 转发一条，一次\n• <code>" + p +
			"re 3</code> - 转发截至被回复消息的 3 条消息\n• <code>" + p + "re 3 2</code> - 将这 3 条消息重复转发 2 次\n\n消息数最多 20，复读次数最多 10，目标消息需要允许转发。"
	}
	a.Registry.Register(&command.Command{Name: "re", Description: "回复消息后重复转发，可指定数量和次数", Usage: "[消息数] [复读次数]", Help: help,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			reply, err := inv.Client.GetReply(ctx, inv.Message)
			if err != nil {
				return err
			}
			count, _ := strconv.Atoi(inv.Arg(0))
			repeat, _ := strconv.Atoi(inv.Arg(1))
			count, repeat = kit.Clamp(count, 1, 20), kit.Clamp(repeat, 1, 10)
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
			var ids []int
			for index := 0; index < count; index++ {
				if id := reply.ID - count + index + 1; id > 0 {
					ids = append(ids, id)
				}
			}
			for index := 0; index < repeat; index++ {
				if err := inv.Client.Forward(ctx, source, target, ids); err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					return inv.EditText(ctx, "复读失败：目标消息可能禁止转发")
				}
			}
			return inv.Client.DeleteMessage(ctx, inv.Message)
		}})
}
