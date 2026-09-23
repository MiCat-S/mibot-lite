package imaging_test

import (
	"encoding/json"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/imaging"
)

// TestRealAnimationAssets 用插件仓库里的一个真实动画跑一遍 .eatgif 的
// 合成流程。这一点用合成图像的单元测试覆盖不到：真实动画的蒙版是
// 边缘抗锯齿的不规则形状，配置里的旋转角度和偏移量，手工做的测试数据
// 也复现不出来。
//
// 只有 MIBOT_EATGIF_ASSETS 指向一份 eatgif 素材目录时才会运行，否则
// 跳过，这样测试套件不依赖外部环境：
//
//	MIBOT_EATGIF_ASSETS=/path/to/eatgif go test ./internal/imaging/ -run RealAnimation -v
func TestRealAnimationAssets(t *testing.T) {
	root := os.Getenv("MIBOT_EATGIF_ASSETS")
	if root == "" {
		t.Skip("set MIBOT_EATGIF_ASSETS to a directory of eatgif assets to run this")
	}
	type role struct {
		X, Y       int
		Mask       string
		Rotate     *float64
		Brightness *float64
	}
	type frame struct {
		URL string
		Me  *role
		You *role
	}
	var spec struct {
		Width, Height int
		Res           []frame
	}
	raw, err := os.ReadFile(filepath.Join(root, "j", "j.json"))
	if err != nil {
		t.Skipf("no j/j.json under %s: %v", root, err)
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Width < 1 || len(spec.Res) == 0 {
		t.Fatalf("unusable spec: %+v", spec)
	}

	load := func(name string) image.Image {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		value, err := imaging.DecodePNG(data)
		if err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		return value
	}
	// 脸上横着一道亮条，如果旋转悄悄没生效，
	// 就能看到一道没转过的横条。
	face := func(tint color.RGBA) image.Image {
		value := image.NewRGBA(image.Rect(0, 0, 640, 640))
		for y := 0; y < 640; y++ {
			for x := 0; x < 640; x++ {
				shade := tint
				if y > 300 && y < 340 {
					shade = color.RGBA{R: 255, G: 255, B: 255, A: 255}
				}
				value.SetRGBA(x, y, shade)
			}
		}
		return value
	}
	me, you := face(color.RGBA{R: 255, A: 255}), face(color.RGBA{B: 255, A: 255})

	output := t.TempDir()
	if target := os.Getenv("MIBOT_EATGIF_OUT"); target != "" {
		output = target
	}
	for index, entry := range spec.Res {
		canvas := imaging.ToRGBA(load(entry.URL))
		if canvas.Bounds().Dx() != spec.Width || canvas.Bounds().Dy() != spec.Height {
			t.Errorf("frame %d canvas is %v, spec says %dx%d", index, canvas.Bounds(), spec.Width, spec.Height)
		}
		before := canvas.Pix[0:]
		_ = before
		painted := 0
		for _, item := range []*role{entry.You, entry.Me} {
			if item == nil {
				continue
			}
			mask := load(item.Mask)
			bounds := mask.Bounds()
			shaped := image.Image(imaging.Resize(face(color.RGBA{A: 255}), bounds.Dx(), bounds.Dy()))
			if item == entry.Me {
				shaped = imaging.Resize(me, bounds.Dx(), bounds.Dy())
			} else {
				shaped = imaging.Resize(you, bounds.Dx(), bounds.Dy())
			}
			if item.Rotate != nil && *item.Rotate != 0 {
				shaped = imaging.Rotate(shaped, *item.Rotate)
			}
			if item.Brightness != nil && *item.Brightness != 1 {
				shaped = imaging.Brightness(shaped, *item.Brightness)
			}
			masked := imaging.ApplyMask(shaped, mask)
			// 蒙版必须真的裁掉一部分，否则每个头像都会
			// 贴成一整个矩形。
			opaque, transparent := 0, 0
			for offset := 3; offset < len(masked.Pix); offset += 4 {
				if masked.Pix[offset] == 255 {
					opaque++
				} else if masked.Pix[offset] == 0 {
					transparent++
				}
			}
			if opaque == 0 {
				t.Errorf("frame %d: mask %s produced nothing opaque", index, item.Mask)
			}
			if transparent == 0 {
				t.Errorf("frame %d: mask %s cut nothing away", index, item.Mask)
			}
			imaging.Composite(canvas, masked, item.X, item.Y)
			painted++
		}
		if painted == 0 {
			t.Errorf("frame %d painted no avatar", index)
		}
		encoded, err := imaging.EncodePNG(canvas)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) == 0 {
			t.Fatalf("frame %d encoded to nothing", index)
		}
		if err := os.WriteFile(filepath.Join(output, "frame"+string(rune('0'+index))+".png"), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("composited %d frames of %dx%d into %s", len(spec.Res), spec.Width, spec.Height, output)
}
