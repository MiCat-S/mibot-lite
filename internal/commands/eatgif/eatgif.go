// Package eatgif 实现 .eatgif 和 .eat / .eat2：把头像合成表情贴纸，前者是动图，
// 后者是静态图。两者的素材都来自 TeleBox 插件仓库，头像的摆放规则也一样。
package eatgif

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"sort"
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
)

// 动画素材放在 TeleBox 插件仓库里：一份按名字列出各个动画的目录；每个动画
// 有一份列出各帧的 JSON 定义；每一帧有一张底图，外加两个头像贴上去时要套的
// 遮罩。素材第一次用过之后就缓存在磁盘上。
const eatgifRoot = "https://raw.githubusercontent.com/TeleBoxOrg/TeleBox-Plugins/main/eatgif/"

// eatgifRole 描述一个头像在某一帧里的摆放方式。
type eatgifRole struct {
	X          int      `json:"x"`
	Y          int      `json:"y"`
	Mask       string   `json:"mask"`
	Rotate     *float64 `json:"rotate,omitempty"`
	Brightness *float64 `json:"brightness,omitempty"`
}

type eatgifFrame struct {
	URL   string      `json:"url"`
	Delay *int        `json:"delay,omitempty"`
	Me    *eatgifRole `json:"me,omitempty"`
	You   *eatgifRole `json:"you,omitempty"`
}

type eatgifSpec struct {
	Width  int           `json:"width"`
	Height int           `json:"height"`
	Frames []eatgifFrame `json:"res"`
}

type eatgifEntry struct {
	URL  string `json:"url"`
	Desc string `json:"desc"`
}

type eatgifService struct {
	a       *app.App
	mu      sync.Mutex
	catalog map[string]eatgifEntry
	running bool
}

// safeRelative 拒绝可能跑出缓存目录或素材根路径的素材路径。这里的每个路径
// 都来自远程的 JSON 文档，属于不可信输入。
func safeRelative(value string) (string, error) {
	if value == "" || strings.Contains(value, "\\") || strings.Contains(value, "://") {
		return "", kit.Fail("素材路径无效")
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return "", kit.Fail("素材路径无效")
		}
	}
	return value, nil
}

// asset 获取远程素材，磁盘上已有副本时直接复用。
func (s *eatgifService) asset(ctx context.Context, relative string, limit int64) ([]byte, error) {
	clean, err := safeRelative(relative)
	if err != nil {
		return nil, err
	}
	return fetchCached(ctx, s.a, "eatgif", clean, eatgifRoot+clean, limit)
}

// fetchCached 下载 url，缓存在 data/<directory>/ 下，文件名由 key 的哈希加上
// key 的扩展名构成；磁盘上已有就直接用。
func fetchCached(ctx context.Context, a *app.App, directory, key, url string, limit int64) ([]byte, error) {
	digest := sha256.Sum256([]byte(key))
	cache := filepath.Join(a.DataDir(), directory, hex.EncodeToString(digest[:])+filepath.Ext(key))
	if data, err := os.ReadFile(cache); err == nil && len(data) > 0 && int64(len(data)) <= limit {
		return data, nil
	}
	response, err := httpx.Do(ctx, httpx.Request{URL: url, Timeout: 30 * time.Second, MaxBytes: limit})
	if err != nil {
		return nil, err
	}
	if !response.OK() {
		return nil, &httpx.StatusError{Status: response.Status}
	}
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		return nil, err
	}
	// 以原子方式落盘：同时执行的两条命令绝不能读到写了一半的素材。
	temporary := cache + ".tmp"
	if err := os.WriteFile(temporary, response.Body, 0o600); err == nil {
		_ = os.Rename(temporary, cache)
	}
	return response.Body, nil
}

func (s *eatgifService) assetJSON(ctx context.Context, relative string, out any) error {
	data, err := s.asset(ctx, relative, 1<<20)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return kit.Fail("素材配置无效")
	}
	return nil
}

func (s *eatgifService) getCatalog(ctx context.Context) (map[string]eatgifEntry, error) {
	s.mu.Lock()
	cached := s.catalog
	s.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	catalog := map[string]eatgifEntry{}
	if err := s.assetJSON(ctx, "config.json", &catalog); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.catalog = catalog
	s.mu.Unlock()
	return catalog, nil
}

// paste 给一个头像套上它的遮罩，再按定义里指定的位置合成到画布上。
func (s *eatgifService) paste(ctx context.Context, canvas *image.RGBA, role *eatgifRole, face image.Image) error {
	maskData, err := s.asset(ctx, role.Mask, 5<<20)
	if err != nil {
		return err
	}
	return pasteWithMask(canvas, role, face, maskData)
}

// pasteWithMask 把头像缩放到遮罩大小，按需旋转、调亮度，套上遮罩后贴到 (X, Y)。
func pasteWithMask(canvas *image.RGBA, role *eatgifRole, face image.Image, maskData []byte) error {
	mask, err := imaging.DecodePNG(maskData)
	if err != nil {
		return err
	}
	bounds := mask.Bounds()
	if bounds.Dx() > 512 || bounds.Dy() > 512 {
		return kit.Fail("素材遮罩尺寸异常")
	}
	shaped := image.Image(imaging.Resize(face, bounds.Dx(), bounds.Dy()))
	if role.Rotate != nil && *role.Rotate != 0 {
		shaped = imaging.Rotate(shaped, clampFloat(*role.Rotate, -360, 360))
	}
	if role.Brightness != nil && *role.Brightness != 1 {
		shaped = imaging.Brightness(shaped, clampFloat(*role.Brightness, 0.1, 2))
	}
	imaging.Composite(canvas, imaging.ApplyMask(shaped, mask), role.X, role.Y)
	return nil
}

func clampFloat(value, low, high float64) float64 {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func eatgifHelp(prefix string) string {
	p := command.Escape(prefix)
	return "🎬 <b>头像动图表情</b>\n\n回复一条消息（用户或频道发的都可以），把双方头像合成为动画贴纸。\n\n• <code>" + p + "eatgif 名称</code> 生成\n• <code>" + p +
		"eatgif list</code> 列出全部可用动画\n• <code>" + p + "eatgif clear</code> 清空素材缓存\n\n素材首次使用时从远程下载并缓存，需要主机装有 ffmpeg。"
}

// Register 注册 .eatgif、.eat 和 .eat2。
func Register(a *app.App) {
	registerEat(a)
	service := &eatgifService{a: a}
	a.Registry.Register(&command.Command{Name: "eatgif", Description: "将双方头像合成为动画贴纸", Usage: "名称", Help: eatgifHelp, Timeout: 5 * time.Minute,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			sub := strings.ToLower(inv.Arg(0))
			if sub == "clear" {
				if err := os.RemoveAll(filepath.Join(a.DataDir(), "eatgif")); err != nil {
					return err
				}
				service.mu.Lock()
				service.catalog = nil
				service.mu.Unlock()
				return inv.EditText(ctx, "缓存已清理并将在下次请求时刷新")
			}
			catalog, err := service.getCatalog(ctx)
			if err != nil {
				return inv.Edit(ctx, "❌ 无法读取素材列表："+command.Escape(httpx.Reason(err)))
			}
			if sub == "" || sub == "list" || sub == "ls" || sub == "help" || sub == "h" {
				names := make([]string, 0, len(catalog))
				for name := range catalog {
					names = append(names, name)
				}
				sort.Strings(names)
				lines := []string{"<b>头像动图表情</b>", command.Code(inv.Prefix+"eatgif 名称") + "（需回复目标）", ""}
				for _, name := range names {
					lines = append(lines, "• "+command.Code(name)+" - "+command.Escape(catalog[name].Desc))
				}
				return kit.SendPages(ctx, inv, command.HTMLPages(strings.Join(lines, "\n"), 3800))
			}
			selected, ok := catalog[sub]
			if !ok {
				return inv.Edit(ctx, "未找到："+command.Code(sub))
			}
			reply, err := inv.Client.GetReply(ctx, inv.Message)
			if err != nil {
				return err
			}
			if reply == nil {
				return inv.EditText(ctx, "请回复一条消息后再生成，用户或频道发的都可以")
			}

			// 一次只跑一个：每次运行都要解码几十帧、再 fork 一个 ffmpeg，
			// 而这个程序的宗旨就是保持轻量。
			service.mu.Lock()
			if service.running {
				service.mu.Unlock()
				return inv.EditText(ctx, "已有一个动图正在生成，请稍候")
			}
			service.running = true
			service.mu.Unlock()
			defer func() { service.mu.Lock(); service.running = false; service.mu.Unlock() }()

			if err := inv.EditText(ctx, "正在生成："+selected.Desc); err != nil {
				return err
			}
			var spec eatgifSpec
			if err := service.assetJSON(ctx, selected.URL, &spec); err != nil {
				return inv.Edit(ctx, "❌ 无法读取动画定义："+command.Escape(httpx.Reason(err)))
			}
			if spec.Width < 1 || spec.Height < 1 || spec.Width > 512 || spec.Height > 512 || len(spec.Frames) < 1 || len(spec.Frames) > 60 {
				return inv.EditText(ctx, "❌ 动画定义无效")
			}

			faces, err := service.faces(ctx, inv, reply)
			if err != nil {
				if text, ok := kit.IsUserError(err); ok {
					return inv.EditText(ctx, "❌ "+text)
				}
				return err
			}

			directory, err := os.MkdirTemp("", "mibot-eatgif-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(directory)

			frames := make([]media.Frame, 0, len(spec.Frames))
			for index, entry := range spec.Frames {
				if err := ctx.Err(); err != nil {
					return err
				}
				canvasData, err := service.asset(ctx, entry.URL, 5<<20)
				if err != nil {
					return inv.Edit(ctx, "❌ 素材下载失败："+command.Escape(httpx.Reason(err)))
				}
				decoded, err := imaging.DecodePNG(canvasData)
				if err != nil {
					return inv.EditText(ctx, "❌ 素材图片无效")
				}
				canvas := imaging.ToRGBA(decoded)
				// 先贴被回复者的头像，再贴本账号的，与素材定义编写时的顺序一致。
				if entry.You != nil {
					if err := service.paste(ctx, canvas, entry.You, faces.you); err != nil {
						return inv.Edit(ctx, "❌ 合成失败："+command.Escape(httpx.Reason(err)))
					}
				}
				if entry.Me != nil {
					if err := service.paste(ctx, canvas, entry.Me, faces.me); err != nil {
						return inv.Edit(ctx, "❌ 合成失败："+command.Escape(httpx.Reason(err)))
					}
				}
				path := filepath.Join(directory, fmt.Sprintf("frame%04d.png", index))
				file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
				if err != nil {
					return err
				}
				err = imaging.WritePNG(file, canvas)
				if closeErr := file.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					return err
				}
				delay := 100
				if entry.Delay != nil {
					delay = kit.Clamp(*entry.Delay, 20, 5000)
				}
				frames = append(frames, media.Frame{Path: path, Delay: time.Duration(delay) * time.Millisecond})
			}

			webm, err := media.StickerWebM(ctx, directory, frames, spec.Width, spec.Height)
			if err != nil {
				return inv.Edit(ctx, "❌ 视频编码失败："+command.Escape(command.Brief(err)))
			}
			peer, err := inv.Client.InputPeer(inv.Message.Peer)
			if err != nil {
				return err
			}
			options := bot.DocumentOptions{Name: "sticker.webm", MimeType: "video/webm", ReplyTo: inv.Message.ReplyToID,
				Attributes: []tg.DocumentAttributeClass{
					&tg.DocumentAttributeSticker{Alt: "✨", Stickerset: &tg.InputStickerSetEmpty{}},
					&tg.DocumentAttributeImageSize{W: spec.Width, H: spec.Height},
				}}
			if err := inv.Client.SendDocumentWith(ctx, peer, webm, options); err != nil {
				return err
			}
			return inv.Client.DeleteMessage(ctx, inv.Message)
		}})
}

// avatarPair 是一个动画要合成的两张头像。
type avatarPair struct{ me, you image.Image }

// faces 下载并解码双方的头像。
//
// 「对方」是被回复消息的发送者，用户、频道、群都可以：以频道身份发言、
// 频道推到讨论组的消息、匿名管理员，发送者都不是用户，以前一律被拒。
// 「自己」见 ownFace。
func (s *eatgifService) faces(ctx context.Context, inv *command.Invocation, reply *bot.Message) (*avatarPair, error) {
	me, err := loadFace(ctx, inv.Client, ownFace(inv), "你")
	if err != nil {
		return nil, err
	}
	if reply.Sender == nil {
		return nil, kit.Fail("看不出被回复的消息是谁发的")
	}
	peer, err := inv.Client.InputPeer(reply.Sender)
	if err != nil {
		return nil, kit.Fail("无法解析对方的身份")
	}
	you, err := loadFace(ctx, inv.Client, peer, "对方")
	if err != nil {
		return nil, err
	}
	return &avatarPair{me: me, you: you}, nil
}

// ownFace 是「自己」那张头像该用谁的：别人借用账号（.sudo、.sure）时用借用者的，
// 和 MiBox 一样；否则是发命令的身份。
func ownFace(inv *command.Invocation) tg.InputPeerClass {
	if inv.Trigger != nil && inv.Trigger.Sender != nil {
		if peer, err := inv.Client.InputPeer(inv.Trigger.Sender); err == nil {
			return peer
		}
	}
	return speakerOf(inv.Client, inv.Message)
}

// speakerOf 是一条自己发的消息以谁的身份发出：以频道身份发言时是那个频道，否则是本人。
func speakerOf(client *bot.Client, message *bot.Message) tg.InputPeerClass {
	if channel, ok := message.Sender.(*tg.PeerChannel); ok {
		if peer, err := client.InputPeer(channel); err == nil {
			return peer
		}
	}
	return &tg.InputPeerSelf{}
}

// loadFace 下载并解码一个头像。先下小图，下不了再下大图：小图拿不到的对象，往往还能拿到大图。
func loadFace(ctx context.Context, client *bot.Client, peer tg.InputPeerClass, who string) (image.Image, error) {
	data, err := client.DownloadProfilePhoto(ctx, peer, false, 2<<20)
	if err != nil || len(data) == 0 {
		data, err = client.DownloadProfilePhoto(ctx, peer, true, 2<<20)
	}
	if err != nil {
		return nil, kit.Failf("无法获取%s的头像", who)
	}
	if len(data) == 0 {
		return nil, kit.Failf("%s没有公开头像，合成不了", who)
	}
	decoded, err := imaging.Decode(data)
	if err != nil {
		return nil, kit.Failf("无法解析%s的头像", who)
	}
	return decoded, nil
}
