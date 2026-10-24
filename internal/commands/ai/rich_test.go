package ai

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
)

// richKinds 列出块的类型名，比如 PageBlockHeading1，方便整体比较。
func richKinds(blocks []tg.PageBlockClass) string {
	kinds := make([]string, len(blocks))
	for index, block := range blocks {
		kinds[index] = strings.TrimPrefix(fmt.Sprintf("%T", block), "*tg.")
	}
	return strings.Join(kinds, " ")
}

// mustEncode 确认这些块能按 MTProto 编码：必填的字段（引用的 caption、表格的 title 之类）不能是空的。
func mustEncode(t *testing.T, blocks []tg.PageBlockClass) {
	t.Helper()
	var buffer bin.Buffer
	if err := (&tg.InputRichMessage{Blocks: blocks}).Encode(&buffer); err != nil {
		t.Fatalf("编码失败：%v", err)
	}
}

// TestMarkdownRich 覆盖各种块：标题、段落里的行内格式、嵌套列表和任务列表、有序列表、
// 表格、Markdown 引用、HTML 的可展开引用、代码块、公式块和分隔线。
func TestMarkdownRich(t *testing.T) {
	source := "# 标题\n段落 **粗** [链](https://e.com) $x^2$\n第二行\n\n" +
		"- 一\n  - 一点一\n- [x] 完成\n\n3. 丙\n4. 丁\n\n" +
		"| A | B |\n|:-:|--:|\n| 1 | 2 |\n\n> 引用 *斜*\n\n<blockquote expandable>折叠</blockquote>\n\n" +
		"```go\nx := 1\n```\n\n$$\nE=mc^2\n$$\n\n---"
	blocks := MarkdownRich(source, false)
	mustEncode(t, blocks)
	want := "PageBlockHeading1 PageBlockParagraph PageBlockList PageBlockOrderedList PageBlockTable " +
		"PageBlockBlockquoteBlocks PageBlockBlockquote PageBlockPreformatted PageBlockMath PageBlockDivider"
	if got := richKinds(blocks); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	paragraph := blocks[1].(*tg.PageBlockParagraph).Text.(*tg.TextConcat)
	var kinds []string
	for _, part := range paragraph.Texts {
		kinds = append(kinds, strings.TrimPrefix(fmt.Sprintf("%T", part), "*tg."))
	}
	if got := strings.Join(kinds, " "); got != "TextPlain TextBold TextPlain TextURL TextPlain TextMath TextPlain" {
		t.Errorf("段落的行内格式不对：%s", got)
	}
	if last := paragraph.Texts[len(paragraph.Texts)-1].(*tg.TextPlain); last.Text != "\n第二行" {
		t.Errorf("段落里的换行没保留：%q", last.Text)
	}
	list := blocks[2].(*tg.PageBlockList)
	first := list.Items[0].(*tg.PageListItemBlocks)
	done := list.Items[1].(*tg.PageListItemBlocks)
	if richKinds(first.Blocks) != "PageBlockParagraph PageBlockList" || !done.Checkbox || !done.Checked {
		t.Errorf("列表不对：%s %+v", richKinds(first.Blocks), done)
	}
	ordered := blocks[3].(*tg.PageBlockOrderedList)
	if ordered.Start != 3 || ordered.Items[1].(*tg.PageListOrderedItemBlocks).Value != 4 {
		t.Errorf("有序列表的序号不对：%+v", ordered)
	}
	table := blocks[4].(*tg.PageBlockTable)
	header, cell := table.Rows[0].Cells, table.Rows[1].Cells
	if len(table.Rows) != 2 || !header[0].Header || cell[0].Header || !cell[0].AlignCenter || !cell[1].AlignRight {
		t.Errorf("表格不对：%+v", table.Rows)
	}
	if quote := blocks[6].(*tg.PageBlockBlockquote); !quote.Collapsed {
		t.Error("<blockquote expandable> 应该是收起的引用")
	}
	if code := blocks[7].(*tg.PageBlockPreformatted); code.Language != "go" {
		t.Errorf("代码块语言不对：%q", code.Language)
	}
	if math := blocks[8].(*tg.PageBlockMath); math.Source != "E=mc^2" {
		t.Errorf("公式不对：%q", math.Source)
	}
}

// TestFoldRichSections 每个最高级的标题连同下面的内容折成详情块，标题前的内容原样留着；
// 没有标题时整段折进「展开全文」。
func TestFoldRichSections(t *testing.T) {
	blocks := MarkdownRich("前言\n\n## 甲\n- 一\n\n### 子\n内容\n\n## 乙\n内容", true)
	mustEncode(t, blocks)
	if got := richKinds(blocks); got != "PageBlockParagraph PageBlockDetails PageBlockDetails" {
		t.Fatalf("got %s", got)
	}
	first := blocks[1].(*tg.PageBlockDetails)
	if first.Open || richKinds(first.Blocks) != "PageBlockList PageBlockHeading3 PageBlockParagraph" || first.Title.(*tg.TextPlain).Text != "甲" {
		t.Errorf("第一节不对：%s %+v", richKinds(first.Blocks), first.Title)
	}
	if got := richKinds(MarkdownRich("## 空\n\n## 有\n内容", true)); got != "PageBlockHeading2 PageBlockDetails" {
		t.Fatalf("下面没有内容的标题应原样留着：%s", got)
	}
	whole := MarkdownRich("只有一段", true)
	if got := richKinds(whole); got != "PageBlockDetails" {
		t.Fatalf("没有标题时应整段折起来：%s", got)
	}
}

// TestAnswerRich 检查 Rich Message 版回答的版式：折叠时问题是收起的引用、长回答只露开头，
// 来源放进详情块，模型名写在页脚。
func TestAnswerRich(t *testing.T) {
	sources := []aiSource{{URL: "https://e.com/a", Title: "甲"}, {URL: "https://e.com/b"}}
	plain := answerRich("问<题>", "**答**", sources, "gpt-5.1", false)
	mustEncode(t, plain)
	if got := richKinds(plain); got != "PageBlockParagraph PageBlockParagraph PageBlockParagraph PageBlockParagraph PageBlockDetails PageBlockFooter" {
		t.Fatalf("不折叠时的版式不对：%s", got)
	}
	if question := plain[1].(*tg.PageBlockParagraph).Text.(*tg.TextPlain); question.Text != "问<题>" {
		t.Errorf("问题不该转义：%q", question.Text)
	}
	if title := plain[4].(*tg.PageBlockDetails).Title.(*tg.TextPlain); title.Text != "来源（2）" {
		t.Errorf("来源标题不对：%q", title.Text)
	}

	short := answerRich("q", "短回答", nil, "", true)
	if got := richKinds(short); got != "PageBlockParagraph PageBlockBlockquote PageBlockParagraph PageBlockParagraph" {
		t.Fatalf("短回答不用折：%s", got)
	}
	if quote := short[1].(*tg.PageBlockBlockquote); !quote.Collapsed {
		t.Error("折叠时问题应该是收起的引用")
	}

	long := answerRich("q", "## 头\n\n"+strings.Repeat("长", answerFoldLength)+"\n\n第二段\n\n第三段", nil, "m", true)
	mustEncode(t, long)
	if got := richKinds(long); got != "PageBlockParagraph PageBlockBlockquote PageBlockParagraph PageBlockHeading2 PageBlockParagraph PageBlockDetails PageBlockFooter" {
		t.Fatalf("长回答应该露出标题和第一段，其余折起来：%s", got)
	}
	if folded := long[5].(*tg.PageBlockDetails); len(folded.Blocks) != 2 {
		t.Errorf("折起来的应该是后两段：%s", richKinds(folded.Blocks))
	}
	if !bot.RichFits(long) {
		t.Error("这么短的回答应该能发")
	}
}
