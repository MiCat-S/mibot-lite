package media

import (
	"testing"

	"github.com/gotd/td/tg"
)

// 格式按文件头认；读不出宽高时按 512×512 填，而不是 teleproto 的 1×1。
func TestStickerDocument(t *testing.T) {
	webm := StickerDocument("quote", []byte{0x1a, 0x45, 0xdf, 0xa3, 0, 0}, "📝", 7)
	if webm.Name != "quote.webm" || webm.MimeType != "video/webm" || webm.ReplyTo != 7 {
		t.Fatalf("webm 参数 %+v", webm)
	}
	var video *tg.DocumentAttributeVideo
	var sticker *tg.DocumentAttributeSticker
	for _, attribute := range webm.Attributes {
		switch value := attribute.(type) {
		case *tg.DocumentAttributeVideo:
			video = value
		case *tg.DocumentAttributeSticker:
			sticker = value
		}
	}
	if sticker == nil || sticker.Alt != "📝" || video == nil || video.W != 512 || video.H != 512 || video.SupportsStreaming {
		t.Errorf("webm 贴纸应带贴纸属性和视频属性，实际 %+v", webm.Attributes)
	}
	webp := StickerDocument("sticker.png", []byte("not a webp"), "✨", 0)
	if webp.Name != "sticker.webp" || webp.MimeType != "image/webp" {
		t.Fatalf("webp 参数 %+v", webp)
	}
	if size, ok := webp.Attributes[1].(*tg.DocumentAttributeImageSize); !ok || size.W != 512 || size.H != 512 || len(webp.Attributes) != 2 {
		t.Errorf("webp 贴纸的属性 %+v", webp.Attributes)
	}
}
