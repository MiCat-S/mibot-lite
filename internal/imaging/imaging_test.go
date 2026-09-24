package imaging

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"testing"
)

func solid(width, height int, c color.RGBA) *image.RGBA {
	value := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			value.SetRGBA(x, y, c)
		}
	}
	return value
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	source := solid(8, 6, color.RGBA{R: 200, G: 100, B: 50, A: 255})
	encoded, err := EncodePNG(source)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePNG(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds().Dx() != 8 || decoded.Bounds().Dy() != 6 {
		t.Fatalf("decoded %v", decoded.Bounds())
	}
	r, g, b, _ := decoded.At(3, 3).RGBA()
	if r>>8 != 200 || g>>8 != 100 || b>>8 != 50 {
		t.Fatalf("colour changed: %d %d %d", r>>8, g>>8, b>>8)
	}
}

// 格式错误或恶意构造的素材必须在读文件头时就被拒绝，
// 不能等到分配像素之后。
func TestDecodeRejectsOversize(t *testing.T) {
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, MaxDimension+1, 1))); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePNG(buffer.Bytes()); err == nil {
		t.Fatal("an oversize PNG should be refused")
	}
	if _, err := DecodePNG([]byte("not a png")); err == nil {
		t.Fatal("garbage should be refused")
	}
}

func TestResizeCoverCropsCentre(t *testing.T) {
	// 宽图缩成正方形时保留完整高度、裁掉两侧，
	// 这样位于中间的脸能保留下来。
	source := image.NewRGBA(image.Rect(0, 0, 200, 100))
	for x := 0; x < 200; x++ {
		shade := color.RGBA{A: 255}
		if x >= 50 && x < 150 {
			shade = color.RGBA{R: 255, A: 255}
		}
		for y := 0; y < 100; y++ {
			source.SetRGBA(x, y, shade)
		}
	}
	result := ResizeCover(source, 50, 50)
	if result.Bounds().Dx() != 50 || result.Bounds().Dy() != 50 {
		t.Fatalf("size %v", result.Bounds())
	}
	if r, _, _, _ := result.At(25, 25).RGBA(); r>>8 < 200 {
		t.Fatalf("the centre was cropped away, red is %d", r>>8)
	}
}

func TestResizeFitKeepsAspectAndNeverUpscales(t *testing.T) {
	result := ResizeFit(image.NewRGBA(image.Rect(0, 0, 1000, 500)), 512, 512)
	if result.Bounds().Dx() != 512 || result.Bounds().Dy() != 256 {
		t.Fatalf("size %v, want 512x256", result.Bounds())
	}
	small := ResizeFit(image.NewRGBA(image.Rect(0, 0, 40, 30)), 512, 512)
	if small.Bounds().Dx() != 40 || small.Bounds().Dy() != 30 {
		t.Fatalf("a smaller image should not be upscaled, got %v", small.Bounds())
	}
}

// 旋转前后画布尺寸必须不变，否则动画配置给出的粘贴位置
// 会落到别处。
func TestRotateKeepsCanvasSize(t *testing.T) {
	result := Rotate(solid(30, 20, color.RGBA{B: 255, A: 255}), 45)
	if result.Bounds().Dx() != 30 || result.Bounds().Dy() != 20 {
		t.Fatalf("size %v", result.Bounds())
	}
}

func TestApplyMaskUsesAlpha(t *testing.T) {
	face := solid(10, 10, color.RGBA{R: 255, A: 255})
	mask := image.NewRGBA(image.Rect(0, 0, 10, 10))
	for y := 0; y < 10; y++ {
		for x := 0; x < 5; x++ {
			mask.SetRGBA(x, y, color.RGBA{A: 255})
		}
	}
	result := ApplyMask(face, mask)
	if _, _, _, alpha := result.At(2, 2).RGBA(); alpha>>8 != 255 {
		t.Fatalf("the masked-in half should be opaque, alpha is %d", alpha>>8)
	}
	if _, _, _, alpha := result.At(8, 2).RGBA(); alpha != 0 {
		t.Fatalf("the masked-out half should be transparent, alpha is %d", alpha>>8)
	}
}

// 预乘像素必须保持合法：任何通道都不能超过它自己的 alpha。
func TestBrightnessClampsToAlpha(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 2, 1))
	source.SetRGBA(0, 0, color.RGBA{R: 100, G: 100, B: 100, A: 128})
	source.SetRGBA(1, 0, color.RGBA{R: 10, G: 10, B: 10, A: 255})
	result := Brightness(source, 4)
	for index := 0; index < len(result.Pix); index += 4 {
		alpha := result.Pix[index+3]
		for channel := 0; channel < 3; channel++ {
			if result.Pix[index+channel] > alpha {
				t.Fatalf("channel %d is %d, above alpha %d", channel, result.Pix[index+channel], alpha)
			}
		}
	}
	if darker := Brightness(source, 0.5); darker.Pix[0] != 50 {
		t.Fatalf("halving 100 gave %d", darker.Pix[0])
	}
}

func TestCompositePlacesOverlay(t *testing.T) {
	canvas := solid(20, 20, color.RGBA{A: 255})
	Composite(canvas, solid(4, 4, color.RGBA{G: 255, A: 255}), 10, 10)
	if _, g, _, _ := canvas.At(11, 11).RGBA(); g>>8 != 255 {
		t.Fatal("overlay did not land at its position")
	}
	if _, g, _, _ := canvas.At(2, 2).RGBA(); g != 0 {
		t.Fatal("overlay leaked outside its rectangle")
	}
}

func TestFlattenDropsTransparency(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 2, 1))
	source.SetRGBA(0, 0, color.RGBA{})
	result := Flatten(source, color.Black)
	if _, _, _, alpha := result.At(0, 0).RGBA(); alpha>>8 != 255 {
		t.Fatalf("flattened pixel alpha is %d", alpha>>8)
	}
}

// Telegram 要求贴纸附带尺寸，而语录服务返回的是 WebP；
// 这里只解析文件头。
func TestWebPSize(t *testing.T) {
	lossy := make([]byte, 30)
	copy(lossy[0:], "RIFF")
	copy(lossy[8:], "WEBPVP8 ")
	lossy[23], lossy[24], lossy[25] = 0x9d, 0x01, 0x2a
	lossy[26], lossy[27] = 0x00, 0x02 // 512
	lossy[28], lossy[29] = 0x00, 0x03 // 768
	width, height, err := WebPSize(lossy)
	if err != nil || width != 512 || height != 768 {
		t.Fatalf("lossy: %dx%d err=%v", width, height, err)
	}

	extended := make([]byte, 30)
	copy(extended[0:], "RIFF")
	copy(extended[8:], "WEBPVP8X")
	extended[24], extended[25], extended[26] = 0xff, 0x01, 0x00 // 511 + 1
	extended[27], extended[28], extended[29] = 0xff, 0x02, 0x00 // 767 + 1
	width, height, err = WebPSize(extended)
	if err != nil || width != 512 || height != 768 {
		t.Fatalf("extended: %dx%d err=%v", width, height, err)
	}

	if _, _, err := WebPSize([]byte("short")); err == nil {
		t.Fatal("a short buffer should not parse")
	}
	if _, _, err := WebPSize(bytes.Repeat([]byte{0}, 40)); err == nil {
		t.Fatal("zeroed bytes should not parse as WebP")
	}
}

// 旋转 90 度后宽高互换，画布正好装下；四个角外面是透明的。
func TestRotateExpand(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 40, 10))
	draw.Draw(source, source.Bounds(), image.NewUniform(color.RGBA{G: 255, A: 255}), image.Point{}, draw.Src)
	turned := RotateExpand(source, 90)
	if bounds := turned.Bounds(); bounds.Dx() < 10 || bounds.Dx() > 11 || bounds.Dy() < 40 || bounds.Dy() > 41 {
		t.Fatalf("旋转 90 度后是 %v，应约为 10x40", bounds)
	}
	diagonal := RotateExpand(source, 45)
	if _, _, _, alpha := diagonal.At(0, 0).RGBA(); alpha != 0 {
		t.Errorf("45 度时左上角应该透明，alpha=%d", alpha)
	}
	if _, g, _, _ := diagonal.At(diagonal.Bounds().Dx()/2, diagonal.Bounds().Dy()/2).RGBA(); g>>8 < 250 {
		t.Errorf("中心应该还是绿色，g=%d", g>>8)
	}
}

func TestOpacity(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 2, 2))
	draw.Draw(source, source.Bounds(), image.NewUniform(color.RGBA{R: 200, A: 255}), image.Point{}, draw.Src)
	half := Opacity(source, 0.5)
	if r, _, _, a := half.At(1, 1).RGBA(); r>>8 != 100 || a>>8 != 128 {
		t.Errorf("半透明后应为 r=100 a=128，实际 r=%d a=%d", r>>8, a>>8)
	}
	if source.Pix[0] != 200 {
		t.Error("Opacity 改动了原图")
	}
}
