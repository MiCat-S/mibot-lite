// Package bot wraps gotd for the commands: sending, editing, deleting and
// fetching messages by peer, with the access-hash bookkeeping hidden.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message/entity"
	"github.com/gotd/td/telegram/message/html"
	"github.com/gotd/td/telegram/message/unpack"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// ErrUnaddressablePeer means no access hash is known for a peer. The client
// never guesses one: a wrong hash sends the message somewhere else.
var ErrUnaddressablePeer = errors.New("telegram peer is not addressable from this session")

// Client is the connected account.
type Client struct {
	tg     *telegram.Client
	api    *tg.Client
	peers  *PeerCache
	self   *tg.User
	logger *slog.Logger
	upload *uploader.Uploader

	// dcConns holds the extra data-centre connections downloads opened.
	dcMu    sync.Mutex
	dcConns map[int]*dcConnection
}

// FromAPI 只用一个 RPC 接口构造客户端，没有底层连接。
//
// 给测试用：把一个假的 Telegram 接进来，就能在不连网络的情况下，
// 逐个请求地检查 .dme、.da 这类会删消息的命令到底发了什么。
// 需要底层连接的功能（跨数据中心下载）在这样的客户端上不可用。
func FromAPI(api *tg.Client, peers *PeerCache, self *tg.User, logger *slog.Logger) *Client {
	return &Client{api: api, peers: peers, self: self, logger: logger, upload: uploader.NewUploader(api)}
}

// New wraps an authorized client.
func New(client *telegram.Client, peers *PeerCache, self *tg.User, logger *slog.Logger) *Client {
	api := client.API()
	return &Client{tg: client, api: api, peers: peers, self: self, logger: logger, upload: uploader.NewUploader(api)}
}

// API is the raw TL client.
func (c *Client) API() *tg.Client { return c.api }

// Peers is the access-hash cache.
func (c *Client) Peers() *PeerCache { return c.peers }

// Self is the authenticated user.
func (c *Client) Self() *tg.User { return c.self }

// SelfID is the authenticated user's id.
func (c *Client) SelfID() int64 { return c.self.ID }

// Logger is the process logger.
func (c *Client) Logger() *slog.Logger { return c.logger }

// Uploader uploads files.
func (c *Client) Uploader() *uploader.Uploader { return c.upload }

// Ping measures one cheap authenticated round trip.
func (c *Client) Ping(ctx context.Context) (time.Duration, error) {
	started := time.Now()
	if _, err := c.tg.Self(ctx); err != nil {
		return 0, err
	}
	return time.Since(started), nil
}

// InputPeer resolves a peer through the cache.
func (c *Client) InputPeer(peer tg.PeerClass) (tg.InputPeerClass, error) {
	resolved, ok := c.peers.InputPeer(peer)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnaddressablePeer, PeerID(peer))
	}
	return resolved, nil
}

// InputPeerFromChatID resolves a marked decimal id.
func (c *Client) InputPeerFromChatID(chatID string) (tg.InputPeerClass, error) {
	peer, ok := PeerFromID(chatID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnaddressablePeer, chatID)
	}
	return c.InputPeer(peer)
}

// ResolveTarget accepts "me", a marked decimal id or a @username.
func (c *Client) ResolveTarget(ctx context.Context, target string) (tg.InputPeerClass, error) {
	target = strings.TrimSpace(target)
	switch {
	case target == "" || target == "me" || target == "self":
		return &tg.InputPeerSelf{}, nil
	case strings.HasPrefix(target, "@"):
		return c.ResolveUsername(ctx, target[1:])
	}
	if peer, ok := PeerFromID(target); ok {
		if resolved, ok := c.peers.InputPeer(peer); ok {
			return resolved, nil
		}
		return nil, fmt.Errorf("%w: %s", ErrUnaddressablePeer, target)
	}
	return c.ResolveUsername(ctx, target)
}

// ResolveUsername resolves a public username to a peer, remembering the
// entities the reply carried.
func (c *Client) ResolveUsername(ctx context.Context, username string) (tg.InputPeerClass, error) {
	resolved, err := c.api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: strings.TrimPrefix(username, "@")})
	if err != nil {
		return nil, err
	}
	c.peers.RememberUsers(resolved.Users)
	c.peers.RememberChats(resolved.Chats)
	return c.InputPeer(resolved.Peer)
}

// InputChannel converts an input peer to an input channel when it is one.
func InputChannel(peer tg.InputPeerClass) (*tg.InputChannel, bool) {
	channel, ok := peer.(*tg.InputPeerChannel)
	if !ok {
		return nil, false
	}
	return &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash}, true
}

// InputUser converts an input peer to an input user when it is one.
func InputUser(peer tg.InputPeerClass) (tg.InputUserClass, bool) {
	switch value := peer.(type) {
	case *tg.InputPeerUser:
		return &tg.InputUser{UserID: value.UserID, AccessHash: value.AccessHash}, true
	case *tg.InputPeerSelf:
		return &tg.InputUserSelf{}, true
	}
	return nil, false
}

// ParseHTML turns Telegram-flavoured HTML into text plus entities.
func ParseHTML(text string) (string, []tg.MessageEntityClass, error) {
	builder := &entity.Builder{}
	if err := html.HTML(strings.NewReader(text), builder, html.Options{}); err != nil {
		return "", nil, err
	}
	plain, entities := builder.Complete()
	return plain, entities, nil
}

// SendOptions tune a send.
type SendOptions struct {
	ReplyTo     int
	LinkPreview bool
	Silent      bool
}

// SendHTML sends a message and returns its id.
func (c *Client) SendHTML(ctx context.Context, peer tg.InputPeerClass, text string, options SendOptions) (int, error) {
	id, _, err := c.SendHTMLRaw(ctx, peer, text, options)
	return id, err
}

// SendHTMLRaw sends a message and returns its id together with the raw
// updates Telegram answered with.
//
// Those updates are not delivered to this process by the server: an action
// taken on this connection is reported in its own RPC result, not pushed
// back. Ordinary sending discards them, which is right — re-dispatching
// one's own outgoing message would make a command that sends text starting
// with the prefix invoke itself. --verify is the one caller that wants
// them, because it has to drive the dispatcher from inside.
func (c *Client) SendHTMLRaw(ctx context.Context, peer tg.InputPeerClass, text string, options SendOptions) (int, tg.UpdatesClass, error) {
	plain, entities, err := ParseHTML(text)
	if err != nil {
		return 0, nil, err
	}
	request := &tg.MessagesSendMessageRequest{Peer: peer, Message: plain, RandomID: rand.Int64(), NoWebpage: !options.LinkPreview, Silent: options.Silent}
	if len(entities) > 0 {
		request.SetEntities(entities)
	}
	if options.ReplyTo > 0 {
		request.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: options.ReplyTo})
	}
	updates, err := c.api.MessagesSendMessage(ctx, request)
	if err != nil {
		return 0, nil, err
	}
	id, err := unpack.MessageID(updates, nil)
	return id, updates, err
}

// SendText sends literal text.
func (c *Client) SendText(ctx context.Context, peer tg.InputPeerClass, text string, options SendOptions) (int, error) {
	return c.SendHTML(ctx, peer, Escape(text), options)
}

// SendSelf sends to Saved Messages.
func (c *Client) SendSelf(ctx context.Context, text string) (int, error) {
	return c.SendHTML(ctx, &tg.InputPeerSelf{}, text, SendOptions{})
}

// EditMessage replaces a message's text. MESSAGE_NOT_MODIFIED is success.
func (c *Client) EditMessage(ctx context.Context, peer tg.InputPeerClass, id int, text string, linkPreview bool) error {
	plain, entities, err := ParseHTML(text)
	if err != nil {
		return err
	}
	request := &tg.MessagesEditMessageRequest{Peer: peer, ID: id, NoWebpage: !linkPreview}
	request.SetMessage(plain)
	if len(entities) > 0 {
		request.SetEntities(entities)
	}
	if _, err := c.api.MessagesEditMessage(ctx, request); err != nil && !tgerr.Is(err, "MESSAGE_NOT_MODIFIED") {
		return err
	}
	return nil
}

// Edit replaces a message's text with HTML.
func (c *Client) Edit(ctx context.Context, message *Message, text string) error {
	peer, err := c.InputPeer(message.Peer)
	if err != nil {
		return err
	}
	return c.EditMessage(ctx, peer, message.ID, text, false)
}

// EditText replaces a message's text with literal text.
func (c *Client) EditText(ctx context.Context, message *Message, text string) error {
	return c.Edit(ctx, message, Escape(text))
}

// Reply sends a new message answering the given one.
func (c *Client) Reply(ctx context.Context, message *Message, text string) (int, error) {
	peer, err := c.InputPeer(message.Peer)
	if err != nil {
		return 0, err
	}
	return c.SendHTML(ctx, peer, text, SendOptions{ReplyTo: message.ID})
}

// Delete removes messages for everyone.
func (c *Client) Delete(ctx context.Context, peer tg.InputPeerClass, ids []int) error {
	if len(ids) == 0 {
		return nil
	}
	if channel, ok := InputChannel(peer); ok {
		_, err := c.api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{Channel: channel, ID: ids})
		return err
	}
	_, err := c.api.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{Revoke: true, ID: ids})
	return err
}

// DeleteMessage removes one message.
func (c *Client) DeleteMessage(ctx context.Context, message *Message) error {
	peer, err := c.InputPeer(message.Peer)
	if err != nil {
		return err
	}
	return c.Delete(ctx, peer, []int{message.ID})
}

// Forward copies messages from one chat to another.
func (c *Client) Forward(ctx context.Context, from, to tg.InputPeerClass, ids []int) error {
	randomIDs := make([]int64, len(ids))
	for index := range ids {
		randomIDs[index] = rand.Int64()
	}
	_, err := c.api.MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{FromPeer: from, ID: ids, RandomID: randomIDs, ToPeer: to})
	return err
}

// GetMessages reads specific messages, remembering the entities the reply
// carried.
func (c *Client) GetMessages(ctx context.Context, peer tg.InputPeerClass, ids []int) ([]*tg.Message, error) {
	inputs := make([]tg.InputMessageClass, len(ids))
	for index, id := range ids {
		inputs[index] = &tg.InputMessageID{ID: id}
	}
	var result tg.MessagesMessagesClass
	var err error
	if channel, ok := InputChannel(peer); ok {
		result, err = c.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: channel, ID: inputs})
	} else {
		result, err = c.api.MessagesGetMessages(ctx, inputs)
	}
	if err != nil {
		return nil, err
	}
	messages, _ := c.Unpack(result)
	var found []*tg.Message
	for _, candidate := range messages {
		if message, ok := candidate.(*tg.Message); ok {
			found = append(found, message)
		}
	}
	return found, nil
}

// Unpack reads a messages reply, remembers its entities and returns the
// messages plus whether the reply carried a total count.
func (c *Client) Unpack(result tg.MessagesMessagesClass) ([]tg.MessageClass, int) {
	switch value := result.(type) {
	case *tg.MessagesMessages:
		c.peers.RememberUsers(value.Users)
		c.peers.RememberChats(value.Chats)
		return value.Messages, len(value.Messages)
	case *tg.MessagesMessagesSlice:
		c.peers.RememberUsers(value.Users)
		c.peers.RememberChats(value.Chats)
		return value.Messages, value.Count
	case *tg.MessagesChannelMessages:
		c.peers.RememberUsers(value.Users)
		c.peers.RememberChats(value.Chats)
		return value.Messages, value.Count
	}
	return nil, 0
}

// GetReply reads the message this one replies to, or nil.
func (c *Client) GetReply(ctx context.Context, message *Message) (*Message, error) {
	if message.ReplyToID == 0 {
		return nil, nil
	}
	target := message.Peer
	if message.ReplyToPeer != nil {
		target = message.ReplyToPeer
	}
	peer, err := c.InputPeer(target)
	if err != nil {
		return nil, err
	}
	found, err := c.GetMessages(ctx, peer, []int{message.ReplyToID})
	if err != nil {
		return nil, err
	}
	for _, candidate := range found {
		if candidate.ID == message.ReplyToID {
			envelope, ok := Envelope(candidate, c.SelfID(), false, c.peers)
			if !ok {
				return nil, nil
			}
			return envelope, nil
		}
	}
	return nil, nil
}

// SendDocument uploads bytes and sends them as a file with an HTML caption.
func (c *Client) SendDocument(ctx context.Context, peer tg.InputPeerClass, name, mimeType string, data []byte, caption string, replyTo int) error {
	file, err := c.upload.FromBytes(ctx, name, data)
	if err != nil {
		return err
	}
	plain, entities, err := ParseHTML(caption)
	if err != nil {
		return err
	}
	media := &tg.InputMediaUploadedDocument{File: file, MimeType: mimeType, Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: name}}}
	request := &tg.MessagesSendMediaRequest{Peer: peer, Media: media, Message: plain, RandomID: rand.Int64()}
	if len(entities) > 0 {
		request.SetEntities(entities)
	}
	if replyTo > 0 {
		request.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: replyTo})
	}
	_, err = c.api.MessagesSendMedia(ctx, request)
	return err
}

// FloodWait reports the wait a FLOOD_WAIT error asks for.
func FloodWait(err error) (time.Duration, bool) { return tgerr.AsFloodWait(err) }

// Escape renders untrusted text safe for the HTML parse mode.
func Escape(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Code wraps escaped text in <code>.
func Code(value string) string { return "<code>" + Escape(value) + "</code>" }

// Bold wraps escaped text in <b>.
func Bold(value string) string { return "<b>" + Escape(value) + "</b>" }

// UserID renders a user id.
func UserID(id int64) string { return strconv.FormatInt(id, 10) }

// DocumentOptions describe an uploaded document beyond its bytes.
type DocumentOptions struct {
	// Name is the file name Telegram records.
	Name string
	// MimeType is the declared content type.
	MimeType string
	// Caption is HTML, and may be empty.
	Caption string
	// ReplyTo answers a message, when non-zero.
	ReplyTo int
	// Attributes beyond the file name — a sticker attribute and an image
	// size, for the commands that send stickers.
	Attributes []tg.DocumentAttributeClass
	// ForceDocument sends the file as a plain document rather than letting
	// Telegram interpret it.
	ForceDocument bool
}

// SendDocumentWith uploads bytes and sends them with the given attributes.
func (c *Client) SendDocumentWith(ctx context.Context, peer tg.InputPeerClass, data []byte, options DocumentOptions) error {
	file, err := c.upload.FromBytes(ctx, options.Name, data)
	if err != nil {
		return err
	}
	plain, entities, err := ParseHTML(options.Caption)
	if err != nil {
		return err
	}
	attributes := append([]tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: options.Name}}, options.Attributes...)
	media := &tg.InputMediaUploadedDocument{File: file, MimeType: options.MimeType, Attributes: attributes, ForceFile: options.ForceDocument}
	request := &tg.MessagesSendMediaRequest{Peer: peer, Media: media, Message: plain, RandomID: rand.Int64()}
	if len(entities) > 0 {
		request.SetEntities(entities)
	}
	if options.ReplyTo > 0 {
		request.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: options.ReplyTo})
	}
	_, err = c.api.MessagesSendMedia(ctx, request)
	return err
}

// SendPhoto uploads an image and sends it as a photo, so clients show it
// inline rather than as a file to download.
func (c *Client) SendPhoto(ctx context.Context, peer tg.InputPeerClass, name string, data []byte, caption string, replyTo int) error {
	file, err := c.upload.FromBytes(ctx, name, data)
	if err != nil {
		return err
	}
	plain, entities, err := ParseHTML(caption)
	if err != nil {
		return err
	}
	request := &tg.MessagesSendMediaRequest{Peer: peer, Media: &tg.InputMediaUploadedPhoto{File: file},
		Message: plain, RandomID: rand.Int64()}
	if len(entities) > 0 {
		request.SetEntities(entities)
	}
	if replyTo > 0 {
		request.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: replyTo})
	}
	_, err = c.api.MessagesSendMedia(ctx, request)
	return err
}

// UploadDocument uploads bytes and registers them as a document Telegram
// will accept in a sticker RPC, which only takes an InputDocument.
func (c *Client) UploadDocument(ctx context.Context, name, mimeType string, data []byte) (*tg.InputDocument, error) {
	file, err := c.upload.FromBytes(ctx, name, data)
	if err != nil {
		return nil, err
	}
	result, err := c.api.MessagesUploadMedia(ctx, &tg.MessagesUploadMediaRequest{
		Peer: &tg.InputPeerSelf{},
		Media: &tg.InputMediaUploadedDocument{File: file, MimeType: mimeType,
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: name}}},
	})
	if err != nil {
		return nil, err
	}
	media, ok := result.(*tg.MessageMediaDocument)
	if !ok {
		return nil, errors.New("upload did not produce a document")
	}
	document, ok := media.Document.(*tg.Document)
	if !ok {
		return nil, errors.New("upload did not produce a document")
	}
	return &tg.InputDocument{ID: document.ID, AccessHash: document.AccessHash, FileReference: document.FileReference}, nil
}
