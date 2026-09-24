package ai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/bot"
)

// TestMarkdownToHTML 覆盖常见的块和行内格式，其余文本必须转义。
func TestMarkdownToHTML(t *testing.T) {
	cases := []struct{ name, markdown, want string }{
		{"escape", "a < b & c > \"d\"", "a &lt; b &amp; c &gt; &quot;d&quot;"},
		{"bold italic", "**粗** 和 *斜* 以及 _斜_", "<b>粗</b> 和 <i>斜</i> 以及 <i>斜</i>"},
		{"nested", "***都有***", "<b><i>都有</i></b>"},
		{"strike underline spoiler", "~~删~~ __下__ ||藏||", "<s>删</s> <u>下</u> <tg-spoiler>藏</tg-spoiler>"},
		{"code span keeps markup", "用 `a*b*<c>` 看", "用 <code>a*b*&lt;c&gt;</code> 看"},
		{"snake case", "snake_case_name 和 2 * 3 * 4", "snake_case_name 和 2 * 3 * 4"},
		{"link", "[文档](https://example.com/a?b=1&c=2)", `<a href="https://example.com/a?b=1&amp;c=2">文档</a>`},
		{"unsafe link", "[点我](javascript:alert(1))", "点我)"},
		{"bare link", "见 https://example.com/x。然后", `见 <a href="https://example.com/x">https://example.com/x</a>。然后`},
		{"bare link punctuation", "see https://example.com.", `see <a href="https://example.com">https://example.com</a>.`},
		{"headings", "# 一\n## 二\n### 三\n#### 四", "<b>一</b>\n\n<b>▌二</b>\n\n<b>• 三</b>\n\n<b>四</b>"},
		{"code block with language", "```Go\nfmt.Println(\"<x>\")\n```", `<pre><code class="language-go">fmt.Println(&quot;&lt;x&gt;&quot;)</code></pre>`},
		{"code block without language", "```\na & b\n```", "<pre><code>a &amp; b</code></pre>"},
		{"unclosed fence", "```py\nprint(1)", `<pre><code class="language-py">print(1)</code></pre>`},
		{"bullet list", "- 一\n- 二\n  - 二点一\n    - 深\n      - 更深", "• 一\n• 二\n\u00a0\u00a0• 二点一\n\u00a0\u00a0\u00a0\u00a0• 深\n\u00a0\u00a0\u00a0\u00a0\u00a0\u00a0更深"},
		{"ordered list", "1. **甲**\n   说明\n2. 乙", "1. <b>甲</b>\n\u00a0\u00a0说明\n2. 乙"},
		{"quote", "> 引用 **重点**\n> > 再深", "<blockquote>引用 <b>重点</b>\n┃ 再深</blockquote>"},
		{"rule", "上\n\n---\n\n下", "上\n\n────────────────\n\n下"},
		{"paragraph lines", "第一行\n  缩进行", "第一行\n\u00a0\u00a0缩进行"},
		{"cite removed", "结论<cite>1</cite>", "结论1"},
	}
	for _, test := range cases {
		if got := markdownToHTML(test.markdown, false); got != test.want {
			t.Errorf("%s:\n got %q\nwant %q", test.name, got, test.want)
		}
	}
	if got := markdownToHTML("> 引用", true); got != "引用" {
		t.Errorf("collapseSafe 时引用不应再包 blockquote：%q", got)
	}
}

// TestMarkdownHTMLParses 确认生成的 HTML 能被发送时用的解析器接受。
func TestMarkdownHTMLParses(t *testing.T) {
	source := "# 标题\n\n**粗 [链接](https://example.com) `code`**\n\n```js\nlet a = 1 < 2\n```\n\n> 引用\n\n- 列表 ||剧透||"
	html := markdownToHTML(source, false)
	plain, entities, err := bot.ParseHTML(html)
	if err != nil {
		t.Fatalf("解析失败：%v\n%s", err, html)
	}
	if strings.Contains(plain, "<b>") || strings.Contains(plain, "**") || len(entities) < 6 {
		t.Fatalf("解析结果不对：%q %d 个实体", plain, len(entities))
	}
}

// TestHTMLPages 检查分页：可见字符不超过上限，标签在页间闭合并原样重开，优先在换行处断开。
func TestHTMLPages(t *testing.T) {
	code := `<pre><code class="language-go">` + strings.Repeat("x", 30) + "\n" + strings.Repeat("y", 30) + "</code></pre>"
	pages := htmlPages("<b>头</b>\n"+code, 40)
	if len(pages) != 2 {
		t.Fatalf("应分成两页：%q", pages)
	}
	if !strings.HasSuffix(pages[0], "</code></pre>") || !strings.HasPrefix(pages[1], `<pre><code class="language-go">`) {
		t.Fatalf("代码块没有在页间闭合并重开：%q", pages)
	}
	if strings.Contains(pages[0], "y") || strings.Contains(pages[1], "x") {
		t.Fatalf("应在换行处断开：%q", pages)
	}
	for _, page := range pages {
		if _, _, err := bot.ParseHTML(page); err != nil {
			t.Fatalf("分页后的 HTML 无效：%v %q", err, page)
		}
	}
	long := htmlPages("<i>"+strings.Repeat("字", 95)+"&amp;</i>", 32)
	if len(long) != 3 {
		t.Fatalf("没有换行时按字符切：%q", long)
	}
	for _, page := range long {
		plain, _, err := bot.ParseHTML(page)
		if err != nil || len([]rune(plain)) > 32 {
			t.Fatalf("页面超长或无效：%q %v", page, err)
		}
	}
	if got := htmlPages("", 10); len(got) != 1 || got[0] != "" {
		t.Fatalf("空文本应得到一页空内容：%q", got)
	}
}

// TestAnswerPages 检查 Q/A 版式、折叠、续页标签和署名。
func TestAnswerPages(t *testing.T) {
	pages := answerPages("问<题>", "<b>答</b>", "main", true)
	want := "Q:\n<blockquote expandable>问&lt;题&gt;</blockquote>\nA:\n<blockquote expandable><b>答</b></blockquote>\n<i>🍀Powered by main</i>"
	if len(pages) != 1 || pages[0] != want {
		t.Fatalf("got %q", pages)
	}
	plain := answerPages("q", "a", "", false)
	if plain[0] != "Q:\nq\n\nA:\na" {
		t.Fatalf("不折叠时的版式不对：%q", plain)
	}
	long := answerPages("q", strings.Repeat("长\n", 3000), "t", true)
	if len(long) < 2 || !strings.HasPrefix(long[1], "📋 <b>续 (1/") || !strings.HasSuffix(long[len(long)-1], "Powered by t</i>") {
		t.Fatalf("续页标签或署名不对：%d 页，%q", len(long), long[len(long)-1])
	}
	for _, page := range long {
		if _, _, err := bot.ParseHTML(page); err != nil {
			t.Fatalf("分页 HTML 无效：%v", err)
		}
	}
}

// TestTelegraphNodes 检查 Telegraph 节点的标签。
func TestTelegraphNodes(t *testing.T) {
	nodes := telegraphNodes("## 标题\n\n**粗** [链](https://e.com)\n第二行\n\n```go\nx := 1\n```\n\n> 引用\n\n1. 甲\n   - 子\n2. 乙\n\n---")
	raw, err := json.Marshal(nodes)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"children":["标题"],"tag":"h3"},` +
		`{"children":[{"children":["粗"],"tag":"strong"}," ",{"attrs":{"href":"https://e.com"},"children":["链"],"tag":"a"},{"tag":"br"},"第二行"],"tag":"p"},` +
		`{"children":[{"children":["x := 1"],"tag":"code"}],"tag":"pre"},` +
		`{"children":[{"children":["引用"],"tag":"p"}],"tag":"blockquote"},` +
		`{"children":[{"children":["甲",{"children":[{"children":["子"],"tag":"li"}],"tag":"ul"}],"tag":"li"},{"children":["乙"],"tag":"li"}],"tag":"ol"},` +
		`{"tag":"hr"}]`
	if string(raw) != want {
		t.Fatalf("got  %s\nwant %s", raw, want)
	}
}
