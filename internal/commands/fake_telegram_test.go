package commands

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
)

// fakeTelegram 是一个只存在于内存里的对话，外加刚好够 .dme、.da 用的那几个接口。
//
// 它按真实接口的规则回答：历史按编号倒序分页、搜索只返回自己发的、删掉的消息
// 从此消失。每个请求都记一行，测试比对的就是这份记录——删了哪些编号、按什么
// 批次、改了哪些消息，一个请求都不能多也不能少。
type fakeTelegram struct {
	t    *testing.T
	self int64

	mu       sync.Mutex
	messages map[int]*tg.Message
	calls    []string

	// 下面这些用来模拟出错和权限。
	failDelete func(ids []int) bool
	failEdit   func(id int) bool
	failSearch bool
	creator    bool
	sendAs     []tg.PeerClass
	admin      bool
	// restrictForward 模拟开了「禁止转发」的对话。
	restrictForward bool
	// fullSelf 为真时，发到收藏夹的编辑也完整记录；默认只记第一行，因为 .da 的进度里有时间戳。
	fullSelf bool
}

func newFakeTelegram(t *testing.T, self int64, messages ...*tg.Message) *fakeTelegram {
	fake := &fakeTelegram{t: t, self: self, messages: map[int]*tg.Message{}}
	for _, message := range messages {
		fake.messages[message.ID] = message
	}
	return fake
}

func (f *fakeTelegram) log(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeTelegram) alive() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]int, 0, len(f.messages))
	for id := range f.messages {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func (f *fakeTelegram) mine(message *tg.Message) bool {
	if message.Out {
		return true
	}
	from, ok := message.GetFromID()
	if !ok {
		return false
	}
	user, isUser := from.(*tg.PeerUser)
	return isUser && user.UserID == f.self
}

// page 按真实接口的规则取一页：编号倒序，比 offset 和 max 都小，最多 limit 条。
func (f *fakeTelegram) page(offset, maxID, limit int, keep func(*tg.Message) bool) []tg.MessageClass {
	ids := make([]int, 0, len(f.messages))
	for id, message := range f.messages {
		if (offset == 0 || id < offset) && (maxID == 0 || id < maxID) && (keep == nil || keep(message)) {
			ids = append(ids, id)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ids)))
	if len(ids) > limit {
		ids = ids[:limit]
	}
	page := make([]tg.MessageClass, 0, len(ids))
	for _, id := range ids {
		page = append(page, f.messages[id])
	}
	return page
}

func respond(output bin.Decoder, value bin.Encoder) error {
	var buffer bin.Buffer
	if err := value.Encode(&buffer); err != nil {
		return err
	}
	return output.Decode(&buffer)
}

func (f *fakeTelegram) forget(ids []int) {
	for _, id := range ids {
		delete(f.messages, id)
	}
}

func (f *fakeTelegram) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch request := input.(type) {
	case *tg.MessagesGetHistoryRequest:
		f.log("history offset=%d limit=%d", request.OffsetID, request.Limit)
		return respond(output, &tg.MessagesMessages{Messages: f.page(request.OffsetID, request.MaxID, request.Limit, nil)})
	case *tg.MessagesSearchRequest:
		top, _ := request.GetTopMsgID()
		f.log("search offset=%d limit=%d top=%d", request.OffsetID, request.Limit, top)
		if f.failSearch {
			return tgerr.New(400, "SEARCH_QUERY_EMPTY")
		}
		return respond(output, &tg.MessagesMessages{Messages: f.page(request.OffsetID, request.MaxID, request.Limit, f.mine)})
	case *tg.MessagesDeleteMessagesRequest:
		f.log("delete %v", request.ID)
		if f.failDelete != nil && f.failDelete(request.ID) {
			return tgerr.New(403, "MESSAGE_DELETE_FORBIDDEN")
		}
		f.forget(request.ID)
		return respond(output, &tg.MessagesAffectedMessages{Pts: 1, PtsCount: len(request.ID)})
	case *tg.ChannelsDeleteMessagesRequest:
		f.log("delete %v", request.ID)
		if f.failDelete != nil && f.failDelete(request.ID) {
			return tgerr.New(403, "MESSAGE_DELETE_FORBIDDEN")
		}
		f.forget(request.ID)
		return respond(output, &tg.MessagesAffectedMessages{Pts: 1, PtsCount: len(request.ID)})
	case *tg.MessagesEditMessageRequest:
		text, hasText := request.GetMessage()
		_, hasMedia := request.GetMedia()
		if _, self := request.Peer.(*tg.InputPeerSelf); self && !f.fullSelf {
			// 发到收藏夹的是进度报告，里面有耗时和时间戳，只记第一行。
			f.log("progress %q", firstLine(text))
			return respond(output, &tg.Updates{})
		}
		f.log("edit %d text=%v:%q media=%v", request.ID, hasText, text, hasMedia)
		if f.failEdit != nil && f.failEdit(request.ID) {
			return tgerr.New(400, "MESSAGE_EDIT_TIME_EXPIRED")
		}
		return respond(output, &tg.Updates{})
	case *tg.UploadSaveFilePartRequest:
		return respond(output, &tg.BoolTrue{})
	case *tg.UpdatesGetStateRequest:
		return respond(output, &tg.UpdatesState{})
	case *tg.ChannelsGetParticipantRequest:
		f.log("participant")
		var participant tg.ChannelParticipantClass = &tg.ChannelParticipant{UserID: f.self, Date: 1}
		switch {
		case f.creator:
			participant = &tg.ChannelParticipantCreator{UserID: f.self}
		case f.admin:
			participant = &tg.ChannelParticipantAdmin{UserID: f.self, Date: 1, AdminRights: tg.ChatAdminRights{DeleteMessages: true}}
		}
		return respond(output, &tg.ChannelsChannelParticipant{Participant: participant})
	case *tg.ChannelsGetSendAsRequest:
		f.log("send-as")
		peers := make([]tg.SendAsPeer, 0, len(f.sendAs))
		for _, peer := range f.sendAs {
			peers = append(peers, tg.SendAsPeer{Peer: peer})
		}
		return respond(output, &tg.ChannelsSendAsPeers{Peers: peers})
	case *tg.ChannelsGetMessagesRequest:
		f.log("get %s", describeIDs(request.ID))
		return respond(output, &tg.MessagesMessages{Messages: f.byID(request.ID)})
	case *tg.MessagesGetMessagesRequest:
		f.log("get %s", describeIDs(request.ID))
		return respond(output, &tg.MessagesMessages{Messages: f.byID(request.ID)})
	case *tg.MessagesForwardMessagesRequest:
		f.log("forward %v to %s", request.ID, describePeer(request.ToPeer))
		if f.restrictForward {
			return tgerr.New(400, "CHAT_FORWARDS_RESTRICTED")
		}
		return respond(output, &tg.Updates{})
	case *tg.UploadGetFileRequest:
		// 头像下载：记下是谁的头像，返回一张小 PNG。
		if location, ok := request.Location.(*tg.InputPeerPhotoFileLocation); ok && request.Offset == 0 {
			f.log("avatar %s big=%v", describePeer(location.Peer), location.Big)
		}
		if request.Offset > 0 {
			return respond(output, &tg.UploadFile{Type: &tg.StorageFilePng{}, Bytes: []byte{}})
		}
		return respond(output, &tg.UploadFile{Type: &tg.StorageFilePng{}, Bytes: tinyPNG()})
	case *tg.AccountUpdateProfileRequest:
		first, _ := request.GetFirstName()
		last, _ := request.GetLastName()
		f.log("profile first=%q last=%q", first, last)
		return respond(output, &tg.User{ID: f.self, FirstName: first, LastName: last})
	case *tg.MessagesSendMessageRequest:
		if _, self := request.Peer.(*tg.InputPeerSelf); !self {
			reply := ""
			if header, ok := request.GetReplyTo(); ok {
				if to, ok := header.(*tg.InputReplyToMessage); ok {
					reply = fmt.Sprintf(" reply=%d", to.ReplyToMsgID)
					if top, ok := to.GetTopMsgID(); ok {
						reply += fmt.Sprintf(" topic=%d", top)
					}
				}
			}
			f.log("send %q to %s%s", request.Message, describePeer(request.Peer), reply)
			// 账号发出的消息存进对话里，好让随后按编号读回来。
			id := f.nextID()
			sent := &tg.Message{ID: id, PeerID: peerOf(request.Peer, f.self), Out: true, Message: request.Message, Date: int(time.Now().Unix())}
			sent.SetFromID(&tg.PeerUser{UserID: f.self})
			f.messages[id] = sent
			return respond(output, &tg.UpdateShortSentMessage{ID: id, Date: sent.Date})
		}
		f.log("send-self %q", firstLine(request.Message))
		return respond(output, &tg.UpdateShortSentMessage{ID: 1, Date: int(time.Now().Unix())})
	}
	f.t.Fatalf("the fake has no answer for %T", input)
	return nil
}

// fakeClient 把假的 Telegram 接进一个真正的 bot.Client。
func fakeClient(fake *fakeTelegram, users []tg.UserClass, chats []tg.ChatClass) *bot.Client {
	peers := bot.NewPeerCache()
	peers.SetSelf(fake.self)
	peers.RememberUsers(users)
	peers.RememberChats(chats)
	self := &tg.User{ID: fake.self, Self: true, FirstName: "Cat", LastName: "S"}
	return bot.FromAPI(tg.NewClient(fake), peers, self, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func fakeInvocation(client *bot.Client, message *bot.Message, args ...string) *command.Invocation {
	return &command.Invocation{Prefix: ".", Args: args, Message: message, Client: client,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// chatMessage 造一条历史消息。from 为 0 表示没有发送者字段。
func chatMessage(id int, peer tg.PeerClass, from tg.PeerClass, out bool, text string, age time.Duration) *tg.Message {
	message := &tg.Message{ID: id, PeerID: peer, Out: out, Message: text, Date: int(time.Now().Add(-age).Unix())}
	if from != nil {
		message.SetFromID(from)
	}
	return message
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// byID 按编号取消息，不存在的跳过，和真实接口一样。
func (f *fakeTelegram) byID(ids []tg.InputMessageClass) []tg.MessageClass {
	var found []tg.MessageClass
	for _, input := range ids {
		if id, ok := input.(*tg.InputMessageID); ok {
			if message, exists := f.messages[id.ID]; exists {
				found = append(found, message)
			}
		}
	}
	return found
}

func describeIDs(ids []tg.InputMessageClass) string {
	var parts []string
	for _, input := range ids {
		if id, ok := input.(*tg.InputMessageID); ok {
			parts = append(parts, fmt.Sprint(id.ID))
		}
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func describePeer(peer tg.InputPeerClass) string {
	switch value := peer.(type) {
	case *tg.InputPeerSelf:
		return "self"
	case *tg.InputPeerUser:
		return fmt.Sprintf("user%d", value.UserID)
	case *tg.InputPeerChannel:
		return fmt.Sprintf("channel%d", value.ChannelID)
	case *tg.InputPeerChat:
		return fmt.Sprintf("chat%d", value.ChatID)
	}
	return fmt.Sprintf("%T", peer)
}

// tinyPNG 是一张 8×8 的纯色 PNG，够头像解码用。
func tinyPNG() []byte {
	canvas := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			canvas.Set(x, y, color.RGBA{R: 200, G: 80, B: 40, A: 255})
		}
	}
	var buffer bytes.Buffer
	_ = png.Encode(&buffer, canvas)
	return buffer.Bytes()
}

// nextID 给账号新发的消息编号，从 2000 起，和场景里的消息不重叠。
func (f *fakeTelegram) nextID() int {
	id := 2000
	for {
		if _, taken := f.messages[id]; !taken {
			return id
		}
		id++
	}
}

func peerOf(input tg.InputPeerClass, self int64) tg.PeerClass {
	switch value := input.(type) {
	case *tg.InputPeerChannel:
		return &tg.PeerChannel{ChannelID: value.ChannelID}
	case *tg.InputPeerUser:
		return &tg.PeerUser{UserID: value.UserID}
	case *tg.InputPeerChat:
		return &tg.PeerChat{ChatID: value.ChatID}
	}
	return &tg.PeerUser{UserID: self}
}
