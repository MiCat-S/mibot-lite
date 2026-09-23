// Package bot 为命令封装 gotd：按 peer 发送、编辑、删除和读取消息，
// access hash 的记录维护都藏在里面。
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

// ErrUnaddressablePeer 表示不知道某个 peer 的 access hash。客户端从不
// 猜测 hash：猜错了，消息就会发到别处。
var ErrUnaddressablePeer = errors.New("telegram peer is not addressable from this session")

// Client 是已连接的账号。
type Client struct {
	tg     *telegram.Client
	api    *tg.Client
	peers  *PeerCache
	self   *tg.User
	logger *slog.Logger
	upload *uploader.Uploader

	// dcConns 保存下载时额外打开的数据中心连接。
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

// New 封装一个已授权的客户端。
func New(client *telegram.Client, peers *PeerCache, self *tg.User, logger *slog.Logger) *Client {
	api := client.API()
	return &Client{tg: client, api: api, peers: peers, self: self, logger: logger, upload: uploader.NewUploader(api)}
}

// API 返回原始的 TL 客户端。
func (c *Client) API() *tg.Client { return c.api }

// Peers 返回 access hash 缓存。
func (c *Client) Peers() *PeerCache { return c.peers }

// Self 返回已认证的用户。
func (c *Client) Self() *tg.User { return c.self }

// SelfID 返回已认证用户的 id。
func (c *Client) SelfID() int64 { return c.self.ID }

// Logger 返回进程的日志器。
func (c *Client) Logger() *slog.Logger { return c.logger }

// Uploader 返回文件上传器。
func (c *Client) Uploader() *uploader.Uploader { return c.upload }

// Ping 测量一次开销很小的、带认证的往返。
func (c *Client) Ping(ctx context.Context) (time.Duration, error) {
	started := time.Now()
	if _, err := c.tg.Self(ctx); err != nil {
		return 0, err
	}
	return time.Since(started), nil
}

// InputPeer 通过缓存解析 peer。
func (c *Client) InputPeer(peer tg.PeerClass) (tg.InputPeerClass, error) {
	resolved, ok := c.peers.InputPeer(peer)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnaddressablePeer, PeerID(peer))
	}
	return resolved, nil
}

// InputPeerFromChatID 解析带标记的十进制 id。
func (c *Client) InputPeerFromChatID(chatID string) (tg.InputPeerClass, error) {
	peer, ok := PeerFromID(chatID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnaddressablePeer, chatID)
	}
	return c.InputPeer(peer)
}

// ResolveTarget 接受 "me"、带标记的十进制 id 或 @username。
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

// ResolveUsername 把公开用户名解析成 peer，并记下回复里带的实体。
func (c *Client) ResolveUsername(ctx context.Context, username string) (tg.InputPeerClass, error) {
	resolved, err := c.api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: strings.TrimPrefix(username, "@")})
	if err != nil {
		return nil, err
	}
	c.peers.RememberUsers(resolved.Users)
	c.peers.RememberChats(resolved.Chats)
	return c.InputPeer(resolved.Peer)
}

// InputChannel 在 input peer 是频道时，把它转成 input channel。
func InputChannel(peer tg.InputPeerClass) (*tg.InputChannel, bool) {
	channel, ok := peer.(*tg.InputPeerChannel)
	if !ok {
		return nil, false
	}
	return &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash}, true
}

// InputUser 在 input peer 是用户时，把它转成 input user。
func InputUser(peer tg.InputPeerClass) (tg.InputUserClass, bool) {
	switch value := peer.(type) {
	case *tg.InputPeerUser:
		return &tg.InputUser{UserID: value.UserID, AccessHash: value.AccessHash}, true
	case *tg.InputPeerSelf:
		return &tg.InputUserSelf{}, true
	}
	return nil, false
}

// ParseHTML 把 Telegram 风格的 HTML 转成纯文本加实体。
func ParseHTML(text string) (string, []tg.MessageEntityClass, error) {
	builder := &entity.Builder{}
	if err := html.HTML(strings.NewReader(text), builder, html.Options{}); err != nil {
		return "", nil, err
	}
	plain, entities := builder.Complete()
	return plain, entities, nil
}

// SendOptions 是一次发送的选项。
type SendOptions struct {
	ReplyTo     int
	LinkPreview bool
	Silent      bool
}

// SendHTML 发送一条消息，返回它的 id。
func (c *Client) SendHTML(ctx context.Context, peer tg.InputPeerClass, text string, options SendOptions) (int, error) {
	id, _, err := c.SendHTMLRaw(ctx, peer, text, options)
	return id, err
}

// SendHTMLRaw 发送一条消息，返回它的 id 和 Telegram 应答的原始更新。
//
// 服务器不会把这些更新推送给本进程：在这条连接上做的操作，只在它自己的
// RPC 结果里报告，不会再推送回来。普通发送直接丢掉它们，这样做是对的：
// 要是把自己发出的消息重新分发一遍，某个命令发出以前缀开头的文本时，
// 就会触发它自己。只有 --verify 需要这些更新，因为它得从内部驱动分发器。
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

// SendText 按原样发送文本。
func (c *Client) SendText(ctx context.Context, peer tg.InputPeerClass, text string, options SendOptions) (int, error) {
	return c.SendHTML(ctx, peer, Escape(text), options)
}

// SendSelf 发到收藏夹。
func (c *Client) SendSelf(ctx context.Context, text string) (int, error) {
	return c.SendHTML(ctx, &tg.InputPeerSelf{}, text, SendOptions{})
}

// EditMessage 替换消息文本。MESSAGE_NOT_MODIFIED 也算成功。
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

// Edit 用 HTML 替换消息文本。
func (c *Client) Edit(ctx context.Context, message *Message, text string) error {
	peer, err := c.InputPeer(message.Peer)
	if err != nil {
		return err
	}
	return c.EditMessage(ctx, peer, message.ID, text, false)
}

// EditText 用原样文本替换消息文本。
func (c *Client) EditText(ctx context.Context, message *Message, text string) error {
	return c.Edit(ctx, message, Escape(text))
}

// Reply 发一条新消息，回复给定的那条。
func (c *Client) Reply(ctx context.Context, message *Message, text string) (int, error) {
	peer, err := c.InputPeer(message.Peer)
	if err != nil {
		return 0, err
	}
	return c.SendHTML(ctx, peer, text, SendOptions{ReplyTo: message.ID})
}

// Delete 为所有人删除消息。
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

// DeleteMessage 删除一条消息。
func (c *Client) DeleteMessage(ctx context.Context, message *Message) error {
	peer, err := c.InputPeer(message.Peer)
	if err != nil {
		return err
	}
	return c.Delete(ctx, peer, []int{message.ID})
}

// Forward 把消息从一个对话转发到另一个对话。
func (c *Client) Forward(ctx context.Context, from, to tg.InputPeerClass, ids []int) error {
	randomIDs := make([]int64, len(ids))
	for index := range ids {
		randomIDs[index] = rand.Int64()
	}
	_, err := c.api.MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{FromPeer: from, ID: ids, RandomID: randomIDs, ToPeer: to})
	return err
}

// GetMessages 读取指定的消息，并记下回复里带的实体。
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

// Unpack 读取一个消息列表类的回复，记下其中的实体，返回消息和回复里的
// 总数。回复本身不带总数时（MessagesMessages），总数就是这一批的条数。
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

// GetReply 读取这条消息所回复的消息，没有则返回 nil。
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

// SendDocument 上传字节，作为文件发送，并附上 HTML 说明文字。
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

// FloodWait 返回 FLOOD_WAIT 错误要求等待的时长。
func FloodWait(err error) (time.Duration, bool) { return tgerr.AsFloodWait(err) }

// Escape 转义不可信的文本，让它在 HTML 解析模式下是安全的。
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

// Code 把转义后的文本包进 <code>。
func Code(value string) string { return "<code>" + Escape(value) + "</code>" }

// Bold 把转义后的文本包进 <b>。
func Bold(value string) string { return "<b>" + Escape(value) + "</b>" }

// UserID 把用户 id 转成字符串。
func UserID(id int64) string { return strconv.FormatInt(id, 10) }

// DocumentOptions 描述上传的文档除字节内容之外的信息。
type DocumentOptions struct {
	// Name 是 Telegram 记录的文件名。
	Name string
	// MimeType 是声明的内容类型。
	MimeType string
	// Caption 是 HTML，可以为空。
	Caption string
	// ReplyTo 非零时，回复对应的那条消息。
	ReplyTo int
	// Attributes 是文件名以外的属性：发贴纸的命令会放入贴纸属性和图片尺寸。
	Attributes []tg.DocumentAttributeClass
	// ForceDocument 把文件当普通文档发送，不让 Telegram 自行解读它。
	ForceDocument bool
}

// SendDocumentWith 上传字节，并带上给定的属性发送。
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

// SendPhoto 上传图片并作为照片发送，这样客户端会直接显示它，
// 而不是显示成一个要下载的文件。
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

// UploadDocument 上传字节并登记成文档，供贴纸相关的 RPC 使用，
// 这些 RPC 只接受 InputDocument。
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
