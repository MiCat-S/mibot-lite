package commands

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
)

const (
	dmeSelf    = int64(100)
	dmeOther   = int64(200)
	dmeAlias   = int64(300) // 自己能以它的身份发言的频道
	dmeGroup   = int64(5000)
	dmeCommand = 1000
)

type dmeScenario struct {
	name     string
	peer     tg.PeerClass
	saved    bool
	topic    int
	count    int
	anti     bool
	users    []tg.UserClass
	chats    []tg.ChatClass
	messages func() []*tg.Message
	setup    func(*fakeTelegram)
	// deleted 是跑完之后必须消失的消息，kept 是必须还在的。
	deleted, kept []int
}

func withHash[T interface{ SetAccessHash(int64) }](value T) T {
	value.SetAccessHash(1)
	return value
}

var (
	dmePrivate    = &tg.PeerUser{UserID: dmeOther}
	dmeSupergroup = &tg.PeerChannel{ChannelID: dmeGroup}
	otherUser     = []tg.UserClass{withHash(&tg.User{ID: dmeOther})}
	supergroup    = []tg.ChatClass{withHash(&tg.Channel{ID: dmeGroup, Megagroup: true, Title: "群"})}
	broadcast     = []tg.ChatClass{withHash(&tg.Channel{ID: dmeGroup, Broadcast: true, Title: "频道"})}
)

// alternating 造一段自己和对方交替发言的私聊：奇数编号是自己的。
func alternating(peer tg.PeerClass, count int) func() []*tg.Message {
	return func() []*tg.Message {
		var messages []*tg.Message
		for id := 1; id <= count; id++ {
			messages = append(messages, chatMessage(id, peer, nil, id%2 == 1, "消息", time.Minute))
		}
		return append(messages, chatMessage(dmeCommand, peer, nil, true, ".dme", 0))
	}
}

func odd(from, to int) []int {
	var ids []int
	for id := from; id <= to; id++ {
		if id%2 == 1 {
			ids = append(ids, id)
		}
	}
	return ids
}

func dmeScenarios() []dmeScenario {
	photo := func(id int) *tg.Message {
		message := chatMessage(id, dmePrivate, nil, true, "", time.Minute)
		message.SetMedia(&tg.MessageMediaPhoto{})
		return message
	}
	sticker := func(id int) *tg.Message {
		message := chatMessage(id, dmePrivate, nil, true, "", time.Minute)
		document := &tg.Document{Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeSticker{Stickerset: &tg.InputStickerSetEmpty{}}}}
		message.SetMedia(&tg.MessageMediaDocument{Document: document})
		return message
	}
	inGroup := func(id int, from tg.PeerClass, topic int) *tg.Message {
		message := chatMessage(id, dmeSupergroup, from, false, "群消息", time.Minute)
		if topic != 0 {
			message.SetReplyTo(&tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: topic})
		}
		return message
	}
	me := &tg.PeerUser{UserID: dmeSelf}
	someone := &tg.PeerUser{UserID: dmeOther}
	alias := &tg.PeerChannel{ChannelID: dmeAlias}
	stranger := &tg.PeerChannel{ChannelID: 400}

	return []dmeScenario{
		{
			name: "私聊按搜索删最近几条", peer: dmePrivate, count: 5, users: otherUser,
			messages: alternating(dmePrivate, 20),
			deleted:  []int{11, 13, 15, 17, 19, dmeCommand}, kept: append(odd(1, 9), 2, 4, 20),
		},
		{
			name: "防撤回：文字改占位、图片换图、贴纸和过期的不动", peer: dmePrivate, count: 5, anti: true, users: otherUser,
			messages: func() []*tg.Message {
				return []*tg.Message{
					chatMessage(1, dmePrivate, nil, true, "三天前的", 72*time.Hour),
					photo(2), sticker(3),
					chatMessage(4, dmePrivate, nil, false, "对方的", time.Minute),
					chatMessage(5, dmePrivate, nil, true, "要改的", time.Minute),
					chatMessage(6, dmePrivate, nil, true, "占位符", time.Minute),
					chatMessage(dmeCommand, dmePrivate, nil, true, ".dme -f 5", 0),
				}
			},
			deleted: []int{1, 2, 3, 5, 6, dmeCommand}, kept: []int{4},
		},
		{
			name: "收藏夹直接翻历史全删", peer: me, saved: true, count: 4,
			messages: alternating(me, 10),
			deleted:  []int{7, 8, 9, 10, dmeCommand}, kept: []int{1, 2, 3, 4, 5, 6},
		},
		{
			name: "超级群里以频道身份发的也算自己的", peer: dmeSupergroup, count: 10, chats: supergroup,
			setup: func(fake *fakeTelegram) { fake.sendAs = []tg.PeerClass{alias} },
			messages: func() []*tg.Message {
				return []*tg.Message{
					inGroup(1, me, 0), inGroup(2, someone, 0), inGroup(3, alias, 0), inGroup(4, stranger, 0),
					inGroup(5, me, 0), inGroup(6, someone, 0),
					chatMessage(dmeCommand, dmeSupergroup, me, true, ".dme 10", 0),
				}
			},
			deleted: []int{1, 3, 5, dmeCommand}, kept: []int{2, 4, 6},
		},
		{
			name: "论坛只删当前话题", peer: dmeSupergroup, topic: 50, count: 10, chats: supergroup,
			messages: func() []*tg.Message {
				return []*tg.Message{
					inGroup(51, me, 50), inGroup(52, me, 60), inGroup(53, someone, 50), inGroup(54, me, 50),
					chatMessage(dmeCommand, dmeSupergroup, me, true, ".dme 10", 0),
				}
			},
			deleted: []int{51, 54, dmeCommand}, kept: []int{52, 53},
		},
		{
			name: "删不掉的那条会被单独拆出来记为失败", peer: dmePrivate, count: 6, users: otherUser,
			messages: alternating(dmePrivate, 20),
			setup: func(fake *fakeTelegram) {
				fake.failDelete = func(ids []int) bool { return slices.Contains(ids, 15) }
			},
			deleted: []int{9, 11, 13, 17, 19, dmeCommand}, kept: []int{15},
		},
		{
			name: "搜索失败就改成翻历史", peer: dmePrivate, count: 3, users: otherUser,
			messages: alternating(dmePrivate, 10),
			setup:    func(fake *fakeTelegram) { fake.failSearch = true },
			deleted:  []int{5, 7, 9, dmeCommand}, kept: []int{1, 3, 2, 10},
		},
		{
			name: "999999 删掉全部自己的", peer: dmePrivate, count: 999999, users: otherUser,
			messages: alternating(dmePrivate, 12),
			deleted:  append(odd(1, 12), dmeCommand), kept: []int{2, 4, 6, 8, 10, 12},
		},
		{
			name: "频道主用 -f 直接删，不做编辑", peer: dmeSupergroup, count: 3, anti: true, chats: broadcast,
			setup: func(fake *fakeTelegram) { fake.creator = true },
			messages: func() []*tg.Message {
				return []*tg.Message{
					inGroup(1, nil, 0), inGroup(2, nil, 0), inGroup(3, nil, 0), inGroup(4, nil, 0),
					chatMessage(dmeCommand, dmeSupergroup, nil, true, ".dme -f 3", 0),
				}
			},
			deleted: []int{2, 3, 4, dmeCommand}, kept: []int{1},
		},
	}
}

// runDme 在一个新的假 Telegram 上跑一次，返回它收到的全部请求和最后还剩的消息。
func runDme(t *testing.T, scenario dmeScenario, execute func(context.Context, *command.Invocation, int, bool, dmeConfig) error) ([]string, []int) {
	t.Helper()
	fake := newFakeTelegram(t, dmeSelf, scenario.messages()...)
	if scenario.setup != nil {
		scenario.setup(fake)
	}
	client := fakeClient(fake, scenario.users, scenario.chats)
	message := &bot.Message{ID: dmeCommand, Peer: scenario.peer, ChatID: bot.PeerID(scenario.peer),
		Saved: scenario.saved, TopicID: scenario.topic, Out: true}
	// 批量设得很小，好让一个场景里就走到拆批、续翻页这些分支。
	config := dmeConfig{BatchSize: 3, SearchLimit: 4, RetryAttempts: 0}
	if err := execute(context.Background(), fakeInvocation(client, message), scenario.count, scenario.anti, config); err != nil {
		t.Fatalf("%s: %v", scenario.name, err)
	}
	return fake.calls, fake.alive()
}

// TestDmeSnapshot 把每个场景发出的请求和最后剩下的消息与快照比较。
// 快照是拆分 dmeExecute 时新旧两版逐条比对一致之后生成的。
func TestDmeSnapshot(t *testing.T) {
	var out strings.Builder
	for _, scenario := range dmeScenarios() {
		calls, alive := runDme(t, scenario, dmeExecute)
		out.WriteString(section(scenario.name, calls, fmt.Sprint("剩下的消息 ", alive)))
	}
	golden(t, "dme", out.String())
}

// 删掉的必须恰好是该删的：自己的、在范围内的；别人的一条都不能碰。
func TestDmeDeletesExactlyWhatItShould(t *testing.T) {
	for _, scenario := range dmeScenarios() {
		calls, alive := runDme(t, scenario, dmeExecute)
		for _, id := range scenario.deleted {
			if slices.Contains(alive, id) {
				t.Errorf("%s：#%d 应该被删掉\n%s", scenario.name, id, strings.Join(calls, "\n"))
			}
		}
		for _, id := range scenario.kept {
			if !slices.Contains(alive, id) {
				t.Errorf("%s：#%d 不该被删\n%s", scenario.name, id, strings.Join(calls, "\n"))
			}
		}
	}
}
