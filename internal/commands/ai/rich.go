package ai

import (
	"strings"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
)

// 这里把 Markdown 转成 Rich Message 的块（MTProto 的 PageBlock / RichText）：
// 标题是真正的一到六级标题，列表是可以嵌套的列表（含任务列表），表格、公式、
// 代码块、引用都用对应的块，不再像 HTML 那样拿粗体和缩进模拟。

// MarkdownRich 把 Markdown 转成 Rich Message 的块。foldSections 为真时每个标题连同它下面
// 的内容折成一个收起的详情块，标题就是详情块的标题。
func MarkdownRich(markdown string, foldSections bool) []tg.PageBlockClass {
	blocks := blocksToRich(parseMarkdown(mdCitePattern.ReplaceAllString(markdown, "")))
	if foldSections {
		return foldRichSections(blocks)
	}
	return blocks
}

func blocksToRich(blocks []mdBlock) []tg.PageBlockClass {
	var out []tg.PageBlockClass
	for _, block := range blocks {
		switch block.kind {
		case mdRule:
			out = append(out, &tg.PageBlockDivider{})
		case mdHeading:
			out = append(out, richHeading(block.level, inlineRich(parseInline(block.text, false))))
		case mdCode:
			if strings.TrimSpace(block.text) == "" {
				continue
			}
			lang := strings.ToLower(strings.TrimSpace(block.lang))
			if lang == "math" {
				out = append(out, &tg.PageBlockMath{Source: block.text})
				continue
			}
			if !mdLanguage.MatchString(lang) {
				lang = ""
			}
			out = append(out, &tg.PageBlockPreformatted{Text: bot.RichPlain(block.text), Language: lang})
		case mdMathBlock:
			out = append(out, &tg.PageBlockMath{Source: block.text})
		case mdQuote:
			if block.html {
				if strings.TrimSpace(strings.Join(block.lines, "")) == "" {
					continue
				}
				out = append(out, &tg.PageBlockBlockquote{Collapsed: block.expandable, Text: richLines(block.lines), Caption: &tg.TextEmpty{}})
				continue
			}
			if inner := blocksToRich(parseMarkdown(quoteInner(block.lines))); len(inner) > 0 {
				out = append(out, &tg.PageBlockBlockquoteBlocks{Blocks: inner, Caption: &tg.TextEmpty{}})
			}
		case mdList:
			out = append(out, listRich(block.items)...)
		case mdTable:
			out = append(out, tableRich(block))
		default:
			out = append(out, &tg.PageBlockParagraph{Text: richLines(block.lines)})
		}
	}
	return out
}

func richHeading(level int, text tg.RichTextClass) tg.PageBlockClass {
	switch level {
	case 1:
		return &tg.PageBlockHeading1{Text: text}
	case 2:
		return &tg.PageBlockHeading2{Text: text}
	case 3:
		return &tg.PageBlockHeading3{Text: text}
	case 4:
		return &tg.PageBlockHeading4{Text: text}
	case 5:
		return &tg.PageBlockHeading5{Text: text}
	}
	return &tg.PageBlockHeading6{Text: text}
}

// richHeadingOf 认出标题块，返回级别和文字。
func richHeadingOf(block tg.PageBlockClass) (int, tg.RichTextClass, bool) {
	switch value := block.(type) {
	case *tg.PageBlockHeading1:
		return 1, value.Text, true
	case *tg.PageBlockHeading2:
		return 2, value.Text, true
	case *tg.PageBlockHeading3:
		return 3, value.Text, true
	case *tg.PageBlockHeading4:
		return 4, value.Text, true
	case *tg.PageBlockHeading5:
		return 5, value.Text, true
	case *tg.PageBlockHeading6:
		return 6, value.Text, true
	}
	return 0, nil, false
}

// foldRichSections 把最高一级的每个标题连同下面的内容（到下一个同级或更高的标题为止）
// 换成收起的详情块。第一个标题之前的内容、下面没有内容的标题原样留着；没有标题时整段折进
// 一个「展开全文」。
func foldRichSections(blocks []tg.PageBlockClass) []tg.PageBlockClass {
	top := 0
	for _, block := range blocks {
		if level, _, ok := richHeadingOf(block); ok && (top == 0 || level < top) {
			top = level
		}
	}
	if top == 0 {
		if len(blocks) == 0 {
			return blocks
		}
		return []tg.PageBlockClass{&tg.PageBlockDetails{Title: bot.RichPlain("展开全文"), Blocks: blocks}}
	}
	var out []tg.PageBlockClass
	var heading tg.PageBlockClass
	var section *tg.PageBlockDetails
	closeSection := func() {
		switch {
		case section != nil && len(section.Blocks) > 0:
			out = append(out, section)
		case heading != nil:
			out = append(out, heading)
		}
		heading, section = nil, nil
	}
	for _, block := range blocks {
		if level, text, ok := richHeadingOf(block); ok && level <= top {
			closeSection()
			heading, section = block, &tg.PageBlockDetails{Title: text}
			continue
		}
		if section == nil {
			out = append(out, block)
			continue
		}
		section.Blocks = append(section.Blocks, block)
	}
	closeSection()
	return out
}

// listRich 把相邻的同类项合成一个无序或有序列表，每项是一段文字加上它的子列表。
func listRich(items []*mdItem) []tg.PageBlockClass {
	var out []tg.PageBlockClass
	for start := 0; start < len(items); {
		end := start
		for end < len(items) && items[end].ordered == items[start].ordered {
			end++
		}
		group := items[start:end]
		if !group[0].ordered {
			list := &tg.PageBlockList{}
			for _, item := range group {
				list.Items = append(list.Items, &tg.PageListItemBlocks{Checkbox: item.task, Checked: item.checked, Blocks: itemRich(item)})
			}
			out = append(out, list)
		} else {
			list := &tg.PageBlockOrderedList{Start: group[0].number}
			for _, item := range group {
				list.Items = append(list.Items, &tg.PageListOrderedItemBlocks{Checkbox: item.task, Checked: item.checked, Value: item.number, Blocks: itemRich(item)})
			}
			out = append(out, list)
		}
		start = end
	}
	return out
}

func itemRich(item *mdItem) []tg.PageBlockClass {
	blocks := []tg.PageBlockClass{&tg.PageBlockParagraph{Text: richLines(item.lines)}}
	return append(blocks, listRich(item.children)...)
}

// tableRich 生成带边框的表格，第一行是表头。列数超过 Telegram 的上限时退回成一行一段的文字。
func tableRich(block mdBlock) tg.PageBlockClass {
	if len(block.rows[0]) > bot.RichColumnLimit {
		lines := make([]string, len(block.rows))
		for index, row := range block.rows {
			lines[index] = strings.Join(row, " | ")
		}
		return &tg.PageBlockParagraph{Text: richLines(lines)}
	}
	table := &tg.PageBlockTable{Bordered: true, Title: &tg.TextEmpty{}}
	for index, row := range block.rows {
		var cells []tg.PageTableCell
		for column, text := range row {
			cell := tg.PageTableCell{Header: index == 0, Text: inlineRich(parseInline(text, false))}
			if column < len(block.align) {
				cell.AlignCenter, cell.AlignRight = block.align[column] == 1, block.align[column] == 2
			}
			cells = append(cells, cell)
		}
		table.Rows = append(table.Rows, tg.PageTableRow{Cells: cells})
	}
	return table
}

// richLines 把几行文字排成一段，行间换行，行首缩进换成不换行空格。
func richLines(lines []string) tg.RichTextClass {
	var parts []tg.RichTextClass
	for index, line := range lines {
		if index > 0 {
			parts = append(parts, bot.RichPlain("\n"))
		}
		indent, rest := leadingIndent(line)
		if indent != "" {
			parts = append(parts, bot.RichPlain(indent))
		}
		parts = append(parts, inlineRich(parseInline(rest, false)))
	}
	return bot.RichConcat(parts...)
}

// inlineRich 把行内节点转成富文本。图片和 HTML 版一样只给链接：Rich Message 的图片
// 要先上传成单独的媒体块。
func inlineRich(nodes []mdInline) tg.RichTextClass {
	parts := make([]tg.RichTextClass, 0, len(nodes))
	for _, node := range nodes {
		children := func() tg.RichTextClass { return inlineRich(node.children) }
		switch node.kind {
		case mdText:
			parts = append(parts, bot.RichPlain(node.text))
		case mdCodeSpan:
			parts = append(parts, &tg.TextFixed{Text: bot.RichPlain(node.text)})
		case mdBold:
			parts = append(parts, &tg.TextBold{Text: children()})
		case mdItalic:
			parts = append(parts, &tg.TextItalic{Text: children()})
		case mdStrike:
			parts = append(parts, &tg.TextStrike{Text: children()})
		case mdUnderline:
			parts = append(parts, &tg.TextUnderline{Text: children()})
		case mdSpoiler:
			parts = append(parts, &tg.TextSpoiler{Text: children()})
		case mdMarked:
			parts = append(parts, &tg.TextMarked{Text: children()})
		case mdSub:
			parts = append(parts, &tg.TextSubscript{Text: children()})
		case mdSup:
			parts = append(parts, &tg.TextSuperscript{Text: children()})
		case mdSpan:
			parts = append(parts, children())
		case mdMath:
			parts = append(parts, &tg.TextMath{Source: node.text})
		case mdBreak:
			parts = append(parts, bot.RichPlain("\n"))
		case mdLink:
			parts = append(parts, &tg.TextURL{Text: children(), URL: node.url})
		case mdImage:
			label := node.text
			if label == "" {
				label = node.url
			}
			if node.url == "" {
				parts = append(parts, bot.RichPlain(node.text))
				continue
			}
			parts = append(parts, &tg.TextURL{Text: bot.RichPlain(label), URL: node.url})
		}
	}
	return bot.RichConcat(parts...)
}
