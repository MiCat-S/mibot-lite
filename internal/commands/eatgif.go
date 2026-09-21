package commands

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
	"github.com/MiCat-S/mibot-lite/internal/httpx"
	"github.com/MiCat-S/mibot-lite/internal/imaging"
	"github.com/MiCat-S/mibot-lite/internal/media"
)

// The animation assets live in the TeleBox plugin repository: a catalog of
// named animations, each a JSON spec listing frames, and for every frame a
// canvas image plus the masks the two avatars are pasted through. Assets
// are cached on disk after the first use.
const eatgifRoot = "https://raw.githubusercontent.com/TeleBoxOrg/TeleBox-Plugins/main/eatgif/"

// eatgifRole places one avatar in one frame.
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

// safeRelative refuses an asset path that could escape the cache directory
// or the asset root. Every path here comes from a remote JSON document, so
// it is untrusted input.
func safeRelative(value string) (string, error) {
	if value == "" || strings.Contains(value, "\\") || strings.Contains(value, "://") {
		return "", fail("素材路径无效")
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fail("素材路径无效")
		}
	}
	return value, nil
}

// asset fetches a remote asset, reusing the on-disk copy when there is one.
func (s *eatgifService) asset(ctx context.Context, relative string, limit int64) ([]byte, error) {
	clean, err := safeRelative(relative)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(clean))
	cache := filepath.Join(s.a.DataDir(), "eatgif", hex.EncodeToString(digest[:])+filepath.Ext(clean))
	if data, err := os.ReadFile(cache); err == nil && len(data) > 0 && int64(len(data)) <= limit {
		return data, nil
	}
	response, err := httpx.Do(ctx, httpx.Request{URL: eatgifRoot + clean, Timeout: 30 * time.Second, MaxBytes: limit})
	if err != nil {
		return nil, err
	}
	if !response.OK() {
		return nil, &httpx.StatusError{Status: response.Status}
	}
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		return nil, err
	}
	// Publish atomically: two simultaneous commands must never read a
	// half-written asset.
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
		return fail("素材配置无效")
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

// paste renders one avatar through its mask and composites it onto the
// canvas at the position the spec names.
func (s *eatgifService) paste(ctx context.Context, canvas *image.RGBA, role *eatgifRole, face image.Image) error {
	maskData, err := s.asset(ctx, role.Mask, 5<<20)
	if err != nil {
		return err
	}
	mask, err := imaging.DecodePNG(maskData)
	if err != nil {
		return err
	}
	bounds := mask.Bounds()
	if bounds.Dx() > 512 || bounds.Dy() > 512 {
		return fail("素材遮罩尺寸异常")
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
	return "🎬 <b>头像动图表情</b>\n\n回复一个用户的消息，把双方头像合成为动画贴纸。\n\n• <code>" + p + "eatgif 名称</code> 生成\n• <code>" + p +
		"eatgif list</code> 列出全部可用动画\n• <code>" + p + "eatgif clear</code> 清空素材缓存\n\n素材首次使用时从远程下载并缓存，需要主机装有 ffmpeg。"
}

// Eatgif registers .eatgif.
func Eatgif(a *app.App) {
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
				return sendPages(ctx, inv, command.HTMLPages(strings.Join(lines, "\n"), 3800))
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
				return inv.EditText(ctx, "请回复一个用户的消息后再生成")
			}

			// One at a time: each run decodes dozens of frames and forks
			// ffmpeg, and the point of this program is to stay small.
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
				if text, ok := isUserError(err); ok {
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
				// The reply's avatar goes down first, then the account's,
				// matching the order the specs are authored in.
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
					delay = clampInt(*entry.Delay, 20, 5000)
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

// avatarPair is the two faces an animation composites.
type avatarPair struct{ me, you image.Image }

// faces downloads and decodes both avatars.
func (s *eatgifService) faces(ctx context.Context, inv *command.Invocation, reply *bot.Message) (*avatarPair, error) {
	load := func(peer tg.InputPeerClass, who string) (image.Image, error) {
		data, err := inv.Client.DownloadProfilePhoto(ctx, peer, false, 2<<20)
		if err != nil || len(data) == 0 {
			return nil, failf("无法获取%s的头像", who)
		}
		decoded, err := imaging.Decode(data)
		if err != nil {
			return nil, failf("无法解析%s的头像", who)
		}
		return decoded, nil
	}
	me, err := load(&tg.InputPeerSelf{}, "你")
	if err != nil {
		return nil, err
	}
	sender, ok := reply.Sender.(*tg.PeerUser)
	if !ok {
		return nil, fail("请回复一个用户的消息")
	}
	peer, err := inv.Client.InputPeer(sender)
	if err != nil {
		return nil, fail("无法解析对方的身份")
	}
	you, err := load(peer, "对方")
	if err != nil {
		return nil, err
	}
	return &avatarPair{me: me, you: you}, nil
}
