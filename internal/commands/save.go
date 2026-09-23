package commands

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
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
)

// saveDocument is data/save.json.
type saveDocument struct {
	// Target is where saved messages go: "" for Saved Messages, "local"
	// for disk, otherwise a username or chat id.
	Target string `json:"target,omitempty"`
	// Source adds a line pointing back at where each message came from.
	Source bool `json:"source,omitempty"`
}

const (
	// A range walks message ids one by one and every one of them may be a
	// video; past a few hundred this is an archive job, not a command.
	saveRangeLimit = 500
	// Telegram's own ceiling for a file.
	saveMaxBytes = 4 << 30
)

// messageLink is one t.me message link.
type messageLink struct {
	Username string // public chats
	ChatID   string // private ones, as -100…
	ID       int
	Raw      string
}

// linkPattern reads the four shapes Telegram hands out: public and
// private, each with or without a forum topic in the middle.
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

// sameChat reports whether two links point into one chat, which a range
// needs.
func (l messageLink) sameChat(other messageLink) bool {
	return l.ChatID == other.ChatID && strings.EqualFold(l.Username, other.Username)
}

// url is the message's t.me address, or "" for a private chat with a
// person, which has none.
func (l messageLink) url() string {
	if l.Username != "" {
		return "https://t.me/" + l.Username + "/" + strconv.Itoa(l.ID)
	}
	if channel, ok := strings.CutPrefix(l.ChatID, "-100"); ok {
		return "https://t.me/c/" + channel + "/" + strconv.Itoa(l.ID)
	}
	return ""
}

// saveRequest is what the arguments asked for.
type saveRequest struct {
	Links  []messageLink
	Range  *[2]messageLink
	Target string // empty: the saved default
}

// parseSaveArgs splits the arguments into links, a range and an optional
// target. A target is the one argument that is not a link; more than one
// is a mistake worth saying so about.
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

// saver carries one command's work.
type saver struct {
	client   *bot.Client
	root     string
	partial  string
	upload   *uploader.Uploader
	learned  bool
	progress func(string)
}

// peerOf finds the chat a link points into. A private chat the account
// has not heard from since it started is not in the peer cache yet, so
// the dialogs are read once to learn it.
func (s *saver) peerOf(ctx context.Context, link messageLink) (tg.InputPeerClass, error) {
	if link.Username != "" {
		return s.client.ResolveUsername(ctx, link.Username)
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

// targetOf resolves where saved messages go.
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

// learnDialogs reads the dialog list into the peer cache.
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

// fetch reads messages by id, skipping the ones that are gone.
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

// send forwards a message, or copies it when the chat forbids forwarding.
//
// A message that says noforwards is copied without trying: the forward
// would only come back refused.
func (s *saver) send(ctx context.Context, message *tg.Message, from, to tg.InputPeerClass) (copied bool, err error) {
	if !message.Noforwards {
		err := onFlood(ctx, func() error {
			_, err := s.client.API().MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
				FromPeer: from, ID: []int{message.ID}, RandomID: []int64{rand.Int64()}, ToPeer: to})
			return err
		})
		if err == nil {
			return false, nil
		}
		if !tgerr.Is(err, "CHAT_FORWARDS_RESTRICTED") {
			return false, err
		}
	}
	return true, s.copy(ctx, message, to)
}

// copy sends the same content again from scratch: the text with its
// formatting, and any media downloaded and uploaded anew.
func (s *saver) copy(ctx context.Context, message *tg.Message, to tg.InputPeerClass) error {
	media, err := s.remake(ctx, message)
	if err != nil {
		return err
	}
	if media == nil {
		if strings.TrimSpace(message.Message) == "" {
			return errors.New("消息里没有能保存的内容")
		}
		request := &tg.MessagesSendMessageRequest{Peer: to, Message: message.Message, RandomID: rand.Int64()}
		if len(message.Entities) > 0 {
			request.SetEntities(message.Entities)
		}
		return onFlood(ctx, func() error { _, err := s.client.API().MessagesSendMessage(ctx, request); return err })
	}
	request := &tg.MessagesSendMediaRequest{Peer: to, Media: media, Message: message.Message, RandomID: rand.Int64()}
	if len(message.Entities) > 0 {
		request.SetEntities(message.Entities)
	}
	return onFlood(ctx, func() error { _, err := s.client.API().MessagesSendMedia(ctx, request); return err })
}

// remake builds media that can be sent again: an upload for photos and
// documents, a fresh copy of the value for the kinds that carry no file.
// It returns nil for a message that is only text, or only a link preview.
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
		// A quiz's right answer is not visible to a voter, so the copy is
		// an ordinary poll with the same question and options.
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

// download writes a media file into the deployment's own partial
// directory rather than /tmp: on this kind of host /tmp is a tmpfs, and a
// video parked there is a video held in memory.
func (s *saver) download(ctx context.Context, source *bot.MediaSource) (string, error) {
	if source.Size > saveMaxBytes {
		return "", fmt.Errorf("文件 %s，超过 Telegram 的上限", formatBytes(int(source.Size)))
	}
	if err := os.MkdirAll(s.partial, 0o700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(s.partial, "part-*")
	if err != nil {
		return "", err
	}
	if source.Size > 0 {
		s.progress("⬇️ 下载中… " + formatBytes(int(source.Size)))
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

// saveLocal writes a message's media under save/<chat>/ with a JSON file
// beside it saying where it came from. Text-only messages have nothing to
// write and are skipped.
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

// knownExtensions pins the types Telegram actually sends. The system MIME
// table lists several extensions for most of them in no useful order —
// video/mp4 came back as .m4v.
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

// onFlood retries a call once, and only after a flood wait. retryFlood
// retries any error, which is wrong here: a forward refused because the
// chat protects its content is refused the same way every time, and that
// refusal is the signal to switch to copying.
func onFlood(ctx context.Context, call func() error) error {
	err := call()
	if wait, ok := bot.FloodWait(err); ok && wait <= 5*time.Minute {
		if sleepCtx(ctx, wait+time.Second) != nil {
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
		"• <code>" + p + "save 链接 [链接…]</code> 按链接批量保存\n" +
		"• <code>" + p + "save 链接1|链接2</code> 保存两条之间的所有消息，缺号自动跳过，一次最多 " + strconv.Itoa(saveRangeLimit) + " 条\n" +
		"• 末尾加一个目标只这一次发到那里：<code>" + p + "save 链接 @某人</code>、<code>" + p + "save 链接 local</code>\n\n" +
		"<b>设置</b>\n" +
		"• <code>" + p + "save to 目标</code> 默认目标：<code>me</code>（收藏夹）、<code>@用户名</code>、对话 ID、<code>local</code>（存到服务器）\n" +
		"• <code>" + p + "save target</code> 看当前设置\n" +
		"• <code>" + p + "save source on|off</code> 在保存的内容后附上来源链接\n\n" +
		"<b>链接</b>\n支持 <code>t.me/用户名/编号</code>、<code>t.me/c/数字/编号</code>，以及带话题的形式。私密对话要求账号是成员。\n\n" +
		"<b>本地模式</b>\n只存媒体，纯文本跳过。文件放在部署目录的 <code>save/对话/</code> 下，旁边有同名 <code>.json</code> 记录来源。"
}

// Save registers .save.
func Save(a *app.App) {
	settings := newStore(a, "save.json", func() saveDocument { return saveDocument{} })
	showSettings := func(ctx context.Context, inv *command.Invocation) error {
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

	handle := func(ctx context.Context, inv *command.Invocation) error {
		switch strings.ToLower(inv.Arg(0)) {
		case "help", "h":
			return inv.Edit(ctx, saveHelp(inv.Prefix))
		case "target", "config":
			return showSettings(ctx, inv)
		case "source":
			switch strings.ToLower(inv.Arg(1)) {
			case "on", "off":
				on := strings.EqualFold(inv.Arg(1), "on")
				if err := settings.Update(func(value *saveDocument) error { value.Source = on; return nil }); err != nil {
					return err
				}
			}
			return showSettings(ctx, inv)
		case "to":
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
			return showSettings(ctx, inv)
		}

		request, err := parseSaveArgs(inv.Args)
		if err != nil {
			return inv.EditText(ctx, "❌ "+err.Error())
		}
		current, err := settings.Read()
		if err != nil {
			return err
		}
		target := current.Target
		if request.Target != "" {
			target = request.Target
		}

		lastEdit := time.Time{}
		work := &saver{client: inv.Client, root: a.Root, partial: filepath.Join(a.Root, "save", ".partial"),
			upload: uploader.NewUploader(inv.Client.API()).WithThreads(4)}
		status := ""
		work.progress = func(step string) {
			// Edits are throttled: a range of a few hundred would
			// otherwise spend its time rate-limited on the status line.
			if time.Since(lastEdit) < 3*time.Second {
				return
			}
			lastEdit = time.Now()
			_ = inv.EditText(ctx, strings.TrimSpace(status+" "+step))
		}

		// What to save, as (chat, messages) pairs.
		type job struct {
			link     messageLink
			peer     tg.InputPeerClass
			messages []*tg.Message
		}
		var jobs []job
		switch {
		case request.Range != nil:
			from, to := request.Range[0], request.Range[1]
			if to.ID-from.ID+1 > saveRangeLimit {
				return inv.EditText(ctx, fmt.Sprintf("❌ 范围有 %d 条，一次最多 %d 条，分几段来", to.ID-from.ID+1, saveRangeLimit))
			}
			peer, err := work.peerOf(ctx, from)
			if err != nil {
				return inv.EditText(ctx, "❌ "+err.Error())
			}
			ids := make([]int, 0, to.ID-from.ID+1)
			for id := from.ID; id <= to.ID; id++ {
				ids = append(ids, id)
			}
			if err := inv.EditText(ctx, "💾 正在读取 "+strconv.Itoa(len(ids))+" 个编号…"); err != nil {
				return err
			}
			messages, err := work.fetch(ctx, peer, ids)
			if err != nil {
				return err
			}
			jobs = append(jobs, job{link: from, peer: peer, messages: messages})
		case len(request.Links) > 0:
			for _, link := range request.Links {
				peer, err := work.peerOf(ctx, link)
				if err != nil {
					return inv.EditText(ctx, "❌ "+link.Raw+"："+err.Error())
				}
				messages, err := work.fetch(ctx, peer, []int{link.ID})
				if err != nil {
					return err
				}
				if len(messages) == 0 {
					return inv.EditText(ctx, "❌ "+link.Raw+" 这条消息不存在或已删除")
				}
				jobs = append(jobs, job{link: link, peer: peer, messages: messages})
			}
		default:
			if inv.Message.ReplyToID == 0 {
				return inv.Edit(ctx, saveHelp(inv.Prefix))
			}
			reply, err := inv.Client.GetReply(ctx, inv.Message)
			if err != nil || reply == nil || reply.Raw == nil {
				return inv.EditText(ctx, "❌ 读不到被回复的消息")
			}
			peer, err := inv.Client.InputPeer(reply.Peer)
			if err != nil {
				return err
			}
			link := messageLink{ChatID: reply.ChatID, ID: reply.ID}
			jobs = append(jobs, job{link: link, peer: peer, messages: []*tg.Message{reply.Raw}})
		}

		total := 0
		for _, item := range jobs {
			total += len(item.messages)
		}
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

		saved, copied, skipped := 0, 0, 0
		var failures []string
		var files []string
		index := 0
		for _, item := range jobs {
			for _, message := range item.messages {
				index++
				status = fmt.Sprintf("💾 %d/%d", index, total)
				work.progress("")
				link := messageLink{Username: item.link.Username, ChatID: item.link.ChatID, ID: message.ID}
				if local {
					path, err := work.saveLocal(ctx, message, link)
					switch {
					case err != nil:
						failures = append(failures, "#"+strconv.Itoa(message.ID)+"："+err.Error())
					case path == "":
						skipped++
					default:
						saved++
						files = append(files, path)
					}
					continue
				}
				wasCopied, err := work.send(ctx, message, item.peer, destination)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					failures = append(failures, "#"+strconv.Itoa(message.ID)+"："+command.Brief(err))
					continue
				}
				saved++
				if wasCopied {
					copied++
				}
			}
		}

		if current.Source && !local && saved > 0 && jobs[0].link.url() != "" {
			first := jobs[0].link
			if len(jobs) == 1 && len(jobs[0].messages) > 0 {
				first.ID = jobs[0].messages[0].ID
			}
			line := "📎 来源：" + first.url()
			if total > 1 {
				line += fmt.Sprintf(" 等 %d 条", total)
			}
			_, _ = inv.Client.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer: destination, Message: line, RandomID: rand.Int64(), NoWebpage: true})
		}

		lines := []string{fmt.Sprintf("✅ 已保存 %d 条到%s", saved, describeTarget(target))}
		if copied > 0 {
			lines = append(lines, fmt.Sprintf("其中 %d 条来自禁止转发的对话，已重新上传", copied))
		}
		if skipped > 0 {
			lines = append(lines, fmt.Sprintf("跳过 %d 条纯文本（本地模式只存媒体）", skipped))
		}
		if len(files) > 0 {
			lines = append(lines, "文件在 "+filepath.Join(a.Root, "save")+"/")
		}
		if len(failures) > 0 {
			if saved == 0 {
				lines[0] = "❌ 没有保存成功"
			}
			shown := failures
			if len(shown) > 5 {
				shown = shown[:5]
			}
			lines = append(lines, fmt.Sprintf("失败 %d 条：", len(failures)))
			lines = append(lines, shown...)
		}
		return inv.EditText(ctx, strings.Join(lines, "\n"))
	}
	a.Registry.Register(&command.Command{
		Name: "save", Description: "保存或转发消息，突破禁止转发", Usage: "[链接…|链接1|链接2] [目标]",
		Help: saveHelp, Timeout: time.Hour, Handle: handle,
	})
}
