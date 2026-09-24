package eatgif

import (
	"context"
	"encoding/json"
	"image"
	"math"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
	"github.com/MiCat-S/mibot-lite/internal/imaging"
	"github.com/MiCat-S/mibot-lite/internal/media"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// eatSource 是 .eat 默认的素材目录：一份 JSON，列出每款表情的底图、遮罩和头像位置。
// 素材路径多数相对于仓库根目录，少数是别的 GitHub 仓库的完整链接。
const eatSource = "https://raw.githubusercontent.com/TeleBoxOrg/TeleBox-Plugins/main/eat/config.json"

// eatStamp 是「印章」款式：头像铺满画布，素材图旋转、半透明后盖在正中间。
type eatStamp struct {
	Size    *int     `json:"size,omitempty"`
	Scale   *float64 `json:"scale,omitempty"`
	Rotate  *float64 `json:"rotate,omitempty"`
	Opacity *float64 `json:"opacity,omitempty"`
}

// eatEntry 是一款表情。有 Stamp 时按印章合成，否则把 You（被回复者）和 Me（自己）
// 的头像贴到底图上。
type eatEntry struct {
	Name  string      `json:"name"`
	URL   string      `json:"url"`
	Me    *eatgifRole `json:"me,omitempty"`
	You   *eatgifRole `json:"you,omitempty"`
	Stamp *eatStamp   `json:"stamp,omitempty"`
}

// eatSettings 存在 data/eat.json：用 .eat set 换过的素材目录。
type eatSettings struct {
	Source string `json:"source,omitempty"`
}

type eatService struct {
	a        *app.App
	settings *store.Store[eatSettings]
	mu       sync.Mutex
	catalog  map[string]eatEntry
	root     string
}

var eatKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// eatRoot 从素材目录的链接推出仓库根目录，相对路径都相对于它。
// 只接受 raw.githubusercontent.com/<用户>/<仓库>/<分支>/... 和带 refs/heads 的写法。
func eatRoot(source string) (string, error) {
	parsed, err := url.Parse(source)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "raw.githubusercontent.com" || parsed.User != nil {
		return "", kit.Fail("素材目录只能是 raw.githubusercontent.com 上的链接")
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	keep := 3
	if len(parts) > 4 && parts[2] == "refs" && parts[3] == "heads" {
		keep = 5
	}
	if len(parts) <= keep {
		return "", kit.Fail("素材目录链接不完整，要指向仓库里的 config.json")
	}
	return "https://raw.githubusercontent.com/" + strings.Join(parts[:keep], "/") + "/", nil
}

// eatAssetURL 把素材目录里写的路径变成下载地址。完整链接只放行 GitHub；
// 相对路径和 eatgif 一样检查，不能跳出仓库。
func eatAssetURL(root, value string) (string, error) {
	if strings.HasPrefix(value, "https://") {
		parsed, err := url.Parse(value)
		if err != nil || parsed.User != nil || (parsed.Host != "github.com" && parsed.Host != "raw.githubusercontent.com") {
			return "", kit.Fail("素材链接只能指向 GitHub")
		}
		return value, nil
	}
	clean, err := safeRelative(value)
	if err != nil {
		return "", err
	}
	return root + clean, nil
}

func (s *eatService) source() string {
	if current, err := s.settings.Read(); err == nil && current.Source != "" {
		return current.Source
	}
	return eatSource
}

// load 读取素材目录，force 时丢掉内存里的副本重新下载。
func (s *eatService) load(ctx context.Context, force bool) (map[string]eatEntry, string, error) {
	s.mu.Lock()
	catalog, root := s.catalog, s.root
	s.mu.Unlock()
	if catalog != nil && !force {
		return catalog, root, nil
	}
	source := s.source()
	root, err := eatRoot(source)
	if err != nil {
		return nil, "", err
	}
	response, err := httpx.Do(ctx, httpx.Request{URL: source, Timeout: 30 * time.Second, MaxBytes: 1 << 20})
	if err != nil {
		return nil, "", err
	}
	if !response.OK() {
		return nil, "", &httpx.StatusError{Status: response.Status}
	}
	var document struct {
		Resources map[string]eatEntry `json:"resources"`
	}
	if json.Unmarshal(response.Body, &document) != nil || len(document.Resources) == 0 || len(document.Resources) > 200 {
		return nil, "", kit.Fail("素材目录格式不对")
	}
	for key, entry := range document.Resources {
		if !eatKey.MatchString(key) || entry.Name == "" || len([]rune(entry.Name)) > 100 || entry.URL == "" {
			return nil, "", kit.Fail("素材目录格式不对")
		}
	}
	s.mu.Lock()
	s.catalog, s.root = document.Resources, root
	s.mu.Unlock()
	return document.Resources, root, nil
}

func (s *eatService) asset(ctx context.Context, root, value string) ([]byte, error) {
	link, err := eatAssetURL(root, value)
	if err != nil {
		return nil, err
	}
	return fetchCached(ctx, s.a, "eat", link, link, 10<<20)
}

// compose 按一款表情的定义合成图片。you 是被回复者的头像（.eat2 时是被回复的图片），
// me 只在这款表情用得到时才去取。
func (s *eatService) compose(ctx context.Context, root string, entry eatEntry, you image.Image, me func() (image.Image, error)) (image.Image, error) {
	baseData, err := s.asset(ctx, root, entry.URL)
	if err != nil {
		return nil, err
	}
	base, err := imaging.Decode(baseData)
	if err != nil {
		return nil, kit.Fail("素材图片无效")
	}
	if entry.Stamp != nil {
		return stamp(entry.Stamp, you, base), nil
	}
	canvas := imaging.ToRGBA(base)
	// 先贴被回复者，再贴自己，与素材编写时的顺序一致。
	for _, pair := range []struct {
		role *eatgifRole
		face func() (image.Image, error)
	}{{entry.You, func() (image.Image, error) { return you, nil }}, {entry.Me, me}} {
		if pair.role == nil {
			continue
		}
		face, err := pair.face()
		if err != nil {
			return nil, err
		}
		maskData, err := s.asset(ctx, root, pair.role.Mask)
		if err != nil {
			return nil, err
		}
		if err := pasteWithMask(canvas, pair.role, face, maskData); err != nil {
			return nil, err
		}
	}
	return canvas, nil
}

// stamp 合成印章款式：头像按 size 铺成正方形，素材缩到 size*scale 宽，
// 旋转、调成半透明，再缩放到正好放进画布，盖在正中间。
func stamp(config *eatStamp, face, mark image.Image) image.Image {
	size := 512
	if config.Size != nil {
		size = kit.Clamp(*config.Size, 64, 512)
	}
	scale, rotate, opacity := 0.9, -12.0, 0.6
	if config.Scale != nil {
		scale = clampFloat(*config.Scale, 0.05, 4)
	}
	if config.Rotate != nil {
		rotate = clampFloat(*config.Rotate, -360, 360)
	}
	if config.Opacity != nil {
		opacity = clampFloat(*config.Opacity, 0, 1)
	}
	canvas := imaging.ResizeCover(face, size, size)
	bounds := mark.Bounds()
	width := max(1, int(math.Round(float64(size)*scale)))
	height := max(1, int(math.Round(float64(bounds.Dy())*float64(width)/float64(bounds.Dx()))))
	turned := imaging.Opacity(imaging.RotateExpand(imaging.Resize(mark, width, height), rotate), opacity)
	fit := math.Min(float64(size)/float64(turned.Bounds().Dx()), float64(size)/float64(turned.Bounds().Dy()))
	fitted := imaging.Resize(turned, max(1, int(float64(turned.Bounds().Dx())*fit)), max(1, int(float64(turned.Bounds().Dy())*fit)))
	imaging.Composite(canvas, fitted, (size-fitted.Bounds().Dx())/2, (size-fitted.Bounds().Dy())/2)
	return canvas
}

// repliedImage 取被回复消息里的图片（.eat2）：照片、静态贴纸、图片文件直接用；
// 视频贴纸、动态贴纸、视频用它们的缩略图。
func repliedImage(ctx context.Context, client *bot.Client, reply *bot.Message) (image.Image, error) {
	if reply.Raw == nil {
		return nil, kit.Fail("请回复一条图片或贴纸")
	}
	if content, ok := reply.Raw.GetMedia(); ok {
		if value, ok := content.(*tg.MessageMediaDocument); ok {
			if document, ok := value.Document.(*tg.Document); ok && !strings.HasPrefix(document.MimeType, "image/") {
				return documentThumb(ctx, client, document)
			}
		}
	}
	file, err := client.DownloadMedia(ctx, reply.Raw, 10<<20)
	if err != nil {
		return nil, kit.Fail("请回复一条图片或贴纸")
	}
	decoded, err := imaging.Decode(file.Data)
	if err != nil {
		return nil, kit.Fail("这张图片解析不了")
	}
	return decoded, nil
}

// documentThumb 下载文档最大的一张缩略图。
func documentThumb(ctx context.Context, client *bot.Client, document *tg.Document) (image.Image, error) {
	best, area := "", 0
	for _, thumb := range document.Thumbs {
		if size, ok := thumb.(*tg.PhotoSize); ok && size.W*size.H > area {
			best, area = size.Type, size.W*size.H
		}
	}
	if best == "" {
		return nil, kit.Fail("这个文件没有可用的预览图")
	}
	location := &tg.InputDocumentFileLocation{ID: document.ID, AccessHash: document.AccessHash,
		FileReference: document.FileReference, ThumbSize: best}
	data, err := client.DownloadFile(ctx, location, document.DCID, 5<<20)
	if err != nil {
		return nil, kit.Fail("预览图下载失败")
	}
	decoded, err := imaging.Decode(data)
	if err != nil {
		return nil, kit.Fail("预览图解析不了")
	}
	return decoded, nil
}

// eatCatalogLines 按名称排序列出每一款：名称、中文名，用到自己头像的标出来。
func eatCatalogLines(catalog map[string]eatEntry) []string {
	keys := make([]string, 0, len(catalog))
	for key := range catalog {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		entry := catalog[key]
		line := "• " + command.Code(key) + " " + command.Escape(entry.Name)
		switch {
		case entry.Me != nil && entry.You != nil:
			line += " 👥"
		case entry.Me != nil:
			line += " 🙋"
		}
		lines = append(lines, line)
	}
	return lines
}

// eatLegend 解释列表里的标记。
const eatLegend = "👥 同时用对方和你的头像　🙋 只用你的头像　其余只用对方的"

func eatList(catalog map[string]eatEntry, prefix, name string) string {
	lines := []string{"<b>头像表情包</b>（" + strconv.Itoa(len(catalog)) + " 款）",
		"回复一条消息发 " + command.Code(prefix+name+" 名称") + "，不写名称随机挑一款", eatLegend, ""}
	return strings.Join(append(lines, eatCatalogLines(catalog)...), "\n")
}

// help 是 .eat 的帮助：用法，加上当前素材目录里的全部款式。目录读不到时只给用法。
func (s *eatService) help(prefix string) string {
	p := command.Escape(prefix)
	text := "😋 <b>头像表情包</b>\n\n把头像合成到表情图里，发成贴纸。\n\n" +
		"<b>用法</b>\n" +
		"• 回复一条消息发 <code>" + p + "eat 名称</code>，用对方的头像生成，比如 <code>" + p + "eat bc</code>；不写名称随机挑一款\n" +
		"• <code>" + p + "eat2 名称</code> 同上，但用被回复的图片或贴纸代替头像\n" +
		"• 不回复消息发 <code>" + p + "eat</code> 也能看到下面这份列表\n" +
		"• <code>" + p + "eat set</code> 重新下载素材目录；<code>" + p + "eat set 链接</code> 换成别的目录" +
		"（要是 raw.githubusercontent.com 上的 config.json），<code>" + p + "eat set default</code> 换回默认\n\n"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	catalog, _, err := s.load(ctx, false)
	if err != nil {
		return text + "素材目录暂时读不到（" + command.Escape(httpx.Reason(err)) + "），稍后发 <code>" + p + "eat</code> 查看全部款式。"
	}
	lines := []string{"<b>全部款式</b>（" + strconv.Itoa(len(catalog)) + " 款）", eatLegend}
	lines = append(lines, eatCatalogLines(catalog)...)
	return text + strings.Join(lines, "\n") + "\n\n素材来自 TeleBox 插件仓库，首次使用时下载并缓存，需要主机装有 ffmpeg。"
}

// stickerOptions 是把一张 WebP 当贴纸发出去的参数：不属于任何贴纸包，alt 用款式名。
func stickerOptions(alt string, webp []byte, replyTo int) bot.DocumentOptions {
	width, height, _ := imaging.WebPSize(webp)
	return bot.DocumentOptions{Name: "sticker.webp", MimeType: "image/webp", ReplyTo: replyTo,
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeSticker{Alt: alt, Stickerset: &tg.InputStickerSetEmpty{}},
			&tg.DocumentAttributeImageSize{W: width, H: height},
		}}
}

// registerEat 注册 .eat 和 .eat2。
func registerEat(a *app.App) {
	service := &eatService{a: a, settings: kit.NewStore(a, "eat.json", func() eatSettings { return eatSettings{} })}
	handle := func(fromImage bool) func(context.Context, *command.Invocation) error {
		name := map[bool]string{false: "eat", true: "eat2"}[fromImage]
		return func(ctx context.Context, inv *command.Invocation) error {
			sub := inv.Arg(0)
			if strings.EqualFold(sub, "help") || strings.EqualFold(sub, "h") {
				return kit.SendPages(ctx, inv, command.HTMLPages(service.help(inv.Prefix), 3800))
			}
			if strings.EqualFold(sub, "set") && inv.Message.ReplyToID == 0 {
				return service.set(ctx, inv, name, inv.Arg(1))
			}
			reply, err := inv.Client.GetReply(ctx, inv.Message)
			if err != nil {
				return err
			}
			catalog, root, err := service.load(ctx, false)
			if err != nil {
				return inv.Edit(ctx, "❌ 读不到素材目录："+command.Escape(httpx.Reason(err)))
			}
			if reply == nil {
				return kit.SendPages(ctx, inv, command.HTMLPages(eatList(catalog, inv.Prefix, name), 3800))
			}
			key := sub
			if key == "" {
				keys := make([]string, 0, len(catalog))
				for candidate := range catalog {
					keys = append(keys, candidate)
				}
				sort.Strings(keys)
				key = keys[rand.IntN(len(keys))]
			}
			entry, ok := catalog[key]
			if !ok {
				return inv.Edit(ctx, "找不到 "+command.Code(key)+"，不回复消息发 "+command.Code(inv.Prefix+name)+" 看全部款式")
			}
			if err := inv.EditText(ctx, "正在生成「"+entry.Name+"」…"); err != nil {
				return err
			}
			result, err := service.render(ctx, inv, reply, root, entry, fromImage)
			if err != nil {
				if text, ok := kit.IsUserError(err); ok {
					return inv.EditText(ctx, "❌ "+text)
				}
				return inv.Edit(ctx, "❌ 生成失败："+command.Escape(httpx.Reason(err)))
			}
			peer, err := inv.Client.InputPeer(inv.Message.Peer)
			if err != nil {
				return err
			}
			if err := inv.Client.SendDocumentWith(ctx, peer, result, stickerOptions(entry.Name, result, inv.Message.ReplyToID)); err != nil {
				return err
			}
			return inv.Client.DeleteMessage(ctx, inv.Message)
		}
	}
	a.Registry.Register(
		&command.Command{Name: "eat", Description: "用头像生成表情包", Usage: "[名称]", Help: service.help, Timeout: 2 * time.Minute, Handle: handle(false)},
		&command.Command{Name: "eat2", Description: "用图片生成表情包", Usage: "[名称]", Help: service.help, Timeout: 2 * time.Minute, Handle: handle(true)},
	)
}

// render 取头像、合成、编码成 WebP 贴纸。
func (s *eatService) render(ctx context.Context, inv *command.Invocation, reply *bot.Message, root string, entry eatEntry, fromImage bool) ([]byte, error) {
	var you image.Image
	var err error
	if fromImage {
		you, err = repliedImage(ctx, inv.Client, reply)
	} else {
		if reply.Sender == nil {
			return nil, kit.Fail("看不出被回复的消息是谁发的")
		}
		peer, perr := inv.Client.InputPeer(reply.Sender)
		if perr != nil {
			return nil, kit.Fail("无法解析对方的身份")
		}
		you, err = loadFace(ctx, inv.Client, peer, "对方")
	}
	if err != nil {
		return nil, err
	}
	me := func() (image.Image, error) { return loadFace(ctx, inv.Client, ownFace(inv), "你") }
	composed, err := s.compose(ctx, root, entry, you, me)
	if err != nil {
		return nil, err
	}
	png, err := imaging.EncodePNG(stickerSize(composed))
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", "mibot-eat-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	return media.StickerWebP(ctx, directory, png)
}

// stickerSize 把成品等比缩放到长边正好 512：素材有大有小，贴纸包要求一边是 512，
// 这样发出去的贴纸也能用 .sticker 存进包里。
func stickerSize(source image.Image) image.Image {
	bounds := source.Bounds()
	longest := max(bounds.Dx(), bounds.Dy())
	if longest == 512 {
		return source
	}
	scale := 512 / float64(longest)
	return imaging.Resize(source, max(1, int(math.Round(float64(bounds.Dx())*scale))), max(1, int(math.Round(float64(bounds.Dy())*scale))))
}

// set 重新下载素材目录，可以顺带换一个来源。
func (s *eatService) set(ctx context.Context, inv *command.Invocation, name, link string) error {
	if link != "" {
		source := link
		if strings.EqualFold(link, "default") {
			source = ""
		} else if _, err := eatRoot(link); err != nil {
			text, _ := kit.IsUserError(err)
			return inv.EditText(ctx, "❌ "+text)
		}
		if err := s.settings.Update(func(settings *eatSettings) error { settings.Source = source; return nil }); err != nil {
			return err
		}
	}
	if err := inv.EditText(ctx, "正在重新下载素材目录…"); err != nil {
		return err
	}
	catalog, _, err := s.load(ctx, true)
	if err != nil {
		return inv.Edit(ctx, "❌ 读不到素材目录："+command.Escape(httpx.Reason(err)))
	}
	// 仓库里的图可能也更新过，清掉素材缓存，下次用到时重新下载。
	_ = os.RemoveAll(filepath.Join(s.a.DataDir(), "eat"))
	return kit.SendPages(ctx, inv, command.HTMLPages("✅ 素材目录已更新\n"+eatList(catalog, inv.Prefix, name), 3800))
}
