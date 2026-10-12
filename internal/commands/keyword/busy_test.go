package keyword

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// 关键词动作正在发回复时要算进 Registry.Busy，自动更新才不会在这时重启；
// 维护期间命中的关键词不再开始新动作。
func TestKeywordActionCountsAsBusy(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	r := command.New(nil, logger)
	defer r.Abort()
	a := &app.App{Root: t.TempDir(), Registry: r, Logger: logger}
	Register(a)
	saved := store.New(filepath.Join(a.DataDir(), "keyword.json"), defaults)
	if err := saved.Update(func(doc *document) error {
		doc.Tasks = append(doc.Tasks, task{ID: 1, ChatID: "200", Key: "ad", Response: "reply", Include: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	started, release, finished := make(chan struct{}, 2), make(chan struct{}), make(chan struct{}, 2)
	fake := telegram.InvokeFunc(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		if _, ok := input.(*tg.MessagesSendMessageRequest); !ok {
			t.Errorf("意外的请求：%T", input)
			return nil
		}
		started <- struct{}{}
		defer func() { finished <- struct{}{} }()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
		}
		output.(*tg.UpdatesBox).Updates = &tg.UpdateShortSentMessage{ID: 9}
		return nil
	})
	peers := bot.NewPeerCache()
	peers.SetSelf(100)
	target := &tg.User{ID: 200}
	target.SetAccessHash(1)
	peers.RememberUsers([]tg.UserClass{target})
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 100}, logger)
	message := func(id int) *bot.Message {
		return &bot.Message{ID: id, ChatID: "200", Peer: &tg.PeerUser{UserID: 200}, Sender: &tg.PeerUser{UserID: 200}, Text: "ad"}
	}
	a.OfferForeign(r.Lifecycle(), client, message(1))
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("关键词回复没有开始")
	}
	if busy := r.Busy(); busy != 1 {
		t.Fatalf("回复进行中 Busy = %d", busy)
	}
	close(release)
	<-finished
	for r.Busy() != 0 {
		time.Sleep(time.Millisecond)
	}
	if !r.Quiesce(context.Background(), time.Second) {
		t.Fatal("没能进入维护")
	}
	a.OfferForeign(r.Lifecycle(), client, message(2))
	select {
	case <-started:
		t.Fatal("维护期间仍开始了关键词动作")
	case <-time.After(50 * time.Millisecond):
	}
}
