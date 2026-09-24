package re

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sort"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
)

const group = int64(5000)

// history 是一个只懂 .re 用到的几个接口的假 Telegram，按真实规则翻历史：
// 编号倒序排好，从第一条小于 offset_id 的位置起，再挪 add_offset 条，取 limit 条。
type history struct {
	messages   map[int]tg.MessageClass
	restricted bool
	calls      []string
}

func respond(output bin.Decoder, value bin.Encoder) error {
	var buffer bin.Buffer
	if err := value.Encode(&buffer); err != nil {
		return err
	}
	return output.Decode(&buffer)
}

func topicOf(header tg.InputReplyToClass) int {
	if reply, ok := header.(*tg.InputReplyToMessage); ok {
		top, _ := reply.GetTopMsgID()
		return top
	}
	return 0
}

func (h *history) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	switch request := input.(type) {
	case *tg.ChannelsGetMessagesRequest:
		var found []tg.MessageClass
		for _, id := range request.ID {
			if message, ok := h.messages[id.(*tg.InputMessageID).ID]; ok {
				found = append(found, message)
			}
		}
		return respond(output, &tg.MessagesMessages{Messages: found})
	case *tg.MessagesGetHistoryRequest:
		h.calls = append(h.calls, fmt.Sprintf("history offset=%d add=%d limit=%d", request.OffsetID, request.AddOffset, request.Limit))
		ids := make([]int, 0, len(h.messages))
		for id := range h.messages {
			ids = append(ids, id)
		}
		sort.Sort(sort.Reverse(sort.IntSlice(ids)))
		start := len(ids)
		for index, id := range ids {
			if id < request.OffsetID {
				start = index
				break
			}
		}
		start = max(start+request.AddOffset, 0)
		end := min(start+request.Limit, len(ids))
		var page []tg.MessageClass
		for _, id := range ids[start:end] {
			page = append(page, h.messages[id])
		}
		return respond(output, &tg.MessagesChannelMessages{Messages: page, Count: len(page)})
	case *tg.MessagesForwardMessagesRequest:
		top, _ := request.GetTopMsgID()
		h.calls = append(h.calls, fmt.Sprintf("forward %v top=%d", request.ID, top))
		if h.restricted {
			return tgerr.New(400, "CHAT_FORWARDS_RESTRICTED")
		}
		return respond(output, &tg.Updates{})
	case *tg.MessagesSendMessageRequest:
		header, _ := request.GetReplyTo()
		h.calls = append(h.calls, fmt.Sprintf("send %q top=%d", request.Message, topicOf(header)))
		return respond(output, &tg.UpdateShortSentMessage{ID: 900})
	case *tg.ChannelsDeleteMessagesRequest:
		h.calls = append(h.calls, fmt.Sprintf("delete %v", request.ID))
		return respond(output, &tg.MessagesAffectedMessages{Pts: 1, PtsCount: len(request.ID)})
	case *tg.MessagesEditMessageRequest:
		text, _ := request.GetMessage()
		h.calls = append(h.calls, fmt.Sprintf("edit %q", text))
		return respond(output, &tg.Updates{})
	}
	return fmt.Errorf("假 Telegram 不认识 %T", input)
}

// chat 是超级群里的历史：11 号已删除，14 号是服务消息，12 号往后隔了几个编号才是 20 号。
func chat(restricted bool) *history {
	h := &history{messages: map[int]tg.MessageClass{}, restricted: restricted}
	for _, id := range []int{10, 12, 13, 15, 20} {
		message := &tg.Message{ID: id, PeerID: &tg.PeerChannel{ChannelID: group}, Message: fmt.Sprintf("第%d条", id)}
		message.SetFromID(&tg.PeerUser{UserID: 2})
		h.messages[id] = message
	}
	h.messages[14] = &tg.MessageService{ID: 14, PeerID: &tg.PeerChannel{ChannelID: group}, Action: &tg.MessageActionPinMessage{}}
	return h
}

// invocation 构造一条回复 12 号的命令。forum 为真时命令发在论坛的 7 号话题里；
// 否则是普通超级群的回复链，根消息同样是 7 号，但那不是话题。
func invocation(h *history, trigger *bot.Message, forum bool, args ...string) *command.Invocation {
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	channel := &tg.Channel{ID: group, Megagroup: true, Title: "群"}
	channel.SetAccessHash(1)
	peers.RememberChats([]tg.ChatClass{channel})
	client := bot.FromAPI(tg.NewClient(h), peers, &tg.User{ID: 1, Self: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	raw := &tg.Message{ID: 100, PeerID: &tg.PeerChannel{ChannelID: group}, Out: true}
	header := &tg.MessageReplyHeader{ReplyToMsgID: 12, ForumTopic: forum}
	header.SetReplyToTopID(7)
	raw.SetReplyTo(header)
	message, _ := bot.Envelope(raw, 1, false, peers)
	return &command.Invocation{Prefix: ".", Command: "re", Args: args, Message: message, Client: client, Trigger: trigger}
}

func run(t *testing.T, h *history, trigger *bot.Message, forum bool, args ...string) []string {
	t.Helper()
	if err := handle(context.Background(), t.TempDir(), invocation(h, trigger, forum, args...)); err != nil {
		t.Fatal(err)
	}
	return h.calls
}

// 从被回复的消息往后数：删掉的 11 号不在其中，服务消息占名额但不转发；转发留在命令所在的话题里。
func TestRepeatForwardsFromTheRepliedMessageOn(t *testing.T) {
	calls := run(t, chat(false), nil, true, "3", "2")
	want := []string{
		"history offset=12 add=-3 limit=3",
		"forward [12 13] top=7",
		"forward [12 13] top=7",
		"delete [100]",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("calls:\n%q\nwant:\n%q", calls, want)
	}
}

// 对话禁止转发时改为把内容重新发一遍，同样发在话题里；借用账号发起的，对方那条命令也删掉。
func TestRepeatCopiesWhenForwardingIsRestricted(t *testing.T) {
	trigger := &bot.Message{ID: 99, Peer: &tg.PeerChannel{ChannelID: group}, ChatID: "-1005000"}
	calls := run(t, chat(true), trigger, true, "2", "2")
	want := []string{
		"history offset=12 add=-2 limit=2",
		"forward [12 13] top=7",
		`send "第12条" top=7`,
		`send "第13条" top=7`,
		`send "第12条" top=7`,
		`send "第13条" top=7`,
		"delete [100]",
		"delete [99]",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("calls:\n%q\nwant:\n%q", calls, want)
	}
}

// 普通超级群的回复链不是话题，不带 top_msg_id。
func TestRepeatOutsideForumsHasNoTopic(t *testing.T) {
	calls := run(t, chat(false), nil, false, "1")
	want := []string{"history offset=12 add=-1 limit=1", "forward [12] top=0", "delete [100]"}
	if !slices.Equal(calls, want) {
		t.Errorf("calls:\n%q\nwant:\n%q", calls, want)
	}
}
