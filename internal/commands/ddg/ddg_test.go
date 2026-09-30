package ddg

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

func TestParseArgs(t *testing.T) {
	for _, c := range []struct {
		in    []string
		query string
		limit int
	}{
		{[]string{"golang", "generics"}, "golang generics", defaultLimit},
		{[]string{"-n", "5", "golang"}, "golang", 5},
		{[]string{"golang", "-n", "99"}, "golang", maxLimit},
		{[]string{"golang", "--limit", "0"}, "golang", 1},
		{[]string{"--num", "3", "a", "-l", "4", "b"}, "a b", 4},
		{[]string{"-n", "abc"}, "-n abc", defaultLimit},
		{[]string{"golang", "-n"}, "golang -n", defaultLimit},
		{nil, "", defaultLimit},
	} {
		query, limit := parseArgs(c.in)
		if query != c.query || limit != c.limit {
			t.Errorf("parseArgs(%q) = %q, %d, want %q, %d", c.in, query, limit, c.query, c.limit)
		}
	}
}

// resultsPage 是 DuckDuckGo HTML 版结果页的样子（节选、简化）：一条广告、两条普通结果、
// 一条重复、一条 javascript: 链接，标题里有实体和 <b>。
const resultsPage = `<!DOCTYPE html><html><body><div id="links" class="results">
<div class="result results_links results_links_deep result--ad">
  <div class="links_main links_deep result__body"><h2 class="result__title">
  <a rel="nofollow" class="result__a" href="https://duckduckgo.com/y.js?ad_domain=ads.example&amp;u3=x">Buy Go Books</a></h2>
  <a class="result__snippet" href="https://duckduckgo.com/y.js?x">Sponsored</a></div>
</div>
<div class="result results_links results_links_deep web-result ">
  <div class="links_main links_deep result__body">
    <h2 class="result__title"><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2F%3Fa%3D1%26b%3D2&amp;rut=abc">The <b>Go</b> Programming Language &amp; Docs</a></h2>
    <div class="result__extras"><a class="result__url" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F">go.dev</a></div>
    <a class="result__snippet" href="//duckduckgo.com/l/?uddg=x">Go is an   open source
      programming language that makes it <b>simple</b> to build &lt;secure&gt; software.</a>
  </div>
</div>
<div class="result results_links web-result"><div class="result__body">
  <h2 class="result__title"><a href='https://pkg.go.dev/' class='result__a'>Go Packages</a></h2>
</div></div>
<div class="result results_links web-result"><div class="result__body">
  <h2 class="result__title"><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2F%3Fa%3D1%26b%3D2">Duplicate</a></h2>
</div></div>
<div class="result results_links web-result"><div class="result__body">
  <h2 class="result__title"><a class="result__a" href="javascript:alert(1)">Evil</a></h2>
</div></div>
</div></body></html>`

// anomalyPage 是机房 IP 拿到的验证页：没有结果块。
const anomalyPage = `<html><body><form id="challenge-form" action="//duckduckgo.com/anomaly.js?sv=html&amp;cc=botnet">
<div class="anomaly-modal__title">Unfortunately, bots use DuckDuckGo too.</div></form></body></html>`

func TestParseDDG(t *testing.T) {
	items, err := parseDDG([]byte(resultsPage), 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []result{
		{Title: "The Go Programming Language & Docs", URL: "https://go.dev/doc/?a=1&b=2",
			Snippet: "Go is an open source programming language that makes it simple to build <secure> software.", Source: sourceDDG},
		{Title: "Go Packages", URL: "https://pkg.go.dev/", Source: sourceDDG},
	}
	if len(items) != len(want) {
		t.Fatalf("got %d results: %+v", len(items), items)
	}
	for index := range want {
		if items[index] != want[index] {
			t.Errorf("result %d = %+v\nwant %+v", index+1, items[index], want[index])
		}
	}
	if items, _ := parseDDG([]byte(resultsPage), 1); len(items) != 1 {
		t.Errorf("limit 1 gave %d results", len(items))
	}
	if items, _ := parseDDG([]byte(anomalyPage), 10); len(items) != 0 {
		t.Errorf("the challenge page gave results: %+v", items)
	}
}

func TestParseFirecrawl(t *testing.T) {
	v2 := `{"success":true,"data":{"web":[
		{"url":"https://go.dev/","title":"The Go   Programming Language","description":"Build simple, secure, scalable systems with Go.","position":1},
		{"url":"ftp://files.example.com/x","title":"FTP"},
		{"url":"https://example.org/path?q=1","title":"","description":""},
		{"url":"https://third.example/","title":"Third"}
	]}}`
	items, err := parseFirecrawl([]byte(v2), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Title != "The Go Programming Language" || items[0].Source != sourceFirecrawl ||
		items[1].Title != "example.org" || items[1].URL != "https://example.org/path?q=1" {
		t.Errorf("v2 shape: %+v", items)
	}
	v1 := `{"success":true,"data":[{"url":"https://go.dev/","title":"Go","description":"d"}]}`
	if items, err := parseFirecrawl([]byte(v1), 5); err != nil || len(items) != 1 || items[0].Snippet != "d" {
		t.Errorf("v1 shape: %+v, %v", items, err)
	}
	if _, err := parseFirecrawl([]byte(`{"success":false,"error":"Rate limit exceeded"}`), 5); err == nil {
		t.Error("an error answer should be an error")
	}
	if _, err := parseFirecrawl([]byte(`<html>`), 5); err == nil {
		t.Error("a non-JSON answer should be an error")
	}
}

// fakeServices 冒充 DuckDuckGo 和 Firecrawl，记下 Firecrawl 被要了几条。
func fakeServices(t *testing.T, ddgStatus int, ddgBody string, firecrawl func(limit int) (int, string)) *int {
	t.Helper()
	asked := new(int)
	ddg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Method != http.MethodPost || r.PostForm.Get("q") == "" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		w.WriteHeader(ddgStatus)
		_, _ = io.WriteString(w, ddgBody)
	}))
	fc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		body, _ := io.ReadAll(r.Body)
		if json.Unmarshal(body, &request) != nil || request.Query == "" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		*asked = request.Limit
		status, answer := firecrawl(request.Limit)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(func() { ddg.Close(); fc.Close() })
	oldDDG, oldFC := ddgURL, firecrawlURL
	ddgURL, firecrawlURL = ddg.URL, fc.URL
	t.Cleanup(func() { ddgURL, firecrawlURL = oldDDG, oldFC })
	return asked
}

func firecrawlItems(n int) string {
	var items []string
	for i := 0; i < n; i++ {
		items = append(items, `{"url":"https://fc`+string(rune('a'+i))+`.example/","title":"FC `+string(rune('A'+i))+`"}`)
	}
	return `{"success":true,"data":{"web":[` + strings.Join(items, ",") + `]}}`
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// 验证页（202、没有结果）时整页结果都来自 Firecrawl。
func TestSearchFallsBackWhenBlocked(t *testing.T) {
	asked := fakeServices(t, http.StatusAccepted, anomalyPage, func(limit int) (int, string) { return 200, firecrawlItems(limit) })
	items, err := search(context.Background(), quiet, "golang", 5)
	if err != nil || len(items) != 5 || *asked != 5 || items[0].Source != sourceFirecrawl {
		t.Fatalf("search = %+v, %v (firecrawl asked for %d)", items, err, *asked)
	}
	if note := sourceNote(items); !strings.Contains(note, "以下来自 Firecrawl") {
		t.Errorf("note = %q", note)
	}
}

// DuckDuckGo 给得不够时，Firecrawl 只补差的条数，重复的链接不要。
func TestSearchSupplements(t *testing.T) {
	asked := fakeServices(t, http.StatusOK, resultsPage, func(int) (int, string) {
		return 200, `{"success":true,"data":{"web":[{"url":"https://pkg.go.dev/","title":"dup"},{"url":"https://fca.example/","title":"FC A"},{"url":"https://fcb.example/","title":"FC B"}]}}`
	})
	items, err := search(context.Background(), quiet, "golang", 4)
	if err != nil || *asked != 2 {
		t.Fatalf("search = %+v, %v (firecrawl asked for %d)", items, err, *asked)
	}
	var urls []string
	for _, item := range items {
		urls = append(urls, item.URL)
	}
	if strings.Join(urls, " ") != "https://go.dev/doc/?a=1&b=2 https://pkg.go.dev/ https://fca.example/ https://fcb.example/" {
		t.Errorf("merged = %q", urls)
	}
	if note := sourceNote(items); note != "第 3–4 条来自 Firecrawl" {
		t.Errorf("note = %q", note)
	}
}

// DuckDuckGo 够数时不问 Firecrawl；两边都失败时是给用户看的失败，不带地址。
func TestSearchEnoughAndFailures(t *testing.T) {
	asked := fakeServices(t, http.StatusOK, resultsPage, func(int) (int, string) { return 500, "" })
	if items, err := search(context.Background(), quiet, "golang", 2); err != nil || len(items) != 2 || *asked != 0 {
		t.Errorf("search = %+v, %v (firecrawl asked for %d)", items, err, *asked)
	}
	// 结果不够、Firecrawl 失败：有几条给几条。
	if items, err := search(context.Background(), quiet, "golang", 8); err != nil || len(items) != 2 {
		t.Errorf("partial search = %+v, %v", items, err)
	}
	fakeServices(t, http.StatusForbidden, "", func(int) (int, string) { return http.StatusTooManyRequests, `{"success":false}` })
	_, err := search(context.Background(), quiet, "golang", 8)
	if err == nil {
		t.Fatal("both services failed but search succeeded")
	}
	text, _ := kit.IsUserError(kit.FailWith("搜索失败", err))
	if !strings.HasPrefix(text, "搜索失败") || strings.Contains(text, "http") || strings.Contains(text, "127.0.0.1") {
		t.Errorf("failure text = %q", text)
	}
}

func TestRender(t *testing.T) {
	if pages := render("nothing <here>", nil); len(pages) != 1 || !strings.Contains(pages[0], "没有找到结果") || !strings.Contains(pages[0], "nothing &lt;here&gt;") {
		t.Errorf("empty render = %q", pages)
	}
	var items []result
	for i := 0; i < maxLimit; i++ {
		items = append(items, result{Title: strings.Repeat("标题<&>", 30), URL: "https://example.com/?q=" + strings.Repeat("x", 300) + "&n=" + string(rune('a'+i)),
			Snippet: strings.Repeat("摘要 ", 200), Source: sourceDDG})
	}
	pages := render("a&b", items)
	if len(pages) < 2 {
		t.Fatalf("15 long results fit on %d page", len(pages))
	}
	for index, page := range pages {
		if kit.UTF16Len(page) > command.PageLimit {
			t.Errorf("page %d has %d units", index+1, kit.UTF16Len(page))
		}
		if _, _, err := bot.ParseHTML(page); err != nil {
			t.Errorf("page %d does not parse: %v", index+1, err)
		}
	}
	if !strings.HasPrefix(pages[0], "🦆 <b>DuckDuckGo</b> · <code>a&amp;b</code> · 15 条") || !strings.HasPrefix(pages[1], "🦆 <b>DuckDuckGo</b> · 续") {
		t.Errorf("headers: %q / %q", firstLine(pages[0]), firstLine(pages[1]))
	}
	last := pages[len(pages)-1]
	if !strings.Contains(last, `href="https://duckduckgo.com/?q=`+url.QueryEscape("a&b")+`"`) {
		t.Errorf("footer link missing:\n%s", last)
	}
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}
