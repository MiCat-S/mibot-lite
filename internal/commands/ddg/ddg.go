// Package ddg 实现 .ddg（.duckduckgo）：用 DuckDuckGo 的 HTML 版搜索网页，结果不够时
// 用 Firecrawl 的免 Key 搜索补上。
//
// 主机所在的机房 IP 常被 DuckDuckGo 当成机器人，拿到的是一页验证（anomaly），一条结果也没有；
// 这时整页结果都来自 Firecrawl。MiBox 早期用 Python 的 curl_cffi 伪装浏览器绕过验证，
// v2 已经不用，这里也不移植。
package ddg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

const (
	defaultLimit = 8
	maxLimit     = 15
	// maxQuery 是关键词的长度上限（字数），和 MiBox 一样 200。
	maxQuery = 200
	// maxURL 是结果链接转义后的长度上限，和 MiBox 一样；更长的多半是跟踪链接，丢掉。
	maxURL          = 1500
	titleRunes      = 200
	snippetRunes    = 400
	sourceDDG       = "DuckDuckGo"
	sourceFirecrawl = "Firecrawl"
)

// 两个服务的地址，测试里换成本地的假服务。
var (
	ddgURL       = "https://html.duckduckgo.com/html/"
	firecrawlURL = "https://api.firecrawl.dev/v2/search"
)

// result 是一条搜索结果。
type result struct {
	Title, URL, Snippet, Source string
}

// parseArgs 从参数里取出关键词和条数。条数写 -n 5（也认 --num、-l、--limit），放在哪里都行，
// 超出 1–15 的按边界算；-n 后面不是数字时，两个词都当关键词。
func parseArgs(args []string) (string, int) {
	limit := defaultLimit
	var words []string
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "-n", "--num", "-l", "--limit":
			if index+1 < len(args) {
				if n, err := strconv.Atoi(args[index+1]); err == nil {
					limit = kit.Clamp(n, 1, maxLimit)
					index++
					continue
				}
			}
		}
		words = append(words, args[index])
	}
	return strings.TrimSpace(strings.Join(words, " ")), limit
}

// searchDDG 向 DuckDuckGo 的 HTML 版发一次搜索，返回解析出的结果和是否碰到了验证页。
func searchDDG(ctx context.Context, query string, limit int) ([]result, bool, error) {
	response, err := httpx.Do(ctx, httpx.Request{Method: http.MethodPost, URL: ddgURL,
		Headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		Body:    []byte(url.Values{"q": {query}}.Encode()), Timeout: 15 * time.Second, MaxBytes: 2 << 20})
	if err != nil {
		return nil, false, err
	}
	// 验证页的状态码是 202，也算 2xx；认出来只为了在日志里说清楚为什么没有结果。
	blocked := bytes.Contains(response.Body, []byte("anomaly"))
	if !response.OK() {
		return nil, blocked, &httpx.StatusError{Status: response.Status}
	}
	items, err := parseDDG(response.Body, limit)
	return items, blocked, err
}

// parseDDG 解析 DuckDuckGo HTML 版的结果页。
//
// 每条结果是 class 含 result 的块，标题链接是 a.result__a，摘要是 .result__snippet。
// 广告块（result--ad）跳过：它们的链接指向 duckduckgo.com/y.js，MiBox 会把它们当成普通结果。
// 用真正的 HTML 解析器而不是正则切字符串，属性顺序、引号换了也认得出来。
func parseDDG(body []byte, limit int) ([]result, error) {
	root, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var items []result
	seen := map[string]bool{}
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if len(items) >= limit {
			return
		}
		if node.Type == html.ElementNode && hasClass(node, "result") {
			if !hasClass(node, "result--ad") {
				if item, ok := parseResult(node); ok && !seen[item.URL] {
					seen[item.URL] = true
					items = append(items, item)
				}
			}
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return items, nil
}

func parseResult(block *html.Node) (result, bool) {
	link := find(block, func(n *html.Node) bool { return n.Data == "a" && hasClass(n, "result__a") })
	if link == nil {
		return result{}, false
	}
	target, ok := resultURL(attr(link, "href"))
	title := clip(text(link), titleRunes)
	if !ok || title == "" {
		return result{}, false
	}
	snippet := ""
	if node := find(block, func(n *html.Node) bool { return hasClass(n, "result__snippet") }); node != nil {
		snippet = clip(text(node), snippetRunes)
	}
	return result{Title: title, URL: target, Snippet: snippet, Source: sourceDDG}, true
}

// resultURL 取出结果真正指向的地址：DuckDuckGo 把它包在 //duckduckgo.com/l/?uddg=… 里。
// 只要 http 和 https，指回 duckduckgo.com 自己的（广告跳转之类）不要。
func resultURL(raw string) (string, bool) {
	base, _ := url.Parse("https://duckduckgo.com/")
	parsed, err := base.Parse(raw)
	if err != nil {
		return "", false
	}
	if wrapped := parsed.Query().Get("uddg"); wrapped != "" {
		if parsed, err = url.Parse(wrapped); err != nil {
			return "", false
		}
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "duckduckgo.com" || strings.HasSuffix(host, ".duckduckgo.com") {
		return "", false
	}
	return checkedURL(parsed)
}

// checkedURL 只放行 http、https，转义后不超过 maxURL 的地址。
func checkedURL(parsed *url.URL) (string, bool) {
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", false
	}
	value := parsed.String()
	if len(command.Escape(value)) > maxURL {
		return "", false
	}
	return value, true
}

func hasClass(node *html.Node, class string) bool {
	for _, value := range strings.Fields(attr(node, "class")) {
		if value == class {
			return true
		}
	}
	return false
}

func attr(node *html.Node, key string) string {
	for _, a := range node.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// find 按文档顺序找第一个满足 match 的元素（不含 node 自己）。
func find(node *html.Node, match func(*html.Node) bool) *html.Node {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && match(child) {
			return child
		}
		if found := find(child, match); found != nil {
			return found
		}
	}
	return nil
}

// text 是元素里的全部文字，空白合并成一个空格。实体（&amp; 之类）解析器已经换好了。
func text(node *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return strings.Join(strings.Fields(b.String()), " ")
}

// clip 把文字截到 n 个字，截掉了就补一个省略号。
func clip(value string, n int) string { return command.Truncate(strings.TrimSpace(value), n) }

// firecrawlResponse 是 Firecrawl 搜索的应答。v2 接口把网页结果放在 data.web 里，
// 旧的 v1 直接是 data 数组，两种都认。
type firecrawlResponse struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

type firecrawlItem struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// searchFirecrawl 用 Firecrawl 的免 Key 搜索要 limit 条。
func searchFirecrawl(ctx context.Context, query string, limit int) ([]result, error) {
	response, err := httpx.PostJSON(ctx, firecrawlURL, nil, map[string]any{"query": query, "limit": limit}, 20*time.Second, 2<<20)
	if err != nil {
		return nil, err
	}
	if !response.OK() {
		return nil, &httpx.StatusError{Status: response.Status}
	}
	// 解析时不按 limit 截：返回的可能和 DuckDuckGo 的重复，merge 去重之后再截。
	return parseFirecrawl(response.Body, maxLimit)
}

func parseFirecrawl(body []byte, limit int) ([]result, error) {
	var decoded firecrawlResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("invalid firecrawl response: %w", err)
	}
	if !decoded.Success && decoded.Error != "" {
		return nil, errors.New("firecrawl: " + decoded.Error)
	}
	var list []firecrawlItem
	var nested struct {
		Web []firecrawlItem `json:"web"`
	}
	if json.Unmarshal(decoded.Data, &nested) == nil && nested.Web != nil {
		list = nested.Web
	} else if err := json.Unmarshal(decoded.Data, &list); err != nil {
		return nil, fmt.Errorf("unexpected firecrawl data: %w", err)
	}
	var items []result
	for _, item := range list {
		if len(items) >= limit {
			break
		}
		parsed, err := url.Parse(strings.TrimSpace(item.URL))
		if err != nil {
			continue
		}
		target, ok := checkedURL(parsed)
		if !ok {
			continue
		}
		title := clip(strings.Join(strings.Fields(item.Title), " "), titleRunes)
		if title == "" {
			title = parsed.Hostname()
		}
		items = append(items, result{Title: title, URL: target, Source: sourceFirecrawl,
			Snippet: clip(strings.Join(strings.Fields(item.Description), " "), snippetRunes)})
	}
	return items, nil
}

// merge 把补充的结果接在后面，同一个链接只留一条，最多 limit 条。
func merge(items, more []result, limit int) []result {
	seen := map[string]bool{}
	for _, item := range items {
		seen[item.URL] = true
	}
	for _, item := range more {
		if len(items) >= limit {
			break
		}
		if !seen[item.URL] {
			seen[item.URL] = true
			items = append(items, item)
		}
	}
	return items
}

// render 把结果排成若干页。一条结果不拆到两页：按条往页里装，装不下就换页。
func render(query string, items []result) []string {
	header := "🦆 <b>DuckDuckGo</b> · " + command.Code(query) + " · " + strconv.Itoa(len(items)) + " 条"
	if note := sourceNote(items); note != "" {
		header += "\n" + note
	}
	footer := `<a href="https://duckduckgo.com/?q=` + command.Escape(url.QueryEscape(query)) + `">在 DuckDuckGo 打开</a>`
	if len(items) == 0 {
		return []string{header + "\n\n没有找到结果，换个关键词试试\n\n" + footer}
	}
	blocks := make([]string, 0, len(items)+1)
	for index, item := range items {
		block := "<b>" + strconv.Itoa(index+1) + ".</b> " + `<a href="` + command.Escape(item.URL) + `">` + command.Escape(item.Title) + "</a>"
		if item.Snippet != "" {
			block += "\n<blockquote expandable>" + command.Escape(item.Snippet) + "</blockquote>"
		}
		blocks = append(blocks, block)
	}
	blocks = append(blocks, footer)
	var pages []string
	page := header + "\n"
	for _, block := range blocks {
		if kit.UTF16Len(page)+1+kit.UTF16Len(block) > command.PageLimit {
			pages = append(pages, strings.TrimRight(page, "\n"))
			page = "🦆 <b>DuckDuckGo</b> · 续\n"
		}
		page += "\n" + block
	}
	return append(pages, page)
}

// sourceNote 说明哪些结果来自 Firecrawl。Firecrawl 的结果总是接在 DuckDuckGo 的后面。
func sourceNote(items []result) string {
	first := -1
	for index, item := range items {
		if item.Source == sourceFirecrawl {
			first = index
			break
		}
	}
	switch {
	case first < 0:
		return ""
	case first == 0:
		return "DuckDuckGo 没有给出结果，以下来自 Firecrawl"
	case first == len(items)-1:
		return fmt.Sprintf("第 %d 条来自 Firecrawl", first+1)
	}
	return fmt.Sprintf("第 %d–%d 条来自 Firecrawl", first+1, len(items))
}

func help(prefix string) string {
	p := command.Escape(prefix)
	return "🦆 <b>DuckDuckGo 搜索</b>\n\n搜索网页，列出标题、链接和摘要。DuckDuckGo 拦下请求或结果不够时，" +
		"用 Firecrawl 的免 Key 搜索补上。\n\n" +
		"• <code>" + p + "ddg 关键词</code> 默认 " + strconv.Itoa(defaultLimit) + " 条\n" +
		"• <code>" + p + "ddg 关键词 -n 5</code> 指定条数，1–" + strconv.Itoa(maxLimit) + "\n" +
		"• <code>" + p + "duckduckgo 关键词</code> 同 " + command.Code(prefix+"ddg") + "\n\n" +
		"关键词最多 " + strconv.Itoa(maxQuery) + " 字。"
}

// Register 注册 .ddg（别名 .duckduckgo）。不存东西，每次现查。
func Register(a *app.App) {
	a.Registry.Register(&command.Command{Name: "ddg", Aliases: []string{"duckduckgo"}, Group: command.GroupTools,
		Description: "搜索网页", Usage: "关键词 [-n 条数]", Help: help, Timeout: time.Minute, FreeText: true,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			query, limit := parseArgs(inv.Args)
			if query == "" {
				return inv.Edit(ctx, help(inv.Prefix))
			}
			if len([]rune(query)) > maxQuery {
				return kit.Failf("关键词太长，最多 %d 字", maxQuery)
			}
			if err := inv.EditText(ctx, kit.Working("正在搜索 "+query)); err != nil {
				return err
			}
			items, err := search(ctx, inv.Log, query, limit)
			if err != nil {
				return kit.FailWith("搜索失败", err)
			}
			return inv.EditPages(ctx, render(query, items))
		}})
}

// search 先问 DuckDuckGo，不够 limit 条时用 Firecrawl 补上。两边都没有结果、
// Firecrawl 又失败时才算失败；DuckDuckGo 失败只记日志，它在机房 IP 上本来就常被拦。
func search(ctx context.Context, logger *slog.Logger, query string, limit int) ([]result, error) {
	items, blocked, err := searchDDG(ctx, query, limit)
	switch {
	case err != nil:
		logger.Info("ddg.primary_failed", "error", err.Error())
	case blocked && len(items) == 0:
		logger.Info("ddg.primary_blocked")
	}
	if len(items) >= limit {
		return items, nil
	}
	more, err := searchFirecrawl(ctx, query, limit-len(items))
	if err != nil {
		if len(items) == 0 {
			return nil, err
		}
		logger.Info("ddg.supplement_failed", "error", err.Error())
	}
	return merge(items, more, limit), nil
}
