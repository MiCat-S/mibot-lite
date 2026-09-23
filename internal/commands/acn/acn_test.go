package acn

import (
	"strings"
	"testing"
)

func TestApplyTextStyle(t *testing.T) {
	if got := applyTextStyle("ab12", "mono"); got != "𝚊𝚋𝟷𝟸" {
		t.Fatalf("mono style = %q", got)
	}
	if got := applyTextStyle("CZ", "double"); got != "ℂℤ" {
		t.Fatalf("double-struck capitals outside the block are wrong: %q", got)
	}
	if got := applyTextStyle("中文 ok", "sans"); !strings.HasPrefix(got, "中文 ") {
		t.Fatalf("non-latin text must pass through: %q", got)
	}
	if got := applyTextStyle("abc", "normal"); got != "abc" {
		t.Fatalf("normal style changed the text: %q", got)
	}
}

func TestZoneLabel(t *testing.T) {
	for format, want := range map[string]string{"GMT": "GMT+8", "UTC": "UTC+8", "OFFSET": "+08:00", "SIMP": "CST", "custom:北京时间": "北京时间"} {
		if got := zoneLabel("Asia/Shanghai", format); got != want {
			t.Errorf("zoneLabel(%q) = %q, want %q", format, got, want)
		}
	}
	if got := zoneLabel("UTC", "GMT"); got != "GMT" {
		t.Fatalf("UTC label = %q", got)
	}
	if got := zoneLabel("Not/AZone", "GMT"); got != "" {
		t.Fatalf("an unknown zone should render empty, got %q", got)
	}
}

// 重新保存昵称时，不能把上一次运行追加的时钟也存进去，否则基础昵称
// 每次都会多长出一个时间戳。
func TestCleanNickname(t *testing.T) {
	if got := cleanNickname("我要一直陪着我 🕘 09:30"); got != "我要一直陪着我" {
		t.Fatalf("cleanNickname = %q", got)
	}
	if got := cleanNickname("  Alice   Smith  "); got != "Alice Smith" {
		t.Fatalf("cleanNickname = %q", got)
	}
}
