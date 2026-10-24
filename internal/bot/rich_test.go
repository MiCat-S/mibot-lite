package bot

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/delivery"
)

// TestSendRich 检查 Rich Message 的请求：正文留空、不带实体，块原样放进 inputRichMessage，
// 回复照旧；编辑时不带 message 字段，MESSAGE_NOT_MODIFIED 算成功。
func TestSendRich(t *testing.T) {
	var sent *tg.MessagesSendMessageRequest
	var edited *tg.MessagesEditMessageRequest
	api := tg.NewClient(invokerFunc(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		switch request := input.(type) {
		case *tg.MessagesSendMessageRequest:
			sent = request
			return replying{&tg.UpdateShortSentMessage{ID: 77}}.Invoke(ctx, input, output)
		case *tg.MessagesEditMessageRequest:
			edited = request
			return tgerr.New(400, "MESSAGE_NOT_MODIFIED")
		}
		t.Fatalf("意外的请求 %T", input)
		return nil
	}))
	client := FromAPI(api, NewPeerCache(), &tg.User{ID: 1, Premium: true}, slog.New(slog.DiscardHandler))
	if !client.CanSendRich() {
		t.Fatal("有 Premium 应该能发 Rich Message")
	}
	if FromAPI(api, NewPeerCache(), &tg.User{ID: 1}, slog.New(slog.DiscardHandler)).CanSendRich() {
		t.Fatal("没有 Premium 不能发 Rich Message")
	}
	blocks := []tg.PageBlockClass{&tg.PageBlockParagraph{Text: &tg.TextPlain{Text: "你好"}}}
	id, err := client.SendRich(context.Background(), &tg.InputPeerSelf{}, blocks, SendOptions{ReplyTo: 5})
	if err != nil || id != 77 {
		t.Fatalf("发送结果不对：%d %v", id, err)
	}
	rich, ok := sent.GetRichMessage()
	input, isBlocks := rich.(*tg.InputRichMessage)
	if !ok || !isBlocks || len(input.Blocks) != 1 || sent.Message != "" || len(sent.Entities) != 0 {
		t.Fatalf("请求不对：%+v", sent)
	}
	if reply, ok := sent.GetReplyTo(); !ok || reply.(*tg.InputReplyToMessage).ReplyToMsgID != 5 {
		t.Fatalf("没有回复第 5 条：%+v", sent.ReplyTo)
	}
	if err := client.EditRich(context.Background(), &tg.InputPeerSelf{}, 9, blocks); err != nil {
		t.Fatal(err)
	}
	if _, hasText := edited.GetMessage(); hasText || edited.ID != 9 {
		t.Fatalf("编辑请求不对：%+v", edited)
	}
	if _, ok := edited.GetRichMessage(); !ok {
		t.Fatal("编辑请求没带 rich_message")
	}
}

// TestRichRejected 只有 Telegram 明确拒收（4xx，限流除外）才算，可以改发普通消息。
func TestRichRejected(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"成功", nil, false},
		{"没有 Premium", tgerr.New(403, "PREMIUM_ACCOUNT_REQUIRED"), true},
		{"块不合法", tgerr.New(400, "RICH_MESSAGE_INVALID"), true},
		{"限流", tgerr.New(420, "FLOOD_WAIT_5"), false},
		{"结果不明", delivery.Unknown(tgerr.New(500, "INTERNAL")), false},
		{"不是 RPC 错误", errors.New("断开了"), false},
	}
	for _, test := range cases {
		if got := RichRejected(test.err); got != test.want {
			t.Errorf("%s：got %v", test.name, got)
		}
	}
}

// TestRichMessageText 把收到的 Rich Message 摊平成纯文本。
func TestRichMessageText(t *testing.T) {
	plain := func(text string) tg.RichTextClass { return &tg.TextPlain{Text: text} }
	message := &tg.Message{}
	message.SetRichMessage(tg.RichMessage{Blocks: []tg.PageBlockClass{
		&tg.PageBlockHeading2{Text: plain("标题")},
		&tg.PageBlockParagraph{Text: &tg.TextConcat{Texts: []tg.RichTextClass{plain("看 "), &tg.TextURL{Text: &tg.TextBold{Text: plain("链接")}, URL: "https://e.com"}, plain(" 和 "), &tg.TextMath{Source: "x^2"}}}},
		&tg.PageBlockList{Items: []tg.PageListItemClass{
			&tg.PageListItemBlocks{Blocks: []tg.PageBlockClass{&tg.PageBlockParagraph{Text: plain("一")},
				&tg.PageBlockList{Items: []tg.PageListItemClass{&tg.PageListItemText{Text: plain("一点一")}}}}},
		}},
		&tg.PageBlockOrderedList{Items: []tg.PageListOrderedItemClass{&tg.PageListOrderedItemBlocks{Value: 3, Blocks: []tg.PageBlockClass{&tg.PageBlockParagraph{Text: plain("丙")}}}}},
		&tg.PageBlockTable{Title: &tg.TextEmpty{}, Rows: []tg.PageTableRow{{Cells: []tg.PageTableCell{{Text: plain("A")}, {Text: plain("B")}}}}},
		&tg.PageBlockDetails{Title: plain("详情"), Blocks: []tg.PageBlockClass{&tg.PageBlockMath{Source: "E=mc^2"}}},
	}})
	want := "标题\n看 链接 和 x^2\n• 一\n  • 一点一\n3. 丙\nA | B\n详情\nE=mc^2"
	if got := RichMessageText(message); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if got := RichMessageText(&tg.Message{Message: "普通消息"}); got != "" {
		t.Fatalf("普通消息没有 Rich 内容：%q", got)
	}
}

// TestRichFits 检查字数和块数的上限。
func TestRichFits(t *testing.T) {
	paragraph := func(text string) tg.PageBlockClass { return &tg.PageBlockParagraph{Text: &tg.TextPlain{Text: text}} }
	if !RichFits([]tg.PageBlockClass{paragraph(strings.Repeat("字", RichTextLimit))}) {
		t.Error("刚好到字数上限应该能发")
	}
	if RichFits([]tg.PageBlockClass{paragraph(strings.Repeat("字", RichTextLimit+1))}) {
		t.Error("超过字数上限不能发")
	}
	items := make([]tg.PageListItemClass, RichBlockLimit)
	for index := range items {
		items[index] = &tg.PageListItemBlocks{Blocks: []tg.PageBlockClass{paragraph("项")}}
	}
	if RichFits([]tg.PageBlockClass{&tg.PageBlockList{Items: items}}) {
		t.Error("列表项和里面的段落都算块，超过块数上限不能发")
	}
}
