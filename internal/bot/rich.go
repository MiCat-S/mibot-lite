package bot

import (
	"context"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/delivery"
)

// Rich Message 是 Telegram 2026 年加的富文本消息（Bot API 10.1，MTProto 里是 messages.sendMessage
// 的 rich_message 字段）：真正的标题、嵌套列表、表格、可折叠段落和公式，一条最多 32768 个字符。
// 用户号要有 Premium 才能发；让服务端解析 Markdown、HTML 的两种写法（inputRichMessageMarkdown/HTML）
// 只给机器人用，所以这里只发本地拼好的块（inputRichMessage），和 TDLib 一样正文留空。

// Rich Message 的上限，见 https://core.telegram.org/bots/api#rich-message-limits。
const (
	// RichTextLimit 是全部文字（含公式源码）的字符数上限。
	RichTextLimit = 32768
	// RichBlockLimit 是块的个数上限，列表项、表格行和嵌套的块都算。
	RichBlockLimit = 500
	// RichColumnLimit 是表格的列数上限。
	RichColumnLimit = 20
)

// CanSendRich 报告这个账号能不能发 Rich Message：要有 Telegram Premium。
// Premium 是登录时读的，中途过期了发送会被拒，所以调用方还要能退回普通消息，见 RichRejected。
func (c *Client) CanSendRich() bool { return c.self != nil && c.self.Premium }

// SendRich 发一条 Rich Message，返回它的 id。
func (c *Client) SendRich(ctx context.Context, peer tg.InputPeerClass, blocks []tg.PageBlockClass, options SendOptions) (int, error) {
	request := sendRequest(peer, options)
	request.SetRichMessage(&tg.InputRichMessage{Blocks: blocks})
	return c.sendMessage(ctx, request)
}

// EditRich 把消息换成 Rich Message，普通文字消息也能这样改。MESSAGE_NOT_MODIFIED 也算成功。
func (c *Client) EditRich(ctx context.Context, peer tg.InputPeerClass, id int, blocks []tg.PageBlockClass) error {
	request := &tg.MessagesEditMessageRequest{Peer: peer, ID: id, NoWebpage: true}
	request.SetRichMessage(&tg.InputRichMessage{Blocks: blocks})
	if _, err := c.api.MessagesEditMessage(ctx, request); err != nil && !tgerr.Is(err, "MESSAGE_NOT_MODIFIED") {
		return err
	}
	return nil
}

// RichFits 判断这些块在不在 Rich Message 的上限之内：文字不超过 RichTextLimit 个字符，
// 块（连同列表项、表格行和嵌套的块）不超过 RichBlockLimit 个。超了就该分页发普通消息。
func RichFits(blocks []tg.PageBlockClass) bool {
	var lines []string
	richBlocksText(blocks, "", &lines)
	chars := 0
	for _, line := range lines {
		chars += utf8.RuneCountInString(line)
	}
	return chars <= RichTextLimit && richBlockCount(blocks) <= RichBlockLimit
}

func richBlockCount(blocks []tg.PageBlockClass) int {
	count := len(blocks)
	for _, block := range blocks {
		switch value := block.(type) {
		case *tg.PageBlockBlockquoteBlocks:
			count += richBlockCount(value.Blocks)
		case *tg.PageBlockDetails:
			count += richBlockCount(value.Blocks)
		case *tg.PageBlockTable:
			count += len(value.Rows)
		case *tg.PageBlockList:
			for _, item := range value.Items {
				count++
				if entry, ok := item.(*tg.PageListItemBlocks); ok {
					count += richBlockCount(entry.Blocks)
				}
			}
		case *tg.PageBlockOrderedList:
			for _, item := range value.Items {
				count++
				if entry, ok := item.(*tg.PageListOrderedItemBlocks); ok {
					count += richBlockCount(entry.Blocks)
				}
			}
		}
	}
	return count
}

// RichRejected 判断 Rich Message 是不是被 Telegram 明确拒收了（比如没有 Premium、块不合法），
// 拒收时消息没有发出，调用方可以改发普通消息。限流不算，它照样会挡住普通消息；
// 结果不明的失败也不算，消息可能已经发出去了。
func RichRejected(err error) bool {
	if err == nil || delivery.Uncertain(err) {
		return false
	}
	if _, flood := tgerr.AsFloodWait(err); flood {
		return false
	}
	rpcErr, ok := tgerr.As(err)
	return ok && rpcErr.Code < 500
}

// RichPlain 是一段纯文字，空字符串是 textEmpty（textPlain 不收空串）。
func RichPlain(text string) tg.RichTextClass {
	if text == "" {
		return &tg.TextEmpty{}
	}
	return &tg.TextPlain{Text: text}
}

// RichCode 是一段等宽的行内代码。
func RichCode(text string) tg.RichTextClass { return &tg.TextFixed{Text: RichPlain(text)} }

// RichConcat 把几段富文本连起来：丢掉空的，相邻的纯文字合并，只剩一段时不再包一层。
func RichConcat(parts ...tg.RichTextClass) tg.RichTextClass {
	var texts []tg.RichTextClass
	for _, part := range parts {
		switch value := part.(type) {
		case nil, *tg.TextEmpty:
			continue
		case *tg.TextConcat:
			texts = append(texts, value.Texts...)
			continue
		case *tg.TextPlain:
			if len(texts) > 0 {
				if last, ok := texts[len(texts)-1].(*tg.TextPlain); ok {
					texts[len(texts)-1] = &tg.TextPlain{Text: last.Text + value.Text}
					continue
				}
			}
		}
		texts = append(texts, part)
	}
	switch len(texts) {
	case 0:
		return &tg.TextEmpty{}
	case 1:
		return texts[0]
	}
	return &tg.TextConcat{Texts: texts}
}

// RichMessageText 把收到的 Rich Message 摊平成纯文本：每块一段，列表项前加「• 」，
// 表格每行的格子用「 | 」连起来。普通消息的正文是空的，要读它的文字就得这样取。
func RichMessageText(message *tg.Message) string {
	if message == nil {
		return ""
	}
	rich, ok := message.GetRichMessage()
	if !ok {
		return ""
	}
	var lines []string
	richBlocksText(rich.Blocks, "", &lines)
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func richBlocksText(blocks []tg.PageBlockClass, prefix string, lines *[]string) {
	add := func(text tg.RichTextClass) {
		if line := richPlain(text); strings.TrimSpace(line) != "" {
			*lines = append(*lines, prefix+line)
			prefix = strings.Repeat(" ", len([]rune(prefix)))
		}
	}
	for _, block := range blocks {
		switch value := block.(type) {
		case *tg.PageBlockParagraph:
			add(value.Text)
		case *tg.PageBlockHeading1:
			add(value.Text)
		case *tg.PageBlockHeading2:
			add(value.Text)
		case *tg.PageBlockHeading3:
			add(value.Text)
		case *tg.PageBlockHeading4:
			add(value.Text)
		case *tg.PageBlockHeading5:
			add(value.Text)
		case *tg.PageBlockHeading6:
			add(value.Text)
		case *tg.PageBlockTitle:
			add(value.Text)
		case *tg.PageBlockSubtitle:
			add(value.Text)
		case *tg.PageBlockHeader:
			add(value.Text)
		case *tg.PageBlockSubheader:
			add(value.Text)
		case *tg.PageBlockKicker:
			add(value.Text)
		case *tg.PageBlockFooter:
			add(value.Text)
		case *tg.PageBlockPreformatted:
			add(value.Text)
		case *tg.PageBlockBlockquote:
			add(value.Text)
		case *tg.PageBlockPullquote:
			add(value.Text)
		case *tg.PageBlockMath:
			*lines = append(*lines, prefix+value.Source)
		case *tg.PageBlockBlockquoteBlocks:
			richBlocksText(value.Blocks, prefix, lines)
		case *tg.PageBlockDetails:
			add(value.Title)
			richBlocksText(value.Blocks, prefix, lines)
		case *tg.PageBlockList:
			for _, item := range value.Items {
				switch entry := item.(type) {
				case *tg.PageListItemText:
					*lines = append(*lines, prefix+"• "+richPlain(entry.Text))
				case *tg.PageListItemBlocks:
					richBlocksText(entry.Blocks, prefix+"• ", lines)
				}
			}
		case *tg.PageBlockOrderedList:
			for index, item := range value.Items {
				switch entry := item.(type) {
				case *tg.PageListOrderedItemText:
					*lines = append(*lines, prefix+orderedLabel(entry.Num, entry.Value, index)+richPlain(entry.Text))
				case *tg.PageListOrderedItemBlocks:
					richBlocksText(entry.Blocks, prefix+orderedLabel(entry.Num, entry.Value, index), lines)
				}
			}
		case *tg.PageBlockTable:
			add(value.Title)
			for _, row := range value.Rows {
				cells := make([]string, 0, len(row.Cells))
				for _, cell := range row.Cells {
					cells = append(cells, richPlain(cell.Text))
				}
				*lines = append(*lines, prefix+strings.Join(cells, " | "))
			}
		}
	}
}

// orderedLabel 是有序列表项前的序号：有 num 用 num，否则用 value，都没有就按位置数。
func orderedLabel(num string, value, index int) string {
	switch {
	case num != "":
		return num + " "
	case value != 0:
		return strconv.Itoa(value) + ". "
	}
	return strconv.Itoa(index+1) + ". "
}

// richPlain 取出富文本里的文字，公式取源码，图片和按钮之类没有文字的部分略过。
func richPlain(text tg.RichTextClass) string {
	switch value := text.(type) {
	case nil, *tg.TextEmpty:
		return ""
	case *tg.TextPlain:
		return value.Text
	case *tg.TextConcat:
		var b strings.Builder
		for _, part := range value.Texts {
			b.WriteString(richPlain(part))
		}
		return b.String()
	case *tg.TextMath:
		return value.Source
	case *tg.TextCustomEmoji:
		return value.Alt
	case interface{ GetText() tg.RichTextClass }:
		return richPlain(value.GetText())
	}
	return ""
}
