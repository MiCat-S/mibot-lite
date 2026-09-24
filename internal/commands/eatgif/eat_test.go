package eatgif

import (
	"image"
	"image/color"
	"testing"
)

func TestEatRoot(t *testing.T) {
	for source, want := range map[string]string{
		"https://raw.githubusercontent.com/TeleBoxOrg/TeleBox-Plugins/main/eat/config.json":            "https://raw.githubusercontent.com/TeleBoxOrg/TeleBox-Plugins/main/",
		"https://raw.githubusercontent.com/TeleBoxOrg/TeleBox-Plugins/refs/heads/main/eat/config.json": "https://raw.githubusercontent.com/TeleBoxOrg/TeleBox-Plugins/refs/heads/main/",
		"https://raw.githubusercontent.com/a/b/dev/config.json":                                        "https://raw.githubusercontent.com/a/b/dev/",
	} {
		if got, err := eatRoot(source); err != nil || got != want {
			t.Errorf("%s → %q %v，应为 %q", source, got, err, want)
		}
	}
	for _, bad := range []string{
		"http://raw.githubusercontent.com/a/b/main/config.json",
		"https://example.com/a/b/main/config.json",
		"https://user@raw.githubusercontent.com/a/b/main/config.json",
		"https://raw.githubusercontent.com/a/b/main",
		"https://raw.githubusercontent.com/a/b/refs/heads/main",
	} {
		if _, err := eatRoot(bad); err == nil {
			t.Errorf("%s 应该被拒", bad)
		}
	}
}

func TestEatAssetURL(t *testing.T) {
	root := "https://raw.githubusercontent.com/o/r/main/"
	for value, want := range map[string]string{
		"eat/eatat.png": root + "eat/eatat.png",
		"https://github.com/FoKit/PagerMaid_Plugins/raw/modify/eat/eatada.png": "https://github.com/FoKit/PagerMaid_Plugins/raw/modify/eat/eatada.png",
	} {
		if got, err := eatAssetURL(root, value); err != nil || got != want {
			t.Errorf("%s → %q %v", value, got, err)
		}
	}
	for _, bad := range []string{"../secret", "eat/../../x", "/etc/passwd", "https://evil.example/x.png", "http://github.com/x.png", "file:///etc/passwd"} {
		if _, err := eatAssetURL(root, bad); err == nil {
			t.Errorf("%s 应该被拒", bad)
		}
	}
}

func solid(width, height int, fill color.RGBA) *image.RGBA {
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	for index := 0; index < len(canvas.Pix); index += 4 {
		canvas.Pix[index], canvas.Pix[index+1], canvas.Pix[index+2], canvas.Pix[index+3] = fill.R, fill.G, fill.B, fill.A
	}
	return canvas
}

// 印章：画布是头像铺满的正方形，正中间盖着半透明的素材，角上还是头像原色。
func TestStamp(t *testing.T) {
	face := solid(300, 200, color.RGBA{R: 255, A: 255})
	mark := solid(400, 100, color.RGBA{B: 255, A: 255})
	result := stamp(&eatStamp{}, face, mark)
	if bounds := result.Bounds(); bounds.Dx() != 512 || bounds.Dy() != 512 {
		t.Fatalf("尺寸 %v，应为 512x512", bounds)
	}
	if r, g, b, _ := result.At(5, 5).RGBA(); r>>8 != 255 || g != 0 || b != 0 {
		t.Errorf("角上应该是头像的红色，实际 %d %d %d", r>>8, g>>8, b>>8)
	}
	r, _, b, _ := result.At(256, 256).RGBA()
	// 60% 不透明的蓝盖在红上：蓝约 153，红约 102。
	if b>>8 < 140 || b>>8 > 165 || r>>8 < 90 || r>>8 > 115 {
		t.Errorf("中心应是半透明的蓝盖在红上，实际 红 %d 蓝 %d", r>>8, b>>8)
	}
	small := 256
	if got := stamp(&eatStamp{Size: &small}, face, mark).Bounds().Dx(); got != 256 {
		t.Errorf("size=256 时宽 %d", got)
	}
}

func TestStickerSize(t *testing.T) {
	for _, c := range []struct{ w, h, wantW, wantH int }{{231, 218, 512, 483}, {1266, 830, 512, 336}, {512, 510, 512, 510}, {300, 600, 256, 512}} {
		got := stickerSize(solid(c.w, c.h, color.RGBA{A: 255})).Bounds()
		if got.Dx() != c.wantW || got.Dy() != c.wantH {
			t.Errorf("%dx%d → %v，应为 %dx%d", c.w, c.h, got, c.wantW, c.wantH)
		}
	}
}
