// Package imaging is the small amount of pixel work two commands need:
// .yvlu resizes an avatar, .eatgif pastes avatars through a mask onto
// animation frames.
//
// It is the Go standard library plus golang.org/x/image for the scaler.
// MiBox used sharp, which binds libvips — a shared library, an image cache
// and a thread pool resident for the whole process, to do work that here
// happens a few times a day and is entirely expressible in image/draw.
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
)

// MaxDimension bounds every image this package will decode or produce.
// Telegram stickers are at most 512 on a side and the remote assets
// declare their own size, so anything larger is a malformed or hostile
// asset rather than a picture worth resizing.
const MaxDimension = 2048

// DecodePNG reads a PNG, refusing one larger than MaxDimension.
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

// Decode reads PNG or JPEG, whichever the bytes are. Telegram serves
// avatars as JPEG and the animation assets are PNG.
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

// EncodePNG writes an image as PNG.
func EncodePNG(value image.Image) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := encoder.Encode(&buffer, value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// WritePNG writes an image straight to a writer, so a frame sequence never
// holds more than one encoded frame in memory.
func WritePNG(w io.Writer, value image.Image) error {
	return (&png.Encoder{CompressionLevel: png.DefaultCompression}).Encode(w, value)
}

// Resize scales an image to exactly width x height, ignoring aspect ratio.
func Resize(source image.Image, width, height int) *image.RGBA {
	target := image.NewRGBA(image.Rect(0, 0, width, height))
	xdraw.CatmullRom.Scale(target, target.Bounds(), source, source.Bounds(), draw.Src, nil)
	return target
}

// ResizeCover scales an image to fill width x height, cropping the longer
// side from the centre — what a square avatar wants from a rectangular
// photo.
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

// ResizeFit scales an image down to fit inside width x height, keeping its
// aspect ratio. An image already smaller is returned unscaled.
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

// Rotate turns an image by degrees about its centre, keeping the canvas
// size — the animation specs rotate a face inside a fixed mask, so a
// growing canvas would shift the paste position.
func Rotate(source image.Image, degrees float64) *image.RGBA {
	bounds := source.Bounds()
	target := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	radians := degrees * math.Pi / 180
	sin, cos := math.Sin(radians), math.Cos(radians)
	centreX := float64(bounds.Dx()) / 2
	centreY := float64(bounds.Dy()) / 2
	// Translate the centre to the origin, rotate, translate back.
	matrix := f64Aff3{
		cos, -sin, centreX - cos*centreX + sin*centreY,
		sin, cos, centreY - sin*centreX - cos*centreY,
	}
	xdraw.CatmullRom.Transform(target, matrix, source, bounds, draw.Src, nil)
	return target
}

// Brightness multiplies every channel by factor, clamped to the alpha so
// premultiplied pixels stay valid.
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

// ApplyMask keeps the parts of source the mask marks opaque, which is
// sharp's "dest-in" blend. The mask's own colour is ignored; only its
// alpha channel counts.
func ApplyMask(source image.Image, mask image.Image) *image.RGBA {
	bounds := mask.Bounds()
	target := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	// Drawing Over onto a transparent target through the mask yields
	// source multiplied by the mask's alpha, which is the intent.
	draw.DrawMask(target, target.Bounds(), source, source.Bounds().Min, alphaOf(mask), bounds.Min, draw.Over)
	return target
}

// alphaOf presents an image's alpha channel as a mask.
type alphaMask struct{ image.Image }

func (a alphaMask) At(x, y int) color.Color {
	_, _, _, alpha := a.Image.At(x, y).RGBA()
	return color.Alpha16{A: uint16(alpha)}
}

func (a alphaMask) ColorModel() color.Model { return color.Alpha16Model }

func alphaOf(value image.Image) image.Image { return alphaMask{Image: value} }

// Composite pastes an overlay onto a canvas at (x, y), blending alpha.
func Composite(canvas draw.Image, overlay image.Image, x, y int) {
	bounds := overlay.Bounds()
	target := image.Rect(x, y, x+bounds.Dx(), y+bounds.Dy())
	draw.Draw(canvas, target, overlay, bounds.Min, draw.Over)
}

// ToRGBA returns an editable copy of an image.
func ToRGBA(source image.Image) *image.RGBA {
	if value, ok := source.(*image.RGBA); ok {
		return value
	}
	bounds := source.Bounds()
	target := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(target, target.Bounds(), source, bounds.Min, draw.Src)
	return target
}

// Flatten composites an image over a solid background, dropping alpha.
func Flatten(source image.Image, background color.Color) *image.RGBA {
	bounds := source.Bounds()
	target := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(target, target.Bounds(), image.NewUniform(background), image.Point{}, draw.Src)
	draw.Draw(target, target.Bounds(), source, bounds.Min, draw.Over)
	return target
}

// f64Aff3 is x/image's affine matrix type, spelled locally so callers of
// this package do not need the golang.org/x/image/math/f64 import.
type f64Aff3 = [6]float64

// ErrNotWebP means the bytes are not a WebP file.
var ErrNotWebP = errors.New("not a WebP image")

// WebPSize reads a WebP's dimensions from its header.
//
// Telegram wants a DocumentAttributeImageSize alongside a sticker, and the
// remote quote service returns WebP. Only the header is read: decoding the
// image would mean a WebP decoder for two integers.
func WebPSize(data []byte) (width, height int, err error) {
	if len(data) < 30 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, ErrNotWebP
	}
	switch string(data[12:16]) {
	case "VP8X":
		// Extended format: 24-bit little-endian, stored minus one.
		width = int(data[24]) | int(data[25])<<8 | int(data[26])<<16
		height = int(data[27]) | int(data[28])<<8 | int(data[29])<<16
		return width + 1, height + 1, nil
	case "VP8 ":
		// Lossy: a 3-byte start code, then 14-bit dimensions.
		if len(data) < 30 || data[23] != 0x9d || data[24] != 0x01 || data[25] != 0x2a {
			return 0, 0, ErrNotWebP
		}
		width = int(binary.LittleEndian.Uint16(data[26:28]) & 0x3fff)
		height = int(binary.LittleEndian.Uint16(data[28:30]) & 0x3fff)
		return width, height, nil
	case "VP8L":
		// Lossless: a signature byte, then two 14-bit fields, minus one.
		if data[20] != 0x2f {
			return 0, 0, ErrNotWebP
		}
		bits := binary.LittleEndian.Uint32(data[21:25])
		return int(bits&0x3fff) + 1, int((bits>>14)&0x3fff) + 1, nil
	}
	return 0, 0, ErrNotWebP
}
