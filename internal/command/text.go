package command

import (
	"strings"

	"github.com/MiCat-S/mibot-lite/internal/bot"
)

// PageLimit 是一页消息 HTML 源文的长度上限，按 UTF-16 单位计（Telegram 的算法）。
// Telegram 一条消息最多 4096 个单位，而且只算解析后的文字、不算标签；这里按源文算，
// 本来就偏保守，再给调用方在首页加标题、在每页外面套 <pre> 之类的留出 300 左右的余地。
const PageLimit = 3800

// UTF16Len 是 text 按 Telegram 的算法（UTF-16 单位）有多长：一个汉字算 1，
// 一个 emoji 之类的补充平面字符算 2。
func UTF16Len(text string) int {
	count := 0
	for _, r := range text {
		if r >= 0x10000 {
			count += 2
		} else {
			count++
		}
	}
	return count
}

// Escape 把不可信的文本转义成能安全放进 HTML 的形式。
func Escape(value string) string { return bot.Escape(value) }

// Code 用 <code> 包住转义后的文本。
func Code(value string) string { return bot.Code(value) }

// Bold 用 <b> 包住转义后的文本。
func Bold(value string) string { return bot.Bold(value) }

// EscapedPages 把纯文本分页，每页转义后的 HTML 不超过 limit 个 UTF-16 单位，
// 不会把一个字符拆开。每一页都已经转义过。不按字节算：中文一个字 3 字节，
// 那样一页只装得下三分之一。
func EscapedPages(text string, limit int) []string {
	var pages []string
	var page strings.Builder
	units := 0
	for _, r := range text {
		escaped := Escape(string(r))
		size := UTF16Len(escaped)
		if units+size > limit {
			pages = append(pages, page.String())
			page.Reset()
			units = 0
		}
		page.WriteString(escaped)
		units += size
	}
	if page.Len() > 0 || len(pages) == 0 {
		pages = append(pages, page.String())
	}
	return pages
}

var htmlTags = map[string]bool{"b": true, "strong": true, "i": true, "em": true, "u": true, "ins": true, "s": true,
	"strike": true, "del": true, "code": true, "pre": true, "a": true, "blockquote": true, "tg-spoiler": true}

// HTMLPages 把 HTML 分成每页最多 limit 个 UTF-16 单位，在每页末尾闭合还开着的
// 标签，到下一页再重新打开。格式不对的标记按可见文本显示。
func HTMLPages(text string, limit int) []string {
	type open struct{ name, tag string }
	var pages []string
	var stack []open
	page, visible := "", false
	// units 是 page 的 UTF-16 长度，随 page 一起更新，不用每次重算。标签只有 ASCII，
	// 它们的 len 就是单位数。
	units := 0
	closing := func() string {
		var b strings.Builder
		for index := len(stack) - 1; index >= 0; index-- {
			b.WriteString("</" + stack[index].name + ">")
		}
		return b.String()
	}
	flush := func() {
		if visible {
			pages = append(pages, page+closing())
		}
		var b strings.Builder
		for _, tag := range stack {
			b.WriteString(tag.tag)
		}
		page, visible = b.String(), false
		units = UTF16Len(page)
	}
	appendToken := func(token string, content bool) {
		size := UTF16Len(token)
		if units+size+len(closing()) > limit {
			flush()
		}
		page += token
		units += size
		visible = visible || content
	}
	for _, token := range tokenizeHTML(text) {
		if strings.HasPrefix(token, "</") && strings.HasSuffix(token, ">") {
			name := strings.ToLower(strings.TrimSpace(token[2 : len(token)-1]))
			if len(stack) > 0 && stack[len(stack)-1].name == name {
				page += "</" + name + ">"
				units += len(name) + 3
				stack = stack[:len(stack)-1]
				continue
			}
		}
		if strings.HasPrefix(token, "<") && strings.HasSuffix(token, ">") && !strings.HasPrefix(token, "</") && len(token) < 1000 && len(stack) < 16 {
			body := token[1 : len(token)-1]
			name, attrs, _ := strings.Cut(body, " ")
			name = strings.ToLower(name)
			if htmlTags[name] {
				attrs = strings.TrimSpace(attrs)
				valid := attrs == ""
				switch name {
				case "a":
					valid = strings.HasPrefix(attrs, "href=\"") && strings.HasSuffix(attrs, "\"") && (strings.HasPrefix(attrs, "href=\"http") || strings.HasPrefix(attrs, "href=\"tg://"))
				case "blockquote":
					valid = attrs == "" || attrs == "expandable"
				}
				if valid {
					if units+UTF16Len(token)+len(closing())+len(name)+3 > limit {
						flush()
					}
					page += token
					units += UTF16Len(token)
					stack = append(stack, open{name: name, tag: token})
					continue
				}
			}
		}
		if isEntity(token) {
			appendToken(token, true)
			continue
		}
		for _, r := range token {
			appendToken(Escape(string(r)), true)
		}
	}
	flush()
	if len(pages) == 0 {
		return []string{""}
	}
	return pages
}

func isEntity(token string) bool {
	if !strings.HasPrefix(token, "&") || !strings.HasSuffix(token, ";") {
		return false
	}
	switch strings.ToLower(token[1 : len(token)-1]) {
	case "amp", "lt", "gt", "quot", "apos":
		return true
	}
	body := token[1 : len(token)-1]
	if strings.HasPrefix(body, "#x") || strings.HasPrefix(body, "#X") {
		return len(body) > 2
	}
	return strings.HasPrefix(body, "#") && len(body) > 1
}

// tokenizeHTML 把文本切成标签、实体和连续的普通文本。
func tokenizeHTML(text string) []string {
	var tokens []string
	for index := 0; index < len(text); {
		switch text[index] {
		case '<':
			end := strings.IndexByte(text[index:], '>')
			if end < 0 {
				tokens = append(tokens, text[index:index+1])
				index++
				continue
			}
			tokens = append(tokens, text[index:index+end+1])
			index += end + 1
		case '&':
			end := strings.IndexByte(text[index:], ';')
			if end < 0 || end > 12 {
				tokens = append(tokens, text[index:index+1])
				index++
				continue
			}
			candidate := text[index : index+end+1]
			if isEntity(candidate) {
				tokens = append(tokens, candidate)
				index += end + 1
			} else {
				tokens = append(tokens, "&")
				index++
			}
		default:
			end := strings.IndexAny(text[index:], "<&")
			if end < 0 {
				tokens = append(tokens, text[index:])
				index = len(text)
			} else {
				tokens = append(tokens, text[index:index+end])
				index += end
			}
		}
	}
	return tokens
}
