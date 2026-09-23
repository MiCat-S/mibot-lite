package yvlu

import (
	"strings"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/command"
)

// .yvlu 的参数语法按位置解析，而且不规则；下面这些是用户早已用顺手的写法。
func TestParseYvlu(t *testing.T) {
	cases := []struct {
		text     string
		ok       bool
		count    int
		reply    bool
		format   string
		fakeText string
	}{
		{".yvlu", true, 1, false, "quote", ""},
		{".yvlu 3", true, 3, false, "quote", ""},
		{".yvlu r", true, 1, true, "quote", ""},
		{".yvlu r 4", true, 4, true, "quote", ""},
		{".yvlu r stories 2", true, 2, true, "stories", ""},
		{".yvlu png", true, 1, false, "image", ""},
		{".yvlu image 5", true, 5, false, "image", ""},
		{".yvlu stories", true, 1, false, "stories", ""},
		{".yvlu f 你好 世界", true, 1, false, "quote", "你好 世界"},
		{".yvlu fr 测试", true, 1, true, "quote", "测试"},
		{".yvlu u 12345 2", true, 2, false, "quote", ""},
		{".yvlu ur @name", true, 1, true, "quote", ""},
		{".yvlu nonsense", false, 0, false, "", ""},
	}
	for _, item := range cases {
		fields := strings.Fields(item.text)
		inv := &command.Invocation{Prefix: ".", Command: "yvlu", Args: fields[1:], Text: item.text}
		options, ok := parseYvlu(inv)
		if ok != item.ok {
			t.Errorf("%q: parsed=%v, want %v", item.text, ok, item.ok)
			continue
		}
		if !ok {
			continue
		}
		if options.Count != item.count || options.IncludeReply != item.reply || options.Format != item.format {
			t.Errorf("%q: count=%d reply=%v format=%q", item.text, options.Count, options.IncludeReply, options.Format)
		}
		if options.FakeText != item.fakeText {
			t.Errorf("%q: fake text %q, want %q", item.text, options.FakeText, item.fakeText)
		}
	}
}

func TestConvertEntities(t *testing.T) {
	entities := []tg.MessageEntityClass{
		&tg.MessageEntityBold{Offset: 0, Length: 4},
		&tg.MessageEntityTextURL{Offset: 5, Length: 3, URL: "https://example.com"},
		&tg.MessageEntityCustomEmoji{Offset: 9, Length: 2, DocumentID: 77},
		&tg.MessageEntityPre{Offset: 12, Length: 5, Language: "go"},
		&tg.MessageEntityUnknown{Offset: 20, Length: 1},
	}
	converted := convertEntities(entities, 0)
	if len(converted) != 4 {
		t.Fatalf("converted %d entities, want 4 (the unknown one is dropped)", len(converted))
	}
	if converted[0].Type != "bold" || converted[1].Type != "text_link" || converted[1].URL != "https://example.com" {
		t.Fatalf("unexpected conversion: %+v", converted)
	}
	if converted[2].CustomEmojiID != "77" || converted[3].Language != "go" {
		t.Fatalf("unexpected conversion: %+v", converted)
	}
}

// 伪造的文字是从命令中间截出来的，所以保留下来的实体要跟着平移，
// 横跨截断点的实体要被裁掉一截。
func TestConvertEntitiesShift(t *testing.T) {
	converted := convertEntities([]tg.MessageEntityClass{
		&tg.MessageEntityBold{Offset: 10, Length: 4},
		&tg.MessageEntityItalic{Offset: 6, Length: 6},
		&tg.MessageEntityCode{Offset: 0, Length: 3},
	}, 8)
	if len(converted) != 2 {
		t.Fatalf("converted %+v, want the two that reach past the cut", converted)
	}
	if converted[0].Offset != 2 || converted[0].Length != 4 {
		t.Errorf("shifted entity is %+v", converted[0])
	}
	if converted[1].Offset != 0 || converted[1].Length != 4 {
		t.Errorf("straddling entity should clip to 0..4, got %+v", converted[1])
	}
}

func TestNameHashIsStableAndPositive(t *testing.T) {
	first, second := nameHash("某个频道"), nameHash("某个频道")
	if first != second {
		t.Fatalf("the same name hashed to %d and %d", first, second)
	}
	if first < 0 {
		t.Fatalf("hash must be positive to serve as an id, got %d", first)
	}
	if nameHash("a") == nameHash("b") {
		t.Fatal("different names should not collide this easily")
	}
}
