package bot

import (
	"strconv"
	"strings"

	"github.com/gotd/td/tg"
)

// ChatType 表示消息所在对话的类型。
type ChatType string

const (
	ChatPrivate    ChatType = "private"
	ChatGroup      ChatType = "group"
	ChatSupergroup ChatType = "supergroup"
	ChatBroadcast  ChatType = "broadcast"
)

// Message 是规整之后的一条协议消息。
type Message struct {
	ID       int
	Peer     tg.PeerClass
	ChatID   string
	ChatType ChatType
	Text     string
	Out      bool
	Edited   bool
	Forward  bool
	Post     bool
	Saved    bool
	// Sender 有 FromID 时就是 FromID 对应的 peer；否则收到的私聊消息取
	// 对话本身，发出的消息取本账号。
	Sender      tg.PeerClass
	ReplyToID   int
	ReplyToPeer tg.PeerClass
	TopicID     int
	// QuoteText 是发送者只回复原消息的一部分而不是整条时，选中的那一段。
	// .yvlu 渲染的是这段选中的内容，而不是整条消息。
	QuoteText     string
	QuoteEntities []tg.MessageEntityClass
	Raw           *tg.Message
}

// PeerID 按 teleproto 标记十进制 id 的方式渲染 peer。
func PeerID(peer tg.PeerClass) string {
	switch value := peer.(type) {
	case *tg.PeerUser:
		return strconv.FormatInt(value.UserID, 10)
	case *tg.PeerChat:
		return "-" + strconv.FormatInt(value.ChatID, 10)
	case *tg.PeerChannel:
		return "-100" + strconv.FormatInt(value.ChannelID, 10)
	}
	return ""
}

// PeerFromID 是 PeerID 的逆操作。
func PeerFromID(id string) (tg.PeerClass, bool) {
	if id == "" {
		return nil, false
	}
	if strings.HasPrefix(id, "-100") {
		value, err := strconv.ParseInt(id[4:], 10, 64)
		if err != nil || value <= 0 {
			return nil, false
		}
		return &tg.PeerChannel{ChannelID: value}, true
	}
	value, err := strconv.ParseInt(id, 10, 64)
	if err != nil || value == 0 {
		return nil, false
	}
	if value < 0 {
		return &tg.PeerChat{ChatID: -value}, true
	}
	return &tg.PeerUser{UserID: value}, true
}

// SenderID 是发送者的纯数字 id（用户 id 或频道 id），没有则为 0。
func (m *Message) SenderID() int64 {
	switch value := m.Sender.(type) {
	case *tg.PeerUser:
		return value.UserID
	case *tg.PeerChannel:
		return value.ChannelID
	case *tg.PeerChat:
		return value.ChatID
	}
	return 0
}

// IsGroup 判断是不是普通群或超级群。
func (m *Message) IsGroup() bool { return m.ChatType == ChatGroup || m.ChatType == ChatSupergroup }

// Channel 在对话是频道或超级群时返回频道 id。
func (m *Message) Channel() (int64, bool) {
	channel, ok := m.Peer.(*tg.PeerChannel)
	if !ok {
		return 0, false
	}
	return channel.ChannelID, true
}

// Envelope 规整一条协议消息。peer 完全无法寻址时返回 false。
func Envelope(message *tg.Message, selfID int64, edited bool, peers *PeerCache) (*Message, bool) {
	chatID := PeerID(message.PeerID)
	if chatID == "" {
		return nil, false
	}
	result := &Message{
		ID: message.ID, Peer: message.PeerID, ChatID: chatID, Text: message.Message,
		Out: message.Out, Post: message.Post, Raw: message,
	}
	switch peer := message.PeerID.(type) {
	case *tg.PeerUser:
		result.ChatType = ChatPrivate
		result.Saved = peer.UserID == selfID
	case *tg.PeerChat:
		result.ChatType = ChatGroup
	case *tg.PeerChannel:
		result.ChatType = ChatSupergroup
		if info, ok := peers.Channel(peer.ChannelID); ok && info.Broadcast {
			result.ChatType = ChatBroadcast
		} else if !ok && message.Post {
			result.ChatType = ChatBroadcast
		}
	}
	if from, present := message.GetFromID(); present {
		result.Sender = from
	} else if message.Out {
		result.Sender = &tg.PeerUser{UserID: selfID}
	} else {
		result.Sender = message.PeerID
	}
	if _, hasSaved := message.GetSavedPeerID(); hasSaved {
		result.Saved = true
	}
	// 在和自己的对话里，Telegram 不设 Out：没有方向可记。那里只有本账号
	// 会写消息，所以 envelope 直接把这一点标出来，免得调用方各自再推一遍。
	if result.Saved && result.SenderID() == selfID {
		result.Out = true
	}
	editDate, hasEdit := message.GetEditDate()
	result.Edited = edited || (hasEdit && editDate != 0)
	_, result.Forward = message.GetFwdFrom()
	if header, present := message.GetReplyTo(); present {
		if reply, ok := header.(*tg.MessageReplyHeader); ok {
			result.ReplyToID = reply.ReplyToMsgID
			if replyPeer, hasPeer := reply.GetReplyToPeerID(); hasPeer {
				result.ReplyToPeer = replyPeer
			}
			if topID, hasTop := reply.GetReplyToTopID(); hasTop {
				result.TopicID = topID
			} else if reply.ForumTopic {
				result.TopicID = reply.ReplyToMsgID
			}
			if reply.Quote {
				result.QuoteText = reply.QuoteText
				result.QuoteEntities = reply.QuoteEntities
			}
		}
	}
	return result, true
}
