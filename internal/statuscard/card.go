// Package statuscard 画 .status 的状态卡片：1600×900 的 PNG，布局和配色照 MiBox v2 的
// status-card。只用 golang.org/x/image 的矢量光栅和字体渲染，不引入别的依赖；
// 字体是 v2 附带的 Noto Sans SC 子集（73 KB，只含 ASCII、卡片上的标点和固定的中文标签，
// SIL OFL 1.1，许可证见 NotoSansSC-OFL.txt）。卡片只在 .status 时画，画完就丢。
package statuscard

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

//go:embed NotoSansSC-status-subset.ttf
var fontData []byte

const (
	width  = 1600
	height = 900
)

// Gauge 是一个圆环仪表的读数，Known 为假时显示 --。
type Gauge struct {
	Percent float64
	Known   bool
}

// Card 是卡片上要画的内容。
type Card struct {
	Name   string
	Uptime time.Duration
	CPU    Gauge
	Memory Gauge
	Disk   Gauge
	Swap   Gauge
	// Footer 是底部一行版本信息。
	Footer string
}

var (
	parsed     *sfnt.Font
	parseOnce  sync.Once
	parseError error
)

func loadFont() (*sfnt.Font, error) {
	parseOnce.Do(func() { parsed, parseError = opentype.Parse(fontData) })
	return parsed, parseError
}

// health 按四项里最高的一项给出总体状态，阈值和颜色同 v2。
func health(card Card) (string, color.RGBA) {
	highest := 0.0
	for _, gauge := range []Gauge{card.CPU, card.Memory, card.Disk, card.Swap} {
		if gauge.Known {
			highest = math.Max(highest, gauge.Percent)
		}
	}
	switch {
	case highest >= 90:
		return "资源告警", hex(0xfb7185)
	case highest >= 75:
		return "需要关注", hex(0xfbbf24)
	}
	return "运行正常", hex(0x34f59a)
}

func gaugeColor(gauge Gauge, normal color.RGBA) color.RGBA {
	switch {
	case gauge.Known && gauge.Percent >= 90:
		return hex(0xfb7185)
	case gauge.Known && gauge.Percent >= 75:
		return hex(0xfbbf24)
	}
	return normal
}

// duration 写成「2天 03:04:05」或「03:04:05」。
func duration(value time.Duration) string {
	if value < 0 {
		return "--"
	}
	total := int(value.Seconds())
	clock := fmt.Sprintf("%02d:%02d:%02d", total%86400/3600, total%3600/60, total%60)
	if days := total / 86400; days > 0 {
		return fmt.Sprintf("%d天 %s", days, clock)
	}
	return clock
}

// Render 画出卡片，返回 PNG。
func Render(card Card) ([]byte, error) {
	face, err := loadFont()
	if err != nil {
		return nil, err
	}
	c := &canvas{img: image.NewRGBA(image.Rect(0, 0, width, height)), font: face, faces: map[float64]font.Face{}}
	defer c.close()

	c.background()
	c.grid()
	c.strokeRoundedRect(30, 30, width-60, height-60, 34, 4, hex(0x0e83bd))
	c.radioMark(120, 140)
	c.text(c.fitted(card.Name+" · 运行状态", 595, 68, 42), 180, 150, hex(0xf1f5f9), alignLeft)
	c.line(82, 235, 755, 235, 3, hex(0x215a78))

	label, tone := health(card)
	// 光晕近似 canvas 的 shadowBlur：外面叠几圈很淡的同色圆。
	for layer := 4; layer >= 1; layer-- {
		radius := 42 + float64(layer)*6
		c.fillPath(func() { c.circle(128, 370, radius) }, withAlpha(tone, 0.08))
	}
	c.fillPath(func() { c.circle(128, 370, 42) }, tone)
	c.size = 68
	c.text(label, 205, 374, tone, alignLeft)

	c.text(c.fitted("在线", 100, 42, 30), 88, 555, hex(0x9fb2c5), alignLeft)
	c.text(c.fitted(duration(card.Uptime), 500, 68, 48), 205, 555, hex(0xf1f5f9), alignLeft)

	c.gauge(810, 115, "CPU", card.CPU, gaugeColor(card.CPU, hex(0x34f59a)))
	c.gauge(1174, 115, "内存", card.Memory, gaugeColor(card.Memory, hex(0x34f59a)))
	c.gauge(810, 395, "磁盘", card.Disk, gaugeColor(card.Disk, hex(0xfbbf24)))
	c.gauge(1174, 395, "Swap", card.Swap, gaugeColor(card.Swap, hex(0x22d3ee)))

	c.line(82, 732, 1518, 732, 3, hex(0x215a78))
	c.text(c.fitted(card.Footer, 1370, 34, 24), width/2, 805, hex(0xb8c7d7), alignCenter)

	var out bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&out, c.img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func hex(value uint32) color.RGBA {
	return color.RGBA{R: uint8(value >> 16), G: uint8(value >> 8), B: uint8(value), A: 255}
}

// withAlpha 把不透明色换成预乘过的半透明色。
func withAlpha(value color.RGBA, alpha float64) color.RGBA {
	return color.RGBA{R: uint8(float64(value.R) * alpha), G: uint8(float64(value.G) * alpha), B: uint8(float64(value.B) * alpha), A: uint8(255 * alpha)}
}

type alignment int

const (
	alignLeft alignment = iota
	alignCenter
)

type canvas struct {
	img   *image.RGBA
	font  *sfnt.Font
	faces map[float64]font.Face
	// size 是 fitted 最后选定的字号，text 用它。
	size float64
	// path 是共用的光栅器，pending 是正在拼的形状的子路径。
	path    *vector.Rasterizer
	pending [][][2]float64
}

func (c *canvas) close() {
	for _, face := range c.faces {
		face.Close()
	}
}

// background 是左上到右下的三段渐变（同 v2 的 createLinearGradient）。
func (c *canvas) background() {
	stops := []struct {
		at    float64
		color color.RGBA
	}{{0, hex(0x071927)}, {0.58, hex(0x071520)}, {1, hex(0x05111a)}}
	length := float64(width*width + height*height)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			t := (float64(x)*width + float64(y)*height) / length
			index := 1
			for index < len(stops)-1 && t > stops[index].at {
				index++
			}
			from, to := stops[index-1], stops[index]
			k := (t - from.at) / (to.at - from.at)
			mix := func(a, b uint8) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*k) }
			c.img.SetRGBA(x, y, color.RGBA{R: mix(from.color.R, to.color.R), G: mix(from.color.G, to.color.G), B: mix(from.color.B, to.color.B), A: 255})
		}
	}
}

func (c *canvas) grid() {
	tone := withAlpha(color.RGBA{R: 39, G: 103, B: 139, A: 255}, 0.14)
	for x := 48; x < width; x += 64 {
		c.line(float64(x), 48, float64(x), height-48, 1, tone)
	}
	for y := 48; y < height; y += 64 {
		c.line(48, float64(y), width-48, float64(y), 1, tone)
	}
}

// fillPath 用 build 画出的路径填充一块颜色（抗锯齿，叠加在已有内容上）。
// 光栅器只开形状外接框那么大，并且整张卡片共用一个：整张画布大小的光栅器
// 一个就要 5 MB 多，每个形状新开一个的话，一张卡片会产生几百 MB 的临时分配。
func (c *canvas) fillPath(build func(), tone color.RGBA) {
	c.pending = c.pending[:0]
	build()
	if len(c.pending) == 0 {
		return
	}
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, points := range c.pending {
		for _, point := range points {
			minX, minY = math.Min(minX, point[0]), math.Min(minY, point[1])
			maxX, maxY = math.Max(maxX, point[0]), math.Max(maxY, point[1])
		}
	}
	box := image.Rect(int(math.Floor(minX)), int(math.Floor(minY)), int(math.Ceil(maxX))+1, int(math.Ceil(maxY))+1).Intersect(c.img.Bounds())
	if box.Empty() {
		return
	}
	if c.path == nil {
		c.path = vector.NewRasterizer(box.Dx(), box.Dy())
	} else {
		c.path.Reset(box.Dx(), box.Dy())
	}
	c.path.DrawOp = draw.Over
	for _, points := range c.pending {
		for index, point := range points {
			x, y := float32(point[0]-float64(box.Min.X)), float32(point[1]-float64(box.Min.Y))
			if index == 0 {
				c.path.MoveTo(x, y)
			} else {
				c.path.LineTo(x, y)
			}
		}
		c.path.ClosePath()
	}
	c.path.Draw(c.img, box, image.NewUniform(tone), image.Point{})
}

// polygon 记下一个闭合的子路径，由 fillPath 统一填充。
func (c *canvas) polygon(points [][2]float64) {
	c.pending = append(c.pending, points)
}

// arcPoints 是圆心 (x, y)、半径 r、从角 a0 到 a1（弧度）的一串点。
func arcPoints(x, y, r, a0, a1 float64) [][2]float64 {
	steps := max(8, int(math.Abs(a1-a0)*r/4))
	points := make([][2]float64, 0, steps+1)
	for step := 0; step <= steps; step++ {
		angle := a0 + (a1-a0)*float64(step)/float64(steps)
		points = append(points, [2]float64{x + r*math.Cos(angle), y + r*math.Sin(angle)})
	}
	return points
}

func (c *canvas) circle(x, y, r float64) { c.polygon(arcPoints(x, y, r, 0, 2*math.Pi)) }

func reversed(points [][2]float64) [][2]float64 {
	out := make([][2]float64, len(points))
	for index, point := range points {
		out[len(points)-1-index] = point
	}
	return out
}

// strokeArc 画一段有宽度的圆弧，round 时两端是圆头。
func (c *canvas) strokeArc(x, y, r, a0, a1, lineWidth float64, tone color.RGBA, round bool) {
	c.fillPath(func() {
		outer := arcPoints(x, y, r+lineWidth/2, a0, a1)
		inner := reversed(arcPoints(x, y, r-lineWidth/2, a0, a1))
		c.polygon(append(outer, inner...))
	}, tone)
	if round {
		for _, angle := range []float64{a0, a1} {
			c.fillPath(func() { c.circle(x+r*math.Cos(angle), y+r*math.Sin(angle), lineWidth/2) }, tone)
		}
	}
}

// line 画一条有宽度的直线段。
func (c *canvas) line(x0, y0, x1, y1, lineWidth float64, tone color.RGBA) {
	dx, dy := x1-x0, y1-y0
	length := math.Hypot(dx, dy)
	if length == 0 {
		return
	}
	nx, ny := -dy/length*lineWidth/2, dx/length*lineWidth/2
	c.fillPath(func() {
		c.polygon([][2]float64{{x0 + nx, y0 + ny}, {x1 + nx, y1 + ny}, {x1 - nx, y1 - ny}, {x0 - nx, y0 - ny}})
	}, tone)
}

// roundedRect 是圆角矩形的轮廓点，按顺时针排列。
func roundedRect(x, y, w, h, r float64) [][2]float64 {
	r = math.Min(r, math.Min(w/2, h/2))
	var points [][2]float64
	points = append(points, arcPoints(x+w-r, y+r, r, -math.Pi/2, 0)...)
	points = append(points, arcPoints(x+w-r, y+h-r, r, 0, math.Pi/2)...)
	points = append(points, arcPoints(x+r, y+h-r, r, math.Pi/2, math.Pi)...)
	points = append(points, arcPoints(x+r, y+r, r, math.Pi, 3*math.Pi/2)...)
	return points
}

// strokeRoundedRect 画圆角矩形的边框：外轮廓加反向的内轮廓，中间留空。
func (c *canvas) strokeRoundedRect(x, y, w, h, r, lineWidth float64, tone color.RGBA) {
	half := lineWidth / 2
	c.fillPath(func() {
		c.polygon(roundedRect(x-half, y-half, w+lineWidth, h+lineWidth, r+half))
		c.polygon(reversed(roundedRect(x+half, y+half, w-lineWidth, h-lineWidth, math.Max(0, r-half))))
	}, tone)
}

// radioMark 是标题左边的信号图标（同 v2 的 drawRadioMark）。
func (c *canvas) radioMark(x, y float64) {
	tone := hex(0x22d3ee)
	c.strokeArc(x, y, 40, math.Pi*1.08, math.Pi*1.55, 9, tone, true)
	c.strokeArc(x, y, 22, math.Pi*1.08, math.Pi*1.55, 9, tone, true)
	c.fillPath(func() { c.circle(x-35, y+32, 7) }, hex(0xe2e8f0))
}

// gauge 画一个仪表：圆角底板、圆环、百分比和右边的标签（同 v2 的 drawGauge）。
func (c *canvas) gauge(x, y float64, label string, value Gauge, tone color.RGBA) {
	const boxWidth, boxHeight = 336, 250
	c.fillPath(func() { c.polygon(roundedRect(x, y, boxWidth, boxHeight, 28)) }, withAlpha(color.RGBA{R: 10, G: 35, B: 53, A: 255}, 0.86))
	c.strokeRoundedRect(x, y, boxWidth, boxHeight, 28, 2, withAlpha(color.RGBA{R: 56, G: 139, B: 190, A: 255}, 0.48))
	centerX, centerY, radius := x+104, y+125, 68.0
	c.strokeArc(centerX, centerY, radius, 0, 2*math.Pi, 18, hex(0x213a4d), false)
	if value.Known && value.Percent > 0 {
		end := -math.Pi/2 + 2*math.Pi*math.Min(value.Percent, 100)/100
		// 圆环的光晕：同一段弧加宽、调淡，叠在下面。
		for layer := 3; layer >= 1; layer-- {
			c.strokeArc(centerX, centerY, radius, -math.Pi/2, end, 18+float64(layer)*8, withAlpha(tone, 0.07), true)
		}
		c.strokeArc(centerX, centerY, radius, -math.Pi/2, end, 18, tone, true)
	}
	percent := "--"
	if value.Known {
		percent = fmt.Sprintf("%d%%", int(math.Round(value.Percent)))
	}
	c.size = 38
	c.text(percent, centerX, centerY+2, hex(0xf1f5f9), alignCenter)
	c.line(x+205, y+61, x+205, y+boxHeight-61, 2, withAlpha(color.RGBA{R: 63, G: 132, B: 170, A: 255}, 0.55))
	maximum := 42.0
	if label == "Swap" {
		maximum = 43
	}
	c.text(c.fitted(label, 88, maximum, 24), x+232, centerY+2, hex(0xf1f5f9), alignLeft)
}

func (c *canvas) face(size float64) font.Face {
	if face, ok := c.faces[size]; ok {
		return face
	}
	face, err := opentype.NewFace(c.font, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		return nil
	}
	c.faces[size] = face
	return face
}

// fitted 从 maximum 往下每次减 2 找一个放得下的字号；到 minimum 还放不下就截断加省略号。
// 选定的字号记在 c.size 里，紧接着的 text 用它。
func (c *canvas) fitted(value string, maxWidth, maximum, minimum float64) string {
	for size := maximum; size >= minimum; size -= 2 {
		if face := c.face(size); face != nil && float64(font.MeasureString(face, value).Round()) <= maxWidth {
			c.size = size
			return value
		}
	}
	c.size = minimum
	face := c.face(minimum)
	runes := []rune(value)
	for len(runes) > 0 && face != nil && float64(font.MeasureString(face, string(runes)+"…").Round()) > maxWidth {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

// text 在 (x, y) 写一行字，y 是文字的垂直中线（canvas 的 textBaseline = middle）。
func (c *canvas) text(value string, x, y float64, tone color.RGBA, align alignment) {
	face := c.face(c.size)
	if face == nil {
		return
	}
	metrics := face.Metrics()
	baseline := y + float64(metrics.Ascent-metrics.Descent)/64/2
	if align == alignCenter {
		x -= float64(font.MeasureString(face, value)) / 64 / 2
	}
	drawer := font.Drawer{Dst: c.img, Src: image.NewUniform(tone), Face: face,
		Dot: fixed.Point26_6{X: fixed.Int26_6(x * 64), Y: fixed.Int26_6(baseline * 64)}}
	drawer.DrawString(value)
}
