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

func richRaw(id int, part bool, text string) *tg.Message {
	message := &tg.Message{ID: id, PeerID: &tg.PeerUser{UserID: 1}}
	message.SetRichMessage(tg.RichMessage{Part: part, Blocks: []tg.PageBlockClass{&tg.PageBlockParagraph{Text: &tg.TextPlain{Text: text}}}})
	return message
}

// Content 读得到富文本，但 Text 必须留空：命令解析读 Text，富文本里的「.restart」不能被执行。
func TestContentReadsRichButTextStaysEmpty(t *testing.T) {
	envelope, ok := Envelope(richRaw(5, false, ".restart"), 1, false, NewPeerCache())
	if !ok {
		t.Fatal("消息应该能规整")
	}
	if envelope.Text != "" || envelope.Content() != ".restart" {
		t.Fatalf("Text=%q Content=%q", envelope.Text, envelope.Content())
	}
	if got := (&Message{Text: "普通"}).Content(); got != "普通" {
		t.Fatalf("普通消息：%q", got)
	}
	var missing *Message
	if missing.Content() != "" || MessageText(nil) != "" {
		t.Fatal("空消息没有文字")
	}
	if got := MessageText(&tg.Message{Message: "普通"}); got != "普通" {
		t.Fatalf("普通消息：%q", got)
	}
}

// 只带开头的 Rich Message 才多发一次请求，取不到全文时留着开头。
func TestFullRich(t *testing.T) {
	calls, fail := 0, false
	api := tg.NewClient(invokerFunc(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
		if _, ok := input.(*tg.MessagesGetRichMessageRequest); !ok {
			t.Fatalf("意外的请求 %T", input)
		}
		calls++
		if fail {
			return tgerr.New(400, "MESSAGE_ID_INVALID")
		}
		output.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{richRaw(5, false, "全文")}}
		return nil
	}))
	client := FromAPI(api, NewPeerCache(), &tg.User{ID: 1}, slog.New(slog.DiscardHandler))
	whole := richRaw(5, false, "完整的")
	if got, err := client.FullRich(context.Background(), &tg.InputPeerSelf{}, whole); err != nil || got != whole || calls != 0 {
		t.Fatalf("完整消息不该请求：%v %d", err, calls)
	}
	part := richRaw(5, true, "开头")
	if got, err := client.FullRich(context.Background(), &tg.InputPeerSelf{}, part); err != nil || MessageText(got) != "全文" {
		t.Fatalf("全文：%q %v", MessageText(got), err)
	}
	fail = true
	if got, err := client.FullRich(context.Background(), &tg.InputPeerSelf{}, part); err == nil || got != part {
		t.Fatalf("取不到全文应留着开头：%v", err)
	}
}

// 能发就按块发；没有 Premium 或被拒收时退回纯文字，超长的截断并注明。
func TestCopyRich(t *testing.T) {
	var sent []*tg.MessagesSendMessageRequest
	reject := false
	api := tg.NewClient(invokerFunc(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		request := input.(*tg.MessagesSendMessageRequest)
		sent = append(sent, request)
		if _, rich := request.GetRichMessage(); rich && reject {
			return tgerr.New(400, "MEDIA_INVALID")
		}
		return replying{&tg.UpdateShortSentMessage{ID: 77}}.Invoke(ctx, input, output)
	}))
	premium := FromAPI(api, NewPeerCache(), &tg.User{ID: 1, Premium: true}, slog.New(slog.DiscardHandler))
	plain := FromAPI(api, NewPeerCache(), &tg.User{ID: 1}, slog.New(slog.DiscardHandler))
	message := richRaw(5, false, "正文")
	if id, err := premium.CopyRich(context.Background(), &tg.InputPeerSelf{}, message, SendOptions{}); err != nil || id != 77 || len(sent) != 1 || sent[0].Message != "" {
		t.Fatalf("按块发：%d %v %d", id, err, len(sent))
	}
	reject = true
	if _, err := premium.CopyRich(context.Background(), &tg.InputPeerSelf{}, message, SendOptions{}); err != nil || len(sent) != 3 || sent[2].Message != "正文" {
		t.Fatalf("拒收后退回纯文字：%v %d", err, len(sent))
	}
	if _, err := plain.CopyRich(context.Background(), &tg.InputPeerSelf{}, message, SendOptions{}); err != nil || len(sent) != 4 || sent[3].Message != "正文" {
		t.Fatalf("没有 Premium：%v %d", err, len(sent))
	}
	long := plainCopy(strings.Repeat("😀", 3000))
	if utf16Len(long) > 4096 || !strings.HasSuffix(long, "已截断）") {
		t.Fatalf("截断：%d", utf16Len(long))
	}
	if plainCopy("短") != "短" {
		t.Fatal("短文字不该截断")
	}
}
