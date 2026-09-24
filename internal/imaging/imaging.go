// Package imaging 是几个命令要用到的那一点像素处理：.yvlu 缩放头像，
// .eatgif 和 .eat 把头像透过蒙版贴到底图上。
//
// 只用 Go 标准库，缩放器用 golang.org/x/image。MiBox 用的是 sharp，
// 它绑定 libvips——一个共享库、一份图像缓存和一个线程池，在整个进程
// 生命周期里常驻内存；而这里的活一天只做几次，完全可以用 image/draw
// 写出来。
package imaging

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"io"
	"math"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// MaxDimension 是本包解码或生成的所有图像的尺寸上限。
// Telegram 贴纸每边最多 512，远程素材也会声明自己的尺寸，所以超过
// 这个值的只会是格式错误或恶意构造的素材，不是值得缩放的图片。
const MaxDimension = 2048

// DecodePNG 读取 PNG，超过 MaxDimension 的一律拒绝。
func DecodePNG(data []byte) (image.Image, error) {
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("read PNG header: %w", err)
	}
	if config.Width < 1 || config.Height < 1 || config.Width > MaxDimension || config.Height > MaxDimension {
		return nil, fmt.Errorf("PNG is %dx%d, outside the supported range", config.Width, config.Height)
	}
	return png.Decode(bytes.NewReader(data))
}

// Decode 读取 PNG、JPEG 或 WebP，按字节内容自动判断。Telegram 给的头像是
// JPEG，动画素材是 PNG，静态贴纸是 WebP。
func Decode(data []byte) (image.Image, error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("read image header: %w", err)
	}
	if config.Width < 1 || config.Height < 1 || config.Width > MaxDimension || config.Height > MaxDimension {
		return nil, fmt.Errorf("image is %dx%d, outside the supported range", config.Width, config.Height)
	}
	value, _, err := image.Decode(bytes.NewReader(data))
	return value, err
}

// EncodePNG 把图像编码成 PNG。
func EncodePNG(value image.Image) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := encoder.Encode(&buffer, value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// WritePNG 把图像直接写进 writer，这样处理帧序列时，内存里最多只有
// 一帧编码后的数据。
func WritePNG(w io.Writer, value image.Image) error {
	return (&png.Encoder{CompressionLevel: png.DefaultCompression}).Encode(w, value)
}

// Resize 把图像缩放到正好 width x height，不管宽高比。
func Resize(source image.Image, width, height int) *image.RGBA {
	target := image.NewRGBA(image.Rect(0, 0, width, height))
	xdraw.CatmullRom.Scale(target, target.Bounds(), source, source.Bounds(), draw.Src, nil)
	return target
}

// ResizeCover 缩放图像以铺满 width x height，较长的一边以中心为准裁掉
// 两头——把长方形照片做成方形头像，要的正是这个。
func ResizeCover(source image.Image, width, height int) *image.RGBA {
	bounds := source.Bounds()
	scale := math.Max(float64(width)/float64(bounds.Dx()), float64(height)/float64(bounds.Dy()))
	cropWidth := int(math.Round(float64(width) / scale))
	cropHeight := int(math.Round(float64(height) / scale))
	cropWidth = min(max(cropWidth, 1), bounds.Dx())
	cropHeight = min(max(cropHeight, 1), bounds.Dy())
	crop := image.Rect(0, 0, cropWidth, cropHeight).
		Add(image.Pt(bounds.Min.X+(bounds.Dx()-cropWidth)/2, bounds.Min.Y+(bounds.Dy()-cropHeight)/2))
	target := image.NewRGBA(image.Rect(0, 0, width, height))
	xdraw.CatmullRom.Scale(target, target.Bounds(), source, crop, draw.Src, nil)
	return target
}

// ResizeFit 把图像缩小到能放进 width x height，保持宽高比。
// 本来就更小的图像按原尺寸返回，不做缩放。
func ResizeFit(source image.Image, width, height int) *image.RGBA {
	bounds := source.Bounds()
	scale := math.Min(float64(width)/float64(bounds.Dx()), float64(height)/float64(bounds.Dy()))
	if scale > 1 {
		scale = 1
	}
	targetWidth := max(1, int(math.Round(float64(bounds.Dx())*scale)))
	targetHeight := max(1, int(math.Round(float64(bounds.Dy())*scale)))
	target := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	xdraw.CatmullRom.Scale(target, target.Bounds(), source, bounds, draw.Src, nil)
	return target
}

// Rotate 把图像绕中心旋转 degrees 度，画布尺寸不变——动画配置是在
// 固定的蒙版里旋转脸，画布一变大，粘贴位置就会偏。
func Rotate(source image.Image, degrees float64) *image.RGBA {
	bounds := source.Bounds()
	target := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	radians := degrees * math.Pi / 180
	sin, cos := math.Sin(radians), math.Cos(radians)
	centreX := float64(bounds.Dx()) / 2
	centreY := float64(bounds.Dy()) / 2
	// 先把中心平移到原点，旋转，再平移回去。
	matrix := f64Aff3{
		cos, -sin, centreX - cos*centreX + sin*centreY,
		sin, cos, centreY - sin*centreX - cos*centreY,
	}
	xdraw.CatmullRom.Transform(target, matrix, source, bounds, draw.Src, nil)
	return target
}

// RotateExpand 把图像绕中心旋转 degrees 度，画布扩大到正好装下旋转后的图，
// 空出来的角是透明的。和 sharp 的 rotate 一样；.eat 的印章要的是这个。
func RotateExpand(source image.Image, degrees float64) *image.RGBA {
	bounds := source.Bounds()
	radians := degrees * math.Pi / 180
	sin, cos := math.Abs(math.Sin(radians)), math.Abs(math.Cos(radians))
	width := int(math.Ceil(float64(bounds.Dx())*cos + float64(bounds.Dy())*sin))
	height := int(math.Ceil(float64(bounds.Dx())*sin + float64(bounds.Dy())*cos))
	padded := image.NewRGBA(image.Rect(0, 0, max(width, 1), max(height, 1)))
	offset := image.Pt((padded.Bounds().Dx()-bounds.Dx())/2, (padded.Bounds().Dy()-bounds.Dy())/2)
	draw.Draw(padded, bounds.Sub(bounds.Min).Add(offset), source, bounds.Min, draw.Src)
	return Rotate(padded, degrees)
}

// Opacity 把整张图的不透明度乘以 factor（0 到 1）。像素是预乘的，
// 所以四个通道一起乘。
func Opacity(source image.Image, factor float64) *image.RGBA {
	bounds := source.Bounds()
	target := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(target, target.Bounds(), source, bounds.Min, draw.Src)
	factor = math.Min(math.Max(factor, 0), 1)
	for index := range target.Pix {
		target.Pix[index] = uint8(math.Round(float64(target.Pix[index]) * factor))
	}
	return target
}

// Brightness 把每个通道乘以 factor，结果不超过 alpha，
// 这样预乘像素仍然合法。
func Brightness(source image.Image, factor float64) *image.RGBA {
	bounds := source.Bounds()
	target := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(target, target.Bounds(), source, bounds.Min, draw.Src)
	if factor == 1 {
		return target
	}
	for index := 0; index < len(target.Pix); index += 4 {
		alpha := float64(target.Pix[index+3])
		for channel := 0; channel < 3; channel++ {
			value := float64(target.Pix[index+channel]) * factor
			target.Pix[index+channel] = uint8(math.Min(math.Max(value, 0), alpha))
		}
	}
	return target
}

// ApplyMask 只保留 source 里被蒙版标为不透明的部分，相当于 sharp 的
// "dest-in" 混合。蒙版本身的颜色不起作用，只看它的 alpha 通道。
func ApplyMask(source image.Image, mask image.Image) *image.RGBA {
	bounds := mask.Bounds()
	target := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	// 在透明的目标上透过蒙版用 Over 绘制，得到的就是 source 乘以
	// 蒙版的 alpha，正是想要的结果。
	draw.DrawMask(target, target.Bounds(), source, source.Bounds().Min, alphaOf(mask), bounds.Min, draw.Over)
	return target
}

// alphaMask 把图像的 alpha 通道当作蒙版来用，由 alphaOf 构造。
type alphaMask struct{ image.Image }

func (a alphaMask) At(x, y int) color.Color {
	_, _, _, alpha := a.Image.At(x, y).RGBA()
	return color.Alpha16{A: uint16(alpha)}
}

func (a alphaMask) ColorModel() color.Model { return color.Alpha16Model }

func alphaOf(value image.Image) image.Image { return alphaMask{Image: value} }

// Composite 把 overlay 贴到 canvas 的 (x, y) 处，按 alpha 混合。
func Composite(canvas draw.Image, overlay image.Image, x, y int) {
	bounds := overlay.Bounds()
	target := image.Rect(x, y, x+bounds.Dx(), y+bounds.Dy())
	draw.Draw(canvas, target, overlay, bounds.Min, draw.Over)
}

// ToRGBA 返回图像的一个可编辑副本。
func ToRGBA(source image.Image) *image.RGBA {
	if value, ok := source.(*image.RGBA); ok {
		return value
	}
	bounds := source.Bounds()
	target := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(target, target.Bounds(), source, bounds.Min, draw.Src)
	return target
}

// Flatten 把图像合成到纯色背景上，去掉 alpha。
func Flatten(source image.Image, background color.Color) *image.RGBA {
	bounds := source.Bounds()
	target := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(target, target.Bounds(), image.NewUniform(background), image.Point{}, draw.Src)
	draw.Draw(target, target.Bounds(), source, bounds.Min, draw.Over)
	return target
}

// f64Aff3 就是 x/image 的仿射矩阵类型，在本地重新写一遍，这样本包的
// 调用方不必导入 golang.org/x/image/math/f64。
type f64Aff3 = [6]float64

// ErrNotWebP 表示这些字节不是 WebP 文件。
var ErrNotWebP = errors.New("not a WebP image")

// WebPSize 从文件头读出 WebP 的尺寸。
//
// Telegram 发贴纸时要附带 DocumentAttributeImageSize，而远程的语录服务
// 返回的是 WebP。这里只读文件头：解码整张图的话，就得为了两个整数
// 引入一个 WebP 解码器。
func WebPSize(data []byte) (width, height int, err error) {
	if len(data) < 30 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, ErrNotWebP
	}
	switch string(data[12:16]) {
	case "VP8X":
		// 扩展格式：24 位小端，存的是实际值减一。
		width = int(data[24]) | int(data[25])<<8 | int(data[26])<<16
		height = int(data[27]) | int(data[28])<<8 | int(data[29])<<16
		return width + 1, height + 1, nil
	case "VP8 ":
		// 有损格式：先是 3 字节的起始码，然后是 14 位的宽和高。
		if len(data) < 30 || data[23] != 0x9d || data[24] != 0x01 || data[25] != 0x2a {
			return 0, 0, ErrNotWebP
		}
		width = int(binary.LittleEndian.Uint16(data[26:28]) & 0x3fff)
		height = int(binary.LittleEndian.Uint16(data[28:30]) & 0x3fff)
		return width, height, nil
	case "VP8L":
		// 无损格式：先是一个签名字节，然后是两个 14 位字段，同样存的是减一后的值。
		if data[20] != 0x2f {
			return 0, 0, ErrNotWebP
		}
		bits := binary.LittleEndian.Uint32(data[21:25])
		return int(bits&0x3fff) + 1, int((bits>>14)&0x3fff) + 1, nil
	}
	return 0, 0, ErrNotWebP
}
