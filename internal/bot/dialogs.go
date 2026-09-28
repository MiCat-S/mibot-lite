package bot

import (
	"context"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// DialogPages 是 EachDialog 翻对话列表的范围。
type DialogPages struct {
	// Folder 是 0（主列表）或 1（归档）。
	Folder int
	// MaxPages 是最多翻几页，每页 100 个对话；0 表示 10 页。
	MaxPages int
}

// EachDialog 翻对话列表，每个对话调用一次 visit，同时把回复里带的用户和群组记进缓存
// （以后才有它们的 access hash）。遇到 FLOOD_WAIT 等够了再翻同一页：连接层只等 60 秒以内的，
// 账号在几百个群里时，翻完整个列表常常要等更久。
func (c *Client) EachDialog(ctx context.Context, pages DialogPages, visit func(*tg.Dialog)) error {
	limit := pages.MaxPages
	if limit <= 0 {
		limit = 10
	}
	offsetDate, offsetID := 0, 0
	var offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
	for page := 0; page < limit; page++ {
		request := &tg.MessagesGetDialogsRequest{OffsetDate: offsetDate, OffsetID: offsetID, OffsetPeer: offsetPeer, Limit: 100}
		if pages.Folder != 0 {
			request.SetFolderID(pages.Folder)
		}
		result, err := c.api.MessagesGetDialogs(ctx, request)
		if wait, flooded := tgerr.AsFloodWait(err); flooded {
			if sleep(ctx, wait) != nil {
				return ctx.Err()
			}
			page--
			continue
		}
		if err != nil {
			return err
		}
		var dialogs []tg.DialogClass
		var messages []tg.MessageClass
		last := true
		switch value := result.(type) {
		case *tg.MessagesDialogs:
			c.peers.RememberUsers(value.Users)
			c.peers.RememberChats(value.Chats)
			dialogs, messages = value.Dialogs, value.Messages
		case *tg.MessagesDialogsSlice:
			c.peers.RememberUsers(value.Users)
			c.peers.RememberChats(value.Chats)
			dialogs, messages, last = value.Dialogs, value.Messages, len(value.Dialogs) < 100
		}
		for _, entry := range dialogs {
			if dialog, ok := entry.(*tg.Dialog); ok {
				visit(dialog)
			}
		}
		if last || len(dialogs) == 0 {
			return nil
		}
		// 下一页从这一页最后一个对话接着翻：它的 peer、最后一条消息的编号和时间。
		tail, ok := dialogs[len(dialogs)-1].(*tg.Dialog)
		if !ok {
			return nil
		}
		next, ok := c.peers.InputPeer(tail.Peer)
		if !ok {
			return nil
		}
		offsetPeer, offsetID = next, tail.TopMessage
		for _, item := range messages {
			if message, ok := item.(*tg.Message); ok && message.ID == tail.TopMessage {
				offsetDate = message.Date
			}
		}
	}
	return nil
}

// sleep 等待 d，ctx 结束时提前返回。
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
