package save

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gotd/td/tg"

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
	for _, text := range []string{"https://t.me/durov", "https://example.com/c/1/2", "t.me/c/abc/1", "hello", "t.me/ab/1", "t.me/durov/4294967301"} {
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
		"two ranges":     {"t.me/c/1/1|t.me/c/1/2", "t.me/c/1/3|t.me/c/1/4"},
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

// MiBox 按账号存设置；导入时不知道本账号是谁，挑改过默认目标的那一份。
func TestConvertMiBox(t *testing.T) {
	cases := map[string]string{
		`{"users":{"1":{"target":"me","showSource":false},"2":{"target":"@archive","showSource":true}}}`: `{"target":"@archive","source":true}`,
		`{"users":{"1":{"target":"me","showSource":false},"2":{"target":"me","showSource":true}}}`:       `{"source":true}`,
		`{"users":{"7":{"target":"local","showSource":false}}}`:                                          `{"target":"local"}`,
		`{"users":{}}`: `{}`,
	}
	for raw, want := range cases {
		converted, err := ConvertMiBox([]byte(raw))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		var got, expected saveDocument
		if err := json.Unmarshal(converted, &got); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal([]byte(want), &expected)
		if got != expected {
			t.Errorf("%s converted to %s, want %s", raw, converted, want)
		}
	}
	if _, err := ConvertMiBox([]byte("not json")); err == nil {
		t.Error("broken JSON was accepted")
	}
}

// 来源说明的三种格式：单条、范围、按对话分组的批量；私聊的消息没有地址，不列出。
func TestSourceNotice(t *testing.T) {
	channel := func(id int) savedSource {
		return savedSource{link: messageLink{ChatID: "-1005000", ID: id}, title: "群"}
	}
	public := func(id int) savedSource {
		return savedSource{link: messageLink{Username: "durov", ID: id}, title: "Durov"}
	}
	private := savedSource{link: messageLink{ChatID: "777", ID: 3}, title: "某人"}

	if got := sourceNotice([]savedSource{private}, false); got != "" {
		t.Errorf("a private chat got a notice: %q", got)
	}
	single := sourceNotice([]savedSource{channel(2), private}, false)
	for _, wanted := range []string{"消息来源", `href="https://t.me/c/5000/2"`, "<b>群</b>", "<code>2</code>"} {
		if !strings.Contains(single, wanted) {
			t.Errorf("single notice lost %q:\n%s", wanted, single)
		}
	}
	ranged := sourceNotice([]savedSource{channel(1), channel(3), channel(8)}, true)
	for _, wanted := range []string{"范围保存来源", `href="https://t.me/c/5000/1">1</a>`, `href="https://t.me/c/5000/8">8</a>`} {
		if !strings.Contains(ranged, wanted) {
			t.Errorf("range notice lost %q:\n%s", wanted, ranged)
		}
	}
	batch := sourceNotice([]savedSource{channel(4), public(9), channel(2), channel(3), channel(7)}, false)
	for _, wanted := range []string{
		"批量保存来源",
		`<b>群</b>（4 条）：<a href="https://t.me/c/5000/2">2</a>-<a href="https://t.me/c/5000/4">4</a>, <a href="https://t.me/c/5000/7">7</a>`,
		`<b>Durov</b>（1 条）：<a href="https://t.me/durov/9">9</a>`,
	} {
		if !strings.Contains(batch, wanted) {
			t.Errorf("batch notice lost %q:\n%s", wanted, batch)
		}
	}
	if strings.Index(batch, "Durov") > strings.Index(batch, "群") {
		t.Errorf("chats are not sorted by title:\n%s", batch)
	}
	if _, _, err := bot.ParseHTML(batch); err != nil {
		t.Errorf("batch notice is not valid HTML: %v", err)
	}
}

func TestIDSpans(t *testing.T) {
	got := idSpans([]int{5, 1, 2, 2, 3, 9, 10, 7})
	want := [][2]int{{1, 3}, {5, 5}, {7, 7}, {9, 10}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("idSpans = %v, want %v", got, want)
	}
}

// 转发多条时，来源说明回复的是最后到达目标对话的那条。
func TestSentID(t *testing.T) {
	updates := &tg.Updates{Updates: []tg.UpdateClass{
		&tg.UpdateMessageID{ID: 41, RandomID: 1},
		&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 41}},
		&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 43}},
		&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 42}},
	}}
	if got := sentID(updates); got != 43 {
		t.Errorf("sentID = %d, want 43", got)
	}
	if got := sentID(&tg.UpdateShortSentMessage{ID: 7}); got != 7 {
		t.Errorf("short sent message = %d, want 7", got)
	}
	if got := sentID(&tg.UpdatesTooLong{}); got != 0 {
		t.Errorf("an unreadable reply = %d, want 0", got)
	}
}

func TestPublishLocalKeepsBothDownloads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "1_"+sanitizeSegment(strings.Repeat("𠀀", 80)))
	if !utf8.ValidString(path) {
		t.Fatal("split UTF-8 filename")
	}
	for _, body := range []string{"first", "second"} {
		src := filepath.Join(dir, "temp")
		if err := os.WriteFile(src, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := publishLocal(src, path, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		want := path
		if body == "second" {
			want += "_2"
		}
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "first" {
		t.Fatalf("first download: %q %v", got, err)
	}
	// 老附件占用了新附件对应的元数据名时，仍要另选新名字。
	other := filepath.Join(dir, "3_report")
	if err := os.WriteFile(other+".json", []byte("old attachment"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := publishLocal(path, other, []byte("metadata")); err != nil || got != other+"_2" {
		t.Fatalf("collision: %q %v", got, err)
	}
	if body, err := os.ReadFile(other + ".json"); err != nil || string(body) != "old attachment" {
		t.Fatalf("old attachment: %q %v", body, err)
	}
	if _, err := publishLocal(path, filepath.Join(path, "impossible"), nil); err == nil {
		t.Fatal("invalid destination accepted")
	}
}
