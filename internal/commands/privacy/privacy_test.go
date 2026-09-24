package privacy

import (
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/bot"
)

func TestDescribe(t *testing.T) {
	if got := describe(bot.DefaultIPPolicy); !strings.Contains(got, "IPv4 末尾 2 段") || !strings.Contains(got, "IPv6 末尾 4 段") {
		t.Errorf("默认设置的说明：%q", got)
	}
	if got := describe(bot.IPPolicy{Mode: "hide", IPv4: 2, IPv6: 4}); got != "完全隐藏" {
		t.Errorf("隐藏模式的说明：%q", got)
	}
	if !strings.Contains(help("."), ".privacy ip mask 2 4") || !strings.Contains(help("."), ".privacy ip hide") {
		t.Error("帮助里缺用法")
	}
}
