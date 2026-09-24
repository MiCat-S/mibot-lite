// Package save 实现 .save：保存或转发消息，禁止转发的对话也能存。
package save

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// saveDocument 对应 data/save.json。
type saveDocument struct {
	// Target 是保存的消息发往哪里："" 表示收藏夹，"local" 表示存到磁盘，
	// 其他值是用户名或对话 id。
	Target string `json:"target,omitempty"`
	// Source 为 true 时，额外发一行指回每条消息原出处的链接。
	Source bool `json:"source,omitempty"`
}

// miboxUser 是 MiBox 的 save 插件给一个账号存的设置。
type miboxUser struct {
	Target     string `json:"target"`
	ShowSource bool   `json:"showSource"`
}

// ConvertMiBox 把 MiBox 的 assets/prometheus/config.json（{"users":{"账号ID":{"target","showSource"}}}）
// 转成 data/save.json。
//
// 导入时还不知道本账号的 ID，所以从各账号的设置里挑一份：优先挑改过默认目标的，
// 其次是打开了来源说明的，都没有就按 ID 排序取第一份。MiBox 的默认目标 "me" 就是
// 收藏夹，转成空值。
func ConvertMiBox(raw []byte) ([]byte, error) {
	var config struct {
		Users map[string]miboxUser `json:"users"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(config.Users))
	for id := range config.Users {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var chosen miboxUser
	for _, pick := range []func(miboxUser) bool{
		func(user miboxUser) bool { return !isSelfTarget(user.Target) },
		func(user miboxUser) bool { return user.ShowSource },
		func(miboxUser) bool { return true },
	} {
		index := slices.IndexFunc(ids, func(id string) bool { return pick(config.Users[id]) })
		if index >= 0 {
			chosen = config.Users[ids[index]]
			break
		}
	}
	document := saveDocument{Source: chosen.ShowSource}
	if !isSelfTarget(chosen.Target) {
		document.Target = strings.TrimSpace(chosen.Target)
	}
	return json.MarshalIndent(document, "", " ")
}

const (
	// 范围是按消息编号逐个走的，每一条都可能是视频；超过几百条就是
	// 归档任务了，不该由一条命令来做。
	saveRangeLimit = 500
	// Telegram 自己对单个文件的上限。
	saveMaxBytes = 4 << 30
)

// messageLink 是一条 t.me 消息链接。
type messageLink struct {
	Username string // 公开对话
	ChatID   string // 私密对话，形如 -100…
	ID       int
	Raw      string
}

// linkPattern 识别 Telegram 给出的四种链接形式：公开和私密两类，
// 每类中间都可能带或不带论坛话题。
var linkPattern = regexp.MustCompile(`^(?:https?://)?(?:t|telegram)\.me/(?:c/(\d+)|([A-Za-z][A-Za-z0-9_]{3,31}))(?:/\d+)?/(\d+)(?:[/?#].*)?$`)

func parseLink(text string) (messageLink, bool) {
	match := linkPattern.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return messageLink{}, false
	}
	id, err := strconv.Atoi(match[3])
	if err != nil || id <= 0 {
		return messageLink{}, false
	}
	link := messageLink{ID: id, Raw: text}
	if match[1] != "" {
		link.ChatID = "-100" + match[1]
	} else {
		link.Username = match[2]
	}
	return link, true
}

// sameChat 判断两个链接是否指向同一个对话，范围要求这一点。
func (l messageLink) sameChat(other messageLink) bool {
	return l.ChatID == other.ChatID && strings.EqualFold(l.Username, other.Username)
}

// url 返回消息的 t.me 地址；和人的私聊没有这种地址，返回 ""。
func (l messageLink) url() string {
	if l.Username != "" {
		return "https://t.me/" + l.Username + "/" + strconv.Itoa(l.ID)
	}
	if channel, ok := strings.CutPrefix(l.ChatID, "-100"); ok {
		return "https://t.me/c/" + channel + "/" + strconv.Itoa(l.ID)
	}
	return ""
}

// saveRequest 描述参数要求做什么。
type saveRequest struct {
	Links  []messageLink
	Range  *[2]messageLink
	Target string // 为空时用保存的默认目标
}

// parseSaveArgs 把参数拆成链接、范围和可选的目标。目标就是那个唯一不是
// 链接的参数；这样的参数超过一个就是写错了，值得明确告诉用户。
func parseSaveArgs(args []string) (saveRequest, error) {
	var request saveRequest
	var others []string
	for _, argument := range args {
		if left, right, ok := strings.Cut(argument, "|"); ok {
			from, okFrom := parseLink(left)
			to, okTo := parseLink(right)
			if !okFrom || !okTo {
				return request, errors.New("范围两头都得是消息链接：链接1|链接2")
			}
			if !from.sameChat(to) {
				return request, errors.New("范围的两个链接必须在同一个对话里")
			}
			if from.ID > to.ID {
				from, to = to, from
			}
			request.Range = &[2]messageLink{from, to}
			continue
		}
		if link, ok := parseLink(argument); ok {
			request.Links = append(request.Links, link)
			continue
		}
		others = append(others, argument)
	}
	switch len(others) {
	case 0:
	case 1:
		request.Target = others[0]
	default:
		return request, fmt.Errorf("看不懂 %s：链接之外只能跟一个目标", strings.Join(others, " "))
	}
	if request.Range != nil && len(request.Links) > 0 {
		return request, errors.New("范围和单独的链接不能混在一起")
	}
	return request, nil
}

func isSelfTarget(target string) bool {
	switch strings.ToLower(strings.TrimSpace(target)) {
	case "", "me", "self", "收藏夹":
		return true
	}
	return false
}

func isLocalTarget(target string) bool { return strings.EqualFold(strings.TrimSpace(target), "local") }

func describeTarget(target string) string {
	switch {
	case isSelfTarget(target):
		return "收藏夹"
	case isLocalTarget(target):
		return "服务器本地 save/ 目录"
	}
	return target
}

// saver 承载一次命令要做的工作。
type saver struct {
	client  *bot.Client
	root    string
	partial string
	upload  *uploader.Uploader
	learned bool
	// status 是进度行的前半段（第几条/共几条），progress 在后面接上当前步骤。
	status   string
	progress func(string)
	// topic 不为 0 时，复制出的消息发到目标论坛的这个话题里。
	topic int
	// lastID 是最近一次存进目标对话的消息编号，读不出来时为 0；来源说明回复这一条。
	lastID int
	// usernames 记下这次命令里解析过的用户名，同一个用户名只查一次。
	usernames map[string]tg.InputPeerClass
}

// peerOf 找出链接指向的对话。账号启动以来还没收到过消息的私密对话，
// 不在 peer 缓存里，所以要读一次对话列表来认识它。
func (s *saver) peerOf(ctx context.Context, link messageLink) (tg.InputPeerClass, error) {
	if link.Username != "" {
		key := strings.ToLower(link.Username)
		if peer, ok := s.usernames[key]; ok {
			return peer, nil
		}
		peer, err := s.client.ResolveUsername(ctx, link.Username)
		if err != nil {
			return nil, err
		}
		if s.usernames == nil {
			s.usernames = map[string]tg.InputPeerClass{}
		}
		s.usernames[key] = peer
		return peer, nil
	}
	peer, err := s.client.InputPeerFromChatID(link.ChatID)
	if err == nil || !errors.Is(err, bot.ErrUnaddressablePeer) || s.learned {
		if err != nil {
			return nil, errors.New("找不到这个对话：账号得是它的成员")
		}
		return peer, nil
	}
	s.learned = true
	if err := learnDialogs(ctx, s.client); err != nil {
		return nil, err
	}
	if peer, err = s.client.InputPeerFromChatID(link.ChatID); err != nil {
		return nil, errors.New("找不到这个对话：账号得是它的成员")
	}
	return peer, nil
}

// targetOf 解析保存的消息要发往哪里。
func (s *saver) targetOf(ctx context.Context, target string) (tg.InputPeerClass, error) {
	if isSelfTarget(target) {
		return &tg.InputPeerSelf{}, nil
	}
	peer, err := s.client.ResolveTarget(ctx, target)
	if err == nil || !errors.Is(err, bot.ErrUnaddressablePeer) || s.learned {
		if err != nil {
			return nil, fmt.Errorf("找不到目标 %s", target)
		}
		return peer, nil
	}
	s.learned = true
	if err := learnDialogs(ctx, s.client); err != nil {
		return nil, err
	}
	if peer, err = s.client.ResolveTarget(ctx, target); err != nil {
		return nil, fmt.Errorf("找不到目标 %s", target)
	}
	return peer, nil
}

// learnDialogs 把对话列表读进 peer 缓存。
func learnDialogs(ctx context.Context, client *bot.Client) error {
	offsetDate, offsetID := 0, 0
	var offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
	for page := 0; page < 10; page++ {
		result, err := client.API().MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetDate: offsetDate, OffsetID: offsetID, OffsetPeer: offsetPeer, Limit: 100})
		if err != nil {
			return err
		}
		var dialogs []tg.DialogClass
		var messages []tg.MessageClass
		switch value := result.(type) {
		case *tg.MessagesDialogs:
			client.Peers().RememberUsers(value.Users)
			client.Peers().RememberChats(value.Chats)
			return nil
		case *tg.MessagesDialogsSlice:
			client.Peers().RememberUsers(value.Users)
			client.Peers().RememberChats(value.Chats)
			dialogs, messages = value.Dialogs, value.Messages
		default:
			return nil
		}
		if len(dialogs) < 100 {
			return nil
		}
		last, ok := dialogs[len(dialogs)-1].(*tg.Dialog)
		if !ok {
			return nil
		}
		offsetID = last.TopMessage
		for _, candidate := range messages {
			if message, ok := candidate.(*tg.Message); ok && message.ID == offsetID {
				offsetDate = message.Date
			}
		}
		if next, err := client.InputPeer(last.Peer); err == nil {
			offsetPeer = next
		}
	}
	return nil
}

// fetch 按 id 读取消息，已经不存在的跳过。
func (s *saver) fetch(ctx context.Context, peer tg.InputPeerClass, ids []int) ([]*tg.Message, error) {
	var found []*tg.Message
	for start := 0; start < len(ids); start += 100 {
		end := min(start+100, len(ids))
		batch, err := s.client.GetMessages(ctx, peer, ids[start:end])
		if err != nil {
			return nil, err
		}
		found = append(found, batch...)
	}
	return found, nil
}

// ErrNothingToCopy 表示消息里既没有文字也没有能重新发送的媒体。
var ErrNothingToCopy = errors.New("消息里没有能保存的内容")

// CopyMessage 把一条消息的内容重新发到 to，用于禁止转发的对话：文字连同格式，
// 媒体下载后重新上传。topic 不为 0 时发到目标论坛的那个话题里。root 是部署目录，
// 下载的媒体暂存在它下面的 save/.partial。消息没有可发的内容时返回 ErrNothingToCopy。
func CopyMessage(ctx context.Context, client *bot.Client, root string, message *tg.Message, to tg.InputPeerClass, topic int) error {
	work := &saver{client: client, root: root, partial: filepath.Join(root, "save", ".partial"),
		upload: uploader.NewUploader(client.API()).WithThreads(4), progress: func(string) {}, topic: topic}
	return work.copy(ctx, message, to)
}

// send 转发一条消息；对话禁止转发时改为复制。
//
// 带 noforwards 标记的消息不去尝试转发，直接复制：转发只会被拒。
func (s *saver) send(ctx context.Context, message *tg.Message, from, to tg.InputPeerClass) (copied bool, err error) {
	if !message.Noforwards {
		err := s.forward(ctx, []*tg.Message{message}, from, to)
		if err == nil {
			return false, nil
		}
		if !tgerr.Is(err, "CHAT_FORWARDS_RESTRICTED") {
			return false, err
		}
	}
	return true, s.copy(ctx, message, to)
}

// forward 用一次请求转发这些消息；相册一起转发，到了目标对话里仍是一个相册。
func (s *saver) forward(ctx context.Context, messages []*tg.Message, from, to tg.InputPeerClass) error {
	ids := make([]int, len(messages))
	randomIDs := make([]int64, len(messages))
	for index, message := range messages {
		ids[index], randomIDs[index] = message.ID, rand.Int64()
	}
	var updates tg.UpdatesClass
	err := onFlood(ctx, func() error {
		var err error
		updates, err = s.client.API().MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
			FromPeer: from, ID: ids, RandomID: randomIDs, ToPeer: to})
		return err
	})
	if err == nil {
		s.lastID = sentID(updates)
	}
	return err
}

// copy 从头把同样的内容重新发一遍：文字连同格式，媒体则重新下载再上传。
func (s *saver) copy(ctx context.Context, message *tg.Message, to tg.InputPeerClass) error {
	media, err := s.remake(ctx, message)
	if err != nil {
		return err
	}
	var updates tg.UpdatesClass
	if media == nil {
		if strings.TrimSpace(message.Message) == "" {
			return ErrNothingToCopy
		}
		request := &tg.MessagesSendMessageRequest{Peer: to, Message: message.Message, RandomID: rand.Int64()}
		if len(message.Entities) > 0 {
			request.SetEntities(message.Entities)
		}
		if s.topic > 0 {
			request.SetReplyTo(topicReply(s.topic))
		}
		err = onFlood(ctx, func() error {
			var err error
			updates, err = s.client.API().MessagesSendMessage(ctx, request)
			return err
		})
	} else {
		request := &tg.MessagesSendMediaRequest{Peer: to, Media: media, Message: message.Message, RandomID: rand.Int64()}
		if len(message.Entities) > 0 {
			request.SetEntities(message.Entities)
		}
		if s.topic > 0 {
			request.SetReplyTo(topicReply(s.topic))
		}
		err = onFlood(ctx, func() error {
			var err error
			updates, err = s.client.API().MessagesSendMedia(ctx, request)
			return err
		})
	}
	if err == nil {
		s.lastID = sentID(updates)
	}
	return err
}

// topicReply 让一条新消息进入论坛的某个话题。
func topicReply(topic int) *tg.InputReplyToMessage {
	reply := &tg.InputReplyToMessage{ReplyToMsgID: topic}
	reply.SetTopMsgID(topic)
	return reply
}

// sentID 从发送或转发的应答里读出新消息的编号；一次转发多条时取最大的那个。
// 读不出来返回 0。
func sentID(updates tg.UpdatesClass) int {
	var list []tg.UpdateClass
	switch value := updates.(type) {
	case *tg.UpdateShortSentMessage:
		return value.ID
	case *tg.UpdateShort:
		list = []tg.UpdateClass{value.Update}
	case *tg.Updates:
		list = value.Updates
	case *tg.UpdatesCombined:
		list = value.Updates
	}
	latest := 0
	for _, update := range list {
		var message tg.MessageClass
		switch value := update.(type) {
		case *tg.UpdateNewMessage:
			message = value.Message
		case *tg.UpdateNewChannelMessage:
			message = value.Message
		}
		if message != nil && message.GetID() > latest {
			latest = message.GetID()
		}
	}
	return latest
}

// albumRadius 是找相册其余部分时往前、往后各看的编号数。一个相册最多 10 条，
// 中间可能夹着别人同时发的消息，所以和 MiBox 一样各看 30 个。
const albumRadius = 30

// album 读出 message 所在相册的全部消息，按编号从小到大排。
func (s *saver) album(ctx context.Context, peer tg.InputPeerClass, message *tg.Message) ([]*tg.Message, error) {
	group, _ := message.GetGroupedID()
	ids := make([]int, 0, 2*albumRadius+1)
	for id := message.ID - albumRadius; id <= message.ID+albumRadius; id++ {
		if id > 0 {
			ids = append(ids, id)
		}
	}
	found, err := s.fetch(ctx, peer, ids)
	if err != nil {
		return nil, err
	}
	var album []*tg.Message
	for _, candidate := range found {
		if id, ok := candidate.GetGroupedID(); ok && id == group {
			album = append(album, candidate)
		}
	}
	if len(album) == 0 {
		return []*tg.Message{message}, nil
	}
	sort.Slice(album, func(a, b int) bool { return album[a].ID < album[b].ID })
	return album, nil
}

// remake 构造可以再次发送的媒体：图片和文件要重新上传，不带文件的类型
// 则照原值新建一份。纯文字或只有链接预览的消息返回 nil。
func (s *saver) remake(ctx context.Context, message *tg.Message) (tg.InputMediaClass, error) {
	media, ok := message.GetMedia()
	if !ok {
		return nil, nil
	}
	switch value := media.(type) {
	case *tg.MessageMediaWebPage, *tg.MessageMediaEmpty:
		return nil, nil
	case *tg.MessageMediaGeo:
		if point, ok := value.Geo.(*tg.GeoPoint); ok {
			return &tg.InputMediaGeoPoint{GeoPoint: &tg.InputGeoPoint{Lat: point.Lat, Long: point.Long}}, nil
		}
	case *tg.MessageMediaVenue:
		if point, ok := value.Geo.(*tg.GeoPoint); ok {
			return &tg.InputMediaVenue{GeoPoint: &tg.InputGeoPoint{Lat: point.Lat, Long: point.Long},
				Title: value.Title, Address: value.Address, Provider: value.Provider, VenueID: value.VenueID, VenueType: value.VenueType}, nil
		}
	case *tg.MessageMediaContact:
		return &tg.InputMediaContact{PhoneNumber: value.PhoneNumber, FirstName: value.FirstName, LastName: value.LastName, Vcard: value.Vcard}, nil
	case *tg.MessageMediaPoll:
		// 投票者看不到测验的正确答案，所以复制出来的是一个问题和选项
		// 都相同的普通投票。
		poll := tg.Poll{ID: rand.Int64(), Question: value.Poll.Question, Answers: value.Poll.Answers,
			MultipleChoice: value.Poll.MultipleChoice}
		return &tg.InputMediaPoll{Poll: poll}, nil
	case *tg.MessageMediaPhoto, *tg.MessageMediaDocument:
		source, ok := bot.SourceOf(message)
		if !ok {
			return nil, errors.New("这条消息的媒体已经不可用")
		}
		path, err := s.download(ctx, source)
		if err != nil {
			return nil, err
		}
		defer os.Remove(path)
		s.progress("⬆️ 上传中…")
		file, err := s.upload.FromPath(ctx, path)
		if err != nil {
			return nil, err
		}
		if source.Photo {
			return &tg.InputMediaUploadedPhoto{File: file}, nil
		}
		return &tg.InputMediaUploadedDocument{File: file, MimeType: source.MimeType, Attributes: source.Attributes}, nil
	}
	return nil, errors.New("这种消息在禁止转发的对话里没法复制")
}

// download 把媒体文件写到部署目录自己的 partial 目录，而不是 /tmp：
// 这类主机上 /tmp 是 tmpfs，视频放在那里就等于占着内存。
func (s *saver) download(ctx context.Context, source *bot.MediaSource) (string, error) {
	if source.Size > saveMaxBytes {
		return "", fmt.Errorf("文件 %s，超过 Telegram 的上限", kit.FormatBytes(int(source.Size)))
	}
	if err := os.MkdirAll(s.partial, 0o700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(s.partial, "part-*")
	if err != nil {
		return "", err
	}
	if source.Size > 0 {
		s.progress("⬇️ 下载中… " + kit.FormatBytes(int(source.Size)))
	} else {
		s.progress("⬇️ 下载中…")
	}
	if err := s.client.DownloadTo(ctx, source, file); err != nil {
		file.Close()
		os.Remove(file.Name())
		return "", err
	}
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}

// saveLocal 把消息的媒体写到 save/<chat>/ 下，旁边放一个 JSON 文件
// 记录来源。纯文字消息没有东西可写，直接跳过。
func (s *saver) saveLocal(ctx context.Context, message *tg.Message, link messageLink) (string, error) {
	source, ok := bot.SourceOf(message)
	if !ok {
		return "", nil
	}
	path, err := s.download(ctx, source)
	if err != nil {
		return "", err
	}
	chat := link.Username
	if chat == "" {
		chat = strings.TrimPrefix(link.ChatID, "-100")
	}
	if chat == "" {
		chat = "chat"
	}
	directory := filepath.Join(s.root, "save", sanitizeSegment(chat))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		os.Remove(path)
		return "", err
	}
	name := strconv.Itoa(message.ID) + localExtension(source)
	if source.FileName != "" {
		name = strconv.Itoa(message.ID) + "_" + sanitizeSegment(source.FileName)
	}
	final := uniquePath(filepath.Join(directory, name))
	if err := os.Rename(path, final); err != nil {
		os.Remove(path)
		return "", err
	}
	metadata, _ := json.MarshalIndent(map[string]any{
		"source": link.url(), "message_id": message.ID,
		"date": time.Unix(int64(message.Date), 0).UTC().Format(time.RFC3339),
		"text": message.Message, "mime_type": source.MimeType,
	}, "", "  ")
	if err := os.WriteFile(final+".json", metadata, 0o600); err != nil {
		return "", err
	}
	relative, _ := filepath.Rel(s.root, final)
	return relative, nil
}

var unsafeSegment = regexp.MustCompile(`[^\p{L}\p{N}._-]+`)

func sanitizeSegment(value string) string {
	value = strings.Trim(unsafeSegment.ReplaceAllString(value, "_"), "._")
	if value == "" {
		return "file"
	}
	if runes := []rune(value); len(runes) > 80 {
		value = string(runes[:80])
	}
	return value
}

// knownExtensions 把 Telegram 实际会发的类型的扩展名固定下来。系统的
// MIME 表给其中大多数类型列了好几个扩展名，顺序没有规律，比如
// video/mp4 查出来是 .m4v。
var knownExtensions = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif",
	"video/mp4": ".mp4", "video/webm": ".webm", "video/quicktime": ".mov",
	"audio/ogg": ".ogg", "audio/mpeg": ".mp3", "audio/mp4": ".m4a", "audio/x-m4a": ".m4a",
	"application/x-tgsticker": ".tgs", "application/pdf": ".pdf", "application/zip": ".zip",
}

func localExtension(source *bot.MediaSource) string {
	if source.Photo {
		return ".jpg"
	}
	if extension, ok := knownExtensions[source.MimeType]; ok {
		return extension
	}
	if extensions, _ := mime.ExtensionsByType(source.MimeType); len(extensions) > 0 {
		return extensions[0]
	}
	return ".bin"
}

func uniquePath(path string) string {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return path
	}
	extension := filepath.Ext(path)
	base := strings.TrimSuffix(path, extension)
	for index := 2; ; index++ {
		candidate := base + "_" + strconv.Itoa(index) + extension
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
}

// onFlood 只在遇到 flood wait 时重试一次调用。kit.RetryFlood 遇到任何错误
// 都会重试，这里不能用它：因为对话开启了内容保护而被拒的转发，每次都会
// 以同样的方式被拒，而这个拒绝正是改用复制的信号。
func onFlood(ctx context.Context, call func() error) error {
	err := call()
	if wait, ok := bot.FloodWait(err); ok && wait <= 5*time.Minute {
		if kit.Sleep(ctx, wait+time.Second) != nil {
			return ctx.Err()
		}
		return call()
	}
	return err
}

func saveHelp(prefix string) string {
	p := command.Escape(prefix)
	return "💾 <b>保存消息</b>\n\n转发消息；遇到禁止转发的对话，就下载后重新发一份，文字格式和媒体属性都保留。\n\n" +
		"<b>保存</b>\n" +
		"• 回复一条消息发 <code>" + p + "save</code>\n" +
		"• <code>" + p + "save 链接 [链接…]</code> 按链接批量保存；链接指向相册里的一条时，整个相册一起保存\n" +
		"• <code>" + p + "save 链接1|链接2</code> 保存两条之间的所有消息，缺号自动跳过，一次最多 " + strconv.Itoa(saveRangeLimit) + " 条\n" +
		"• 末尾加一个目标只这一次发到那里：<code>" + p + "save 链接 @某人</code>、<code>" + p + "save 链接 local</code>\n\n" +
		"<b>设置</b>\n" +
		"• <code>" + p + "save to 目标</code> 默认目标：<code>me</code>（收藏夹）、<code>@用户名</code>、对话 ID、<code>local</code>（存到服务器）\n" +
		"• <code>" + p + "save target</code> 看当前设置\n" +
		"• <code>" + p + "save source on|off</code> 保存后回复一条来源说明，带原消息链接\n\n" +
		"<b>链接</b>\n支持 <code>t.me/用户名/编号</code>、<code>t.me/c/数字/编号</code>，以及带话题的形式。私密对话要求账号是成员。\n\n" +
		"<b>本地模式</b>\n只存媒体，纯文本跳过。文件放在部署目录的 <code>save/对话/</code> 下，旁边有同名 <code>.json</code> 记录来源。"
}

// saveJob 是同一个对话里要保存的一批消息。
type saveJob struct {
	link     messageLink
	peer     tg.InputPeerClass
	messages []*tg.Message
	// album 为真时 messages 是链接所指消息所在的整个相册。
	album bool
}

// saveTally 是一次保存的结果。
type saveTally struct {
	saved, copied, skipped int
	failures, files        []string
	// sources 是保存成功的消息的出处，按保存的先后排列。
	sources []savedSource
}

// savedSource 是一条已保存消息的出处。
type savedSource struct {
	link  messageLink
	title string
}

func showSaveSettings(ctx context.Context, inv *command.Invocation, settings *store.Store[saveDocument]) error {
	current, err := settings.Read()
	if err != nil {
		return err
	}
	source := "关"
	if current.Source {
		source = "开"
	}
	return inv.Edit(ctx, "💾 <b>保存设置</b>\n\n默认目标："+command.Code(describeTarget(current.Target))+
		"\n来源链接："+command.Code(source)+"\n\n<i>"+command.Escape(inv.Prefix+"save to 目标")+" 修改默认目标</i>")
}

// saveSetting 处理 help、target、source、to 这几个设置类子命令。
// 第一个返回值表示参数是不是设置命令；不是的话，调用方把它当成保存请求。
func saveSetting(ctx context.Context, inv *command.Invocation, settings *store.Store[saveDocument]) (bool, error) {
	switch strings.ToLower(inv.Arg(0)) {
	case "help", "h":
		return true, inv.Edit(ctx, saveHelp(inv.Prefix))
	case "target", "config":
		return true, showSaveSettings(ctx, inv, settings)
	case "source":
		if choice := strings.ToLower(inv.Arg(1)); choice == "on" || choice == "off" {
			if err := settings.Update(func(value *saveDocument) error { value.Source = choice == "on"; return nil }); err != nil {
				return true, err
			}
		}
		return true, showSaveSettings(ctx, inv, settings)
	case "to":
		return true, setSaveTarget(ctx, inv, settings)
	}
	return false, nil
}

// setSaveTarget 修改默认目标。设之前先解析一次，免得存下一个根本发不过去的目标。
func setSaveTarget(ctx context.Context, inv *command.Invocation, settings *store.Store[saveDocument]) error {
	target := inv.Rest(1)
	if target == "" {
		return inv.EditText(ctx, "用法："+inv.Prefix+"save to me / @用户名 / 对话ID / local")
	}
	if !isSelfTarget(target) && !isLocalTarget(target) {
		check := &saver{client: inv.Client}
		if _, err := check.targetOf(ctx, target); err != nil {
			return inv.EditText(ctx, "❌ "+err.Error())
		}
	}
	if isSelfTarget(target) {
		target = ""
	}
	if err := settings.Update(func(value *saveDocument) error { value.Target = target; return nil }); err != nil {
		return err
	}
	return showSaveSettings(ctx, inv, settings)
}

// newSaver 准备一次保存要用的东西。
//
// 进度编辑每 3 秒最多一次：几百条的范围如果每条都改状态行，时间会耗在被限流上。
func newSaver(ctx context.Context, inv *command.Invocation, root string) *saver {
	work := &saver{client: inv.Client, root: root, partial: filepath.Join(root, "save", ".partial"),
		upload: uploader.NewUploader(inv.Client.API()).WithThreads(4)}
	var lastEdit time.Time
	work.progress = func(step string) {
		if time.Since(lastEdit) < 3*time.Second {
			return
		}
		lastEdit = time.Now()
		_ = inv.EditText(ctx, strings.TrimSpace(work.status+" "+step))
	}
	return work
}

// collectSaveJobs 找出要保存的消息：一个范围、若干链接，或者被回复的那一条。
// 给用户看的错误用 fail 包起来，由调用方原样显示。
func collectSaveJobs(ctx context.Context, inv *command.Invocation, work *saver, request saveRequest) ([]saveJob, error) {
	switch {
	case request.Range != nil:
		return collectSaveRange(ctx, inv, work, request.Range[0], request.Range[1])
	case len(request.Links) > 0:
		return collectSaveLinks(ctx, work, request.Links)
	}
	reply, err := inv.Client.GetReply(ctx, inv.Message)
	if err != nil || reply == nil || reply.Raw == nil {
		return nil, kit.Fail("读不到被回复的消息")
	}
	peer, err := inv.Client.InputPeer(reply.Peer)
	if err != nil {
		return nil, err
	}
	return []saveJob{{link: messageLink{ChatID: reply.ChatID, ID: reply.ID}, peer: peer, messages: []*tg.Message{reply.Raw}}}, nil
}

// collectSaveLinks 读出每个链接指向的消息。链接指向相册里的一条时，整个相册
// 都要保存，和 MiBox 一样；几个链接落在同一个相册里，只存一次。
func collectSaveLinks(ctx context.Context, work *saver, links []messageLink) ([]saveJob, error) {
	var jobs []saveJob
	albums := map[string]bool{}
	for _, link := range links {
		peer, err := work.peerOf(ctx, link)
		if err != nil {
			return nil, kit.Fail(link.Raw + "：" + err.Error())
		}
		messages, err := work.fetch(ctx, peer, []int{link.ID})
		if err != nil {
			return nil, err
		}
		if len(messages) == 0 {
			return nil, kit.Fail(link.Raw + " 这条消息不存在或已删除")
		}
		group, grouped := messages[0].GetGroupedID()
		if !grouped {
			jobs = append(jobs, saveJob{link: link, peer: peer, messages: messages})
			continue
		}
		key := bot.PeerID(messages[0].PeerID) + ":" + strconv.FormatInt(group, 10)
		if albums[key] {
			continue
		}
		albums[key] = true
		album, err := work.album(ctx, peer, messages[0])
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, saveJob{link: link, peer: peer, messages: album, album: true})
	}
	return jobs, nil
}

// collectSaveRange 按编号逐个读出范围内的消息，缺号跳过。
func collectSaveRange(ctx context.Context, inv *command.Invocation, work *saver, from, to messageLink) ([]saveJob, error) {
	count := to.ID - from.ID + 1
	if count > saveRangeLimit {
		return nil, kit.Failf("范围有 %d 条，一次最多 %d 条，分几段来", count, saveRangeLimit)
	}
	peer, err := work.peerOf(ctx, from)
	if err != nil {
		return nil, kit.Fail(err.Error())
	}
	ids := make([]int, 0, count)
	for id := from.ID; id <= to.ID; id++ {
		ids = append(ids, id)
	}
	if err := inv.EditText(ctx, "💾 正在读取 "+strconv.Itoa(count)+" 个编号…"); err != nil {
		return nil, err
	}
	messages, err := work.fetch(ctx, peer, ids)
	if err != nil {
		return nil, err
	}
	return []saveJob{{link: from, peer: peer, messages: messages}}, nil
}

func countMessages(jobs []saveJob) int {
	total := 0
	for _, job := range jobs {
		total += len(job.messages)
	}
	return total
}

// runSaveJobs 逐条保存。单条失败只记下来接着往下走；只有命令本身被取消才中止。
//
// 相册先用一次请求整个转发，到了目标对话里仍是一个相册；整体转发不成（比如
// 对话禁止转发），再逐条处理，逐条的结果和失败原因照常记下。
func runSaveJobs(ctx context.Context, work *saver, jobs []saveJob, destination tg.InputPeerClass, local bool) (saveTally, error) {
	var tally saveTally
	total, index := countMessages(jobs), 0
	for _, job := range jobs {
		if job.album && !local && forwardable(job.messages) {
			work.status = fmt.Sprintf("💾 %d/%d", index+1, total)
			work.progress("")
			err := work.forward(ctx, job.messages, job.peer, destination)
			if ctx.Err() != nil {
				return tally, ctx.Err()
			}
			if err == nil {
				index += len(job.messages)
				for _, message := range job.messages {
					tally.saved++
					tally.sources = append(tally.sources, work.sourceOf(job, message))
				}
				continue
			}
		}
		for _, message := range job.messages {
			index++
			work.status = fmt.Sprintf("💾 %d/%d", index, total)
			work.progress("")
			if local {
				link := messageLink{Username: job.link.Username, ChatID: job.link.ChatID, ID: message.ID}
				path, err := work.saveLocal(ctx, message, link)
				switch {
				case err != nil:
					tally.failures = append(tally.failures, "#"+strconv.Itoa(message.ID)+"："+err.Error())
				case path == "":
					tally.skipped++
				default:
					tally.saved++
					tally.files = append(tally.files, path)
				}
				continue
			}
			copied, err := work.send(ctx, message, job.peer, destination)
			if err != nil {
				if ctx.Err() != nil {
					return tally, ctx.Err()
				}
				tally.failures = append(tally.failures, "#"+strconv.Itoa(message.ID)+"："+command.Brief(err))
				continue
			}
			tally.saved++
			tally.sources = append(tally.sources, work.sourceOf(job, message))
			if copied {
				tally.copied++
			}
		}
	}
	return tally, nil
}

// forwardable 判断这些消息能不能直接转发：带禁止转发标记的一条都不行。
func forwardable(messages []*tg.Message) bool {
	for _, message := range messages {
		if message.Noforwards {
			return false
		}
	}
	return true
}

// sourceOf 记下一条已保存消息的出处：链接的形式跟着命令里给的链接走，
// 对话名取自 peer 缓存。
func (s *saver) sourceOf(job saveJob, message *tg.Message) savedSource {
	return savedSource{
		link:  messageLink{Username: job.link.Username, ChatID: job.link.ChatID, ID: message.ID},
		title: s.client.Peers().Title(message.PeerID),
	}
}

// sendSourceNotice 在目标对话里回复最后存进去的那条消息，说明这些内容的出处。
// 读不出那条消息的编号时不回复，直接发。
func sendSourceNotice(ctx context.Context, work *saver, sources []savedSource, isRange bool, destination tg.InputPeerClass) {
	text := sourceNotice(sources, isRange)
	if text == "" {
		return
	}
	_, _ = work.client.SendHTML(ctx, destination, text, bot.SendOptions{ReplyTo: work.lastID})
}

// sourceNotice 写出来源说明，和 MiBox 的三种格式对应：范围给出第一条和最后一条；
// 只存了一条时给出原消息链接、对话名和编号；其余按对话分组，连续的编号合成一段，
// 内容太长时折叠起来。和人私聊的消息没有 t.me 地址，不列出；一条都列不出时返回 ""。
func sourceNotice(sources []savedSource, isRange bool) string {
	var linked []savedSource
	for _, source := range sources {
		if source.link.url() != "" {
			linked = append(linked, source)
		}
	}
	if len(linked) == 0 {
		return ""
	}
	if isRange {
		first, last := linked[0], linked[len(linked)-1]
		return "🔗 <b>范围保存来源</b>\n\n" +
			"▶️ <b>起始消息</b>\n• " + command.Bold(first.title) + " / " + sourceAnchor(first.link) + "\n\n" +
			"⏹ <b>结尾消息</b>\n• " + command.Bold(last.title) + " / " + sourceAnchor(last.link)
	}
	if len(linked) == 1 {
		source := linked[0]
		return "🔗 <b>消息来源</b>\n\n" +
			`📝 <a href="` + command.Escape(source.link.url()) + `">查看原消息</a>` + "\n" +
			"👤 来源对话：" + command.Bold(source.title) + "\n" +
			"#️⃣ 消息 ID：" + command.Code(strconv.Itoa(source.link.ID))
	}
	type chatGroup struct {
		key, title string
		link       messageLink
		ids        []int
	}
	var groups []*chatGroup
	byKey := map[string]*chatGroup{}
	for _, source := range linked {
		key := source.link.ChatID + "@" + strings.ToLower(source.link.Username)
		group, ok := byKey[key]
		if !ok {
			group = &chatGroup{key: key, title: source.title, link: source.link}
			byKey[key] = group
			groups = append(groups, group)
		}
		group.ids = append(group.ids, source.link.ID)
	}
	sort.SliceStable(groups, func(a, b int) bool {
		if groups[a].title != groups[b].title {
			return groups[a].title < groups[b].title
		}
		return groups[a].key < groups[b].key
	})
	sections := make([]string, 0, len(groups))
	for _, group := range groups {
		spans := idSpans(group.ids)
		parts := make([]string, 0, len(spans))
		count := 0
		for _, span := range spans {
			start, end := group.link, group.link
			start.ID, end.ID = span[0], span[1]
			count += span[1] - span[0] + 1
			if span[0] == span[1] {
				parts = append(parts, sourceAnchor(start))
			} else {
				parts = append(parts, sourceAnchor(start)+"-"+sourceAnchor(end))
			}
		}
		sections = append(sections, "👤 "+command.Bold(group.title)+"（"+strconv.Itoa(count)+" 条）："+strings.Join(parts, ", "))
	}
	body := strings.Join(sections, "\n")
	if len([]rune(body)) > 350 || len(sections) > 6 {
		body = "<blockquote expandable>" + body + "</blockquote>"
	}
	return "🔗 <b>批量保存来源</b>\n\n" + body
}

// sourceAnchor 把消息编号写成指向原消息的链接。
func sourceAnchor(link messageLink) string {
	return `<a href="` + command.Escape(link.url()) + `">` + strconv.Itoa(link.ID) + `</a>`
}

// idSpans 把编号去重排序，连续的合成 [起, 止] 一段。
func idSpans(ids []int) [][2]int {
	sorted := append([]int(nil), ids...)
	sort.Ints(sorted)
	var spans [][2]int
	for _, id := range sorted {
		last := len(spans) - 1
		if last >= 0 && id <= spans[last][1]+1 {
			spans[last][1] = max(spans[last][1], id)
			continue
		}
		spans = append(spans, [2]int{id, id})
	}
	return spans
}

// renderSaveResult 把结果写成给人看的一段话，失败最多列前 5 条。
func renderSaveResult(tally saveTally, target, root string) string {
	lines := []string{fmt.Sprintf("✅ 已保存 %d 条到%s", tally.saved, describeTarget(target))}
	if tally.copied > 0 {
		lines = append(lines, fmt.Sprintf("其中 %d 条来自禁止转发的对话，已重新上传", tally.copied))
	}
	if tally.skipped > 0 {
		lines = append(lines, fmt.Sprintf("跳过 %d 条纯文本（本地模式只存媒体）", tally.skipped))
	}
	if len(tally.files) > 0 {
		lines = append(lines, "文件在 "+filepath.Join(root, "save")+"/")
	}
	if len(tally.failures) > 0 {
		if tally.saved == 0 {
			lines[0] = "❌ 没有保存成功"
		}
		shown := tally.failures
		if len(shown) > 5 {
			shown = shown[:5]
		}
		lines = append(lines, fmt.Sprintf("失败 %d 条：", len(tally.failures)))
		lines = append(lines, shown...)
	}
	return strings.Join(lines, "\n")
}

// saveHandle 处理一次 .save：先看是不是设置类子命令，否则收集要存的消息、逐条保存、汇报结果。
func saveHandle(ctx context.Context, inv *command.Invocation, settings *store.Store[saveDocument], root string) error {
	if handled, err := saveSetting(ctx, inv, settings); handled {
		return err
	}
	request, err := parseSaveArgs(inv.Args)
	if err != nil {
		return inv.EditText(ctx, "❌ "+err.Error())
	}
	if request.Range == nil && len(request.Links) == 0 && inv.Message.ReplyToID == 0 {
		return inv.Edit(ctx, saveHelp(inv.Prefix))
	}
	current, err := settings.Read()
	if err != nil {
		return err
	}
	target := current.Target
	if request.Target != "" {
		target = request.Target
	}

	work := newSaver(ctx, inv, root)
	jobs, err := collectSaveJobs(ctx, inv, work, request)
	if detail, ok := kit.IsUserError(err); ok {
		return inv.EditText(ctx, "❌ "+detail)
	}
	if err != nil {
		return err
	}
	total := countMessages(jobs)
	if total == 0 {
		return inv.EditText(ctx, "❌ 范围内没有消息")
	}
	local := isLocalTarget(target)
	var destination tg.InputPeerClass
	if !local {
		if destination, err = work.targetOf(ctx, target); err != nil {
			return inv.EditText(ctx, "❌ "+err.Error())
		}
	}
	tally, err := runSaveJobs(ctx, work, jobs, destination, local)
	if err != nil {
		return err
	}
	if current.Source && !local {
		sendSourceNotice(ctx, work, tally.sources, request.Range != nil, destination)
	}
	return inv.EditText(ctx, renderSaveResult(tally, target, root))
}

// Register 注册 .save。
func Register(a *app.App) {
	settings := kit.NewStore(a, "save.json", func() saveDocument { return saveDocument{} })
	a.Registry.Register(&command.Command{
		Name: "save", Description: "保存或转发消息，突破禁止转发", Usage: "[链接…|链接1|链接2] [目标]",
		Help: saveHelp, Timeout: time.Hour,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			return saveHandle(ctx, inv, settings, a.Root)
		},
	})
}
