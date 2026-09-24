package aban

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// 场景里的编号：本账号、命令所在的超级群、另外几个管理群、要找的用户和一个频道身份。
const (
	selfID     = int64(1)
	hereID     = int64(10)
	otherID    = int64(20)
	basicID    = int64(30)
	lastID     = int64(40)
	targetID   = int64(42)
	targetHash = int64(4242)
	aliasID    = int64(77)
)

// scripted 是按脚本回答的假 Telegram：answer 给出每个请求的应答或错误，没写到的请求
// 由 fallback 回答。每个请求记一行，测试比对查找链实际发了哪些请求。
type scripted struct {
	mu     sync.Mutex
	calls  []string
	answer func(input bin.Encoder) (bin.Encoder, error)
}

func (s *scripted) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	s.mu.Lock()
	s.calls = append(s.calls, describe(input))
	s.mu.Unlock()
	var value bin.Encoder
	var err error
	if s.answer != nil {
		value, err = s.answer(input)
	}
	if value == nil && err == nil {
		value, err = fallback(input)
	}
	if err != nil {
		return err
	}
	var buffer bin.Buffer
	if err := value.Encode(&buffer); err != nil {
		return err
	}
	return output.Decode(&buffer)
}

func (s *scripted) log() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// count 数出以 prefix 开头的请求有几个。
func (s *scripted) count(prefix string) int {
	n := 0
	for _, call := range s.log() {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

// fallback 是没写进脚本时的应答：查人一律查不到，改动一律成功。
func fallback(input bin.Encoder) (bin.Encoder, error) {
	switch request := input.(type) {
	case *tg.UsersGetUsersRequest:
		return &tg.UserClassVector{}, nil
	case *tg.ChannelsGetParticipantRequest:
		return nil, tgerr.New(400, "USER_NOT_PARTICIPANT")
	case *tg.ChannelsGetParticipantsRequest:
		return &tg.ChannelsChannelParticipants{}, nil
	case *tg.MessagesGetFullChatRequest:
		return &tg.MessagesChatFull{FullChat: &tg.ChatFull{ID: request.ChatID, Participants: &tg.ChatParticipants{ChatID: request.ChatID}}}, nil
	case *tg.ChannelsGetChannelsRequest:
		return nil, tgerr.New(400, "CHANNEL_INVALID")
	case *tg.ChannelsEditBannedRequest, *tg.MessagesEditMessageRequest, *tg.MessagesDeleteChatUserRequest:
		return &tg.Updates{}, nil
	case *tg.ChannelsDeleteParticipantHistoryRequest:
		return &tg.MessagesAffectedHistory{Pts: 1, PtsCount: 1}, nil
	case *tg.ChannelsDeleteMessagesRequest, *tg.MessagesDeleteMessagesRequest:
		return &tg.MessagesAffectedMessages{Pts: 1, PtsCount: 1}, nil
	case *tg.ChannelsGetMessagesRequest:
		return &tg.MessagesMessages{}, nil
	}
	return nil, fmt.Errorf("假 Telegram 不认识 %T", input)
}

func describe(input bin.Encoder) string {
	switch request := input.(type) {
	case *tg.UsersGetUsersRequest:
		var ids []string
		for _, id := range request.ID {
			if user, ok := id.(*tg.InputUser); ok {
				ids = append(ids, fmt.Sprintf("%d#%d", user.UserID, user.AccessHash))
			}
		}
		return "users.getUsers " + strings.Join(ids, " ")
	case *tg.ChannelsGetParticipantRequest:
		return fmt.Sprintf("channels.getParticipant %d %s", channelOf(request.Channel), peerName(request.Participant))
	case *tg.ChannelsGetParticipantsRequest:
		return fmt.Sprintf("channels.getParticipants %d %s offset=%d limit=%d", channelOf(request.Channel), request.Filter.TypeName(), request.Offset, request.Limit)
	case *tg.MessagesGetFullChatRequest:
		return fmt.Sprintf("messages.getFullChat %d", request.ChatID)
	case *tg.ChannelsGetChannelsRequest:
		return "channels.getChannels"
	case *tg.ChannelsEditBannedRequest:
		return fmt.Sprintf("channels.editBanned %d %s view=%v send=%v timed=%v", channelOf(request.Channel), peerName(request.Participant),
			request.BannedRights.ViewMessages, request.BannedRights.SendMessages, request.BannedRights.UntilDate > 0)
	case *tg.MessagesDeleteChatUserRequest:
		return fmt.Sprintf("messages.deleteChatUser %d", request.ChatID)
	case *tg.ChannelsDeleteParticipantHistoryRequest:
		return fmt.Sprintf("channels.deleteParticipantHistory %d %s", channelOf(request.Channel), peerName(request.Participant))
	case *tg.MessagesEditMessageRequest:
		return fmt.Sprintf("edit %d %s", request.ID, request.Message)
	case *tg.ChannelsDeleteMessagesRequest:
		return fmt.Sprintf("channels.deleteMessages %d %v", channelOf(request.Channel), request.ID)
	case *tg.ChannelsGetMessagesRequest:
		return fmt.Sprintf("channels.getMessages %d", channelOf(request.Channel))
	}
	return fmt.Sprintf("%T", input)
}

func channelOf(input tg.InputChannelClass) int64 {
	if channel, ok := input.(*tg.InputChannel); ok {
		return channel.ChannelID
	}
	return 0
}

func peerName(peer tg.InputPeerClass) string {
	switch value := peer.(type) {
	case *tg.InputPeerUser:
		return fmt.Sprintf("user%d#%d", value.UserID, value.AccessHash)
	case *tg.InputPeerChannel:
		return fmt.Sprintf("channel%d", value.ChannelID)
	case *tg.InputPeerSelf:
		return "self"
	}
	return fmt.Sprintf("%T", peer)
}

// fixture 是一个接上假 Telegram 的服务：命令在超级群 hereID 里执行，
// 管理群缓存里有 hereID、otherID、basicID、lastID 四个群。
type fixture struct {
	fake    *scripted
	client  *bot.Client
	service *abanService
	// deletions 记下安排好的自动删除，run 在测试里手动触发。
	deletions []time.Duration
	run       []func()
}

func newFixture(t *testing.T, answer func(input bin.Encoder) (bin.Encoder, error)) *fixture {
	t.Helper()
	fake := &scripted{answer: answer}
	peers := bot.NewPeerCache()
	peers.SetSelf(selfID)
	peers.RememberChats([]tg.ChatClass{
		&tg.Channel{ID: hereID, AccessHash: 111, Megagroup: true, Title: "群"},
		&tg.Channel{ID: otherID, AccessHash: 222, Megagroup: true, Title: "二群"},
		&tg.Channel{ID: lastID, AccessHash: 444, Megagroup: true, Title: "四群"},
		&tg.Chat{ID: basicID, Title: "基本群"},
	})
	self := &tg.User{ID: selfID, Self: true, FirstName: "Cat"}
	client := bot.FromAPI(tg.NewClient(fake), peers, self, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cache := store.New(filepath.Join(t.TempDir(), "aban.json"), func() abanCache { return abanCache{} })
	err := cache.Update(func(value *abanCache) error {
		value.Groups = []managedGroup{
			{ID: hereID, Title: "群", Channel: true, Hash: 111},
			{ID: otherID, Title: "二群", Channel: true, Hash: 222},
			{ID: basicID, Title: "基本群"},
			{ID: lastID, Title: "四群", Channel: true, Hash: 444},
		}
		value.UpdatedAt = time.Now().UnixMilli()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{fake: fake, client: client}
	f.service = &abanService{store: cache, later: func(delay time.Duration, run func()) {
		f.deletions = append(f.deletions, delay)
		f.run = append(f.run, run)
	}}
	return f
}

// invocation 造一条在 peer 里发出的命令，replyTo 不为 0 时回复那条消息。
func (f *fixture) invocation(peer tg.PeerClass, replyTo int, args ...string) *command.Invocation {
	message := &bot.Message{ID: 1000, Peer: peer, ChatID: bot.PeerID(peer), ChatType: bot.ChatSupergroup, Out: true, ReplyToID: replyTo}
	return &command.Invocation{Prefix: ".", Args: args, Message: message, Client: f.client,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

var (
	here      = &tg.InputPeerChannel{ChannelID: hereID, AccessHash: 111}
	herePeer  = &tg.PeerChannel{ChannelID: hereID}
	withHash  = &tg.User{ID: targetID, AccessHash: targetHash, FirstName: "Spam"}
	wantFound = &tg.InputPeerUser{UserID: targetID, AccessHash: targetHash}
)

func samePeer(t *testing.T, got tg.InputPeerClass, want tg.InputPeerClass) {
	t.Helper()
	if peerName(got) != peerName(want) {
		t.Fatalf("解析成 %s，应为 %s", peerName(got), peerName(want))
	}
}

func TestFindUserFromCacheSendsNothing(t *testing.T) {
	f := newFixture(t, nil)
	f.client.Peers().RememberUsers([]tg.UserClass{withHash})
	peer, err := f.service.findUser(context.Background(), f.client, here, targetID)
	if err != nil {
		t.Fatal(err)
	}
	samePeer(t, peer, wantFound)
	if calls := f.fake.log(); len(calls) != 0 {
		t.Fatalf("缓存里有就不该发请求：%v", calls)
	}
}

func TestFindUserThroughCurrentChannelParticipant(t *testing.T) {
	f := newFixture(t, func(input bin.Encoder) (bin.Encoder, error) {
		if request, ok := input.(*tg.ChannelsGetParticipantRequest); ok && channelOf(request.Channel) == hereID {
			return &tg.ChannelsChannelParticipant{Participant: &tg.ChannelParticipant{UserID: targetID}, Users: []tg.UserClass{withHash}}, nil
		}
		return nil, nil
	})
	peer, err := f.service.findUser(context.Background(), f.client, here, targetID)
	if err != nil {
		t.Fatal(err)
	}
	samePeer(t, peer, wantFound)
	want := []string{"users.getUsers 42#0", "channels.getParticipant 10 user42#0"}
	if got := f.fake.log(); !slices.Equal(got, want) {
		t.Fatalf("请求 %v，应为 %v", got, want)
	}
	// 查到之后记进了缓存，下次不用再查。
	if _, ok := f.client.Peers().InputPeer(&tg.PeerUser{UserID: targetID}); !ok {
		t.Fatal("查到的用户没有记进缓存")
	}
}

func TestFindUserByScanningRecentMembers(t *testing.T) {
	crowd := func(from int) *tg.ChannelsChannelParticipants {
		page := &tg.ChannelsChannelParticipants{}
		for id := from; id < from+recentPageSize; id++ {
			page.Participants = append(page.Participants, &tg.ChannelParticipant{UserID: int64(id)})
			page.Users = append(page.Users, &tg.User{ID: int64(id), AccessHash: 1})
		}
		return page
	}
	f := newFixture(t, func(input bin.Encoder) (bin.Encoder, error) {
		if request, ok := input.(*tg.ChannelsGetParticipantsRequest); ok {
			if request.Offset == 0 {
				return crowd(1000), nil
			}
			page := crowd(2000)
			page.Users = append(page.Users, withHash)
			return page, nil
		}
		return nil, nil
	})
	peer, err := f.service.findUser(context.Background(), f.client, here, targetID)
	if err != nil {
		t.Fatal(err)
	}
	samePeer(t, peer, wantFound)
	want := []string{
		"users.getUsers 42#0",
		"channels.getParticipant 10 user42#0",
		"channels.getParticipants 10 channelParticipantsRecent offset=0 limit=200",
		"channels.getParticipants 10 channelParticipantsRecent offset=200 limit=200",
	}
	if got := f.fake.log(); !slices.Equal(got, want) {
		t.Fatalf("请求 %v，应为 %v", got, want)
	}
}

func TestFindUserAcrossManagedGroups(t *testing.T) {
	f := newFixture(t, func(input bin.Encoder) (bin.Encoder, error) {
		if request, ok := input.(*tg.MessagesGetFullChatRequest); ok && request.ChatID == basicID {
			return &tg.MessagesChatFull{FullChat: &tg.ChatFull{ID: basicID, Participants: &tg.ChatParticipants{ChatID: basicID}},
				Users: []tg.UserClass{withHash}}, nil
		}
		return nil, nil
	})
	peer, err := f.service.findUser(context.Background(), f.client, here, targetID)
	if err != nil {
		t.Fatal(err)
	}
	samePeer(t, peer, wantFound)
	if n := f.fake.count("channels.getParticipant 10 "); n != 1 {
		t.Fatalf("命令所在的群在跨群阶段又查了一遍：共 %d 次", n)
	}
	if f.fake.count("messages.getFullChat 30") != 1 {
		t.Fatalf("没有到基本群里找：%v", f.fake.log())
	}
}

func TestFindUserFallsBackToZeroHashInChannels(t *testing.T) {
	f := newFixture(t, nil)
	peer, err := f.service.findUser(context.Background(), f.client, here, targetID)
	if err != nil {
		t.Fatal(err)
	}
	samePeer(t, peer, &tg.InputPeerUser{UserID: targetID})
	for _, prefix := range []string{"channels.getParticipant 20 ", "channels.getParticipant 40 ", "messages.getFullChat 30"} {
		if f.fake.count(prefix) != 1 {
			t.Fatalf("跨群查找少了 %q：%v", prefix, f.fake.log())
		}
	}
}

func TestFindUserOutsideChannelsCanFail(t *testing.T) {
	f := newFixture(t, nil)
	_, err := f.service.findUser(context.Background(), f.client, &tg.InputPeerUser{UserID: 5, AccessHash: 5}, targetID)
	if !errors.Is(err, errUnresolvedUser) {
		t.Fatalf("私聊里查不到应报 errUnresolvedUser，得到 %v", err)
	}
	if f.fake.count("channels.getParticipants") != 0 {
		t.Fatalf("私聊里不该翻频道成员：%v", f.fake.log())
	}
}

// TestReplySenderIsLookedUpOnlyHere 检查回复的发送者没有 access hash（min 用户）时只在当前群里找，不跨群。
func TestReplySenderIsLookedUpOnlyHere(t *testing.T) {
	f := newFixture(t, func(input bin.Encoder) (bin.Encoder, error) {
		if _, ok := input.(*tg.ChannelsGetMessagesRequest); ok {
			message := &tg.Message{ID: 5, PeerID: herePeer, Message: "spam"}
			message.SetFromID(&tg.PeerUser{UserID: targetID})
			return &tg.MessagesMessages{Messages: []tg.MessageClass{message},
				Users: []tg.UserClass{&tg.User{ID: targetID, Min: true, AccessHash: 9, FirstName: "Spam"}}}, nil
		}
		return nil, nil
	})
	_, err := f.service.resolveTarget(context.Background(), f.invocation(herePeer, 5), here)
	if !errors.Is(err, errUnresolvedUser) {
		t.Fatalf("应报 errUnresolvedUser，得到 %v", err)
	}
	if f.fake.count("channels.getParticipant 20") != 0 || f.fake.count("messages.getFullChat") != 0 {
		t.Fatalf("回复的发送者不该跨群查：%v", f.fake.log())
	}
	if f.fake.count("channels.getParticipants 10") == 0 {
		t.Fatalf("应在当前群的成员里找过：%v", f.fake.log())
	}
}

func TestChannelTargets(t *testing.T) {
	f := newFixture(t, func(input bin.Encoder) (bin.Encoder, error) {
		if _, ok := input.(*tg.ChannelsGetMessagesRequest); ok {
			message := &tg.Message{ID: 5, PeerID: herePeer, Message: "spam"}
			message.SetFromID(&tg.PeerChannel{ChannelID: aliasID})
			return &tg.MessagesMessages{Messages: []tg.MessageClass{message},
				Chats: []tg.ChatClass{&tg.Channel{ID: aliasID, AccessHash: 7777, Broadcast: true, Title: "Spam", Username: "spamchan", Photo: &tg.ChatPhotoEmpty{}}}}, nil
		}
		return nil, nil
	})
	ctx := context.Background()
	fromReply, err := f.service.resolveTarget(ctx, f.invocation(herePeer, 5), here)
	if err != nil {
		t.Fatal(err)
	}
	if !fromReply.channel || fromReply.display != "频道: Spam (@spamchan)" {
		t.Fatalf("回复频道身份：%+v", fromReply)
	}
	samePeer(t, fromReply.peer, &tg.InputPeerChannel{ChannelID: aliasID})
	fromID, err := f.service.resolveTarget(ctx, f.invocation(herePeer, 0, "-10077"), here)
	if err != nil {
		t.Fatal(err)
	}
	samePeer(t, fromID.peer, &tg.InputPeerChannel{ChannelID: aliasID})
	if _, err := f.service.resolveTarget(ctx, f.invocation(herePeer, 0, "-100999"), here); err == nil {
		t.Fatal("不认识的频道 ID 应报错")
	}
}

func TestRefusesSelf(t *testing.T) {
	f := newFixture(t, nil)
	if _, err := f.service.resolveTarget(context.Background(), f.invocation(herePeer, 0, "1"), here); err == nil || errors.Is(err, errUnresolvedUser) {
		t.Fatalf("对自己执行应报错，得到 %v", err)
	}
}

func TestTargetIsAdminInBasicGroups(t *testing.T) {
	chat := &tg.InputPeerChat{ChatID: basicID}
	user := &tg.InputPeerUser{UserID: targetID, AccessHash: targetHash}
	for name, tc := range map[string]struct {
		member tg.ChatParticipantClass
		err    error
		want   bool
	}{
		"创建者":  {member: &tg.ChatParticipantCreator{UserID: targetID}, want: true},
		"管理员":  {member: &tg.ChatParticipantAdmin{UserID: targetID, InviterID: selfID}, want: true},
		"普通成员": {member: &tg.ChatParticipant{UserID: targetID, InviterID: selfID}, want: false},
		"被限流":  {err: tgerr.New(420, "FLOOD_WAIT_300"), want: true},
		"查询失败": {err: tgerr.New(400, "CHAT_ID_INVALID"), want: false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, func(input bin.Encoder) (bin.Encoder, error) {
				if _, ok := input.(*tg.MessagesGetFullChatRequest); ok {
					if tc.err != nil {
						return nil, tc.err
					}
					return &tg.MessagesChatFull{FullChat: &tg.ChatFull{ID: basicID, Participants: &tg.ChatParticipants{ChatID: basicID,
						Participants: []tg.ChatParticipantClass{&tg.ChatParticipantCreator{UserID: selfID}, tc.member}}}}, nil
				}
				return nil, nil
			})
			if got := targetIsAdmin(context.Background(), f.client, chat, user); got != tc.want {
				t.Fatalf("targetIsAdmin = %v，应为 %v", got, tc.want)
			}
		})
	}
	f := newFixture(t, nil)
	if targetIsAdmin(context.Background(), f.client, here, &tg.InputPeerChannel{ChannelID: aliasID}) {
		t.Fatal("频道身份不会是管理员")
	}
	if calls := f.fake.log(); len(calls) != 0 {
		t.Fatalf("频道身份不用查：%v", calls)
	}
}

func TestDeleteHistoryRepeatsUntilOffsetIsZero(t *testing.T) {
	offsets := []int{300, 200, 0}
	var mu sync.Mutex
	f := newFixture(t, func(input bin.Encoder) (bin.Encoder, error) {
		if _, ok := input.(*tg.ChannelsDeleteParticipantHistoryRequest); ok {
			mu.Lock()
			defer mu.Unlock()
			next := offsets[0]
			offsets = offsets[1:]
			return &tg.MessagesAffectedHistory{Pts: 1, PtsCount: 100, Offset: next}, nil
		}
		return nil, nil
	})
	if !deleteHistory(context.Background(), f.client, here, wantFound) {
		t.Fatal("offset 归零后应报告删完")
	}
	if n := f.fake.count("channels.deleteParticipantHistory"); n != 3 {
		t.Fatalf("应调用 3 次，实际 %d 次", n)
	}

	endless := newFixture(t, func(input bin.Encoder) (bin.Encoder, error) {
		if _, ok := input.(*tg.ChannelsDeleteParticipantHistoryRequest); ok {
			return &tg.MessagesAffectedHistory{Pts: 1, PtsCount: 1, Offset: 5}, nil
		}
		return nil, nil
	})
	if deleteHistory(context.Background(), endless.client, here, wantFound) {
		t.Fatal("到上限还没删完不应报告删完")
	}
	if n := endless.fake.count("channels.deleteParticipantHistory"); n != historyRounds {
		t.Fatalf("应在 %d 次后停下，实际 %d 次", historyRounds, n)
	}
}

func TestTopReasonsSortsByCount(t *testing.T) {
	reasons := map[string]int{"USER_ADMIN_INVALID": 1, "CHAT_ADMIN_REQUIRED": 5, "CHANNEL_PRIVATE": 2, "A_B": 2, "<x>": 1}
	want := []string{"CHAT_ADMIN_REQUIRED×5", "A_B×2", "CHANNEL_PRIVATE×2"}
	if got := topReasons(reasons, 3); !slices.Equal(got, want) {
		t.Fatalf("topReasons = %v，应为 %v", got, want)
	}
	if got := topReasons(map[string]int{"<x>": 1}, 3); !slices.Equal(got, []string{"&lt;x&gt;×1"}) {
		t.Fatalf("原因要转义：%v", got)
	}
}

// TestMuteInBroadcastChannel 检查广播频道里也能执行单群命令；「5min」按原版解析为 5 分钟；
// 结果 10 秒后连命令消息一起删掉。
func TestMuteInBroadcastChannel(t *testing.T) {
	f := newFixture(t, nil)
	f.client.Peers().RememberUsers([]tg.UserClass{withHash})
	inv := f.invocation(herePeer, 0, "42", "5min")
	inv.Message.ChatType = bot.ChatBroadcast
	if err := f.service.basic(context.Background(), inv, "mute", "禁言"); err != nil {
		t.Fatal(err)
	}
	calls := f.fake.log()
	if last := calls[len(calls)-1]; last != "edit 1000 ✅ 已禁言 Spam 5m" {
		t.Fatalf("结果：%q（全部请求 %v）", last, calls)
	}
	if f.fake.count("channels.editBanned 10 user42#4242 view=false send=true timed=true") != 1 {
		t.Fatalf("禁言请求不对：%v", calls)
	}
	if !slices.Equal(f.deletions, []time.Duration{resultLifetime}) {
		t.Fatalf("应安排一次 10 秒后的删除，实际 %v", f.deletions)
	}
	f.run[0]()
	if f.fake.count("channels.deleteMessages 10 [1000]") != 1 {
		t.Fatalf("到点应删掉命令消息：%v", f.fake.log())
	}
}

// TestBatchBanDoesNotRetry 检查 .sb 对每个群只请求一次，失败原因按次数排序，结果 30 秒后删除。
func TestBatchBanDoesNotRetry(t *testing.T) {
	f := newFixture(t, func(input bin.Encoder) (bin.Encoder, error) {
		if request, ok := input.(*tg.ChannelsEditBannedRequest); ok && channelOf(request.Channel) != hereID {
			return nil, tgerr.New(400, "CHAT_ADMIN_REQUIRED")
		}
		return nil, nil
	})
	f.client.Peers().RememberUsers([]tg.UserClass{withHash})
	if err := f.service.batch(context.Background(), f.invocation(herePeer, 0, "42", "true"), true); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{hereID, otherID, lastID} {
		if n := f.fake.count(fmt.Sprintf("channels.editBanned %d ", id)); n != 1 {
			t.Fatalf("群 %d 请求了 %d 次：%v", id, n, f.fake.log())
		}
	}
	calls := f.fake.log()
	last := calls[len(calls)-1]
	if !strings.Contains(last, "✅ 在2个频道/群组中封禁该用户 Spam") || !strings.Contains(last, "失败 2 个（CHAT_ADMIN_REQUIRED×2）") {
		t.Fatalf("结果：%q", last)
	}
	if !slices.Equal(f.deletions, []time.Duration{batchLifetime}) {
		t.Fatalf("应安排一次 30 秒后的删除，实际 %v", f.deletions)
	}
}

// TestUnresolvedIDMessage 检查纯数字 ID 查不到时，单群命令给出原版的提示，并且提示也会到点删除。
func TestUnresolvedIDMessage(t *testing.T) {
	f := newFixture(t, nil)
	inv := f.invocation(&tg.PeerUser{UserID: 5}, 0, "42")
	f.client.Peers().RememberUsers([]tg.UserClass{&tg.User{ID: 5, AccessHash: 5}})
	if err := f.service.basic(context.Background(), inv, "ban", "封禁"); err != nil {
		t.Fatal(err)
	}
	calls := f.fake.log()
	if last := calls[len(calls)-1]; !strings.Contains(last, "无法解析该用户ID（会话未见过且不在管理群中）") {
		t.Fatalf("提示：%q", last)
	}
	if len(f.deletions) != 1 {
		t.Fatalf("提示也应到点删除：%v", f.deletions)
	}
}
