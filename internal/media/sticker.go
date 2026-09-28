package media

import (
	"bytes"
	"strings"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/imaging"
)

// webmMagic 是 EBML 文件头，WebM 都以它开头。
var webmMagic = []byte{0x1a, 0x45, 0xdf, 0xa3}

// StickerDocument 是把一张 WebP 或一段 WebM 当贴纸发出去的参数：不属于任何贴纸包，
// alt 是它配的 emoji。格式按文件头认，宽高和时长也从文件头读；读不到时按贴纸的常见
// 尺寸 512×512 填，Telegram 只拿它们排版。
//
// WebM 带上视频属性：原插件经 teleproto 按 .webm 文件发送，它会自动加上这一项，
// Telegram 自己的客户端发视频贴纸也带。
func StickerDocument(name string, data []byte, alt string, replyTo int) bot.MediaOptions {
	attributes := []tg.DocumentAttributeClass{&tg.DocumentAttributeSticker{Alt: alt, Stickerset: &tg.InputStickerSetEmpty{}}}
	if bytes.HasPrefix(data, webmMagic) {
		width, height, duration, err := WebMInfo(data)
		if err != nil {
			width, height, duration = 512, 512, 0
		}
		attributes = append(attributes, &tg.DocumentAttributeImageSize{W: width, H: height},
			&tg.DocumentAttributeVideo{W: width, H: height, Duration: duration})
		return bot.MediaOptions{Name: withExtension(name, ".webm"), MimeType: "video/webm", ReplyTo: replyTo, Attributes: attributes}
	}
	width, height, err := imaging.WebPSize(data)
	if err != nil {
		width, height = 512, 512
	}
	attributes = append(attributes, &tg.DocumentAttributeImageSize{W: width, H: height})
	return bot.MediaOptions{Name: withExtension(name, ".webp"), MimeType: "image/webp", ReplyTo: replyTo, Attributes: attributes}
}

// withExtension 给 name 换上 extension：调用方只管起名，扩展名跟着实际格式走。
func withExtension(name, extension string) string {
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		name = name[:dot]
	}
	return name + extension
}
