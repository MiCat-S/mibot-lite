package bot

import (
	"context"
	"log/slog"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// dialogPage 造一页对话：群 from…to，每个群最后一条消息的编号就是群 ID，时间是 1000+ID。
func dialogPage(from, to int64) *tg.MessagesDialogsSlice {
	page := &tg.MessagesDialogsSlice{Count: 130}
	for id := from; id <= to; id++ {
		page.Dialogs = append(page.Dialogs, &tg.Dialog{Peer: &tg.PeerChannel{ChannelID: id}, TopMessage: int(id), NotifySettings: tg.PeerNotifySettings{}})
		page.Messages = append(page.Messages, &tg.Message{ID: int(id), Date: int(1000 + id), PeerID: &tg.PeerChannel{ChannelID: id}})
		page.Chats = append(page.Chats, channel(id))
	}
	return page
}

// 翻两页、中间碰到一次 FLOOD_WAIT：同一页重翻，第二页从第一页最后一个对话接着翻，
// 每个对话都访问到，群的 access hash 都记下来。
func TestEachDialog(t *testing.T) {
	var requests []*tg.MessagesGetDialogsRequest
	replies := []any{dialogPage(1, 100), tgerr.New(420, "FLOOD_WAIT_0"), dialogPage(101, 130)}
	api := tg.NewClient(invokerFunc(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		request, ok := input.(*tg.MessagesGetDialogsRequest)
		if !ok {
			t.Fatalf("意外的请求 %T", input)
		}
		requests = append(requests, request)
		if len(replies) == 0 {
			t.Fatal("翻过头了")
		}
		reply := replies[0]
		replies = replies[1:]
		if err, ok := reply.(error); ok {
			return err
		}
		return replying{reply.(bin.Encoder)}.Invoke(ctx, input, output)
	}))
	client := FromAPI(api, NewPeerCache(), &tg.User{ID: 1}, slog.New(slog.DiscardHandler))

	visited := 0
	if err := client.EachDialog(context.Background(), DialogPages{Folder: 1}, func(*tg.Dialog) { visited++ }); err != nil {
		t.Fatal(err)
	}
	if visited != 130 || len(requests) != 3 {
		t.Fatalf("访问了 %d 个对话、发了 %d 个请求", visited, len(requests))
	}
	if folder, ok := requests[0].GetFolderID(); !ok || folder != 1 {
		t.Errorf("没翻归档：%+v", requests[0])
	}
	second := requests[2]
	offset, ok := second.OffsetPeer.(*tg.InputPeerChannel)
	if !ok || offset.ChannelID != 100 || second.OffsetID != 100 || second.OffsetDate != 1100 {
		t.Errorf("第二页的起点不对：%+v", second)
	}
	if _, ok := client.Peers().Channel(130); !ok {
		t.Error("第二页的群没记下来")
	}
}
