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

// A page break must never split an escaped entity or a multi-byte rune.
func TestEscapedPagesKeepRunesWhole(t *testing.T) {
	for _, page := range EscapedPages(strings.Repeat("中", 50), 13) {
		if !strings.HasPrefix(page, "中") || strings.ContainsRune(page, '�') {
			t.Fatalf("page %q split a rune", page)
		}
	}
}

// HTML paging reopens the tags that were open at a page break, so the
// second page renders the same as the first.
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
