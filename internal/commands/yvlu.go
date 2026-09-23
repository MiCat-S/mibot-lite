package commands

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"image/color"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
	"github.com/MiCat-S/mibot-lite/internal/imaging"
	"github.com/MiCat-S/mibot-lite/internal/media"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// 语录图片本身由远程服务渲染：本命令只负责收集谁说了什么、用的哪个头像、
// 带什么格式，以 JSON 提交过去，再把返回的图片原样发回。文字排版、字体栈、
// emoji 渲染都不在本进程里做。
const quoteEndpoint = "https://quote-api-enhanced.zhetengsha.eu.org/generate.webp"

// quoteUserAgent 不是摆设：该服务前面有一层过滤，凡是它不认识的请求，
// 都会得到 403 和一个质询页。原插件发的就是这个字符串，现在也仍然能通过，
// 所以它是协议的一部分，而不是出于礼貌才带上的。
const quoteUserAgent = "TeleBox/0.2.1"

type yvluConfig struct {
	StickerSet string `json:"stickerSetShortName"`
}

// yvluOptions 是解析好的一次调用。
type yvluOptions struct {
	Count        int
	IncludeReply bool
	// Format 用的是远程服务自己的取值："quote" 渲染成透明贴纸，
	// "image" 是带背景的卡片，"stories" 是 9:16 的卡片。
	Format     string
	FakeText   string
	FakeEnts   []tg.MessageEntityClass
	FakeSender tg.InputPeerClass
}

var stickerSetName = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

func yvluHelp(prefix string) string {
	p := command.Escape(prefix)
	return "📝 <b>生成文字语录贴纸</b>\n\n• <code>" + p + "yvlu [消息数]</code> 回复消息生成语录，最多 5 条\n• <code>" + p +
		"yvlu r [消息数]</code> 包含被引用的内容\n• <code>" + p + "yvlu f 文本</code> 伪造文本（<code>fr</code> 同时包含回复）\n• <code>" + p +
		"yvlu u 用户ID/用户名 [消息数]</code> 伪造发送者（<code>ur</code> 同时包含回复）\n• <code>" + p +
		"yvlu webp|image|png|stories [消息数]</code> 指定输出格式\n• <code>" + p + "yvlu s</code> 保存回复的贴纸或图片到贴纸包\n• <code>" + p +
		"yvlu config</code> 查看配置\n• <code>" + p + "yvlu config sticker 名称</code> 设置贴纸包\n\n图片由远程 quote 服务渲染，需要网络可达。"
}

// parseYvlu 解析参数。参数按位置排列，还有点不规整，
// 因为这是原插件的用户已经用惯的写法。
func parseYvlu(inv *command.Invocation) (*yvluOptions, bool) {
	args := inv.Args
	options := &yvluOptions{Count: 1, Format: "quote"}
	isFormat := func(value string) bool {
		switch value {
		case "webp", "image", "png", "stories":
			return true
		}
		return false
	}
	format := func(value string) string {
		switch value {
		case "png", "image":
			return "image"
		case "stories":
			return "stories"
		}
		return "quote"
	}
	count := func(value string) int {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			return 1
		}
		return parsed
	}
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch {
	case sub == "" || isDigits(sub):
		options.Count = count(sub)
	case sub == "r":
		options.IncludeReply = true
		if len(args) > 1 && isFormat(args[1]) {
			options.Format = format(args[1])
			options.Count = count(inv.Arg(2))
		} else {
			options.Count = count(inv.Arg(1))
		}
	case (sub == "u" || sub == "ur") && len(args) > 1:
		options.IncludeReply = sub == "ur"
		options.Count = count(inv.Arg(2))
	case (sub == "f" || sub == "fr") && len(args) > 1:
		options.IncludeReply = sub == "fr"
		// 伪造的文本从原始消息里取，这样它自带的格式 entity 能保留
		// 下来，只是平移到新的偏移位置。
		marker := regexp.MustCompile(`^\S+\s+` + sub + `\s+`)
		match := marker.FindString(inv.Text)
		if match == "" {
			return nil, false
		}
		offset := utf16Len(match)
		options.FakeText = string([]rune(inv.Text)[len([]rune(match)):])
		// 解析函数拿到的是一次调用而不是一条消息：背后没有协议层
		// 消息的调用方，文本照样能被解析。
		if inv.Message != nil && inv.Message.Raw != nil {
			if entities, ok := inv.Message.Raw.GetEntities(); ok {
				options.FakeEnts = shiftEntities(entities, offset)
			}
		}
	case isFormat(sub):
		options.Format = format(sub)
		options.Count = count(inv.Arg(1))
	default:
		return nil, false
	}
	return options, true
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// shiftEntities 丢掉完全落在 offset 之前的格式实体，其余原样保留。
// 真正按 offset 平移和截断跨界实体，是后面 convertEntities 做的。
func shiftEntities(entities []tg.MessageEntityClass, offset int) []tg.MessageEntityClass {
	var kept []tg.MessageEntityClass
	for _, entity := range entities {
		start, length := entityRange(entity)
		if start+length <= offset {
			continue
		}
		kept = append(kept, entity)
	}
	return kept
}

func entityRange(entity tg.MessageEntityClass) (offset, length int) {
	switch value := entity.(type) {
	case *tg.MessageEntityBold:
		return value.Offset, value.Length
	case *tg.MessageEntityItalic:
		return value.Offset, value.Length
	case *tg.MessageEntityCode:
		return value.Offset, value.Length
	case *tg.MessageEntityPre:
		return value.Offset, value.Length
	case *tg.MessageEntityTextURL:
		return value.Offset, value.Length
	case *tg.MessageEntityURL:
		return value.Offset, value.Length
	case *tg.MessageEntityUnderline:
		return value.Offset, value.Length
	case *tg.MessageEntityStrike:
		return value.Offset, value.Length
	case *tg.MessageEntitySpoiler:
		return value.Offset, value.Length
	case *tg.MessageEntityBlockquote:
		return value.Offset, value.Length
	case *tg.MessageEntityCustomEmoji:
		return value.Offset, value.Length
	case *tg.MessageEntityMention:
		return value.Offset, value.Length
	case *tg.MessageEntityMentionName:
		return value.Offset, value.Length
	}
	return 0, 0
}

// quoteEntity 是远程服务使用的 entity 结构，沿用 Bot API 的格式而不是 TL。
type quoteEntity struct {
	Offset        int    `json:"offset"`
	Length        int    `json:"length"`
	Type          string `json:"type,omitempty"`
	URL           string `json:"url,omitempty"`
	Language      string `json:"language,omitempty"`
	CustomEmojiID string `json:"custom_emoji_id,omitempty"`
	User          *struct {
		ID int64 `json:"id"`
	} `json:"user,omitempty"`
}

func convertEntities(entities []tg.MessageEntityClass, shift int) []quoteEntity {
	converted := make([]quoteEntity, 0, len(entities))
	for _, entity := range entities {
		offset, length := entityRange(entity)
		offset -= shift
		if length <= 0 {
			continue
		}
		if offset < 0 {
			length += offset
			offset = 0
			if length <= 0 {
				continue
			}
		}
		item := quoteEntity{Offset: offset, Length: length}
		switch value := entity.(type) {
		case *tg.MessageEntityBold:
			item.Type = "bold"
		case *tg.MessageEntityItalic:
			item.Type = "italic"
		case *tg.MessageEntityUnderline:
			item.Type = "underline"
		case *tg.MessageEntityStrike:
			item.Type = "strikethrough"
		case *tg.MessageEntityCode:
			item.Type = "code"
		case *tg.MessageEntityPre:
			item.Type, item.Language = "pre", value.Language
		case *tg.MessageEntitySpoiler:
			item.Type = "spoiler"
		case *tg.MessageEntityBlockquote:
			item.Type = "blockquote"
		case *tg.MessageEntityURL:
			item.Type = "url"
		case *tg.MessageEntityTextURL:
			item.Type, item.URL = "text_link", value.URL
		case *tg.MessageEntityMention:
			item.Type = "mention"
		case *tg.MessageEntityMentionName:
			item.Type = "text_mention"
			item.User = &struct {
				ID int64 `json:"id"`
			}{ID: value.UserID}
		case *tg.MessageEntityCustomEmoji:
			item.Type, item.CustomEmojiID = "custom_emoji", strconv.FormatInt(value.DocumentID, 10)
		case *tg.MessageEntityHashtag:
			item.Type = "hashtag"
		case *tg.MessageEntityCashtag:
			item.Type = "cashtag"
		case *tg.MessageEntityBotCommand:
			item.Type = "bot_command"
		case *tg.MessageEntityEmail:
			item.Type = "email"
		case *tg.MessageEntityPhone:
			item.Type = "phone_number"
		default:
			continue
		}
		converted = append(converted, item)
	}
	return converted
}

// quoteFrom 是一条被引用消息的作者信息块。
type quoteFrom struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name,omitempty"`
	FirstName string     `json:"first_name,omitempty"`
	LastName  string     `json:"last_name,omitempty"`
	Username  string     `json:"username,omitempty"`
	Photo     *quotePhot `json:"photo,omitempty"`
	// EmojiStatus 是作者挂在名字旁边的自定义表情。
	EmojiStatus string `json:"emoji_status,omitempty"`
}

type quotePhot struct {
	URL string `json:"url"`
}

type quoteReply struct {
	Name     string        `json:"name"`
	Text     string        `json:"text"`
	Entities []quoteEntity `json:"entities"`
	ChatID   int64         `json:"chatId,omitempty"`
}

type quoteMessage struct {
	From          quoteFrom     `json:"from"`
	Text          string        `json:"text"`
	Entities      []quoteEntity `json:"entities"`
	Avatar        bool          `json:"avatar"`
	ReplyMessage  *quoteReply   `json:"replyMessage,omitempty"`
	Media         *quotePhot    `json:"media,omitempty"`
	MediaType     string        `json:"mediaType,omitempty"`
	MediaDuration int           `json:"mediaDuration,omitempty"`
	Voice         *quoteVoice   `json:"voice,omitempty"`
	Document      *quoteDoc     `json:"document,omitempty"`
	Forward       *quoteFwd     `json:"forward,omitempty"`
	// SenderTag 是作者在本群的管理员头衔，没有就留空。
	SenderTag string      `json:"senderTag,omitempty"`
	Audio     *quoteAudio `json:"audio,omitempty"`
}

type quoteAudio struct {
	Title     string `json:"title"`
	Performer string `json:"performer,omitempty"`
	Duration  int    `json:"duration,omitempty"`
}

type quoteVoice struct {
	Waveform []int `json:"waveform"`
	Duration int   `json:"duration,omitempty"`
}

type quoteDoc struct {
	FileName string `json:"file_name"`
}

type quoteFwd struct {
	Label string `json:"label"`
}

type quotePayload struct {
	Type            string         `json:"type"`
	Format          string         `json:"format"`
	BackgroundColor string         `json:"backgroundColor"`
	Width           int            `json:"width"`
	Height          int            `json:"height"`
	Scale           int            `json:"scale"`
	EmojiBrand      string         `json:"emojiBrand"`
	Messages        []quoteMessage `json:"messages"`
}

type yvluService struct {
	a     *app.App
	store *store.Store[yvluConfig]

	// rendered 缓存编码好的头像，键是 peer 加上 Telegram 给的 photo id。
	// 换了头像就是新的 photo id，所以不可能用到过期的条目；
	// 失效由键本身完成。
	avatarMu sync.Mutex
	rendered map[string]*quotePhot
}

// avatarCacheLimit 限制缓存大小。引用语录往往扎堆出现、反复进行——
// 来来回回总是那几个人——所以一个小 map 就能命中几乎所有情况。
const avatarCacheLimit = 32

func (s *yvluService) cachedAvatar(key string) (*quotePhot, bool) {
	s.avatarMu.Lock()
	defer s.avatarMu.Unlock()
	photo, ok := s.rendered[key]
	return photo, ok
}

func (s *yvluService) cacheAvatar(key string, photo *quotePhot) {
	s.avatarMu.Lock()
	defer s.avatarMu.Unlock()
	if s.rendered == nil {
		s.rendered = map[string]*quotePhot{}
	}
	if len(s.rendered) >= avatarCacheLimit {
		for existing := range s.rendered {
			delete(s.rendered, existing)
			break
		}
	}
	s.rendered[key] = photo
}

// Yvlu 注册 .yvlu。
func Yvlu(a *app.App) {
	service := &yvluService{a: a, store: newStore(a, "yvlu.json", func() yvluConfig { return yvluConfig{} })}
	a.Registry.Register(&command.Command{Name: "yvlu", Description: "生成文字语录贴纸、图片与故事，管理贴纸包",
		Usage: "[消息数|r|f 文本|u 用户|webp|image|stories|s|config]", Help: yvluHelp, Timeout: 5 * time.Minute,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			err := service.handle(ctx, inv)
			if err == nil || ctx.Err() != nil {
				return err
			}
			detail := chatHTMLError(err)
			if detail == "" {
				inv.Log.Error("yvlu.failed", "error", err.Error())
				detail = "请检查网络、媒体转换依赖或贴纸包权限后重试"
			}
			return inv.Edit(ctx, feedback("error", "语录操作失败", "")+"\n"+detail)
		}})
}

func (s *yvluService) handle(ctx context.Context, inv *command.Invocation) error {
	switch strings.ToLower(inv.Arg(0)) {
	case "config":
		return s.config(ctx, inv)
	case "s":
		return s.saveSticker(ctx, inv)
	}
	options, ok := parseYvlu(inv)
	if !ok {
		return inv.Edit(ctx, yvluHelp(inv.Prefix))
	}
	if options.Count > 5 {
		return fail("太多了 哒咩")
	}
	if sub := strings.ToLower(inv.Arg(0)); sub == "u" || sub == "ur" {
		peer, err := inv.Client.ResolveTarget(ctx, inv.Arg(1))
		if err != nil {
			return failf("无法获取 %s 的信息，请检查用户ID/用户名是否正确", inv.Arg(1))
		}
		options.FakeSender = peer
	}
	reply, err := inv.Client.GetReply(ctx, inv.Message)
	if err != nil {
		return err
	}
	if reply == nil {
		return fail("请回复一条消息")
	}
	if err := inv.Edit(ctx, feedback("working", "正在生成语录贴纸", "")); err != nil {
		return err
	}
	started := time.Now()
	payload, err := s.build(ctx, inv, reply, options)
	if err != nil {
		return err
	}
	built := time.Now()
	image, extension, err := s.render(ctx, payload)
	if err != nil {
		return err
	}
	rendered := time.Now()
	peer, err := inv.Client.InputPeer(inv.Message.Peer)
	if err != nil {
		return err
	}
	document := bot.DocumentOptions{Name: "quote." + extension, MimeType: mimeOf(extension), ReplyTo: reply.ID}
	if extension != "png" {
		attributes := []tg.DocumentAttributeClass{&tg.DocumentAttributeSticker{Alt: "📝", Stickerset: &tg.InputStickerSetEmpty{}}}
		if extension == "webp" {
			width, height, err := imaging.WebPSize(image)
			if err != nil {
				width, height = 512, 768
			}
			attributes = append(attributes, &tg.DocumentAttributeImageSize{W: width, H: height})
		}
		document.Attributes = attributes
	}
	if err := inv.Client.SendDocumentWith(ctx, peer, image, document); err != nil {
		return err
	}
	// 记录一条慢语录的时间花在了哪里。用 debug 级别，因为这是诊断信息：
	// 「为什么花了七秒」的答案就是这三个数之一；以前靠猜，白白多部署了
	// 一次。
	inv.Log.Debug("yvlu.timing",
		"collect", built.Sub(started).String(),
		"render", rendered.Sub(built).String(),
		"upload", time.Since(rendered).String(),
		"bytes", len(image))
	return inv.Client.DeleteMessage(ctx, inv.Message)
}

func mimeOf(extension string) string {
	switch extension {
	case "png":
		return "image/png"
	case "webm":
		return "video/webm"
	}
	return "image/webp"
}

func (s *yvluService) config(ctx context.Context, inv *command.Invocation) error {
	action := strings.ToLower(inv.Arg(1))
	if action == "" {
		current, err := s.store.Read()
		if err != nil {
			return err
		}
		name := current.StickerSet
		text := "<b>当前配置</b>\n贴纸包名称：" + command.Code(orDefault(name, "(未设置)"))
		if name != "" {
			text += "\n贴纸包链接：t.me/addstickers/" + command.Escape(name)
		}
		return inv.Edit(ctx, text+"\n"+command.Code(inv.Prefix+"yvlu config sticker 贴纸包名称"))
	}
	if action != "sticker" && action != "stickerset" && action != "set" {
		return failf("未知的配置项：%s。可用配置命令：%syvlu config sticker 贴纸包名称", inv.Arg(1), inv.Prefix)
	}
	name := strings.Join(inv.Args[2:], "_")
	if name == "" {
		return failf("请提供贴纸包名称，用法：%syvlu config sticker 贴纸包名称", inv.Prefix)
	}
	if !stickerSetName.MatchString(name) {
		return fail("贴纸包名称只能包含字母、数字和下划线")
	}
	if len(name) > 64 {
		return fail("贴纸包名称长度应在 1-64 个字符之间")
	}
	if err := s.store.Update(func(config *yvluConfig) error { config.StickerSet = name; return nil }); err != nil {
		return err
	}
	return inv.Edit(ctx, feedback("success", "贴纸包配置已更新", "已设置贴纸包："+name+"\n贴纸包链接：t.me/addstickers/"+name))
}

// saveSticker 把被回复的贴纸或图片加进配置好的贴纸包，
// 贴纸包还不存在时先创建。
func (s *yvluService) saveSticker(ctx context.Context, inv *command.Invocation) error {
	config, err := s.store.Read()
	if err != nil {
		return err
	}
	if strings.TrimSpace(config.StickerSet) == "" {
		return failf("未配置贴纸包，请使用 %syvlu config sticker 贴纸包名称", inv.Prefix)
	}
	reply, err := inv.Client.GetReply(ctx, inv.Message)
	if err != nil {
		return err
	}
	if reply == nil || reply.Raw == nil {
		return fail("请回复一张贴纸或图片")
	}
	api := inv.Client.API()
	set := &tg.InputStickerSetShortName{ShortName: config.StickerSet}
	exists := true
	if _, err := api.MessagesGetStickerSet(ctx, &tg.MessagesGetStickerSetRequest{Stickerset: set}); err != nil {
		if !tgerr.Is(err, "STICKERSET_INVALID") {
			return err
		}
		exists = false
	}

	var document *tg.InputDocument
	if existing, ok := bot.DocumentOf(reply.Raw); ok && isStickerDocument(reply.Raw) {
		// 现成的贴纸按引用添加：重新上传会丢掉它的 emoji 和所属贴纸包，
		// 而把 WebP 经 PNG 重新编码一遍，只会白白损失画质。
		document = existing
	}
	if document == nil {
		file, err := inv.Client.DownloadMedia(ctx, reply.Raw, 8<<20)
		if err != nil {
			return fail("不支持的媒体类型，请回复贴纸或图片")
		}
		decoded, err := imaging.Decode(file.Data)
		if err != nil {
			return fail("不支持的媒体类型，请回复贴纸或图片")
		}
		png, err := imaging.EncodePNG(imaging.ResizeFit(decoded, 512, 512))
		if err != nil {
			return err
		}
		document, err = inv.Client.UploadDocument(ctx, "sticker.png", "image/png", png)
		if err != nil {
			return err
		}
	}

	item := tg.InputStickerSetItem{Document: document, Emoji: "📝"}
	if exists {
		if _, err := api.StickersAddStickerToSet(ctx, &tg.StickersAddStickerToSetRequest{Stickerset: set, Sticker: item}); err != nil {
			return err
		}
	} else {
		self, ok := bot.InputUser(&tg.InputPeerSelf{})
		if !ok {
			return fail("无法获取当前用户信息")
		}
		if _, err := api.StickersCreateStickerSet(ctx, &tg.StickersCreateStickerSetRequest{
			UserID: self, Title: config.StickerSet, ShortName: config.StickerSet, Stickers: []tg.InputStickerSetItem{item},
		}); err != nil {
			return err
		}
	}
	title := "贴纸已添加"
	if !exists {
		title = "贴纸包已创建"
	}
	return inv.Edit(ctx, feedback("success", title, "贴纸包：t.me/addstickers/"+config.StickerSet))
}

// render 提交 payload，返回图片及其扩展名。
func (s *yvluService) render(ctx context.Context, payload *quotePayload) ([]byte, string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	response, err := httpx.Do(ctx, httpx.Request{Method: "POST", URL: quoteEndpoint, Body: encoded,
		Headers: map[string]string{"Content-Type": "application/json", "User-Agent": quoteUserAgent},
		Timeout: 60 * time.Second, MaxBytes: 20 << 20})
	if err != nil {
		return nil, "", failf("quote 服务不可用：%s", httpx.Reason(err))
	}
	if response.Status == 403 {
		return nil, "", fail("quote 服务拒绝了请求（403），可能是它换了准入规则")
	}
	if !response.OK() {
		return nil, "", failf("quote-api HTTP %d", response.Status)
	}
	switch {
	case len(response.Body) >= 12 && string(response.Body[0:4]) == "RIFF" && string(response.Body[8:12]) == "WEBP":
		return response.Body, "webp", nil
	case len(response.Body) >= 8 && string(response.Body[1:4]) == "PNG":
		return response.Body, "png", nil
	case len(response.Body) >= 4 && response.Body[0] == 0x1a && response.Body[1] == 0x45:
		return response.Body, "webm", nil
	}
	return nil, "", fail("quote 服务返回了非图片数据")
}

// build 用被回复的消息组装 payload。
func (s *yvluService) build(ctx context.Context, inv *command.Invocation, reply *bot.Message, options *yvluOptions) (*quotePayload, error) {
	messages := []*bot.Message{reply}
	if options.Count > 1 {
		collected, err := s.following(ctx, inv, reply, options.Count)
		if err == nil && len(collected) > 0 {
			messages = collected
		}
	}
	payload := &quotePayload{Type: options.Format, Format: "webp", BackgroundColor: "#1b1429",
		Width: 512, Height: 768, Scale: 2, EmojiBrand: "apple"}
	if options.Format != "quote" {
		payload.Format = "png"
	}
	if options.Format == "stories" {
		payload.Width, payload.Height = 360, 640
	}

	avatars := map[int64]*quotePhot{}
	previous := int64(-1)
	for index, message := range messages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		author, err := s.author(ctx, inv, message, options)
		if err != nil {
			return nil, err
		}
		show := author.ID != previous
		previous = author.ID
		if show {
			photo, cached := avatars[author.ID]
			if !cached {
				photo = s.avatar(ctx, inv, message, options)
				avatars[author.ID] = photo
			}
			author.Photo = photo
			if info, known := inv.Client.Peers().User(author.ID); known && info.EmojiStatus != "" {
				author.EmojiStatus = info.EmojiStatus
			}
		} else {
			author.Name, author.FirstName, author.LastName, author.Username, author.EmojiStatus = "", "", "", "", ""
		}

		item := quoteMessage{From: *author, Avatar: show}
		switch {
		case index == 0 && options.FakeText != "":
			item.Text = options.FakeText
			item.Entities = convertEntities(options.FakeEnts, utf16Len(inv.Text)-utf16Len(options.FakeText))
		case index == 0 && inv.Message.QuoteText != "":
			// 操作者回复的是消息的一部分而不是全部。引用整条消息，
			// 就无视了对方有意缩小的范围，所以以选中的部分为准。
			item.Text = inv.Message.QuoteText
			item.Entities = convertEntities(inv.Message.QuoteEntities, 0)
		default:
			item.Text = message.Text
			if message.Raw != nil {
				if entities, ok := message.Raw.GetEntities(); ok {
					item.Entities = convertEntities(entities, 0)
				}
			}
			s.describeMedia(ctx, inv, message, &item)
			if label := forwardLabel(inv, message); label != nil {
				item.Forward = label
			}
			if tag := s.senderTag(ctx, inv, message, author.ID); tag != "" {
				item.SenderTag = tag
			}
		}
		if item.Entities == nil {
			item.Entities = []quoteEntity{}
		}
		if options.IncludeReply {
			if block := s.replyBlock(ctx, inv, message); block != nil {
				item.ReplyMessage = block
			}
		}
		payload.Messages = append(payload.Messages, item)
	}
	if len(payload.Messages) == 0 {
		return nil, fail("没有可生成的消息")
	}
	return payload, nil
}

// following 从被回复的那条开始，读取 count 条消息。
//
// 真实聊天里的消息 ID 并不连续——别人的消息、删除的消息、服务消息都会
// 占用 ID——所以逐个递增 ID 会漏掉消息，还会去请求根本不存在的消息。
// 这里沿用原插件的做法，按位置而不是按 ID 遍历历史：getHistory 定位到
// offset_id，负的 add_offset 再往较新的一端挪动相应的条数。
func (s *yvluService) following(ctx context.Context, inv *command.Invocation, reply *bot.Message, count int) ([]*bot.Message, error) {
	peer, err := inv.Client.InputPeer(reply.Peer)
	if err != nil {
		return nil, err
	}
	result, err := inv.Client.API().MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer: peer, OffsetID: reply.ID - 1, AddOffset: -count, Limit: count,
	})
	if err != nil {
		return nil, err
	}
	found, _ := inv.Client.Unpack(result)
	var ordered []*bot.Message
	for _, item := range found {
		message, ok := item.(*tg.Message)
		if !ok || message.ID < reply.ID {
			continue
		}
		if envelope, ok := bot.Envelope(message, inv.Client.SelfID(), false, inv.Client.Peers()); ok {
			ordered = append(ordered, envelope)
		}
	}
	// getHistory 返回的顺序是新的在前。
	sort.Slice(ordered, func(a, b int) bool { return ordered[a].ID < ordered[b].ID })
	if len(ordered) == 0 || ordered[0].ID != reply.ID {
		// 被回复的消息必须排在最前面，即使历史记录里没有返回它。
		ordered = append([]*bot.Message{reply}, ordered...)
	}
	if len(ordered) > count {
		ordered = ordered[:count]
	}
	return ordered, nil
}

// author 确定一条被引用的消息该算在谁名下。
//
// 转发的消息算在原作者名下，而不是转发者：引用一条转发消息，
// 却在上面看到转发者的名字，就等于把话安到了别人头上。
func (s *yvluService) author(ctx context.Context, inv *command.Invocation, message *bot.Message, options *yvluOptions) (*quoteFrom, error) {
	if options.FakeSender != nil {
		if user, ok := options.FakeSender.(*tg.InputPeerUser); ok {
			info, _ := inv.Client.Peers().User(user.UserID)
			return &quoteFrom{ID: info.ID, FirstName: info.FirstName, LastName: info.LastName, Username: info.Username,
				Name: strings.TrimSpace(info.FirstName + " " + info.LastName)}, nil
		}
	}
	if from := forwardAuthor(inv, message); from != nil {
		return from, nil
	}
	switch sender := message.Sender.(type) {
	case *tg.PeerUser:
		info, _ := inv.Client.Peers().User(sender.UserID)
		name := strings.TrimSpace(info.FirstName + " " + info.LastName)
		if name == "" {
			name = info.Username
		}
		return &quoteFrom{ID: sender.UserID, FirstName: info.FirstName, LastName: info.LastName, Username: info.Username, Name: name}, nil
	case *tg.PeerChannel:
		info, _ := inv.Client.Peers().Channel(sender.ChannelID)
		return &quoteFrom{ID: sender.ChannelID, FirstName: info.Title, Name: info.Title, Username: info.Username}, nil
	case *tg.PeerChat:
		info, _ := inv.Client.Peers().Chat(sender.ChatID)
		return &quoteFrom{ID: sender.ChatID, FirstName: info.Title, Name: info.Title}, nil
	}
	return nil, fail("无法获取消息发送者信息")
}

// 语录中可选部分的时间预算。这些都不是命令的重点：没有头像、没有管理员
// 头衔、没有嵌入媒体，语录照样能渲染。所以每一项的上限都远低于命令本身
// 的期限，超时只会让图片少点东西，而不会把整个命令拖住。
const (
	avatarBudget = 20 * time.Second
	mediaBudget  = 45 * time.Second
	tagBudget    = 10 * time.Second
)

// avatar 把作者的头像下载成 data URL，没有头像时返回 nil——
// 没有头像的语录照样能渲染。
func (s *yvluService) avatar(ctx context.Context, inv *command.Invocation, message *bot.Message, options *yvluOptions) *quotePhot {
	ctx, cancel := context.WithTimeout(ctx, avatarBudget)
	defer cancel()
	peer := options.FakeSender
	if peer == nil {
		resolved, ok := inv.Client.Peers().InputPeer(message.Sender)
		if !ok {
			return nil
		}
		peer = resolved
	}
	// 命中缓存就完全省掉一次跨数据中心的下载，
	// 而一条语录的大部分时间正是花在这上面。
	key := avatarKey(inv, peer)
	if key != "" {
		if photo, ok := s.cachedAvatar(key); ok {
			return photo
		}
	}
	// 先下小图，再下大图：小图下载不了的 peer，往往还能拿到大图。
	data, err := inv.Client.DownloadProfilePhoto(ctx, peer, false, 2<<20)
	if err != nil || len(data) == 0 {
		data, err = inv.Client.DownloadProfilePhoto(ctx, peer, true, 2<<20)
	}
	if err != nil || len(data) == 0 {
		return nil
	}
	decoded, err := imaging.Decode(data)
	if err != nil {
		return nil
	}
	encoded, err := imaging.EncodePNG(imaging.Flatten(imaging.ResizeCover(decoded, 256, 256), color.Black))
	if err != nil {
		return nil
	}
	photo := &quotePhot{URL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded)}
	if key != "" {
		s.cacheAvatar(key, photo)
	}
	return photo
}

// avatarKey 标识一张渲染好的头像：peer 加上它当前所用头像的 photo id。
func avatarKey(inv *command.Invocation, peer tg.InputPeerClass) string {
	switch value := peer.(type) {
	case *tg.InputPeerSelf:
		if photo, ok := inv.Client.Self().GetPhoto(); ok {
			if current, ok := photo.(*tg.UserProfilePhoto); ok {
				return "self:" + strconv.FormatInt(current.PhotoID, 10)
			}
		}
	case *tg.InputPeerUser:
		if info, known := inv.Client.Peers().User(value.UserID); known && info.PhotoID != 0 {
			return "user:" + strconv.FormatInt(value.UserID, 10) + ":" + strconv.FormatInt(info.PhotoID, 10)
		}
	case *tg.InputPeerChannel:
		if info, known := inv.Client.Peers().Channel(value.ChannelID); known && info.PhotoID != 0 {
			return "channel:" + strconv.FormatInt(value.ChannelID, 10) + ":" + strconv.FormatInt(info.PhotoID, 10)
		}
	}
	return ""
}

// describeMedia 附上被引用消息携带的媒体：图片直接嵌入，视频转码，
// 其余的只做文字描述。
func (s *yvluService) describeMedia(ctx context.Context, inv *command.Invocation, message *bot.Message, item *quoteMessage) {
	if message.Raw == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, mediaBudget)
	defer cancel()
	media, ok := message.Raw.GetMedia()
	if !ok {
		return
	}
	switch value := media.(type) {
	case *tg.MessageMediaPhoto:
		file, err := inv.Client.DownloadMedia(ctx, message.Raw, 8<<20)
		if err == nil {
			item.Media = &quotePhot{URL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(file.Data)}
		}
	case *tg.MessageMediaDocument:
		document, ok := value.Document.(*tg.Document)
		if !ok {
			return
		}
		s.describeDocument(ctx, inv, message, document, item)
	}
}

func (s *yvluService) describeDocument(ctx context.Context, inv *command.Invocation, message *bot.Message, document *tg.Document, item *quoteMessage) {
	var sticker, animated bool
	var video *tg.DocumentAttributeVideo
	var audio *tg.DocumentAttributeAudio
	fileName := "file"
	for _, attribute := range document.Attributes {
		switch typed := attribute.(type) {
		case *tg.DocumentAttributeSticker:
			sticker = true
		case *tg.DocumentAttributeAnimated:
			animated = true
		case *tg.DocumentAttributeVideo:
			video = typed
		case *tg.DocumentAttributeAudio:
			audio = typed
		case *tg.DocumentAttributeFilename:
			fileName = typed.FileName
		}
	}
	switch {
	case sticker:
		// 贴纸本身就是语录的输出格式；再嵌入贴纸就成了贴纸套贴纸。
		return
	case audio != nil && !audio.Voice:
		title := audio.Title
		if title == "" {
			title = fileName
		}
		if title == "" || title == "file" {
			title = "Audio"
		}
		item.Audio = &quoteAudio{Title: title, Performer: audio.Performer, Duration: audio.Duration}
	case audio != nil && audio.Voice:
		if len(audio.Waveform) == 0 {
			return
		}
		waveform := make([]int, 0, len(audio.Waveform))
		for _, sample := range audio.Waveform {
			waveform = append(waveform, clampInt(int(sample), 0, 31))
		}
		item.Voice = &quoteVoice{Waveform: waveform, Duration: audio.Duration}
	case strings.HasPrefix(document.MimeType, "image/"):
		if file, err := inv.Client.DownloadMedia(ctx, message.Raw, 8<<20); err == nil {
			item.Media = &quotePhot{URL: "data:" + document.MimeType + ";base64," + base64.StdEncoding.EncodeToString(file.Data)}
		}
	case animated || video != nil:
		item.MediaType = "video"
		if animated {
			item.MediaType = "gif"
		}
		if video != nil {
			item.MediaDuration = int(video.Duration)
		}
		s.embedVideo(ctx, inv, message, document, item)
	default:
		item.Document = &quoteDoc{FileName: fileName}
	}
}

// embedVideo 把短动画转码，让语录里能显示会动的画面。这里失败并不致命：
// 语录照样渲染，只是媒体改为文字描述而不直接显示。
func (s *yvluService) embedVideo(ctx context.Context, inv *command.Invocation, message *bot.Message, document *tg.Document, item *quoteMessage) {
	if document.Size > 8<<20 {
		return
	}
	if _, err := media.FFmpeg(); err != nil {
		return
	}
	file, err := inv.Client.DownloadMedia(ctx, message.Raw, 8<<20)
	if err != nil {
		return
	}
	if document.MimeType == "video/webm" {
		item.Media = &quotePhot{URL: "data:video/webm;base64," + base64.StdEncoding.EncodeToString(file.Data)}
		return
	}
	directory, err := os.MkdirTemp("", "mibot-yvlu-")
	if err != nil {
		return
	}
	defer os.RemoveAll(directory)
	converted, err := media.ToStickerWebM(ctx, directory, file.Data, extensionFor(document.MimeType))
	if err != nil {
		inv.Log.Info("yvlu.video_skipped", "error", err.Error())
		return
	}
	item.Media = &quotePhot{URL: "data:video/webm;base64," + base64.StdEncoding.EncodeToString(converted)}
}

func extensionFor(mimeType string) string {
	switch mimeType {
	case "video/mp4":
		return ".mp4"
	case "image/gif":
		return ".gif"
	case "video/webm":
		return ".webm"
	}
	return ".bin"
}

// replyBlock 生成被引用消息本身所回复的那条内容。
func (s *yvluService) replyBlock(ctx context.Context, inv *command.Invocation, message *bot.Message) *quoteReply {
	if message.ReplyToID == 0 {
		return nil
	}
	replied, err := inv.Client.GetReply(ctx, message)
	if err != nil || replied == nil {
		return nil
	}
	// 回复者如果只选中了消息的一部分，那他回应的就是选中的那部分。
	text, entities := replied.Text, []tg.MessageEntityClass(nil)
	if replied.Raw != nil {
		if carried, ok := replied.Raw.GetEntities(); ok {
			entities = carried
		}
	}
	if message.QuoteText != "" {
		text, entities = message.QuoteText, message.QuoteEntities
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	block := &quoteReply{Text: text, Name: "unknown", Entities: convertEntities(entities, 0)}
	switch sender := replied.Sender.(type) {
	case *tg.PeerUser:
		info, _ := inv.Client.Peers().User(sender.UserID)
		if name := strings.TrimSpace(info.FirstName + " " + info.LastName); name != "" {
			block.Name = name
		} else if info.Username != "" {
			block.Name = info.Username
		}
		block.ChatID = sender.UserID
	case *tg.PeerChannel:
		info, _ := inv.Client.Peers().Channel(sender.ChannelID)
		if info.Title != "" {
			block.Name = info.Title
		}
		block.ChatID = sender.ChannelID
	}
	return block
}

// isStickerDocument 判断消息里的文件是否带有贴纸属性。
func isStickerDocument(message *tg.Message) bool {
	media, ok := message.GetMedia()
	if !ok {
		return false
	}
	value, ok := media.(*tg.MessageMediaDocument)
	if !ok {
		return false
	}
	document, ok := value.Document.(*tg.Document)
	if !ok {
		return false
	}
	for _, attribute := range document.Attributes {
		if _, ok := attribute.(*tg.DocumentAttributeSticker); ok {
			return true
		}
	}
	return false
}

// forwardAuthor 从转发消息的头部读出原作者；消息不是转发来的就返回 nil。
//
// 头部可能给出一个 peer，也可能只有一个名字：从隐藏了账号的人那里转发，
// 只留下一个字符串，没有可解析的对象。两种情况都值得署名，所以连名字都
// 没有时也要兜底，让语录保持真实，而不是悄悄算到转发者头上。
func forwardAuthor(inv *command.Invocation, message *bot.Message) *quoteFrom {
	if message.Raw == nil {
		return nil
	}
	header, ok := message.Raw.GetFwdFrom()
	if !ok {
		return nil
	}
	if peer, ok := header.GetFromID(); ok {
		switch value := peer.(type) {
		case *tg.PeerUser:
			info, _ := inv.Client.Peers().User(value.UserID)
			name := strings.TrimSpace(info.FirstName + " " + info.LastName)
			if name == "" {
				name = info.Username
			}
			if name != "" || info.ID != 0 {
				return &quoteFrom{ID: value.UserID, FirstName: info.FirstName, LastName: info.LastName,
					Username: info.Username, Name: name}
			}
		case *tg.PeerChannel:
			info, _ := inv.Client.Peers().Channel(value.ChannelID)
			return &quoteFrom{ID: value.ChannelID, FirstName: info.Title, Name: info.Title, Username: info.Username}
		}
	}
	name := header.FromName
	if name == "" {
		name = header.SavedFromName
	}
	if name == "" {
		name = header.PostAuthor
	}
	if name == "" {
		name = "未知来源"
	}
	return &quoteFrom{ID: int64(nameHash(name)), FirstName: name, Name: name}
}

// nameHash 给没有 ID 的作者一个稳定的 ID，这样在同一次生成里，
// 渲染服务仍会给他们一致的配色。
func nameHash(text string) int32 {
	var value int32
	for _, r := range text {
		value = value*31 + int32(r)
	}
	if value < 0 {
		value = -value
	}
	return value
}

// forwardLabel 给出转发消息的来源，用在正文上方那行小字 "forwarded from" 里。
func forwardLabel(inv *command.Invocation, message *bot.Message) *quoteFwd {
	from := forwardAuthor(inv, message)
	if from == nil {
		return nil
	}
	label := from.Name
	if label == "" {
		label = from.FirstName
	}
	if label == "" {
		return nil
	}
	return &quoteFwd{Label: label}
}

// senderTag 读取作者在本群的管理员头衔。
//
// 渲染出的语录靠它把 "群主" 或自定义头衔和普通成员区分开；
// 只有频道和超级群才有这个信息。
func (s *yvluService) senderTag(ctx context.Context, inv *command.Invocation, message *bot.Message, authorID int64) string {
	ctx, cancel := context.WithTimeout(ctx, tagBudget)
	defer cancel()
	chat, err := inv.Client.InputPeer(message.Peer)
	if err != nil {
		return ""
	}
	channel, ok := bot.InputChannel(chat)
	if !ok {
		return ""
	}
	participant, ok := inv.Client.Peers().InputPeer(&tg.PeerUser{UserID: authorID})
	if !ok {
		return ""
	}
	result, err := inv.Client.API().ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{Channel: channel, Participant: participant})
	if err != nil {
		return ""
	}
	inv.Client.Peers().RememberUsers(result.Users)
	switch value := result.Participant.(type) {
	case *tg.ChannelParticipantCreator:
		return strings.TrimSpace(value.Rank)
	case *tg.ChannelParticipantAdmin:
		return strings.TrimSpace(value.Rank)
	}
	return ""
}
