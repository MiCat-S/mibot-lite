package statuscard

import (
	"bytes"
	"image/png"
	"testing"
	"time"
)

func TestRender(t *testing.T) {
	card := Card{Name: "MiBot Lite", Uptime: 50*time.Hour + 3*time.Minute + 4*time.Second,
		CPU: Gauge{Percent: 12, Known: true}, Memory: Gauge{Percent: 80, Known: true},
		Disk: Gauge{Percent: 95, Known: true}, Swap: Gauge{},
		Footer: "MiBot Lite 0.1.23 · Go go1.26 · gotd v0.162.0"}
	data, err := Render(card)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if bounds := decoded.Bounds(); bounds.Dx() != 1600 || bounds.Dy() != 900 {
		t.Errorf("尺寸 %v", bounds)
	}
	// 磁盘 95% 应该判为资源告警（红点）。
	if label, _ := health(card); label != "资源告警" {
		t.Errorf("健康状态：%s", label)
	}
	if r, g, b, _ := decoded.At(128, 370).RGBA(); r>>8 != 0xfb || g>>8 != 0x71 || b>>8 != 0x85 {
		t.Errorf("状态圆点颜色 %02x%02x%02x", r>>8, g>>8, b>>8)
	}
}

func TestDuration(t *testing.T) {
	for value, want := range map[time.Duration]string{
		3*time.Hour + 4*time.Minute + 5*time.Second: "03:04:05",
		50*time.Hour + 3*time.Minute:                "2天 02:03:00",
	} {
		if got := duration(value); got != want {
			t.Errorf("%v → %q，应为 %q", value, got, want)
		}
	}
}

func TestHealthThresholds(t *testing.T) {
	for _, c := range []struct {
		percent float64
		want    string
	}{{10, "运行正常"}, {75, "需要关注"}, {90, "资源告警"}} {
		if label, _ := health(Card{CPU: Gauge{Percent: c.percent, Known: true}}); label != c.want {
			t.Errorf("%v%% → %s，应为 %s", c.percent, label, c.want)
		}
	}
	if label, _ := health(Card{}); label != "运行正常" {
		t.Error("都读不到时算正常")
	}
}
