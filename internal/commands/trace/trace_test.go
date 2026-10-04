package trace

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// 普通表情按出现顺序取、去重，❤️‍🔥 不会被拆成 ❤️；自定义表情从实体取，盖住的那个字符不再算普通表情。
func TestParse(t *testing.T) {
	got := parse("👍 abc ❤️‍🔥👍🔥", 0, nil)
	if display(got) != "👍 ❤️‍🔥 🔥" {
		t.Errorf("普通表情：%q", display(got))
	}
	text := "👍😎"
	start := command.UTF16Len(".trace ")
	entities := []tg.MessageEntityClass{&tg.MessageEntityCustomEmoji{Offset: start + 2, Length: 2, DocumentID: 555}}
	got = parse(text, start, entities)
	if display(got) != "👍 自定义：555" {
		t.Errorf("自定义表情：%q", display(got))
	}
	if len(parse("hello", 0, nil)) != 0 {
		t.Error("没有表情时应为空")
	}
}

// V2 的数据：表情可能是对象，也可能直接是字符串（数字字符串是自定义表情）。
func TestReactionJSON(t *testing.T) {
	var cfg config
	raw := `{"users":{"42":["👍",{"emoticon":"🔥"},"123"]},"keywords":{},"config":{"keepLog":false,"big":true}}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	items := cfg.Users["42"]
	if len(items) != 3 || items[0].Emoticon != "👍" || items[1].Emoticon != "🔥" || items[2].DocumentID != "123" || cfg.Config.KeepLog {
		t.Errorf("读出来的不对：%+v %+v", items, cfg.Config)
	}
}

// 先看发送者，再按关键词（字典序第一个命中的）。
func TestMatch(t *testing.T) {
	cfg := defaults()
	cfg.Users["42"] = []reaction{{Emoticon: "👍"}}
	cfg.Keywords["机场"] = []reaction{{Emoticon: "🔥"}}
	cfg.Keywords["测速"] = []reaction{{Emoticon: "🤡"}}
	if got := cfg.match("42", "机场"); display(got) != "👍" {
		t.Errorf("发送者应优先：%q", display(got))
	}
	if got := cfg.match("7", "机场测速"); display(got) != "🔥" {
		t.Errorf("关键词：%q", display(got))
	}
	if got := cfg.match("7", "你好"); got != nil {
		t.Errorf("没命中：%v", got)
	}
}

type recorder struct {
	requests []*tg.MessagesSendReactionRequest
}

func (r *recorder) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	if request, ok := input.(*tg.MessagesSendReactionRequest); ok {
		r.requests = append(r.requests, request)
		var buffer bin.Buffer
		_ = (&tg.Updates{}).Encode(&buffer)
		return output.Decode(&buffer)
	}
	return nil
}

// 别人的消息命中就排队，后台给它点上；不占用消息。
func TestOfferReacts(t *testing.T) {
	fake := &recorder{}
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 1}, slog.New(slog.DiscardHandler))
	s := &service{store: store.New(filepath.Join(t.TempDir(), "trace.json"), defaults), queue: make(chan job, 8)}
	if err := s.update(func(cfg *config) { cfg.Keywords["机场"] = []reaction{{Emoticon: "🔥"}, {DocumentID: "9"}} }); err != nil {
		t.Fatal(err)
	}
	message := &bot.Message{ID: 7, Peer: &tg.PeerChat{ChatID: 5}, ChatID: "-5", Sender: &tg.PeerUser{UserID: 42}, Text: "推荐个机场"}
	if s.offer(context.Background(), client, message) {
		t.Error("不该占用消息")
	}
	s.offer(context.Background(), client, &bot.Message{ID: 8, Peer: &tg.PeerChat{ChatID: 5}, ChatID: "-5", Sender: &tg.PeerUser{UserID: 42}, Text: "你好"})
	if len(s.queue) != 1 {
		t.Fatalf("应只排进一条：%d", len(s.queue))
	}
	item := <-s.queue
	if err := react(context.Background(), client, item); err != nil {
		t.Fatal(err)
	}
	if len(fake.requests) != 1 || fake.requests[0].MsgID != 7 || !fake.requests[0].Big || len(fake.requests[0].Reaction) != 2 {
		t.Fatalf("回应请求不对：%+v", fake.requests)
	}
	if emoji, ok := fake.requests[0].Reaction[0].(*tg.ReactionEmoji); !ok || emoji.Emoticon != "🔥" {
		t.Errorf("第一个表情：%+v", fake.requests[0].Reaction[0])
	}
	if custom, ok := fake.requests[0].Reaction[1].(*tg.ReactionCustomEmoji); !ok || custom.DocumentID != 9 {
		t.Errorf("自定义表情：%+v", fake.requests[0].Reaction[1])
	}
}
