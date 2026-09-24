package ai

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/MiCat-S/mibot-lite/internal/command"
)

// 这里把模型回答的 Markdown 转成两种输出：Telegram 的 HTML（聊天消息）和
// Telegraph 的节点（长文页面）。块级规则沿用 MiBox ai 插件的 TelegramFormatter：
// 标题转粗体、列表用「• / 1.」加缩进模拟、代码块转 <pre>、引用转 <blockquote>，
// 其余文本一律转义。

type mdBlockKind int

const (
	mdParagraph mdBlockKind = iota
	mdHeading
	mdCode
	mdQuote
	mdList
	mdRule
)

// mdBlock 是一个块：段落、标题、代码块、引用、列表或分隔线。
type mdBlock struct {
	kind  mdBlockKind
	level int
	lang  string
	text  string
	lines []string
	items []*mdItem
}

// mdItem 是列表的一项，children 是缩进更深的子项。
type mdItem struct {
	ordered  bool
	number   int
	indent   int
	lines    []string
	children []*mdItem
}

var (
	mdRulePattern    = regexp.MustCompile(`^\s*(?:---|\*\*\*)\s*$`)
	mdHeadingPattern = regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(.+?)\s*#*\s*$`)
	mdBulletPattern  = regexp.MustCompile(`^(\s{0,12})([-*+])\s+(.+)$`)
	mdOrderedPattern = regexp.MustCompile(`^(\s{0,12})(\d{1,9})[.)]\s+(.+)$`)
	mdIndentedCode   = regexp.MustCompile(`^ {4,}\S`)
	mdCitePattern    = regexp.MustCompile(`(?i)<\s*/?\s*cite\s*>|<\s*cite\s*/\s*>|&lt;\s*/?\s*cite\s*&gt;|&lt;\s*cite\s*/\s*&gt;`)
	mdLanguage       = regexp.MustCompile(`^[a-z0-9_+-]+$`)
	mdLeadingSpaces  = regexp.MustCompile(`^( {2,})(.*)$`)
)

const mdMaxListDepth = 3

func mdFence(line string) bool { return strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") }

func mdQuoteLine(line string) bool { return strings.HasPrefix(strings.TrimLeft(line, " \t"), ">") }

func mdBlank(line string) bool { return strings.TrimSpace(line) == "" }

// mdMarker 识别列表标记，返回缩进、是否有序、序号和正文。
func mdMarker(line string) (indent int, ordered bool, number int, text string, ok bool) {
	if match := mdBulletPattern.FindStringSubmatch(line); match != nil {
		return len(match[1]), false, 0, match[3], true
	}
	if match := mdOrderedPattern.FindStringSubmatch(line); match != nil {
		number, _ = strconv.Atoi(match[2])
		return len(match[1]), true, number, match[3], true
	}
	return 0, false, 0, "", false
}

// parseMarkdown 把 Markdown 切成块。
func parseMarkdown(source string) []mdBlock {
	source = strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(source, "\n")
	var blocks []mdBlock
	var paragraph []string
	flush := func() {
		if len(paragraph) > 0 && strings.TrimSpace(strings.Join(paragraph, "\n")) != "" {
			blocks = append(blocks, mdBlock{kind: mdParagraph, lines: paragraph})
		}
		paragraph = nil
	}
	for index := 0; index < len(lines); {
		line := lines[index]
		switch {
		case mdBlank(line):
			flush()
			index++
		case mdRulePattern.MatchString(line):
			flush()
			blocks = append(blocks, mdBlock{kind: mdRule})
			index++
		case mdFence(line):
			flush()
			info := strings.TrimSpace(strings.TrimLeft(strings.TrimLeft(line, " \t"), "`"))
			lang := ""
			if fields := strings.Fields(info); len(fields) > 0 {
				lang = fields[0]
			}
			index++
			var code []string
			for index < len(lines) && !mdFence(lines[index]) {
				code = append(code, lines[index])
				index++
			}
			if index < len(lines) {
				index++
			}
			blocks = append(blocks, mdBlock{kind: mdCode, lang: lang, text: strings.Join(code, "\n")})
		case len(paragraph) == 0 && mdIndentedCode.MatchString(line) && !mdIsMarker(line):
			var code []string
			for index < len(lines) && (mdIndentedCode.MatchString(lines[index]) || mdBlank(lines[index])) {
				if mdBlank(lines[index]) {
					code = append(code, "")
				} else {
					code = append(code, strings.TrimPrefix(lines[index], "    "))
				}
				index++
			}
			for len(code) > 0 && code[len(code)-1] == "" {
				code = code[:len(code)-1]
			}
			blocks = append(blocks, mdBlock{kind: mdCode, text: strings.Join(code, "\n")})
		case mdQuoteLine(line):
			flush()
			var quote []string
			for index < len(lines) && mdQuoteLine(lines[index]) {
				quote = append(quote, lines[index])
				index++
			}
			blocks = append(blocks, mdBlock{kind: mdQuote, lines: quote})
		case mdHeadingPattern.MatchString(line):
			flush()
			match := mdHeadingPattern.FindStringSubmatch(line)
			blocks = append(blocks, mdBlock{kind: mdHeading, level: len(match[1]), text: strings.TrimSpace(match[2])})
			index++
		case mdIsMarker(line):
			flush()
			var items []*mdItem
			items, index = parseMarkdownList(lines, index)
			blocks = append(blocks, mdBlock{kind: mdList, items: items})
		default:
			paragraph = append(paragraph, line)
			index++
		}
	}
	flush()
	return blocks
}

func mdIsMarker(line string) bool {
	_, _, _, _, ok := mdMarker(line)
	return ok
}

// parseMarkdownList 读取从 start 开始的连续列表区域，遇到空行或其他块的开头为止；
// 按缩进建成最多三层的树，更深的标记并入当前项作为续行。
func parseMarkdownList(lines []string, start int) ([]*mdItem, int) {
	root := &mdItem{indent: -1}
	stack := []*mdItem{root}
	var current *mdItem
	index := start
	for ; index < len(lines); index++ {
		line := lines[index]
		if mdBlank(line) || mdRulePattern.MatchString(line) || mdQuoteLine(line) || mdFence(line) || mdHeadingPattern.MatchString(line) {
			break
		}
		indent, ordered, number, text, ok := mdMarker(line)
		if !ok {
			if current != nil {
				current.lines = append(current.lines, strings.TrimSpace(line))
			}
			continue
		}
		for len(stack) > 1 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		if len(stack) > mdMaxListDepth && current != nil {
			current.lines = append(current.lines, text)
			continue
		}
		item := &mdItem{ordered: ordered, number: number, indent: indent, lines: []string{text}}
		parent := stack[len(stack)-1]
		parent.children = append(parent.children, item)
		stack = append(stack, item)
		current = item
	}
	return root.children, index
}

// markdownToHTML 把 Markdown 转成 Telegram 的 HTML。collapseSafe 为真时外层会整体
// 包进 <blockquote expandable>，引用块就不再输出 <blockquote>，以免嵌套。
func markdownToHTML(markdown string, collapseSafe bool) string {
	markdown = mdCitePattern.ReplaceAllString(markdown, "")
	var rendered []string
	for _, block := range parseMarkdown(markdown) {
		if html := renderBlockHTML(block, collapseSafe); html != "" {
			rendered = append(rendered, html)
		}
	}
	return strings.Join(rendered, "\n\n")
}

func renderBlockHTML(block mdBlock, collapseSafe bool) string {
	switch block.kind {
	case mdRule:
		return "────────────────"
	case mdHeading:
		return headingHTML(block.text, block.level)
	case mdCode:
		if strings.TrimSpace(block.text) == "" {
			return ""
		}
		lang := strings.ToLower(strings.TrimSpace(block.lang))
		if lang != "" && mdLanguage.MatchString(lang) {
			return `<pre><code class="language-` + lang + `">` + command.Escape(block.text) + "</code></pre>"
		}
		return "<pre><code>" + command.Escape(block.text) + "</code></pre>"
	case mdQuote:
		var out []string
		for _, line := range block.lines {
			depth, rest := quoteDepth(line)
			rest = strings.TrimRight(rest, " \t")
			var html string
			if match := mdHeadingPattern.FindStringSubmatch(rest); match != nil {
				html = headingHTML(strings.TrimSpace(match[2]), len(match[1]))
			} else {
				html = inlineHTML(parseInline(rest, false))
			}
			prefix := ""
			if depth > 1 {
				prefix = strings.Repeat("┃ ", min(3, depth-1))
			}
			out = append(out, prefix+html)
		}
		body := strings.Join(out, "\n")
		if collapseSafe || strings.TrimSpace(body) == "" {
			return body
		}
		return "<blockquote>" + body + "</blockquote>"
	case mdList:
		var out []string
		renderListHTML(block.items, 1, &out)
		return strings.Join(out, "\n")
	}
	var out []string
	for _, line := range block.lines {
		indent, rest := leadingIndent(line)
		out = append(out, indent+inlineHTML(parseInline(rest, false)))
	}
	return strings.TrimRight(strings.Join(out, "\n"), " \n")
}

func headingHTML(text string, level int) string {
	body := inlineHTML(parseInline(strings.TrimSpace(text), false))
	switch level {
	case 2:
		return "<b>▌" + body + "</b>"
	case 3:
		return "<b>• " + body + "</b>"
	}
	return "<b>" + body + "</b>"
}

// quoteDepth 数出行首连续的 >（中间可以夹空格），返回层数和剩下的正文。
func quoteDepth(line string) (int, string) {
	depth, index := 0, 0
	for index < len(line) {
		switch line[index] {
		case ' ', '\t':
			index++
			continue
		case '>':
			depth++
			index++
			if index < len(line) && line[index] == ' ' {
				index++
			}
			continue
		}
		break
	}
	return depth, line[index:]
}

// leadingIndent 把行首两个以上的空格换成不换行空格，Telegram 才不会把缩进吃掉。
func leadingIndent(line string) (string, string) {
	match := mdLeadingSpaces.FindStringSubmatch(line)
	if match == nil {
		return "", line
	}
	return strings.Repeat(" ", len(match[1])), match[2]
}

func renderListHTML(items []*mdItem, depth int, out *[]string) {
	for _, item := range items {
		indent := strings.Repeat(" ", (depth-1)*2)
		marker := "• "
		if item.ordered {
			marker = strconv.Itoa(item.number) + ". "
		}
		*out = append(*out, indent+marker+inlineHTML(parseInline(item.lines[0], false)))
		for _, line := range item.lines[1:] {
			*out = append(*out, strings.Repeat(" ", (depth-1)*2+2)+inlineHTML(parseInline(line, false)))
		}
		renderListHTML(item.children, depth+1, out)
	}
}

type mdInlineKind int

const (
	mdText mdInlineKind = iota
	mdCodeSpan
	mdBold
	mdItalic
	mdStrike
	mdUnderline
	mdSpoiler
	mdLink
	mdImage
)

// mdInline 是行内的一段：纯文本、行内代码、各种强调、链接或图片。
type mdInline struct {
	kind     mdInlineKind
	text     string
	url      string
	children []mdInline
}

// safeURL 只接受 http 和 https 链接。
func safeURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return ""
	}
	return raw
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// urlStop 判断自动识别的裸链接在这里结束：空白、尖括号、圆括号、中文标点和全角符号。
func urlStop(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("<>()", r) || r >= 0x3000 && r <= 0x303f || r >= 0xff00 && r <= 0xffef
}

// parseInline 解析一行里的行内格式。inLink 为真时不再识别链接，免得链接套链接。
func parseInline(text string, inLink bool) []mdInline {
	var nodes []mdInline
	var plain strings.Builder
	emit := func(node mdInline) {
		if plain.Len() > 0 {
			nodes = append(nodes, mdInline{kind: mdText, text: plain.String()})
			plain.Reset()
		}
		nodes = append(nodes, node)
	}
	for index := 0; index < len(text); {
		rest := text[index:]
		previous := byte(' ')
		if index > 0 {
			previous = text[index-1]
		}
		switch {
		case rest[0] == '`':
			run := len(rest) - len(strings.TrimLeft(rest, "`"))
			fence := rest[:run]
			if end := strings.Index(rest[run:], fence); end > 0 && !strings.HasPrefix(rest[run+end+run:], "`") {
				emit(mdInline{kind: mdCodeSpan, text: rest[run : run+end]})
				index += run + end + run
				continue
			}
			plain.WriteString(fence)
			index += run
			continue
		case !inLink && strings.HasPrefix(rest, "!["):
			if label, target, size, ok := linkAt(rest[1:]); ok {
				emit(mdInline{kind: mdImage, text: label, url: safeURL(target)})
				index += 1 + size
				continue
			}
		case !inLink && rest[0] == '[':
			if label, target, size, ok := linkAt(rest); ok {
				if safe := safeURL(target); safe != "" {
					emit(mdInline{kind: mdLink, url: safe, children: parseInline(label, true)})
				} else {
					for _, child := range parseInline(label, true) {
						emit(child)
					}
				}
				index += size
				continue
			}
		case !inLink && !isWordByte(previous) && (hasPrefixFold(rest, "http://") || hasPrefixFold(rest, "https://")):
			end := strings.IndexFunc(rest, urlStop)
			if end < 0 {
				end = len(rest)
			}
			link := strings.TrimRight(rest[:end], ".,!?;:'\"")
			if safe := safeURL(link); safe != "" {
				emit(mdInline{kind: mdLink, url: safe, children: []mdInline{{kind: mdText, text: link}}})
				index += len(link)
				continue
			}
		case strings.HasPrefix(rest, "||"):
			if inner, size, ok := delimited(rest, "||", false); ok {
				emit(mdInline{kind: mdSpoiler, children: parseInline(inner, inLink)})
				index += size
				continue
			}
		case strings.HasPrefix(rest, "~~"):
			if inner, size, ok := delimited(rest, "~~", false); ok {
				emit(mdInline{kind: mdStrike, children: parseInline(inner, inLink)})
				index += size
				continue
			}
		case strings.HasPrefix(rest, "**"):
			if inner, size, ok := delimited(rest, "**", false); ok {
				emit(mdInline{kind: mdBold, children: parseInline(inner, inLink)})
				index += size
				continue
			}
		case strings.HasPrefix(rest, "__") && !isWordByte(previous):
			if inner, size, ok := delimited(rest, "__", true); ok {
				emit(mdInline{kind: mdUnderline, children: parseInline(inner, inLink)})
				index += size
				continue
			}
		case rest[0] == '*' && previous != '*':
			if inner, size, ok := delimited(rest, "*", false); ok && !strings.Contains(inner, "*") {
				emit(mdInline{kind: mdItalic, children: parseInline(inner, inLink)})
				index += size
				continue
			}
		case rest[0] == '_' && previous != '_' && !isWordByte(previous):
			if inner, size, ok := delimited(rest, "_", true); ok && !strings.Contains(inner, "_") {
				emit(mdInline{kind: mdItalic, children: parseInline(inner, inLink)})
				index += size
				continue
			}
		}
		_, width := utf8.DecodeRuneInString(rest)
		plain.WriteString(rest[:width])
		index += width
	}
	if plain.Len() > 0 {
		nodes = append(nodes, mdInline{kind: mdText, text: plain.String()})
	}
	return nodes
}

func hasPrefixFold(text, prefix string) bool {
	return len(text) >= len(prefix) && strings.EqualFold(text[:len(prefix)], prefix)
}

// delimited 在 text 开头找一对 marker 包住的内容：内容不能为空，不能以空白开头或结尾，
// 结束标记后不能紧跟同一个字符；wordBoundary 为真时结束标记后也不能紧跟字母数字，
// 这样 snake_case 里的下划线不会被当成斜体。
func delimited(text, marker string, wordBoundary bool) (string, int, bool) {
	body := text[len(marker):]
	for offset := 0; offset < len(body); {
		end := strings.Index(body[offset:], marker)
		if end < 0 {
			return "", 0, false
		}
		end += offset
		after := body[end+len(marker):]
		if strings.HasPrefix(after, marker[:1]) || wordBoundary && after != "" && isWordByte(after[0]) {
			offset = end + 1
			continue
		}
		inner := body[:end]
		if inner == "" || strings.TrimSpace(inner) != inner {
			return "", 0, false
		}
		return inner, len(marker) + end + len(marker), true
	}
	return "", 0, false
}

// linkAt 解析 text 开头的 [文字](链接)，返回文字、链接和占用的字节数。
func linkAt(text string) (string, string, int, bool) {
	if !strings.HasPrefix(text, "[") {
		return "", "", 0, false
	}
	close := strings.IndexByte(text, ']')
	if close <= 1 || strings.ContainsAny(text[1:close], "[\n") || !strings.HasPrefix(text[close+1:], "(") {
		return "", "", 0, false
	}
	rest := text[close+2:]
	end := strings.IndexByte(rest, ')')
	if end <= 0 || strings.ContainsFunc(rest[:end], unicode.IsSpace) {
		return "", "", 0, false
	}
	return text[1:close], rest[:end], close + 2 + end + 1, true
}

// inlineHTML 把行内节点渲染成 Telegram HTML，文本一律转义。
func inlineHTML(nodes []mdInline) string {
	var b strings.Builder
	for _, node := range nodes {
		switch node.kind {
		case mdText:
			b.WriteString(command.Escape(node.text))
		case mdCodeSpan:
			b.WriteString("<code>" + command.Escape(node.text) + "</code>")
		case mdBold:
			b.WriteString("<b>" + inlineHTML(node.children) + "</b>")
		case mdItalic:
			b.WriteString("<i>" + inlineHTML(node.children) + "</i>")
		case mdStrike:
			b.WriteString("<s>" + inlineHTML(node.children) + "</s>")
		case mdUnderline:
			b.WriteString("<u>" + inlineHTML(node.children) + "</u>")
		case mdSpoiler:
			b.WriteString("<tg-spoiler>" + inlineHTML(node.children) + "</tg-spoiler>")
		case mdLink:
			b.WriteString(`<a href="` + command.Escape(node.url) + `">` + inlineHTML(node.children) + "</a>")
		case mdImage:
			label := node.text
			if label == "" {
				label = node.url
			}
			if node.url == "" {
				b.WriteString(command.Escape(node.text))
				continue
			}
			b.WriteString(`<a href="` + command.Escape(node.url) + `">` + command.Escape(label) + "</a>")
		}
	}
	return b.String()
}

// telegraphNodes 把 Markdown 转成 Telegraph createPage 接受的节点：
// p、h3/h4、pre>code、blockquote、ul/ol>li、hr、a、strong、em、s、u、code、img。
func telegraphNodes(markdown string) []any {
	return blocksToTelegraph(parseMarkdown(mdCitePattern.ReplaceAllString(markdown, "")))
}

func telegraphNode(tag string, children []any) map[string]any {
	node := map[string]any{"tag": tag}
	if len(children) > 0 {
		node["children"] = children
	}
	return node
}

func blocksToTelegraph(blocks []mdBlock) []any {
	var nodes []any
	for _, block := range blocks {
		switch block.kind {
		case mdRule:
			nodes = append(nodes, telegraphNode("hr", nil))
		case mdHeading:
			tag := "h4"
			if block.level <= 2 {
				tag = "h3"
			}
			nodes = append(nodes, telegraphNode(tag, inlineTelegraph(parseInline(block.text, false))))
		case mdCode:
			nodes = append(nodes, telegraphNode("pre", []any{telegraphNode("code", []any{block.text})}))
		case mdQuote:
			inner := make([]string, len(block.lines))
			for index, line := range block.lines {
				trimmed := strings.TrimLeft(line, " \t")
				trimmed = strings.TrimPrefix(trimmed, ">")
				inner[index] = strings.TrimPrefix(trimmed, " ")
			}
			nodes = append(nodes, telegraphNode("blockquote", blocksToTelegraph(parseMarkdown(strings.Join(inner, "\n")))))
		case mdList:
			nodes = append(nodes, listTelegraph(block.items)...)
		default:
			var children []any
			for index, line := range block.lines {
				if index > 0 {
					children = append(children, telegraphNode("br", nil))
				}
				indent, rest := leadingIndent(line)
				if indent != "" {
					children = append(children, indent)
				}
				children = append(children, inlineTelegraph(parseInline(rest, false))...)
			}
			nodes = append(nodes, telegraphNode("p", compactTelegraph(children)))
		}
	}
	return nodes
}

// listTelegraph 把相邻的同类项合成一个 ul 或 ol。
func listTelegraph(items []*mdItem) []any {
	var nodes []any
	for start := 0; start < len(items); {
		end := start
		for end < len(items) && items[end].ordered == items[start].ordered {
			end++
		}
		tag := "ul"
		if items[start].ordered {
			tag = "ol"
		}
		var children []any
		for _, item := range items[start:end] {
			var content []any
			for index, line := range item.lines {
				if index > 0 {
					content = append(content, telegraphNode("br", nil))
				}
				content = append(content, inlineTelegraph(parseInline(line, false))...)
			}
			content = append(content, listTelegraph(item.children)...)
			children = append(children, telegraphNode("li", compactTelegraph(content)))
		}
		nodes = append(nodes, telegraphNode(tag, children))
		start = end
	}
	return nodes
}

func inlineTelegraph(nodes []mdInline) []any {
	var out []any
	for _, node := range nodes {
		switch node.kind {
		case mdText:
			out = append(out, node.text)
		case mdCodeSpan:
			out = append(out, telegraphNode("code", []any{node.text}))
		case mdBold:
			out = append(out, telegraphNode("strong", inlineTelegraph(node.children)))
		case mdItalic:
			out = append(out, telegraphNode("em", inlineTelegraph(node.children)))
		case mdStrike:
			out = append(out, telegraphNode("s", inlineTelegraph(node.children)))
		case mdUnderline:
			out = append(out, telegraphNode("u", inlineTelegraph(node.children)))
		case mdSpoiler:
			out = append(out, inlineTelegraph(node.children)...)
		case mdLink:
			link := telegraphNode("a", inlineTelegraph(node.children))
			link["attrs"] = map[string]any{"href": node.url}
			out = append(out, link)
		case mdImage:
			if node.url == "" {
				out = append(out, node.text)
				continue
			}
			image := telegraphNode("img", nil)
			image["attrs"] = map[string]any{"src": node.url}
			if strings.TrimSpace(node.text) == "" {
				out = append(out, image)
				continue
			}
			out = append(out, telegraphNode("figure", []any{image, telegraphNode("figcaption", inlineTelegraph(parseInline(node.text, true)))}))
		}
	}
	return compactTelegraph(out)
}

// compactTelegraph 合并相邻的文本节点，丢掉空文本。
func compactTelegraph(nodes []any) []any {
	var out []any
	for _, node := range nodes {
		text, isText := node.(string)
		if !isText {
			out = append(out, node)
			continue
		}
		if text == "" {
			continue
		}
		if len(out) > 0 {
			if last, ok := out[len(out)-1].(string); ok {
				out[len(out)-1] = last + text
				continue
			}
		}
		out = append(out, text)
	}
	return out
}

type openTag struct{ name, raw string }

// htmlPages 把本包生成的 HTML 分页：每页可见字符（按 UTF-16 计，实体算一个）不超过
// limit，页末闭合还开着的标签，下一页开头原样重新打开，代码块的语言也就保留下来；
// 能在换行处断开就在换行处断开。输入必须是标签成对的 HTML。
func htmlPages(text string, limit int) []string {
	var pages []string
	var stack []openTag
	var page strings.Builder
	visible := 0
	type breakPoint struct {
		offset, visible int
		stack           []openTag
	}
	var last *breakPoint
	closing := func(tags []openTag) string {
		var b strings.Builder
		for index := len(tags) - 1; index >= 0; index-- {
			b.WriteString("</" + tags[index].name + ">")
		}
		return b.String()
	}
	opening := func(tags []openTag) string {
		var b strings.Builder
		for _, tag := range tags {
			b.WriteString(tag.raw)
		}
		return b.String()
	}
	cut := func() {
		current := page.String()
		if last != nil && last.visible*2 >= limit {
			head, tail := strings.TrimSuffix(current[:last.offset], "\n"), current[last.offset:]
			pages = append(pages, head+closing(last.stack))
			page.Reset()
			page.WriteString(opening(last.stack) + tail)
			visible -= last.visible
		} else {
			pages = append(pages, current+closing(stack))
			page.Reset()
			page.WriteString(opening(stack))
			visible = 0
		}
		last = nil
	}
	add := func(token string, width int) {
		if visible > 0 && visible+width > limit {
			cut()
		}
		page.WriteString(token)
		visible += width
		if token == "\n" {
			last = &breakPoint{offset: page.Len(), visible: visible, stack: append([]openTag(nil), stack...)}
		}
	}
	for index := 0; index < len(text); {
		switch text[index] {
		case '<':
			end := strings.IndexByte(text[index:], '>')
			if end < 0 {
				add("&lt;", 1)
				index++
				continue
			}
			token := text[index : index+end+1]
			index += end + 1
			if strings.HasPrefix(token, "</") {
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				page.WriteString(token)
				continue
			}
			name, _, _ := strings.Cut(strings.Trim(token, "<>"), " ")
			stack = append(stack, openTag{name: strings.ToLower(name), raw: token})
			page.WriteString(token)
		case '&':
			end := strings.IndexByte(text[index:], ';')
			if end < 0 || end > 12 {
				add("&amp;", 1)
				index++
				continue
			}
			add(text[index:index+end+1], 1)
			index += end + 1
		default:
			r, size := utf8.DecodeRuneInString(text[index:])
			width := 1
			if r >= 0x10000 {
				width = 2
			}
			add(text[index:index+size], width)
			index += size
		}
	}
	if visible > 0 || len(pages) == 0 {
		pages = append(pages, page.String()+closing(stack))
	}
	return pages
}
