package eatgif

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/app"
)

// ddw 和 jbff 的帧是 JPEG，其余是 PNG，两种都得能当画布。
func TestFrameCanvasDecodesPNGAndJPEG(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 196, 106))
	for index := range source.Pix {
		source.Pix[index] = 200
	}
	var jpegData, pngData bytes.Buffer
	if err := jpeg.Encode(&jpegData, source, nil); err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(&pngData, source); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"JPEG": jpegData.Bytes(), "PNG": pngData.Bytes()} {
		canvas, err := frameCanvas(data)
		if err != nil || canvas.Bounds().Dx() != 196 || canvas.Bounds().Dy() != 106 {
			t.Errorf("%s 帧：%v %v", name, canvas.Bounds(), err)
			continue
		}
		// 画布要能往上贴头像。
		canvas.Set(0, 0, color.RGBA{R: 255, A: 255})
	}
	if _, err := frameCanvas([]byte("not an image")); err == nil {
		t.Error("不是图片的数据应该报错")
	}
}

// 目录和定义过期后要重新下载；下载失败时退回旧副本，完全没有副本才报错。
// context 事先取消，保证这里不会真的联网。
func TestFetchFreshFallsBackToStaleCopy(t *testing.T) {
	a := &app.App{Root: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	const key, url = "config.json", "https://example.invalid/config.json"
	if _, err := fetchFresh(ctx, a, "eatgif", key, url, 1<<20, time.Hour); err == nil {
		t.Fatal("没有缓存又下载不了，应该报错")
	}
	cache := cachePath(a, "eatgif", key)
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte(`{"old":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(cache, stale, stale); err != nil {
		t.Fatal(err)
	}
	data, err := fetchFresh(ctx, a, "eatgif", key, url, 1<<20, time.Hour)
	if err != nil || string(data) != `{"old":1}` {
		t.Errorf("下载失败时应退回旧副本，实际 %q %v", data, err)
	}
	if _, err := fetchFresh(ctx, a, "eatgif", key, url, 4, time.Hour); err == nil {
		t.Error("超过大小上限的旧副本不该用")
	}
}

// 每个素材路径都来自远程的 JSON 文档，因此不可信。
func TestSafeRelative(t *testing.T) {
	for _, good := range []string{"md/md1.png", "config.json", "a/b/c.png"} {
		if _, err := safeRelative(good); err != nil {
			t.Errorf("%q should be accepted: %v", good, err)
		}
	}
	for _, bad := range []string{"", "../secrets", "a/../../b", "a//b", "C:\\x", "https://evil/x", "./x"} {
		if _, err := safeRelative(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}
