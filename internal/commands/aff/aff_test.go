package aff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

func TestEntitiesHTML(t *testing.T) {
	for _, c := range []struct {
		name     string
		text     string
		entities []tg.MessageEntityClass
		want     string
	}{
		{"plain text is escaped", `a<b> & "c"`, nil, `a&lt;b&gt; &amp; &quot;c&quot;`},
		{"bold and hidden link", "注册 点这里 立减", []tg.MessageEntityClass{
			&tg.MessageEntityBold{Offset: 0, Length: 2},
			&tg.MessageEntityTextURL{Offset: 3, Length: 3, URL: "https://x.example/?a=1&b=2"},
		}, `<b>注册</b> <a href="https://x.example/?a=1&amp;b=2">点这里</a> 立减`},
		// emoji 占两个 UTF-16 单位，后面的偏移要按单位算。
		{"offsets after an emoji", "🎉 优惠 码", []tg.MessageEntityClass{&tg.MessageEntityBold{Offset: 3, Length: 2}}, "🎉 <b>优惠</b> 码"},
		{"nested, same start", "abcdef", []tg.MessageEntityClass{
			&tg.MessageEntityItalic{Offset: 0, Length: 2}, &tg.MessageEntityBold{Offset: 0, Length: 6},
		}, "<b><i>ab</i>cdef</b>"},
		// 交叉的格式拆成成对嵌套的标签。
		{"overlapping", "abcdef", []tg.MessageEntityClass{
			&tg.MessageEntityBold{Offset: 0, Length: 4}, &tg.MessageEntityItalic{Offset: 2, Length: 4},
		}, "<b>ab<i>cd</i></b><i>ef</i>"},
		{"pre with language, code, spoiler, quote", "go\nx y z\nq", []tg.MessageEntityClass{
			&tg.MessageEntityPre{Offset: 0, Length: 2, Language: "go"},
			&tg.MessageEntityCode{Offset: 3, Length: 1},
			&tg.MessageEntitySpoiler{Offset: 5, Length: 1},
			&tg.MessageEntityBlockquote{Offset: 9, Length: 1, Collapsed: true},
		}, "<pre><code class=\"language-go\">go</code></pre>\n<code>x</code> <tg-spoiler>y</tg-spoiler> z\n<blockquote expandable>q</blockquote>"},
		// Telegram 自己会识别的（链接、@、话题标签）不加标签；越界的偏移截掉。
		{"auto entities and bad offsets", "see https://a.example #tag", []tg.MessageEntityClass{
			&tg.MessageEntityURL{Offset: 4, Length: 17}, &tg.MessageEntityHashtag{Offset: 22, Length: 4},
			&tg.MessageEntityUnderline{Offset: 22, Length: 99}, &tg.MessageEntityStrike{Offset: -3, Length: 2},
		}, "see https://a.example <u>#tag</u>"},
		{"custom emoji and mention", "⭐ hi", []tg.MessageEntityClass{
			&tg.MessageEntityCustomEmoji{Offset: 0, Length: 1, DocumentID: 5368324170671202286},
			&tg.MessageEntityMentionName{Offset: 2, Length: 2, UserID: 42},
		}, `<tg-emoji emoji-id="5368324170671202286">⭐</tg-emoji> <a href="tg://user?id=42">hi</a>`},
	} {
		got := entitiesHTML(c.text, c.entities)
		if got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
			continue
		}
		// 存下来的 HTML 发出去时要还原成同样的文字。
		plain, _, err := bot.ParseHTML(got)
		if err != nil || plain != c.text {
			t.Errorf("%s: parses back to %q, %v", c.name, plain, err)
		}
	}
}

// 不交叉的格式存成 HTML 再解析回来，格式实体原样还原。
func TestEntitiesHTMLRoundTrip(t *testing.T) {
	text := "机场 注册送 10G 👉 链接 粗斜"
	entities := []tg.MessageEntityClass{
		&tg.MessageEntityBold{Offset: 0, Length: 2},
		&tg.MessageEntityTextURL{Offset: 14, Length: 2, URL: "https://air.example/auth/register?code=abc"},
		&tg.MessageEntityBold{Offset: 17, Length: 2},
		&tg.MessageEntityItalic{Offset: 18, Length: 1},
	}
	_, parsed, err := bot.ParseHTML(entitiesHTML(text, entities))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != len(entities) {
		t.Fatalf("got %d entities back: %#v", len(parsed), parsed)
	}
	for index := range entities {
		want, got := entities[index], parsed[index]
		if want.TypeID() != got.TypeID() || want.GetOffset() != got.GetOffset() || want.GetLength() != got.GetLength() {
			t.Errorf("entity %d: %#v, want %#v", index, got, want)
		}
	}
	if link, ok := parsed[1].(*tg.MessageEntityTextURL); !ok || link.URL != "https://air.example/auth/register?code=abc" {
		t.Errorf("link = %#v", parsed[1])
	}
}

func TestNewEntry(t *testing.T) {
	now := time.UnixMilli(1727600000000)
	message := &tg.Message{Message: "买它 <3", Entities: []tg.MessageEntityClass{&tg.MessageEntityTextURL{Offset: 0, Length: 2, URL: "https://air.example/"}}}
	item := newEntry(message, message.Message, now)
	if item.Text != `<a href="https://air.example/">买它</a> &lt;3` || item.Format != "html" || item.WebPage == nil || !*item.WebPage ||
		item.LegacyWebPage != nil || item.CreatedAt != 1727600000000 {
		t.Errorf("newEntry = %+v", item)
	}
	// 没有原始消息时照样转义，不会把文字里的 < 当成标签。
	plain := newEntry(nil, "a<b", now)
	if plain.Text != "a&lt;b" || *plain.WebPage {
		t.Errorf("newEntry(nil) = %+v", plain)
	}
}

// 三代数据按各自的含义发送和预览；写回去时字段名不变，MiBox 读得回去。
func TestEntryFormatsAndJSON(t *testing.T) {
	raw := `{"affs":[{"text":"<b>legacy</b> &amp; x","web_page":true,"created_at":1},{"text":"<b>v2</b>","webPage":false},{"text":"<i>new</i>","webPage":true,"format":"html","created_at":2}]}`
	var doc document
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	for index, want := range []struct {
		html    bool
		preview string
	}{{true, "legacy & x"}, {false, "<b>v2</b>"}, {true, "new"}} {
		item := doc.Affs[index]
		if item.html() != want.html || preview(item) != want.preview {
			t.Errorf("entry %d: html %v, preview %q", index+1, item.html(), preview(item))
		}
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var before, after any
	if json.Unmarshal([]byte(raw), &before) != nil || json.Unmarshal(encoded, &after) != nil || !reflect.DeepEqual(before, after) {
		t.Errorf("round trip changed the data:\n got %s\nwant %s", encoded, raw)
	}
}

func newStore(t *testing.T, initial string) *store.Store[document] {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aff.json")
	if initial != "" {
		if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return store.New(path, defaults)
}

// TeleBox 原版的单条字段并进列表末尾，并且写回文件。
func TestLoadMergesLegacySingleEntry(t *testing.T) {
	data := newStore(t, `{"aff":{"text":"old","web_page":false},"affs":[{"text":"a","format":"html"}]}`)
	doc, err := load(data)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Aff != nil || len(doc.Affs) != 2 || doc.Affs[1].Text != "old" || !doc.Affs[1].html() {
		t.Errorf("load = %+v", doc)
	}
	content, _ := os.ReadFile(data.Path())
	if strings.Contains(string(content), `"aff"`) {
		t.Errorf("the legacy field is still on disk: %s", content)
	}
}

func TestAppendAndRemoveLimits(t *testing.T) {
	data := newStore(t, "")
	for i := 1; i <= maxEntries; i++ {
		index, err := appendEntry(data, entry{Text: "x", Format: "html"})
		if err != nil || index != i {
			t.Fatalf("append %d = %d, %v", i, index, err)
		}
	}
	_, err := appendEntry(data, entry{Text: "one too many"})
	if text, ok := kit.IsUserError(err); !ok || !strings.Contains(text, "32") {
		t.Errorf("the 33rd entry: %v", err)
	}
	if err := data.Update(func(doc *document) error {
		for i := range doc.Affs {
			doc.Affs[i].Text = string(rune('a' + i))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := removeEntry(data, 2); err != nil {
		t.Fatal(err)
	}
	doc, _ := data.Read()
	if len(doc.Affs) != maxEntries-1 || doc.Affs[0].Text != "a" || doc.Affs[1].Text != "c" {
		t.Errorf("after removing #2: %d entries, starting %q %q", len(doc.Affs), doc.Affs[0].Text, doc.Affs[1].Text)
	}
	for _, bad := range []int{0, -1, maxEntries} {
		if _, ok := kit.IsUserError(removeEntry(data, bad)); !ok {
			t.Errorf("removeEntry(%d) should be a user error", bad)
		}
	}
}

func TestPagesAndList(t *testing.T) {
	for count, want := range map[int]int{0: 1, 1: 1, 10: 1, 11: 2, 32: 4} {
		if got := pages(count); got != want {
			t.Errorf("pages(%d) = %d, want %d", count, got, want)
		}
	}
	for raw, want := range map[string]int{"1": 1, "4": 4, "5": 0, "0": 0, "01": 0, "-1": 0, "abc": 0, "": 0, "99999999999999999999": 0} {
		if got, ok := parsePage(raw, 32); got != want || ok != (want > 0) {
			t.Errorf("parsePage(%q) = %d, %v", raw, got, ok)
		}
	}
	var items []entry
	for i := 0; i < 12; i++ {
		items = append(items, entry{Text: "第" + string(rune('A'+i)) + "条 <b>粗</b>\n\n" + strings.Repeat("长", 40), Format: "html"})
	}
	items[0] = entry{Text: "a <b> & c", WebPage: new(bool)}
	first := renderList(".", items, 1)
	if !strings.Contains(first, "✈️ <b>Aff 列表</b> · 1/2") || !strings.Contains(first, "1. a &lt;b&gt; &amp; c") ||
		!strings.Contains(first, "10. 第J条 粗 "+strings.Repeat("长", 24)+"…") || strings.Contains(first, "11. ") ||
		!strings.Contains(first, "<code>.aff list 2</code>") {
		t.Errorf("page 1:\n%s", first)
	}
	second := renderList(".", items, 2)
	if !strings.Contains(second, "11. ") || !strings.Contains(second, "12. ") || strings.Contains(second, "list 3") {
		t.Errorf("page 2:\n%s", second)
	}
	if _, _, err := bot.ParseHTML(first); err != nil {
		t.Errorf("page 1 does not parse: %v", err)
	}
}
