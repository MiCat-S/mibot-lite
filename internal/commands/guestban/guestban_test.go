package guestban

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// recorder 冒充 Telegram，记下删了哪些消息、封了谁。
type recorder struct {
	mu      sync.Mutex
	deleted []int
	banned  []string
}

func respond(output bin.Decoder, value bin.Encoder) error {
	var buffer bin.Buffer
	if err := value.Encode(&buffer); err != nil {
		return err
	}
	return output.Decode(&buffer)
}

func (r *recorder) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch request := input.(type) {
	case *tg.ChannelsDeleteMessagesRequest:
		r.deleted = append(r.deleted, request.ID...)
		return respond(output, &tg.MessagesAffectedMessages{Pts: 1, PtsCount: len(request.ID)})
	case *tg.ChannelsEditBannedRequest:
		switch target := request.Participant.(type) {
		case *tg.InputPeerUser:
			r.banned = append(r.banned, "user"+bot.PeerID(&tg.PeerUser{UserID: target.UserID}))
		case *tg.InputPeerChannel:
			r.banned = append(r.banned, "channel"+bot.PeerID(&tg.PeerChannel{ChannelID: target.ChannelID}))
		}
		return respond(output, &tg.Updates{})
	}
	return nil
}

func withHash[T interface{ SetAccessHash(int64) }](value T) T {
	value.SetAccessHash(9)
	return value
}

func setup(t *testing.T) (*service, *bot.Client, *recorder) {
	t.Helper()
	fake := &recorder{}
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	peers.RememberUsers([]tg.UserClass{withHash(&tg.User{ID: 42, Bot: true, Username: "ppzmzbot"}), withHash(&tg.User{ID: 77}),
		withHash(&tg.User{ID: 43, Bot: true, Username: "GoodBot"})})
	peers.RememberChats([]tg.ChatClass{withHash(&tg.Channel{ID: 5000, Megagroup: true, Title: "群"})})
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 1}, slog.New(slog.DiscardHandler))
	s := &service{store: store.New(filepath.Join(t.TempDir(), "guestban.json"), defaults), queue: make(chan event, queueSize),
		banned: map[string]time.Time{}, warned: map[string]time.Time{}}
	return s, client, fake
}

// guestMessage 造一条访客机器人 from 发、via 叫出来的消息；via 为 0 时是普通消息。
func guestMessage(id int, from, via int64) *bot.Message {
	raw := &tg.Message{ID: id, PeerID: &tg.PeerChannel{ChannelID: 5000}, FromID: &tg.PeerUser{UserID: from}, Message: "进直播间当水军，一次给500"}
	if via != 0 {
		raw.SetGuestchatViaFrom(&tg.PeerUser{UserID: via})
	}
	return &bot.Message{ID: id, Peer: raw.PeerID, ChatID: "-1005000", Raw: raw, Text: raw.Message}
}

// drain 把队列里排着的都处理掉。
func drain(s *service, client *bot.Client) {
	for {
		select {
		case item := <-s.queue:
			s.process(context.Background(), client, item)
		default:
			return
		}
	}
}

func TestGuestBotCleanup(t *testing.T) {
	s, client, fake := setup(t)
	ctx := context.Background()

	// 没开启的群：什么都不做。
	s.offer(ctx, client, guestMessage(10, 42, 77))
	drain(s, client)
	if len(fake.deleted)+len(fake.banned) != 0 {
		t.Fatalf("没开启就动手了：%v %v", fake.deleted, fake.banned)
	}

	if err := s.update(func(cfg *config) error { cfg.Groups["-1005000"] = &group{Enabled: true}; return nil }); err != nil {
		t.Fatal(err)
	}
	// 一阵刷屏：三条都删，机器人只封一次；普通消息不管；放行的机器人不管。
	for _, message := range []*bot.Message{guestMessage(11, 42, 77), guestMessage(12, 42, 78), guestMessage(13, 42, 77), guestMessage(14, 77, 0)} {
		if s.offer(ctx, client, message) {
			t.Error("监听者不该占用消息")
		}
	}
	if err := s.update(func(cfg *config) error { cfg.Allow = []string{"goodbot"}; return nil }); err != nil {
		t.Fatal(err)
	}
	s.offer(ctx, client, guestMessage(15, 43, 77))
	drain(s, client)
	if len(fake.deleted) != 3 || fake.deleted[0] != 11 || fake.deleted[2] != 13 {
		t.Errorf("删的消息不对：%v", fake.deleted)
	}
	if len(fake.banned) != 1 || fake.banned[0] != "user42" {
		t.Errorf("应只封机器人一次：%v", fake.banned)
	}
	cfg, _ := s.store.Read()
	if entry := cfg.Groups["-1005000"]; entry.Bots != 1 || entry.Messages != 3 {
		t.Errorf("统计不对：%+v", entry)
	}
}

// 严格模式：叫出机器人的人也封。
func TestStrictBansInvoker(t *testing.T) {
	s, client, fake := setup(t)
	if err := s.update(func(cfg *config) error { cfg.Groups["-1005000"] = &group{Enabled: true, Strict: true}; return nil }); err != nil {
		t.Fatal(err)
	}
	s.offer(context.Background(), client, guestMessage(20, 42, 77))
	drain(s, client)
	if len(fake.banned) != 2 || fake.banned[0] != "user42" || fake.banned[1] != "user77" {
		t.Errorf("严格模式应封机器人和叫它的人：%v", fake.banned)
	}
}

func TestAllowedAndUsername(t *testing.T) {
	cfg := config{Allow: []string{"goodbot", "123"}}
	if !cfg.allowed(5, "GoodBot") || !cfg.allowed(123, "") || cfg.allowed(5, "ppzmzbot") {
		t.Error("放行名单判断不对")
	}
	for name, want := range map[string]bool{"ppzmzbot": true, "abc": false, "1bot": false, "bad-name": false} {
		if validUsername(name) != want {
			t.Errorf("validUsername(%q) 应为 %v", name, want)
		}
	}
}
