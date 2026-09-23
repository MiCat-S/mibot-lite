package commands

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
)

// facesClient 造一个有头像的自己、一个有头像的用户、一个没头像的用户，以及一个有头像的频道。
func facesClient(fake *fakeTelegram) *bot.Client {
	peers := bot.NewPeerCache()
	peers.SetSelf(fake.self)
	// 用户的头像是可选字段，要用 SetPhoto 才会设上标志位；直接赋值会被当成没有头像。
	withPhoto := withHash(&tg.User{ID: dmeOther})
	withPhoto.SetPhoto(&tg.UserProfilePhoto{PhotoID: 7, DCID: 2})
	noPhoto := withHash(&tg.User{ID: 201})
	channel := withHash(&tg.Channel{ID: dmeAlias, Broadcast: true, Title: "频道", Photo: &tg.ChatPhoto{PhotoID: 8, DCID: 2}})
	peers.RememberUsers([]tg.UserClass{withPhoto, noPhoto})
	peers.RememberChats([]tg.ChatClass{channel})
	self := &tg.User{ID: fake.self, Self: true, FirstName: "Cat"}
	self.SetPhoto(&tg.UserProfilePhoto{PhotoID: 9, DCID: 2})
	return bot.FromAPI(tg.NewClient(fake), peers, self, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// 对方是频道、自己戴着皮套，都要能取到头像；这两种以前一律被拒。
func TestEatgifFaces(t *testing.T) {
	group := dmeSupergroup
	for _, c := range []struct {
		name   string
		sender tg.PeerClass // 发命令的身份；nil 表示本人
		target tg.PeerClass
		want   []string
		fails  string
	}{
		{name: "回复用户", target: &tg.PeerUser{UserID: dmeOther}, want: []string{"avatar self", "avatar user200"}},
		{name: "回复频道", target: &tg.PeerChannel{ChannelID: dmeAlias}, want: []string{"avatar self", "avatar channel300"}},
		{name: "戴皮套发命令", sender: &tg.PeerChannel{ChannelID: dmeAlias}, target: &tg.PeerUser{UserID: dmeOther},
			want: []string{"avatar channel300", "avatar user200"}},
		{name: "对方没有公开头像", target: &tg.PeerUser{UserID: 201}, fails: "没有公开头像"},
	} {
		fake := newFakeTelegram(t, dmeSelf)
		client := facesClient(fake)
		sender := c.sender
		if sender == nil {
			sender = &tg.PeerUser{UserID: dmeSelf}
		}
		message := &bot.Message{ID: dmeCommand, Peer: group, ChatID: bot.PeerID(group), Out: true, Sender: sender}
		reply := &bot.Message{ID: 5, Peer: group, ChatID: bot.PeerID(group), Sender: c.target}
		pair, err := (&eatgifService{}).faces(context.Background(), fakeInvocation(client, message), reply)
		if c.fails != "" {
			if detail, _ := isUserError(err); !strings.Contains(detail, c.fails) {
				t.Errorf("%s：应该报「%s」，实际 %v", c.name, c.fails, err)
			}
			continue
		}
		if err != nil || pair.me == nil || pair.you == nil {
			t.Errorf("%s：没取到头像：%v", c.name, err)
			continue
		}
		var got []string
		for _, call := range fake.calls {
			if strings.HasPrefix(call, "avatar ") && strings.HasSuffix(call, "big=false") {
				got = append(got, strings.TrimSuffix(call, " big=false"))
			}
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s：取的是 %v 的头像，应为 %v", c.name, got, c.want)
		}
	}
}
