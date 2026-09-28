package command

import (
	"strings"
	"testing"
)

func TestEscape(t *testing.T) {
	if got := Escape(`<b>&"x"</b>`); got != "&lt;b&gt;&amp;&quot;x&quot;&lt;/b&gt;" {
		t.Fatalf("got %q", got)
	}
}

func TestEscapedPagesRespectLimit(t *testing.T) {
	pages := EscapedPages(strings.Repeat("<", 100), 20)
	for _, page := range pages {
		if len(page) > 20 {
			t.Fatalf("page of %d characters exceeds the limit", len(page))
		}
		if strings.Contains(page, "<") {
			t.Fatal("page content must be escaped")
		}
	}
	if strings.Count(strings.Join(pages, ""), "&lt;") != 100 {
		t.Fatal("paging lost characters")
	}
}

// 分页时绝不能把转义后的实体或多字节字符拆开。
func TestEscapedPagesKeepRunesWhole(t *testing.T) {
	for _, page := range EscapedPages(strings.Repeat("中", 50), 13) {
		if !strings.HasPrefix(page, "中") || strings.ContainsRune(page, '�') {
			t.Fatalf("page %q split a rune", page)
		}
	}
}

// HTML 分页会在下一页重新打开分页处还没闭合的标签，
// 所以第二页的显示效果和第一页一样。
func TestHTMLPagesReopenTags(t *testing.T) {
	pages := HTMLPages("<b>"+strings.Repeat("a", 60)+"</b>", 40)
	if len(pages) < 2 {
		t.Fatalf("expected several pages, got %d", len(pages))
	}
	for index, page := range pages {
		if !strings.HasPrefix(page, "<b>") || !strings.HasSuffix(page, "</b>") {
			t.Fatalf("page %d is not balanced: %q", index, page)
		}
	}
}

func TestHTMLPagesEscapesStrayMarkup(t *testing.T) {
	page := HTMLPages("a < b & c <script>x</script>", 4000)[0]
	if strings.Contains(page, "<script>") {
		t.Fatalf("unknown tags must become text: %q", page)
	}
	if !strings.Contains(page, "&lt;script&gt;") || !strings.Contains(page, "a &lt; b &amp; c") {
		t.Fatalf("got %q", page)
	}
}

func TestHTMLPagesKeepsAllowedAttributes(t *testing.T) {
	page := HTMLPages(`<a href="https://example.com">x</a><blockquote expandable>y</blockquote>`, 4000)[0]
	if !strings.Contains(page, `<a href="https://example.com">`) || !strings.Contains(page, "<blockquote expandable>") {
		t.Fatalf("allowed markup was dropped: %q", page)
	}
	if page := HTMLPages(`<a href="javascript:alert(1)">x</a>`, 4000)[0]; strings.Contains(page, "<a href") {
		t.Fatalf("a non-http link must not survive as markup: %q", page)
	}
}

// 长度按 Telegram 的 UTF-16 单位算：5000 个汉字是两页，不是按字节算出来的四页；
// 带 emoji 的也不会超过上限。
func TestPagesCountUTF16Units(t *testing.T) {
	chinese := strings.Repeat("中", 5000)
	if pages := EscapedPages(chinese, PageLimit); len(pages) != 2 {
		t.Errorf("5000 个汉字分成了 %d 页，应为 2", len(pages))
	}
	if pages := HTMLPages("<b>"+chinese+"</b>", PageLimit); len(pages) != 2 {
		t.Errorf("HTML 里 5000 个汉字分成了 %d 页，应为 2", len(pages))
	}
	for _, page := range HTMLPages(strings.Repeat("😀a", 3000), PageLimit) {
		if n := UTF16Len(page); n > PageLimit {
			t.Errorf("一页有 %d 个单位，超过了 %d", n, PageLimit)
		}
	}
}
