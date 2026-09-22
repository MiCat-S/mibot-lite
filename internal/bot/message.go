package bot

import (
	"strconv"
	"strings"

	"github.com/gotd/td/tg"
)

// ChatType classifies where a message lives.
type ChatType string

const (
	ChatPrivate    ChatType = "private"
	ChatGroup      ChatType = "group"
	ChatSupergroup ChatType = "supergroup"
	ChatBroadcast  ChatType = "broadcast"
)

// Message is one protocol message, normalised.
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
	// Sender is the FromID peer when present, else the chat for incoming
	// private messages, else the account itself for outgoing ones.
	Sender      tg.PeerClass
	ReplyToID   int
	ReplyToPeer tg.PeerClass
	TopicID     int
	// QuoteText is the slice of the replied message the sender picked out,
	// when they replied to part of it rather than the whole. .yvlu renders
	// that selection instead of the full message.
	QuoteText     string
	QuoteEntities []tg.MessageEntityClass
	Raw           *tg.Message
}

// PeerID renders a peer the way teleproto marks decimal ids.
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

// PeerFromID is PeerID's inverse.
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

// SenderID is the sender's plain numeric id (user id or channel id), or 0.
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

// IsGroup reports a group or supergroup.
func (m *Message) IsGroup() bool { return m.ChatType == ChatGroup || m.ChatType == ChatSupergroup }

// Channel returns the channel id when the chat is a channel or supergroup.
func (m *Message) Channel() (int64, bool) {
	channel, ok := m.Peer.(*tg.PeerChannel)
	if !ok {
		return 0, false
	}
	return channel.ChannelID, true
}

// Envelope normalises a protocol message. It reports false when the peer
// cannot be addressed at all.
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
	// Telegram leaves Out clear in a chat with oneself: there is no
	// direction to record. The account is the only writer there, so the
	// envelope says so rather than leaving callers to rediscover it.
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
