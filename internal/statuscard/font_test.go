package statuscard

import (
	"testing"

	"golang.org/x/image/font/sfnt"
)

// 字体是只含卡片用字的子集：卡片上画的每个字都要在里面，否则线上只看得到方框。
// 页脚是运行时拼的版本信息，只允许 ASCII 和「·」，这里用一个典型的页脚一起查。
func TestFontCoversCardText(t *testing.T) {
	face, err := loadFont()
	if err != nil {
		t.Fatal(err)
	}
	var buffer sfnt.Buffer
	samples := append([]string{"MiBot Lite v0.2.0  ·  Go go1.26.0  ·  gotd v0.162.0"}, cardText...)
	for _, text := range samples {
		for _, r := range text {
			if index, err := face.GlyphIndex(&buffer, r); err != nil || index == 0 {
				t.Errorf("字体里没有「%c」（U+%04X），出现在 %q", r, r, text)
			}
		}
	}
}
