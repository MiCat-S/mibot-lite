// Package yvlu 实现 .yvlu：生成语录贴纸、图片与故事，并管理贴纸包。
package yvlu

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
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
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
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
	// FakeAuthor 是 FakeSender 的名字等信息，由 fakeAuthor 查好，每条消息都署这个名。
	FakeAuthor *quoteFrom
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
		offset := kit.UTF16Len(match)
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

// Register 注册 .yvlu。
func Register(a *app.App) {
	service := &yvluService{a: a, store: kit.NewStore(a, "yvlu.json", func() yvluConfig { return yvluConfig{} })}
	a.Registry.Register(&command.Command{Name: "yvlu", Description: "生成文字语录贴纸、图片与故事，管理贴纸包",
		Usage: "[消息数|r|f 文本|u 用户|webp|image|stories|s|config]", Help: yvluHelp, Timeout: 5 * time.Minute,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			err := service.handle(ctx, inv)
			if err == nil || ctx.Err() != nil {
				return err
			}
			detail := kit.ChatHTMLError(err)
			if detail == "" {
				inv.Log.Error("yvlu.failed", "error", err.Error())
				detail = "请检查网络、媒体转换依赖或贴纸包权限后重试"
			}
			return inv.Edit(ctx, kit.Feedback("error", "语录操作失败", "")+"\n"+detail)
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
		return kit.Fail("太多了 哒咩")
	}
	if sub := strings.ToLower(inv.Arg(0)); sub == "u" || sub == "ur" {
		peer, err := inv.Client.ResolveTarget(ctx, inv.Arg(1))
		if err != nil {
			return kit.Failf("无法获取 %s 的信息，请检查用户ID/用户名是否正确", inv.Arg(1))
		}
		author, err := fakeAuthor(ctx, inv, peer)
		if err != nil {
			return kit.Failf("无法获取 %s 的信息，请检查用户ID/用户名是否正确", inv.Arg(1))
		}
		options.FakeSender, options.FakeAuthor = peer, author
	}
	reply, err := inv.Client.GetReply(ctx, inv.Message)
	if err != nil {
		return err
	}
	if reply == nil {
		return kit.Fail("请回复一条消息")
	}
	if err := inv.Edit(ctx, kit.Feedback("working", "正在生成语录贴纸", "")); err != nil {
		return err
	}
	started := time.Now()
	payload, err := s.build(ctx, inv, reply, options)
	if err != nil {
		return err
	}
	built := time.Now()
	picture, extension, err := s.render(ctx, payload)
	if err != nil {
		return err
	}
	rendered := time.Now()
	peer, err := inv.Client.InputPeer(inv.Message.Peer)
	if err != nil {
		return err
	}
	if err := send(ctx, inv, peer, picture, extension, reply.ID); err != nil {
		return err
	}
	// 记录一条慢语录的时间花在了哪里。用 debug 级别，因为这是诊断信息：
	// 「为什么花了七秒」的答案就是这三个数之一；以前靠猜，白白多部署了
	// 一次。
	inv.Log.Debug("yvlu.timing",
		"collect", built.Sub(started).String(),
		"render", rendered.Sub(built).String(),
		"upload", time.Since(rendered).String(),
		"bytes", len(picture))
	return inv.Client.DeleteMessage(ctx, inv.Message)
}

// send 按格式发出渲染结果：WebP 和 WebM 当贴纸发，PNG（image、stories）当照片发。
func send(ctx context.Context, inv *command.Invocation, peer tg.InputPeerClass, data []byte, extension string, replyTo int) error {
	if extension != "png" {
		return inv.Client.SendDocumentWith(ctx, peer, data, stickerDocument(data, extension, replyTo))
	}
	// 原插件把 quote.png 交给 teleproto，它按扩展名认作图片，以照片发出。照片有尺寸和
	// 大小限制；超出限制的，或者被 Telegram 拒收的，退回按文件发，结果至少还能送到。
	if photoFits(data) {
		err := inv.Client.SendPhoto(ctx, peer, "quote.png", data, "", replyTo)
		if err == nil || !photoRejected(err) {
			return err
		}
		inv.Log.Info("yvlu.photo_rejected", "error", err.Error())
	}
	return inv.Client.SendDocumentWith(ctx, peer, data, bot.DocumentOptions{Name: "quote.png", MimeType: "image/png", ReplyTo: replyTo})
}

// stickerDocument 是把 WebP 或 WebM 当贴纸发出去的参数，不属于任何贴纸包。
func stickerDocument(data []byte, extension string, replyTo int) bot.DocumentOptions {
	attributes := []tg.DocumentAttributeClass{&tg.DocumentAttributeSticker{Alt: "📝", Stickerset: &tg.InputStickerSetEmpty{}}}
	switch extension {
	case "webp":
		width, height, err := imaging.WebPSize(data)
		if err != nil {
			width, height = 512, 768
		}
		attributes = append(attributes, &tg.DocumentAttributeImageSize{W: width, H: height})
	case "webm":
		// 原插件把 .webm 文件交给 teleproto，它会自动加上视频属性；Telegram 自己的客户端
		// 发视频贴纸也带这一项。宽高和时长从文件头读，读不到时按贴纸的常见尺寸填。
		width, height, duration, err := media.WebMInfo(data)
		if err != nil {
			width, height, duration = 512, 512, 0
		}
		attributes = append(attributes, &tg.DocumentAttributeVideo{W: width, H: height, Duration: duration})
	}
	return bot.DocumentOptions{Name: "quote." + extension, MimeType: mimeOf(extension), ReplyTo: replyTo, Attributes: attributes}
}

// photoFits 判断图片能不能当照片发。Telegram 的要求：不超过 10 MB，宽高之和不超过
// 10000，长边不超过短边的 20 倍。
func photoFits(data []byte) bool {
	if len(data) > 10<<20 {
		return false
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width+config.Height > 10000 {
		return false
	}
	return max(config.Width, config.Height) <= 20*min(config.Width, config.Height)
}

// photoRejected 判断照片是不是因为内容或权限被拒：这类错误换成文件发还有机会成功；
// 限流和网络错误换了也没用，原样报告。
func photoRejected(err error) bool {
	rpc, ok := tgerr.As(err)
	if !ok {
		return false
	}
	return rpc.Code == 400 || rpc.IsType("CHAT_SEND_PHOTOS_FORBIDDEN")
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
		text := "<b>当前配置</b>\n贴纸包名称：" + command.Code(kit.OrDefault(name, "(未设置)"))
		if name != "" {
			text += "\n贴纸包链接：t.me/addstickers/" + command.Escape(name)
		}
		return inv.Edit(ctx, text+"\n"+command.Code(inv.Prefix+"yvlu config sticker 贴纸包名称"))
	}
	if action != "sticker" && action != "stickerset" && action != "set" {
		return kit.Failf("未知的配置项：%s。可用配置命令：%syvlu config sticker 贴纸包名称", inv.Arg(1), inv.Prefix)
	}
	name := strings.Join(inv.Args[2:], "_")
	if name == "" {
		return kit.Failf("请提供贴纸包名称，用法：%syvlu config sticker 贴纸包名称", inv.Prefix)
	}
	if !stickerSetName.MatchString(name) {
		return kit.Fail("贴纸包名称只能包含字母、数字和下划线")
	}
	if len(name) > 64 {
		return kit.Fail("贴纸包名称长度应在 1-64 个字符之间")
	}
	if err := s.store.Update(func(config *yvluConfig) error { config.StickerSet = name; return nil }); err != nil {
		return err
	}
	return inv.Edit(ctx, kit.Feedback("success", "贴纸包配置已更新", "已设置贴纸包："+name+"\n贴纸包链接：t.me/addstickers/"+name))
}

// saveSticker 把被回复的贴纸或图片加进配置好的贴纸包，
// 贴纸包还不存在时先创建。
func (s *yvluService) saveSticker(ctx context.Context, inv *command.Invocation) error {
	config, err := s.store.Read()
	if err != nil {
		return err
	}
	if strings.TrimSpace(config.StickerSet) == "" {
		return kit.Failf("未配置贴纸包，请使用 %syvlu config sticker 贴纸包名称", inv.Prefix)
	}
	reply, err := inv.Client.GetReply(ctx, inv.Message)
	if err != nil {
		return err
	}
	if reply == nil || reply.Raw == nil {
		return kit.Fail("请回复一张贴纸或图片")
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
			return kit.Fail("不支持的媒体类型，请回复贴纸或图片")
		}
		decoded, err := imaging.Decode(file.Data)
		if err != nil {
			return kit.Fail("不支持的媒体类型，请回复贴纸或图片")
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
			return kit.Fail("无法获取当前用户信息")
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
	return inv.Edit(ctx, kit.Feedback("success", title, "贴纸包：t.me/addstickers/"+config.StickerSet))
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
		return nil, "", kit.Failf("quote 服务不可用：%s", httpx.Reason(err))
	}
	if response.Status == 403 {
		return nil, "", kit.Fail("quote 服务拒绝了请求（403），可能是它换了准入规则")
	}
	if !response.OK() {
		return nil, "", kit.Failf("quote-api HTTP %d", response.Status)
	}
	switch {
	case len(response.Body) >= 12 && string(response.Body[0:4]) == "RIFF" && string(response.Body[8:12]) == "WEBP":
		return response.Body, "webp", nil
	case len(response.Body) >= 8 && string(response.Body[1:4]) == "PNG":
		return response.Body, "png", nil
	case len(response.Body) >= 4 && response.Body[0] == 0x1a && response.Body[1] == 0x45:
		return response.Body, "webm", nil
	}
	return nil, "", kit.Fail("quote 服务返回了非图片数据")
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
		author, face, err := s.author(inv, message, options)
		if err != nil {
			return nil, err
		}
		show := author.ID != previous
		previous = author.ID
		if show {
			photo, cached := avatars[author.ID]
			if !cached {
				photo = s.avatar(ctx, inv, face)
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
		fake := index == 0 && options.FakeText != ""
		switch {
		case fake:
			item.Text = options.FakeText
			item.Entities = convertEntities(options.FakeEnts, kit.UTF16Len(inv.Text)-kit.UTF16Len(options.FakeText))
		case index == 0 && inv.Message.QuoteText != "":
			// 操作者回复的是消息的一部分而不是全部。引用整条消息，
			// 就无视了对方有意缩小的范围，所以文字以选中的部分为准；
			// 媒体、转发来源和头衔仍属于这条消息，照样带上。
			item.Text = inv.Message.QuoteText
			item.Entities = convertEntities(inv.Message.QuoteEntities, 0)
		default:
			item.Text = message.Text
			if message.Raw != nil {
				if entities, ok := message.Raw.GetEntities(); ok {
					item.Entities = convertEntities(entities, 0)
				}
			}
		}
		// 伪造的文字不是这条消息说的，它的媒体和转发来源也就不该出现；
		// 头衔属于作者，和原插件一样照样显示。
		if !fake {
			s.describeMedia(ctx, inv, message, &item)
			if label := forwardLabel(inv, message); label != nil {
				item.Forward = label
			}
		}
		if tag := s.senderTag(ctx, inv, message, author.ID); tag != "" {
			item.SenderTag = tag
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
		return nil, kit.Fail("没有可生成的消息")
	}
	return payload, nil
}

// following 从被回复的那条开始，读取 count 条消息。
//
// 真实聊天里的消息 ID 并不连续——别人的消息、删除的消息、服务消息都会
// 占用 ID——所以逐个递增 ID 会漏掉消息，还会去请求根本不存在的消息。
// 这里沿用原插件的做法，按位置而不是按 ID 遍历历史：getHistory 定位到
// offset_id，负的 add_offset 再往较新的一端挪动相应的条数。
//
// offset_id 取被回复消息自己的 ID：offset_id=X、add_offset=-n、limit=n 返回的是
// ID 不小于 X 的最早 n 条。原插件写的是 offsetId: id-1 加 reverse，但 teleproto
// 在 reverse 时会先给 offsetId 加一，实际发出的正是 offset_id=id。照字面传 id-1，
// 编号 id-1 的消息存在时会占掉一个名额，结果少一条。
func (s *yvluService) following(ctx context.Context, inv *command.Invocation, reply *bot.Message, count int) ([]*bot.Message, error) {
	peer, err := inv.Client.InputPeer(reply.Peer)
	if err != nil {
		return nil, err
	}
	result, err := inv.Client.API().MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer: peer, OffsetID: reply.ID, AddOffset: -count, Limit: count,
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

// author 确定一条被引用的消息该算在谁名下，同时给出该用谁的头像（没有可用头像时为 nil）。
//
// 转发的消息算在原作者名下，而不是转发者：引用一条转发消息，
// 却在上面看到转发者的名字，就等于把话安到了别人头上。头像跟着署名走：
// 名字是原作者、头像却是转发者，一样是张冠李戴。
func (s *yvluService) author(inv *command.Invocation, message *bot.Message, options *yvluOptions) (*quoteFrom, tg.InputPeerClass, error) {
	if options.FakeAuthor != nil {
		// 每条消息各拿一份副本：build 会就地清空名字、填上头像。
		from := *options.FakeAuthor
		return &from, options.FakeSender, nil
	}
	if from, origin := forwardAuthor(inv, message); from != nil {
		return from, inputPeer(inv, origin), nil
	}
	from := peerAuthor(inv, message.Sender)
	if from == nil {
		return nil, nil, kit.Fail("无法获取消息发送者信息")
	}
	return from, inputPeer(inv, message.Sender), nil
}

// inputPeer 把 peer 转成可以拿来下载头像的形式；peer 为空或不知道 access hash 时返回 nil。
func inputPeer(inv *command.Invocation, peer tg.PeerClass) tg.InputPeerClass {
	if peer == nil {
		return nil
	}
	resolved, ok := inv.Client.Peers().InputPeer(peer)
	if !ok {
		return nil
	}
	return resolved
}

// peerAuthor 用缓存里的信息给一个用户、频道或群署名；peer 不是这三种时返回 nil。
func peerAuthor(inv *command.Invocation, peer tg.PeerClass) *quoteFrom {
	switch value := peer.(type) {
	case *tg.PeerUser:
		info, _ := inv.Client.Peers().User(value.UserID)
		return userAuthor(value.UserID, info)
	case *tg.PeerChannel:
		info, _ := inv.Client.Peers().Channel(value.ChannelID)
		return &quoteFrom{ID: value.ChannelID, FirstName: info.Title, Name: info.Title, Username: info.Username}
	case *tg.PeerChat:
		info, _ := inv.Client.Peers().Chat(value.ChatID)
		return &quoteFrom{ID: value.ChatID, FirstName: info.Title, Name: info.Title}
	}
	return nil
}

// userAuthor 给用户署名：名字是姓名，没有姓名时用用户名。
func userAuthor(id int64, info bot.UserInfo) *quoteFrom {
	name := strings.TrimSpace(info.FirstName + " " + info.LastName)
	if name == "" {
		name = info.Username
	}
	return &quoteFrom{ID: id, FirstName: info.FirstName, LastName: info.LastName, Username: info.Username, Name: name}
}

// fakeAuthor 查出 u/ur 指定的伪造发送者的名字。
//
// ResolveTarget 只保证能寻址：access hash 可能来自持久化存储，这时缓存里没有名字和头像，
// 语录上就会是一个空白的作者。原插件用 getEntity 取完整的用户或频道，这里也在缺信息时
// 向 Telegram 查一次；「me」查本账号的最新资料，查不到时退回登录时的那份。
func fakeAuthor(ctx context.Context, inv *command.Invocation, peer tg.InputPeerClass) (*quoteFrom, error) {
	api, peers := inv.Client.API(), inv.Client.Peers()
	switch value := peer.(type) {
	case *tg.InputPeerSelf:
		self := inv.Client.Self()
		if users, err := api.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}}); err == nil {
			peers.RememberUsers(users)
			if len(users) > 0 {
				if fresh, ok := users[0].(*tg.User); ok {
					self = fresh
				}
			}
		}
		return userAuthor(self.ID, bot.UserInfo{FirstName: self.FirstName, LastName: self.LastName, Username: self.Username}), nil
	case *tg.InputPeerUser:
		if _, known := peers.User(value.UserID); !known {
			users, err := api.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUser{UserID: value.UserID, AccessHash: value.AccessHash}})
			if err != nil {
				return nil, err
			}
			peers.RememberUsers(users)
		}
		info, known := peers.User(value.UserID)
		if !known {
			return nil, kit.Fail("查不到这个用户")
		}
		return userAuthor(value.UserID, info), nil
	case *tg.InputPeerChannel:
		if _, known := peers.Channel(value.ChannelID); !known {
			chats, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{&tg.InputChannel{ChannelID: value.ChannelID, AccessHash: value.AccessHash}})
			if err != nil {
				return nil, err
			}
			peers.RememberChats(chats.GetChats())
		}
		if _, known := peers.Channel(value.ChannelID); !known {
			return nil, kit.Fail("查不到这个频道")
		}
		return peerAuthor(inv, &tg.PeerChannel{ChannelID: value.ChannelID}), nil
	case *tg.InputPeerChat:
		if _, known := peers.Chat(value.ChatID); !known {
			chats, err := api.MessagesGetChats(ctx, []int64{value.ChatID})
			if err != nil {
				return nil, err
			}
			peers.RememberChats(chats.GetChats())
		}
		if _, known := peers.Chat(value.ChatID); !known {
			return nil, kit.Fail("查不到这个群")
		}
		return peerAuthor(inv, &tg.PeerChat{ChatID: value.ChatID}), nil
	}
	return nil, kit.Fail("不支持的伪造对象")
}

// 语录中可选部分的时间预算。这些都不是命令的重点：没有头像、没有管理员
// 头衔、没有嵌入媒体，语录照样能渲染。所以每一项的上限都远低于命令本身
// 的期限，超时只会让图片少点东西，而不会把整个命令拖住。
const (
	avatarBudget = 20 * time.Second
	mediaBudget  = 45 * time.Second
	tagBudget    = 10 * time.Second
)

// avatar 把 peer 的头像下载成 data URL；peer 为 nil（比如隐藏了账号的转发来源）
// 或没有头像时返回 nil——没有头像的语录照样能渲染。
func (s *yvluService) avatar(ctx context.Context, inv *command.Invocation, peer tg.InputPeerClass) *quotePhot {
	if peer == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, avatarBudget)
	defer cancel()
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
	case *tg.MessageMediaWebPage:
		// 链接预览的图片：原插件经 teleproto 的 downloadMedia 取到它并嵌进语录。
		// 预览带的文件（视频、动图）不嵌，只用图片。
		if photo, ok := webPagePhoto(message.Raw, value); ok {
			if file, err := inv.Client.DownloadMedia(ctx, photo, 8<<20); err == nil {
				item.Media = &quotePhot{URL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(file.Data)}
			}
		}
	case *tg.MessageMediaDocument:
		document, ok := value.Document.(*tg.Document)
		if !ok {
			return
		}
		s.describeDocument(ctx, inv, message, document, item)
	}
}

// webPagePhoto 把链接预览里的图片包装成一条照片消息，好交给 DownloadMedia；
// 预览没有图片时返回 false。
func webPagePhoto(message *tg.Message, preview *tg.MessageMediaWebPage) (*tg.Message, bool) {
	page, ok := preview.Webpage.(*tg.WebPage)
	if !ok {
		return nil, false
	}
	photo, ok := page.GetPhoto()
	if !ok {
		return nil, false
	}
	value, ok := photo.(*tg.Photo)
	if !ok {
		return nil, false
	}
	media := &tg.MessageMediaPhoto{}
	media.SetPhoto(value)
	wrapped := &tg.Message{ID: message.ID, PeerID: message.PeerID}
	wrapped.SetMedia(media)
	return wrapped, true
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
			waveform = append(waveform, kit.Clamp(int(sample), 0, 31))
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

// forwardAuthor 从转发消息的头部读出原作者，以及该用谁的头像；消息不是转发来的
// 就返回 nil。
//
// 头部可能给出一个 peer，也可能只有一个名字：从隐藏了账号的人那里转发，
// 只留下一个字符串，没有可解析的对象，也就没有头像可用（origin 为 nil）。
// 两种情况都值得署名，所以连名字都没有时也要兜底，让语录保持真实，而不是
// 悄悄算到转发者头上。peer 在缓存里查不到时同样退回头部的名字，但保留它的 ID，
// 和原插件解析失败时的做法一样。
func forwardAuthor(inv *command.Invocation, message *bot.Message) (from *quoteFrom, origin tg.PeerClass) {
	if message.Raw == nil {
		return nil, nil
	}
	header, ok := message.Raw.GetFwdFrom()
	if !ok {
		return nil, nil
	}
	var id int64
	if peer, ok := header.GetFromID(); ok {
		switch value := peer.(type) {
		case *tg.PeerUser:
			if info, known := inv.Client.Peers().User(value.UserID); known {
				return userAuthor(value.UserID, info), peer
			}
			id = value.UserID
		case *tg.PeerChannel:
			if _, known := inv.Client.Peers().Channel(value.ChannelID); known {
				return peerAuthor(inv, peer), peer
			}
			id = value.ChannelID
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
	if id == 0 {
		id = int64(nameHash(name))
	}
	return &quoteFrom{ID: id, FirstName: name, Name: name}, nil
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
	from, _ := forwardAuthor(inv, message)
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
