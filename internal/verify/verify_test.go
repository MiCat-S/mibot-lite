package verify

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
)

// 收藏夹里即使有文字相同的消息，也只能查和删本次命令及其发送回执里的编号。
func TestStatusVerificationTracksOnlyItsOwnAnswer(t *testing.T) {
	const owner, commandID, answerID = 7, 20, 24
	var deleted []int
	rpc := telegram.InvokeFunc(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
		switch req := input.(type) {
		case *tg.MessagesSendMessageRequest:
			output.(*tg.UpdatesBox).Updates = &tg.UpdateShortSentMessage{ID: commandID}
		case *tg.MessagesSendMediaRequest:
			output.(*tg.UpdatesBox).Updates = &tg.UpdateShortSentMessage{ID: answerID}
		case *tg.MessagesGetMessagesRequest:
			id := req.ID[0].(*tg.InputMessageID).ID
			message := &tg.Message{ID: id, PeerID: &tg.PeerUser{UserID: owner}}
			switch id {
			case commandID:
				message.Message = ".status"
			case answerID:
				message.SetRichMessage(tg.RichMessage{Blocks: []tg.PageBlockClass{
					&tg.PageBlockParagraph{Text: bot.RichPlain("运行时间")}}})
			default:
				t.Fatalf("read unrelated saved message %d", id)
			}
			output.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{message}}
		case *tg.MessagesDeleteMessagesRequest:
			deleted = append(deleted, req.ID...)
		default:
			t.Fatalf("unexpected RPC %T", req)
		}
		return nil
	})
	recorder := bot.SentRecorder{Self: func() int64 { return owner }, Mark: func(tg.PeerClass, int) {}}
	api := tg.NewClient(recorder.Handle(rpc))
	client := bot.FromAPI(api, bot.NewPeerCache(), &tg.User{ID: owner}, slog.Default())
	peer := &tg.InputPeerSelf{}
	dispatch := func(ctx context.Context, _ *tg.Message) bool {
		_, err := api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{Peer: peer, Media: &tg.InputMediaEmpty{}})
		return err == nil
	}
	res := run(t.Context(), client, peer, ".", Case{Command: "status", Expect: "运行时间", Reply: true, Wait: 3 * time.Second}, dispatch)
	if !res.Passed || !slices.Equal(deleted, []int{answerID, commandID}) {
		t.Fatalf("result=%+v deleted=%v", res, deleted)
	}
}
