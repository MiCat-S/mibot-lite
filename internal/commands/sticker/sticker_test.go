package sticker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// packs 是一个只懂三个贴纸接口的假 Telegram：包名 → 已有张数，capacity 张就满。
type packs struct {
	count    map[string]int
	capacity int
	notMine  map[string]bool
	calls    []string
}

func (p *packs) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	respond := func(name string) error {
		var buffer bin.Buffer
		value := &tg.MessagesStickerSet{Set: tg.StickerSet{ShortName: name, Count: p.count[name]}}
		if err := value.Encode(&buffer); err != nil {
			return err
		}
		return output.Decode(&buffer)
	}
	switch request := input.(type) {
	case *tg.MessagesGetStickerSetRequest:
		name := request.Stickerset.(*tg.InputStickerSetShortName).ShortName
		p.calls = append(p.calls, "get "+name)
		if _, ok := p.count[name]; !ok {
			return tgerr.New(400, "STICKERSET_INVALID")
		}
		return respond(name)
	case *tg.StickersAddStickerToSetRequest:
		name := request.Stickerset.(*tg.InputStickerSetShortName).ShortName
		p.calls = append(p.calls, "add "+name+" "+request.Sticker.Emoji)
		if p.notMine[name] {
			return tgerr.New(400, "STICKERSET_INVALID")
		}
		if p.count[name] >= p.capacity {
			return tgerr.New(400, "STICKERS_TOO_MUCH")
		}
		p.count[name]++
		return respond(name)
	case *tg.StickersCreateStickerSetRequest:
		p.calls = append(p.calls, fmt.Sprintf("create %s %q", request.ShortName, request.Title))
		p.count[request.ShortName] = 1
		return respond(request.ShortName)
	}
	return fmt.Errorf("假 Telegram 不认识 %T", input)
}

func client(p *packs, username string) *bot.Client {
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	self := &tg.User{ID: 1, Self: true, Username: username}
	return bot.FromAPI(tg.NewClient(p), peers, self, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func staticSticker() *found {
	return &found{document: &tg.InputDocument{ID: 9}, emoji: "😺", kind: staticKind}
}

func TestSaveFindsOrCreatesAPack(t *testing.T) {
	for _, c := range []struct {
		name     string
		existing map[string]int
		notMine  map[string]bool
		username string
		target   string
		sticker  *found
		wantPack string
		created  bool
		failure  string
		calls    []string
	}{
		{name: "没有包就新建第一个", username: "cat", sticker: staticSticker(),
			wantPack: "cat_static_1", created: true,
			calls: []string{"get cat_static_1", `create cat_static_1 "@cat 的收藏（静态）"`}},
		{name: "满了换下一个", username: "cat", existing: map[string]int{"cat_static_1": 3, "cat_static_2": 1}, sticker: staticSticker(),
			wantPack: "cat_static_2",
			calls:    []string{"get cat_static_1", "add cat_static_1 😺", "get cat_static_2", "add cat_static_2 😺"}},
		{name: "视频贴纸进视频包", username: "cat", sticker: &found{document: &tg.InputDocument{ID: 9}, emoji: "🎬", kind: videoKind},
			wantPack: "cat_video_1", created: true,
			calls: []string{"get cat_video_1", `create cat_video_1 "@cat 的收藏（视频）"`}},
		{name: "指定的包存在就加进去", username: "cat", target: "Mine", existing: map[string]int{"Mine": 1}, sticker: staticSticker(),
			wantPack: "Mine", calls: []string{"get Mine", "add Mine 😺"}},
		{name: "指定的包不存在就新建", username: "", target: "Mine", sticker: staticSticker(),
			wantPack: "Mine", created: true, calls: []string{"get Mine", `create Mine "Mine"`}},
		{name: "指定的包满了直接报满", username: "cat", target: "Mine", existing: map[string]int{"Mine": 3}, sticker: staticSticker(),
			failure: "贴纸包已满", calls: []string{"get Mine", "add Mine 😺"}},
		{name: "别人的包", username: "cat", target: "Theirs", existing: map[string]int{"Theirs": 1}, notMine: map[string]bool{"Theirs": true},
			sticker: staticSticker(), failure: "不是你建的", calls: []string{"get Theirs", "add Theirs 😺"}},
		{name: "没用户名又没指定包", username: "", sticker: staticSticker(), failure: "没有用户名"},
	} {
		fake := &packs{count: map[string]int{}, capacity: 3, notMine: c.notMine}
		for name, count := range c.existing {
			fake.count[name] = count
		}
		pack, created, err := save(context.Background(), client(fake, c.username), c.target, c.sticker)
		if c.failure != "" {
			text, _ := kit.IsUserError(err)
			if !strings.Contains(text, c.failure) {
				t.Errorf("%s：错误是 %v，应含 %q", c.name, err, c.failure)
			}
		} else if err != nil || pack != c.wantPack || created != c.created {
			t.Errorf("%s：得到 %q created=%v err=%v，应为 %q created=%v", c.name, pack, created, err, c.wantPack, c.created)
		}
		if !slices.Equal(fake.calls, c.calls) {
			t.Errorf("%s：请求\n  %s\n应为\n  %s", c.name, strings.Join(fake.calls, "\n  "), strings.Join(c.calls, "\n  "))
		}
	}
}

func TestStickerOf(t *testing.T) {
	message := func(mime, alt string, sticker bool) *tg.Message {
		attributes := []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "x"}}
		if sticker {
			attributes = append(attributes, &tg.DocumentAttributeSticker{Alt: alt, Stickerset: &tg.InputStickerSetEmpty{}})
		}
		value := &tg.Message{ID: 1}
		value.SetMedia(&tg.MessageMediaDocument{Document: &tg.Document{ID: 5, AccessHash: 6, MimeType: mime, Attributes: attributes}})
		return value
	}
	for _, c := range []struct {
		mime string
		kind kind
	}{{"image/webp", staticKind}, {"application/x-tgsticker", animatedKind}, {"video/webm", videoKind}} {
		got, ok := stickerOf(message(c.mime, "🐱", true))
		if !ok || got.kind != c.kind || got.emoji != "🐱" || got.document.ID != 5 {
			t.Errorf("%s：%+v %v", c.mime, got, ok)
		}
	}
	if got, ok := stickerOf(message("image/webp", "", true)); !ok || !slices.Contains(fallbackEmojis, got.emoji) {
		t.Errorf("没带 emoji 时应随机配一个，得到 %+v", got)
	}
	if _, ok := stickerOf(message("image/png", "", false)); ok {
		t.Error("普通图片文件不是贴纸")
	}
	if _, ok := stickerOf(&tg.Message{ID: 1}); ok {
		t.Error("没有媒体的消息不是贴纸")
	}
}

func TestValidShortName(t *testing.T) {
	for _, good := range []string{"a", "Cat_static_1", "x9"} {
		if !validShortName(good) {
			t.Errorf("%q 应该合法", good)
		}
	}
	for _, bad := range []string{"", "1abc", "_a", "a-b", "a b", "猫", strings.Repeat("a", 65)} {
		if validShortName(bad) {
			t.Errorf("%q 应该不合法", bad)
		}
	}
}
