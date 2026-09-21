package command

import (
	"strings"

	"github.com/MiCat-S/mibot-lite/internal/bot"
)

// Escape renders untrusted text safe for HTML.
func Escape(value string) string { return bot.Escape(value) }

// Code wraps escaped text in <code>.
func Code(value string) string { return bot.Code(value) }

// Bold wraps escaped text in <b>.
func Bold(value string) string { return bot.Bold(value) }

// EscapedPages splits plain text into pages whose escaped HTML stays under
// limit characters, never splitting a character. Each page is escaped.
func EscapedPages(text string, limit int) []string {
	var pages []string
	var page strings.Builder
	for _, r := range text {
		escaped := Escape(string(r))
		if page.Len()+len(escaped) > limit {
			pages = append(pages, page.String())
			page.Reset()
		}
		page.WriteString(escaped)
	}
	if page.Len() > 0 || len(pages) == 0 {
		pages = append(pages, page.String())
	}
	return pages
}

var htmlTags = map[string]bool{"b": true, "strong": true, "i": true, "em": true, "u": true, "ins": true, "s": true,
	"strike": true, "del": true, "code": true, "pre": true, "a": true, "blockquote": true, "tg-spoiler": true}

// HTMLPages splits HTML into pages of at most limit characters, closing
// open tags at each page end and reopening them on the next. Malformed
// markup is rendered as visible text.
func HTMLPages(text string, limit int) []string {
	type open struct{ name, tag string }
	var pages []string
	var stack []open
	page, visible := "", false
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
	}
	appendToken := func(token string, content bool) {
		if len(page)+len(token)+len(closing()) > limit {
			flush()
		}
		page += token
		visible = visible || content
	}
	for _, token := range tokenizeHTML(text) {
		if strings.HasPrefix(token, "</") && strings.HasSuffix(token, ">") {
			name := strings.ToLower(strings.TrimSpace(token[2 : len(token)-1]))
			if len(stack) > 0 && stack[len(stack)-1].name == name {
				page += "</" + name + ">"
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
					if len(page)+len(token)+len(closing())+len(name)+3 > limit {
						flush()
					}
					page += token
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

// tokenizeHTML splits into tags, entities and text runs.
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
