package save

import (
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/bot"
)

// Telegram 给出的四种链接形式，以及一些只是看起来像链接的东西。
func TestParseLink(t *testing.T) {
	cases := map[string]messageLink{
		"https://t.me/c/1234567890/42":        {ChatID: "-1001234567890", ID: 42},
		"t.me/c/1234567890/5/42":              {ChatID: "-1001234567890", ID: 42},
		"https://t.me/durov/123":              {Username: "durov", ID: 123},
		"https://t.me/some_group/7/88?single": {Username: "some_group", ID: 88},
		"telegram.me/durov/9":                 {Username: "durov", ID: 9},
	}
	for text, want := range cases {
		got, ok := parseLink(text)
		if !ok || got.ChatID != want.ChatID || got.Username != want.Username || got.ID != want.ID {
			t.Errorf("parseLink(%q) = %+v %v, want %+v", text, got, ok, want)
		}
	}
	for _, text := range []string{"https://t.me/durov", "https://example.com/c/1/2", "t.me/c/abc/1", "hello", "t.me/ab/1"} {
		if _, ok := parseLink(text); ok {
			t.Errorf("%q parsed as a message link", text)
		}
	}
}

func TestParseSaveArgs(t *testing.T) {
	request, err := parseSaveArgs([]string{"https://t.me/durov/1", "https://t.me/durov/2", "@friend"})
	if err != nil || len(request.Links) != 2 || request.Target != "@friend" {
		t.Errorf("links and a target: %+v %v", request, err)
	}
	request, err = parseSaveArgs([]string{"t.me/c/123/100|t.me/c/123/5"})
	if err != nil || request.Range == nil || request.Range[0].ID != 5 || request.Range[1].ID != 100 {
		t.Errorf("a reversed range should be put in order: %+v %v", request, err)
	}
	for label, args := range map[string][]string{
		"two chats":      {"t.me/c/1/1|t.me/c/2/9"},
		"half a range":   {"t.me/c/1/1|nope"},
		"two targets":    {"t.me/durov/1", "@a", "@b"},
		"range and link": {"t.me/c/1/1|t.me/c/1/9", "t.me/durov/1"},
	} {
		if _, err := parseSaveArgs(args); err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
}

// 和人的私聊没有 t.me 地址；硬编一个出来，打印的就是一条打不开的链接。
func TestLinkURL(t *testing.T) {
	if got := (messageLink{ChatID: "-1001234", ID: 5}).url(); got != "https://t.me/c/1234/5" {
		t.Errorf("channel url = %q", got)
	}
	if got := (messageLink{Username: "durov", ID: 5}).url(); got != "https://t.me/durov/5" {
		t.Errorf("public url = %q", got)
	}
	if got := (messageLink{ChatID: "777", ID: 5}).url(); got != "" {
		t.Errorf("a private chat got a url: %q", got)
	}
}

// 本地保存按发送者起的名字给文件命名，这个名字不能让文件跑出目录。
func TestSanitizeSegment(t *testing.T) {
	for input, want := range map[string]string{
		"../../etc/passwd": "etc_passwd",
		"视频 2024.mp4":      "视频_2024.mp4",
		"...":              "file",
		"a/b\\c":           "a_b_c",
	} {
		if got := sanitizeSegment(input); got != want {
			t.Errorf("sanitizeSegment(%q) = %q, want %q", input, got, want)
		}
	}
	if got := localExtension(&bot.MediaSource{MimeType: "video/mp4"}); got != ".mp4" {
		t.Errorf("video/mp4 saved as %q", got)
	}
}
