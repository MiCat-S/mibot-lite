package aff

import (
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
)

// span 是一段格式：在 [start, end) 这段 UTF-16 偏移上套 open…close。
type span struct {
	start, end  int
	open, close string
}

// tags 是一个格式实体对应的 HTML 标签，认不得的（链接、@、话题标签这些 Telegram 会自己
// 再识别一遍的）返回 false。标签都是 gotd 的 HTML 解析器认得的写法，存下来的内容发出去时
// 原样还原成同样的格式。
func tags(entity tg.MessageEntityClass) (string, string, bool) {
	switch value := entity.(type) {
	case *tg.MessageEntityBold:
		return "<b>", "</b>", true
	case *tg.MessageEntityItalic:
		return "<i>", "</i>", true
	case *tg.MessageEntityUnderline:
		return "<u>", "</u>", true
	case *tg.MessageEntityStrike:
		return "<s>", "</s>", true
	case *tg.MessageEntitySpoiler:
		return "<tg-spoiler>", "</tg-spoiler>", true
	case *tg.MessageEntityCode:
		return "<code>", "</code>", true
	case *tg.MessageEntityPre:
		if value.Language != "" {
			return `<pre><code class="language-` + bot.Escape(value.Language) + `">`, "</code></pre>", true
		}
		return "<pre>", "</pre>", true
	case *tg.MessageEntityTextURL:
		return `<a href="` + bot.Escape(value.URL) + `">`, "</a>", true
	case *tg.MessageEntityMentionName:
		return `<a href="tg://user?id=` + strconv.FormatInt(value.UserID, 10) + `">`, "</a>", true
	case *tg.MessageEntityBlockquote:
		if value.Collapsed {
			return "<blockquote expandable>", "</blockquote>", true
		}
		return "<blockquote>", "</blockquote>", true
	case *tg.MessageEntityCustomEmoji:
		return `<tg-emoji emoji-id="` + strconv.FormatInt(value.DocumentID, 10) + `">`, "</tg-emoji>", true
	}
	return "", "", false
}

// entitiesHTML 把消息正文和格式实体还原成 HTML。
//
// TeleBox 原版插件存的就是带格式的 HTML（teleproto 的 message.text 按 HTML 还原格式），
// 发送时按 HTML 解析；MiBox v2 改成存纯文本却仍按 HTML 发，文字里的 < & 会被当成标签，
// 藏在文字后面的链接也丢了。这里照原版存 HTML，链接和粗体这些都保得住。
//
// 实体可能交叉（一段粗体跨进一段链接的中间），HTML 标签却必须成对嵌套：遇到这种情况，
// 先关掉压在上面的标签，关掉该结束的，再把上面的重新打开。
func entitiesHTML(text string, entities []tg.MessageEntityClass) string {
	total := len(utf16.Encode([]rune(text)))
	var spans []span
	for _, entity := range entities {
		open, close, ok := tags(entity)
		if !ok {
			continue
		}
		start, end := entity.GetOffset(), entity.GetOffset()+entity.GetLength()
		start, end = max(start, 0), min(end, total)
		if start < end {
			spans = append(spans, span{start: start, end: end, open: open, close: close})
		}
	}
	// 同一处开始的，长的在外面。
	sort.SliceStable(spans, func(a, b int) bool {
		if spans[a].start != spans[b].start {
			return spans[a].start < spans[b].start
		}
		return spans[a].end > spans[b].end
	})
	var b strings.Builder
	var stack []span
	next := 0
	// closeAt 关掉在 position 或之前结束的格式。
	closeAt := func(position int) {
		lowest := -1
		for index, item := range stack {
			if item.end <= position {
				lowest = index
				break
			}
		}
		if lowest < 0 {
			return
		}
		popped := stack[lowest:]
		for index := len(popped) - 1; index >= 0; index-- {
			b.WriteString(popped[index].close)
		}
		kept := append([]span(nil), stack[:lowest]...)
		for _, item := range popped {
			if item.end > position {
				b.WriteString(item.open)
				kept = append(kept, item)
			}
		}
		stack = kept
	}
	position := 0
	for _, r := range text {
		closeAt(position)
		for next < len(spans) && spans[next].start <= position {
			b.WriteString(spans[next].open)
			stack = append(stack, spans[next])
			next++
		}
		b.WriteString(bot.Escape(string(r)))
		if r >= 0x10000 {
			position += 2
		} else {
			position++
		}
	}
	for index := len(stack) - 1; index >= 0; index-- {
		b.WriteString(stack[index].close)
	}
	return b.String()
}
