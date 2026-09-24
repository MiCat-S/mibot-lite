package ids

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
)

func TestEstimateCreation(t *testing.T) {
	now := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	if got := estimateCreation(215959394, now).Year(); got != 2016 {
		t.Errorf("id 215959394 estimated in %d, want 2016", got)
	}
	// 更大的 id 估出的时间不会更早。
	previous := time.Time{}
	for id := int64(0); id <= 8_000_000_000; id += 250_000_000 {
		got := estimateCreation(id, now)
		if got.Before(previous) {
			t.Fatalf("id %d went back in time", id)
		}
		previous = got
	}
	// 超出表的范围后，估计值继续往后推，到当前时间为止。
	if got := estimateCreation(8_600_000_000, now); !got.After(time.Unix(1767225600, 0)) && !got.Equal(now) {
		t.Errorf("an id past the table landed at %v", got)
	}
	if got := estimateCreation(99_000_000_000, now); !got.Equal(now) {
		t.Errorf("a far future id was not clamped to now: %v", got)
	}
}

func TestRenderIDs(t *testing.T) {
	now := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	user := renderIDs(&entityInfo{kind: "user", id: 215959394, name: "Cat", username: "cat", dc: 5, common: 3,
		flags: []string{"⭐ Premium"}}, time.Date(2024, 1, 2, 3, 4, 0, 0, time.Local), now)
	for _, wanted := range []string{"215959394", "@cat", "DC5（新加坡）", "2024-01-02 03:04", "tg://user?id=215959394", "t.me/cat", "⭐ Premium"} {
		if !strings.Contains(user, wanted) {
			t.Errorf("user card lost %q:\n%s", wanted, user)
		}
	}
	channel := renderIDs(&entityInfo{kind: "supergroup", id: 1771725356, name: "群", members: 42}, time.Time{}, now)
	for _, wanted := range []string{"超级群", "-1001771725356", "42", "看不出"} {
		if !strings.Contains(channel, wanted) {
			t.Errorf("channel card lost %q:\n%s", wanted, channel)
		}
	}
	if strings.Contains(channel, "注册时间") || strings.Contains(channel, "tg://user") {
		t.Errorf("a channel was described like a person:\n%s", channel)
	}
}

// 没见过的用户照样给出 ID、估算的注册时间和跳转链接，但不假装知道 DC 和共同群组。
func TestRenderIDsUnknownUser(t *testing.T) {
	now := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	card := renderIDs(&entityInfo{kind: "user", id: 6000000000, name: "用户 6000000000", unknown: true}, time.Time{}, now)
	for _, wanted := range []string{"6000000000", "注册时间", "DC：未知", "tg://user?id=6000000000", "tg://openmessage?user_id=6000000000"} {
		if !strings.Contains(card, wanted) {
			t.Errorf("unknown user card lost %q:\n%s", wanted, card)
		}
	}
	if strings.Contains(card, "共同群组") || strings.Contains(card, "看不出") {
		t.Errorf("an unknown user was described as if looked up:\n%s", card)
	}
}

// offlineClient 是只有 peer 缓存、不连网络的客户端；里面记着用户 42。
func offlineClient() *bot.Client {
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	peers.RememberUsers([]tg.UserClass{&tg.User{ID: 42, AccessHash: 7, FirstName: "Known"}})
	return bot.FromAPI(nil, peers, &tg.User{ID: 1, Self: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// 点名提及带着用户 ID，优先用它；没见过的数字 ID 只留下 ID，没见过的对话报错。
func TestResolveArgument(t *testing.T) {
	ctx := context.Background()
	mention := &tg.Message{Message: ".ids Known"}
	mention.SetEntities([]tg.MessageEntityClass{&tg.MessageEntityBold{Length: 4}, &tg.MessageEntityMentionName{Offset: 5, Length: 5, UserID: 42}})
	invocation := func(raw *tg.Message) *command.Invocation {
		return &command.Invocation{Client: offlineClient(), Message: &bot.Message{Raw: raw}}
	}

	found, err := resolveArgument(ctx, invocation(mention), "Known")
	if err != nil || found.peer == nil {
		t.Fatalf("a mention of a known user: %+v %v", found, err)
	}
	if user, ok := found.peer.(*tg.InputPeerUser); !ok || user.UserID != 42 || user.AccessHash != 7 {
		t.Errorf("the mention resolved to %#v", found.peer)
	}

	unseen := &tg.Message{Message: ".ids Stranger"}
	unseen.SetEntities([]tg.MessageEntityClass{&tg.MessageEntityMentionName{Offset: 5, Length: 8, UserID: 99}})
	if found, err := resolveArgument(ctx, invocation(unseen), "Stranger"); err != nil || found.peer != nil || found.userID != 99 {
		t.Errorf("a mention of an unseen user: %+v %v", found, err)
	}
	if found, err := resolveArgument(ctx, invocation(nil), "123456"); err != nil || found.peer != nil || found.userID != 123456 {
		t.Errorf("an unseen numeric id: %+v %v", found, err)
	}
	if _, err := resolveArgument(ctx, invocation(nil), "-1001234"); err == nil {
		t.Error("an unseen channel id was accepted")
	}
}
