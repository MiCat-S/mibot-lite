package yvlu

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
)

// .yvlu 的参数语法按位置解析，而且不规则；下面这些是用户早已用顺手的写法。
func TestParseYvlu(t *testing.T) {
	cases := []struct {
		text     string
		ok       bool
		count    int
		reply    bool
		format   string
		fakeText string
	}{
		{".yvlu", true, 1, false, "quote", ""},
		{".yvlu 3", true, 3, false, "quote", ""},
		{".yvlu r", true, 1, true, "quote", ""},
		{".yvlu r 4", true, 4, true, "quote", ""},
		{".yvlu r stories 2", true, 2, true, "stories", ""},
		{".yvlu png", true, 1, false, "image", ""},
		{".yvlu image 5", true, 5, false, "image", ""},
		{".yvlu stories", true, 1, false, "stories", ""},
		{".yvlu f 你好 世界", true, 1, false, "quote", "你好 世界"},
		{".yvlu fr 测试", true, 1, true, "quote", "测试"},
		{".yvlu u 12345 2", true, 2, false, "quote", ""},
		{".yvlu ur @name", true, 1, true, "quote", ""},
		{".yvlu nonsense", false, 0, false, "", ""},
	}
	for _, item := range cases {
		fields := strings.Fields(item.text)
		inv := &command.Invocation{Prefix: ".", Command: "yvlu", Args: fields[1:], Text: item.text}
		options, ok := parseYvlu(inv)
		if ok != item.ok {
			t.Errorf("%q: parsed=%v, want %v", item.text, ok, item.ok)
			continue
		}
		if !ok {
			continue
		}
		if options.Count != item.count || options.IncludeReply != item.reply || options.Format != item.format {
			t.Errorf("%q: count=%d reply=%v format=%q", item.text, options.Count, options.IncludeReply, options.Format)
		}
		if options.FakeText != item.fakeText {
			t.Errorf("%q: fake text %q, want %q", item.text, options.FakeText, item.fakeText)
		}
	}
}

func TestConvertEntities(t *testing.T) {
	entities := []tg.MessageEntityClass{
		&tg.MessageEntityBold{Offset: 0, Length: 4},
		&tg.MessageEntityTextURL{Offset: 5, Length: 3, URL: "https://example.com"},
		&tg.MessageEntityCustomEmoji{Offset: 9, Length: 2, DocumentID: 77},
		&tg.MessageEntityPre{Offset: 12, Length: 5, Language: "go"},
		&tg.MessageEntityUnknown{Offset: 20, Length: 1},
	}
	converted := convertEntities(entities, 0)
	if len(converted) != 4 {
		t.Fatalf("converted %d entities, want 4 (the unknown one is dropped)", len(converted))
	}
	if converted[0].Type != "bold" || converted[1].Type != "text_link" || converted[1].URL != "https://example.com" {
		t.Fatalf("unexpected conversion: %+v", converted)
	}
	if converted[2].CustomEmojiID != "77" || converted[3].Language != "go" {
		t.Fatalf("unexpected conversion: %+v", converted)
	}
}

// 伪造的文字是从命令中间截出来的，所以保留下来的实体要跟着平移，
// 横跨截断点的实体要被裁掉一截。
func TestConvertEntitiesShift(t *testing.T) {
	converted := convertEntities([]tg.MessageEntityClass{
		&tg.MessageEntityBold{Offset: 10, Length: 4},
		&tg.MessageEntityItalic{Offset: 6, Length: 6},
		&tg.MessageEntityCode{Offset: 0, Length: 3},
	}, 8)
	if len(converted) != 2 {
		t.Fatalf("converted %+v, want the two that reach past the cut", converted)
	}
	if converted[0].Offset != 2 || converted[0].Length != 4 {
		t.Errorf("shifted entity is %+v", converted[0])
	}
	if converted[1].Offset != 0 || converted[1].Length != 4 {
		t.Errorf("straddling entity should clip to 0..4, got %+v", converted[1])
	}
}

func TestNameHashIsStableAndPositive(t *testing.T) {
	first, second := nameHash("某个频道"), nameHash("某个频道")
	if first != second {
		t.Fatalf("the same name hashed to %d and %d", first, second)
	}
	if first < 0 {
		t.Fatalf("hash must be positive to serve as an id, got %d", first)
	}
	if nameHash("a") == nameHash("b") {
		t.Fatal("different names should not collide this easily")
	}
}

// offlineInvocation 造一个不联网的调用：缓存里有转发者 300、原作者 200，
// 客户端没有接任何连接，碰到 RPC 就会出错，所以下面的测试只走纯缓存的路径。
func offlineInvocation() *command.Invocation {
	peers := bot.NewPeerCache()
	peers.SetSelf(100)
	peers.RememberUsers([]tg.UserClass{
		&tg.User{ID: 200, AccessHash: 2002, FirstName: "原作者"},
		&tg.User{ID: 300, AccessHash: 3003, FirstName: "转发者"},
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := bot.FromAPI(tg.NewClient(nil), peers, &tg.User{ID: 100, Self: true}, logger)
	return &command.Invocation{Prefix: ".", Client: client, Log: logger}
}

// forwarded 造一条由 300 转发进群的消息，header 描述原作者。
func forwarded(header tg.MessageFwdHeader) *bot.Message {
	group := &tg.PeerChat{ChatID: 9}
	raw := &tg.Message{ID: 5, PeerID: group, Message: "原话"}
	raw.SetFromID(&tg.PeerUser{UserID: 300})
	raw.SetFwdFrom(header)
	return &bot.Message{ID: 5, Peer: group, Sender: &tg.PeerUser{UserID: 300}, Text: "原话", Raw: raw}
}

// 转发的消息署原作者的名，头像也用原作者的；隐藏了账号的来源只有名字，没有头像；
// 缓存里查不到的来源退回头部的名字，但保留它的 ID。
func TestForwardedAuthorAndFace(t *testing.T) {
	inv := offlineInvocation()
	service := &yvluService{}

	known := tg.MessageFwdHeader{}
	known.SetFromID(&tg.PeerUser{UserID: 200})
	from, face, err := service.author(inv, forwarded(known), &yvluOptions{})
	if err != nil || from.ID != 200 || from.Name != "原作者" {
		t.Fatalf("署名 %+v %v，应为原作者 200", from, err)
	}
	if user, ok := face.(*tg.InputPeerUser); !ok || user.UserID != 200 || user.AccessHash != 2002 {
		t.Errorf("头像取的是 %#v，应为原作者 200 的", face)
	}

	hidden := tg.MessageFwdHeader{}
	hidden.SetFromName("匿名的人")
	from, face, err = service.author(inv, forwarded(hidden), &yvluOptions{})
	if err != nil || from.Name != "匿名的人" || face != nil {
		t.Errorf("隐藏账号的来源：%+v 头像 %#v %v，应只有名字、没有头像", from, face, err)
	}

	unknown := tg.MessageFwdHeader{}
	unknown.SetFromID(&tg.PeerUser{UserID: 999})
	from, face, err = service.author(inv, forwarded(unknown), &yvluOptions{})
	if err != nil || from.ID != 999 || from.Name != "未知来源" || face != nil {
		t.Errorf("查不到的来源：%+v 头像 %#v %v", from, face, err)
	}

	// 不是转发的消息，署名和头像都是发送者自己。
	group := &tg.PeerChat{ChatID: 9}
	plain := &bot.Message{ID: 6, Peer: group, Sender: &tg.PeerUser{UserID: 300}, Raw: &tg.Message{ID: 6, PeerID: group}}
	from, face, err = service.author(inv, plain, &yvluOptions{})
	if user, ok := face.(*tg.InputPeerUser); err != nil || from.Name != "转发者" || !ok || user.UserID != 300 {
		t.Errorf("普通消息：%+v 头像 %#v %v", from, face, err)
	}
}

// 伪造发送者时，每条消息都拿到署名的一份副本，互不影响；头像取伪造对象的。
func TestFakeAuthorIsCopied(t *testing.T) {
	inv := offlineInvocation()
	options := &yvluOptions{FakeSender: &tg.InputPeerSelf{}, FakeAuthor: &quoteFrom{ID: 100, Name: "我"}}
	first, face, _ := (&yvluService{}).author(inv, forwarded(tg.MessageFwdHeader{}), options)
	first.Name = ""
	second, _, _ := (&yvluService{}).author(inv, forwarded(tg.MessageFwdHeader{}), options)
	if second.Name != "我" || options.FakeAuthor.Name != "我" {
		t.Errorf("署名被改掉了：%+v", second)
	}
	if _, ok := face.(*tg.InputPeerSelf); !ok {
		t.Errorf("头像应取伪造对象的，实际 %#v", face)
	}
}

func TestStickerDocument(t *testing.T) {
	webm := stickerDocument([]byte{0x1a, 0x45, 0xdf, 0xa3, 0, 0}, "webm", 7)
	if webm.MimeType != "video/webm" || webm.ReplyTo != 7 {
		t.Fatalf("webm 参数 %+v", webm)
	}
	var video *tg.DocumentAttributeVideo
	sticker := false
	for _, attribute := range webm.Attributes {
		switch value := attribute.(type) {
		case *tg.DocumentAttributeVideo:
			video = value
		case *tg.DocumentAttributeSticker:
			sticker = true
		}
	}
	// 读不出文件头时按 512x512 填，而不是 teleproto 的 1x1。
	if !sticker || video == nil || video.W != 512 || video.H != 512 || video.SupportsStreaming {
		t.Errorf("webm 贴纸应带贴纸属性和视频属性，实际 %+v", webm.Attributes)
	}
	webp := stickerDocument([]byte("not a webp"), "webp", 0)
	if size, ok := webp.Attributes[1].(*tg.DocumentAttributeImageSize); !ok || size.W != 512 || size.H != 768 {
		t.Errorf("webp 贴纸的尺寸属性 %+v", webp.Attributes)
	}
}

func TestPhotoFits(t *testing.T) {
	encode := func(width, height int) []byte {
		var buffer bytes.Buffer
		_ = png.Encode(&buffer, image.NewGray(image.Rect(0, 0, width, height)))
		return buffer.Bytes()
	}
	for _, c := range []struct {
		w, h int
		fits bool
	}{{1024, 1536, true}, {720, 1280, true}, {6000, 5000, false}, {2100, 100, false}, {2000, 100, true}} {
		if got := photoFits(encode(c.w, c.h)); got != c.fits {
			t.Errorf("%dx%d：能否当照片发 %v，应为 %v", c.w, c.h, got, c.fits)
		}
	}
	if photoFits([]byte("garbage")) {
		t.Error("解不出尺寸的数据不该当照片发")
	}
}
