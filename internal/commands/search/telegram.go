package search

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// source 是解析好的一个频道源。
type source struct {
	peer      tg.InputPeerClass
	title     string
	broadcast bool
	megagroup bool
	channelID int64
}

// 解析结果在进程里缓存一天。频道源存的是用户名或链接，每次搜索都要把它们变回 peer；
// contacts.resolveUsername 的限流很严，每次都去问，频道一多就会被限流。access hash
// 对同一个账号不变，缓存一天足够安全。
const resolveTTL = 24 * time.Hour

type cachedSource struct {
	value *source
	at    time.Time
}

type resolver struct {
	mu    sync.Mutex
	cache map[string]cachedSource
}

// errNotChat 表示解析出来的是用户或机器人，不是频道或群组。
var errNotChat = kit.Fail("不是频道或群组")

// parseHandle 把频道源拆成用户名、邀请链接的 hash 或数字 ID 之一。认得 @name、name、
// t.me/name、t.me/s/name、t.me/+hash、t.me/joinchat/hash 和 -100… 这样的 ID，
// 与 teleproto 的 getEntity 接受的写法一致。
func parseHandle(value string) (username, invite, id string) {
	value = strings.TrimSpace(value)
	if kit.IsNumericID(value) {
		return "", "", value
	}
	lower := strings.ToLower(value)
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(lower, prefix) {
			value, lower = value[len(prefix):], lower[len(prefix):]
		}
	}
	if strings.HasPrefix(lower, "www.") {
		value, lower = value[4:], lower[4:]
	}
	for _, host := range []string{"t.me/", "telegram.me/", "telegram.dog/"} {
		if !strings.HasPrefix(lower, host) {
			continue
		}
		path := value[len(host):]
		if cut := strings.IndexAny(path, "?#"); cut >= 0 {
			path = path[:cut]
		}
		switch {
		case strings.HasPrefix(path, "+"):
			return "", strings.Trim(path[1:], "/"), ""
		case strings.HasPrefix(strings.ToLower(path), "joinchat/"):
			return "", strings.Trim(path[len("joinchat/"):], "/"), ""
		case strings.HasPrefix(strings.ToLower(path), "s/"):
			path = path[2:]
		}
		name, _, _ := strings.Cut(path, "/")
		return strings.TrimPrefix(name, "@"), "", ""
	}
	return strings.TrimPrefix(value, "@"), "", ""
}

// describe 把一个对话变成 source。频道、超级群、普通群都行；无权访问的返回 false。
func describe(chat tg.ChatClass) (*source, bool) {
	switch value := chat.(type) {
	case *tg.Channel:
		return &source{peer: &tg.InputPeerChannel{ChannelID: value.ID, AccessHash: value.AccessHash}, title: value.Title,
			broadcast: value.Broadcast, megagroup: value.Megagroup, channelID: value.ID}, true
	case *tg.Chat:
		return &source{peer: &tg.InputPeerChat{ChatID: value.ID}, title: value.Title}, true
	}
	return nil, false
}

// resolve 把频道源解析成 source，先看缓存。
func (r *resolver) resolve(ctx context.Context, client *bot.Client, handle string) (*source, error) {
	r.mu.Lock()
	if cached, ok := r.cache[handle]; ok && time.Since(cached.at) < resolveTTL {
		r.mu.Unlock()
		return cached.value, nil
	}
	r.mu.Unlock()
	found, err := lookup(ctx, client, handle)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]cachedSource{}
	}
	r.cache[handle] = cachedSource{value: found, at: time.Now()}
	r.mu.Unlock()
	return found, nil
}

// has 判断这个频道源是否已在缓存里（解析它不用发请求）。
func (r *resolver) has(handle string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cached, ok := r.cache[handle]
	return ok && time.Since(cached.at) < resolveTTL
}

// forget 把一个频道源从缓存里去掉（删除频道源时用）。
func (r *resolver) forget(handle string) {
	r.mu.Lock()
	delete(r.cache, handle)
	r.mu.Unlock()
}

var (
	// usernamePattern 是 Telegram 用户名的格式：字母开头，字母、数字、下划线，5 到 32 位
	// （放宽到 4 位起，兼容老的短用户名）。
	usernamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{3,31}$`)
	// invitePattern 是邀请链接里 hash 的格式。
	invitePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{5,64}$`)
)

// errBadHandle 表示频道源连格式都不对，不值得去问 Telegram。
const errBadHandleText = "不是有效的频道用户名或链接"

var errBadHandle = kit.Fail(errBadHandleText)

// validHandle 在发任何请求之前检查频道源的格式。contacts.resolveUsername 的限流很严，
// 被限流时整个账号都要等；随便一段文字（比如回复了一篇文章去 import）不能每个词都去问一次。
func validHandle(handle string) bool {
	username, invite, id := parseHandle(handle)
	switch {
	case id != "":
		return true
	case invite != "":
		return invitePattern.MatchString(invite)
	}
	return usernamePattern.MatchString(username)
}

func lookup(ctx context.Context, client *bot.Client, handle string) (*source, error) {
	if !validHandle(handle) {
		return nil, errBadHandle
	}
	api := client.API()
	username, invite, id := parseHandle(handle)
	var chats []tg.ChatClass
	var want tg.PeerClass
	switch {
	case id != "":
		peer, err := client.InputPeerFromChatID(id)
		if err != nil {
			return nil, err
		}
		var result tg.MessagesChatsClass
		switch value := peer.(type) {
		case *tg.InputPeerChannel:
			result, err = api.ChannelsGetChannels(ctx, []tg.InputChannelClass{&tg.InputChannel{ChannelID: value.ChannelID, AccessHash: value.AccessHash}})
			want = &tg.PeerChannel{ChannelID: value.ChannelID}
		case *tg.InputPeerChat:
			result, err = api.MessagesGetChats(ctx, []int64{value.ChatID})
			want = &tg.PeerChat{ChatID: value.ChatID}
		default:
			return nil, errNotChat
		}
		if err != nil {
			return nil, err
		}
		chats = result.GetChats()
	case invite != "":
		result, err := api.MessagesCheckChatInvite(ctx, invite)
		if err != nil {
			return nil, err
		}
		switch value := result.(type) {
		case *tg.ChatInviteAlready:
			chats = []tg.ChatClass{value.Chat}
		case *tg.ChatInvitePeek:
			chats = []tg.ChatClass{value.Chat}
		default:
			return nil, kit.Fail("还没有加入这个邀请链接的群组或频道")
		}
	case username != "":
		result, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
		if err != nil {
			return nil, err
		}
		client.Peers().RememberUsers(result.Users)
		chats, want = result.Chats, result.Peer
		if _, isUser := result.Peer.(*tg.PeerUser); isUser {
			return nil, errNotChat
		}
	default:
		return nil, kit.Fail("频道源为空")
	}
	client.Peers().RememberChats(chats)
	for _, chat := range chats {
		if want != nil && !samePeer(chat, want) {
			continue
		}
		if found, ok := describe(chat); ok {
			return found, nil
		}
	}
	return nil, kit.Fail("无法访问")
}

func samePeer(chat tg.ChatClass, peer tg.PeerClass) bool {
	switch value := peer.(type) {
	case *tg.PeerChannel:
		return chat.GetID() == value.ChannelID
	case *tg.PeerChat:
		return chat.GetID() == value.ChatID
	}
	return false
}

// linkedGroup 找频道绑定的讨论组，讨论组有公开用户名时返回 @用户名。找不到不算错：
// 没有讨论组照样能搜频道本身。
func linkedGroup(ctx context.Context, client *bot.Client, channel *source) string {
	input, ok := bot.InputChannel(channel.peer)
	if !ok {
		return ""
	}
	full, err := client.API().ChannelsGetFullChannel(ctx, input)
	if err != nil {
		return ""
	}
	client.Peers().RememberChats(full.Chats)
	detail, ok := full.FullChat.(*tg.ChannelFull)
	if !ok {
		return ""
	}
	linked, ok := detail.GetLinkedChatID()
	if !ok || linked == 0 {
		return ""
	}
	for _, chat := range full.Chats {
		if value, ok := chat.(*tg.Channel); ok && value.ID == linked && value.Username != "" {
			return "@" + value.Username
		}
	}
	return ""
}

// onlyMessages 从一页结果里挑出普通消息。
func onlyMessages(page []tg.MessageClass) []*tg.Message {
	messages := make([]*tg.Message, 0, len(page))
	for _, item := range page {
		if message, ok := item.(*tg.Message); ok {
			messages = append(messages, message)
		}
	}
	return messages
}

// searchMessages 按关键词和类型搜一个对话，最多 limit 条，每页 100 条往前翻。
func searchMessages(ctx context.Context, client *bot.Client, peer tg.InputPeerClass, query string, filter tg.MessagesFilterClass, limit int) ([]*tg.Message, error) {
	if filter == nil {
		filter = &tg.InputMessagesFilterEmpty{}
	}
	var found []*tg.Message
	offset := 0
	for len(found) < limit {
		batch := min(100, limit-len(found))
		result, err := client.API().MessagesSearch(ctx, &tg.MessagesSearchRequest{Peer: peer, Q: query, Filter: filter, OffsetID: offset, Limit: batch})
		if err != nil {
			return nil, err
		}
		page, _ := client.Unpack(result)
		found = append(found, onlyMessages(page)...)
		if len(page) < batch {
			break
		}
		offset = kit.MessageID(page[len(page)-1])
		if offset <= 0 {
			break
		}
	}
	return found, nil
}

// history 读编号小于 offset 的最近 limit 条消息。
func history(ctx context.Context, client *bot.Client, peer tg.InputPeerClass, offset, limit int) ([]*tg.Message, error) {
	result, err := client.API().MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, OffsetID: offset, Limit: limit})
	if err != nil {
		return nil, err
	}
	page, _ := client.Unpack(result)
	return onlyMessages(page), nil
}

// replies 读一条消息下面的回复（频道帖子在讨论组里的评论）。
func replies(ctx context.Context, client *bot.Client, peer tg.InputPeerClass, id, limit int) ([]*tg.Message, error) {
	result, err := client.API().MessagesGetReplies(ctx, &tg.MessagesGetRepliesRequest{Peer: peer, MsgID: id, Limit: limit})
	if err != nil {
		return nil, err
	}
	page, _ := client.Unpack(result)
	return onlyMessages(page), nil
}

// forumTopic 是命令消息所在的论坛话题，结果发到这里；不在论坛话题里返回 0。
// 普通超级群里回复链的根消息也记在 reply_to_top_id 里，那不是话题，不能当话题用。
func forumTopic(message *bot.Message) int {
	if message.Raw == nil {
		return 0
	}
	header, ok := message.Raw.ReplyTo.(*tg.MessageReplyHeader)
	if !ok || !header.ForumTopic {
		return 0
	}
	return message.TopicID
}

// captionLimit 是媒体说明的长度上限（UTF-16 单位），超出的截掉，免得整个发送被拒。
const captionLimit = 1024

func caption(text string) string {
	if command.UTF16Len(text) <= captionLimit {
		return text
	}
	runes := []rune(text)
	for len(runes) > 0 && command.UTF16Len(string(runes))+1 > captionLimit {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

// maxMediaBytes 是能下载再上传的视频上限，和 Telegram 普通账号的文件上限一样是 2 GB。
const maxMediaBytes = 2 << 30

// reupload 把视频下载到部署目录下的暂存目录，再作为视频上传发到当前对话，spoiler 为真时
// 打上防剧透遮罩。bot.MediaOptions 没有防剧透选项，所以这里自己组 messages.sendMedia。
// 暂存目录不用 /tmp：这类主机上 /tmp 常是 tmpfs，2 GB 的视频放进去就是占着内存。
func reupload(ctx context.Context, inv *command.Invocation, partial string, picked candidate, text string, spoiler bool) error {
	media, ok := bot.SourceOf(picked.message)
	document, video, isVideo := videoDocument(picked.message)
	if !ok || !isVideo {
		return kit.Fail("这条视频已经不可用")
	}
	if document.Size > maxMediaBytes {
		return kit.Failf("视频 %s，超过 2 GB 上限", kit.FormatBytes(document.Size))
	}
	if err := os.MkdirAll(partial, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(partial, "video-*")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if err := inv.EditText(ctx, kit.Working("正在下载视频 "+kit.FormatBytes(document.Size))); err != nil {
		file.Close()
		return err
	}
	if err := inv.Client.DownloadTo(ctx, media, file); err != nil {
		file.Close()
		return kit.FailWith("下载视频失败", err)
	}
	info, err := file.Stat()
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return kit.Fail("视频下载为空")
	}
	if err := inv.EditText(ctx, kit.Working("正在上传视频")); err != nil {
		return err
	}
	// 和 .save 一样四路并行上传，几百 MB 的视频单线程传得很慢。
	uploaded, err := uploader.NewUploader(inv.Client.API()).WithThreads(4).FromPath(ctx, path)
	if err != nil {
		return kit.FailWith("上传视频失败", err)
	}
	target, err := inv.Client.InputPeer(inv.Message.Peer)
	if err != nil {
		return err
	}
	mimeType := document.MimeType
	if !strings.HasPrefix(mimeType, "video/") {
		mimeType = "video/mp4"
	}
	attributes := []tg.DocumentAttributeClass{
		&tg.DocumentAttributeVideo{Duration: video.Duration, W: video.W, H: video.H, SupportsStreaming: true},
		&tg.DocumentAttributeFilename{FileName: "video.mp4"},
	}
	request := &tg.MessagesSendMediaRequest{Peer: target, RandomID: rand.Int64(), Message: caption(text),
		Media: &tg.InputMediaUploadedDocument{File: uploaded, MimeType: mimeType, Attributes: attributes, Spoiler: spoiler}}
	if reply := replyTo(inv.Message); reply != nil {
		request.SetReplyTo(reply)
	}
	if _, err := inv.Client.API().MessagesSendMedia(ctx, request); err != nil {
		return kit.FailWith("发送视频失败", err)
	}
	return nil
}

// replyTo 是发出的视频要挂在哪里：命令本身回复了别的消息就回复那条（命令随后会被删掉，
// 回复命令本身没有意义），在论坛话题里就留在这个话题。
func replyTo(message *bot.Message) tg.InputReplyToClass {
	topic := forumTopic(message)
	target := message.ReplyToID
	if target == topic {
		target = 0
	}
	if target == 0 && topic == 0 {
		return nil
	}
	reply := &tg.InputReplyToMessage{ReplyToMsgID: target}
	if target == 0 {
		reply.ReplyToMsgID = topic
	}
	if topic > 0 {
		reply.SetTopMsgID(topic)
	}
	return reply
}

// deleteCommand 删掉命令消息，结果已经发出去了；删不掉只记日志。
func deleteCommand(ctx context.Context, inv *command.Invocation) {
	if err := inv.Client.DeleteMessage(ctx, inv.Message); err != nil && !errors.Is(err, context.Canceled) {
		inv.Log.Warn("search.cleanup_failed", "error", err.Error())
	}
}
