// Package encode 实现 .b64encode、.b64decode、.urlencode、.urldecode 四个编码解码命令，
// 外加只显示帮助的 .encode。命令名照 MiBox 的 encode 插件，五条共用一份帮助。
package encode

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

const (
	// maxInput 是一次最多处理的输入长度（UTF-16 单位，和 MiBox 一样 16384）。
	// 再长的内容编码结果要拆成五六条消息，不如换个工具。
	maxInput = 16384
	// previewRunes 是结果里「原文」一段最多显示的字数，完整原文用户自己手里就有。
	previewRunes = 200
)

// operation 是一种编码或解码。
type operation struct {
	name, title string
	// decode 为真时是解码：空输入的提示和失败的说法不一样。
	decode    bool
	transform func(string) (string, error)
}

var operations = []operation{
	{name: "b64encode", title: "Base64 编码", transform: func(s string) (string, error) {
		return base64.StdEncoding.EncodeToString([]byte(s)), nil
	}},
	{name: "b64decode", title: "Base64 解码", decode: true, transform: decodeBase64},
	{name: "urlencode", title: "URL 编码", transform: func(s string) (string, error) { return encodeURIComponent(s), nil }},
	{name: "urldecode", title: "URL 解码", decode: true, transform: decodeURIComponent},
}

func help(prefix string) string {
	p := command.Escape(prefix)
	c := func(text string) string { return "<code>" + p + text + "</code>" }
	return "🔣 <b>编码解码</b>\n\n" +
		"• " + c("b64encode 文本") + " Base64 编码\n" +
		"• " + c("b64decode 文本") + " Base64 解码\n" +
		"• " + c("urlencode 文本") + " URL 编码\n" +
		"• " + c("urldecode 文本") + " URL 解码\n\n" +
		"不写文本时处理所回复的那条消息。\n\n<b>示例</b>\n" +
		"• " + c("b64encode Hello World") + "\n" +
		"• " + c("b64decode SGVsbG8gV29ybGQ=") + "\n" +
		"• " + c("urlencode 你好世界") + "\n" +
		"• " + c("urldecode %E4%BD%A0%E5%A5%BD") + "\n\n" +
		"Base64 解码也认 URL 安全的写法（- 和 _）；URL 编码的规则同 JavaScript 的 encodeURIComponent，空格编成 %20。"
}

// Register 注册 .encode 和四个编码解码命令。不存东西。
func Register(a *app.App) {
	a.Registry.Register(&command.Command{Name: "encode", Group: command.GroupTools, Description: "查看编码解码命令",
		Help: help, Handle: func(ctx context.Context, inv *command.Invocation) error {
			return inv.Edit(ctx, help(inv.Prefix))
		}})
	for _, op := range operations {
		a.Registry.Register(&command.Command{Name: op.name, Group: command.GroupTools, Description: op.title + "文本",
			Usage: "[文本]", Help: help, FreeText: true,
			Handle: func(ctx context.Context, inv *command.Invocation) error { return run(ctx, inv, op) }})
	}
}

// run 取输入、转换、分页发出结果。
//
// 输入用原文（RawAfter），不把 Args 拼回去：编码要一字不差，拼回去会丢掉换行和连续空格。
// 命令收正文（FreeText），「.b64encode help」编码的就是 help 这个词，和 MiBox 一样。
func run(ctx context.Context, inv *command.Invocation, op operation) error {
	input := strings.TrimSpace(inv.RawAfter(0))
	if input == "" {
		reply, err := kit.Reply(ctx, inv)
		if err != nil {
			return err
		}
		if reply != nil {
			input = strings.TrimSpace(reply.Text)
		}
	}
	if input == "" {
		if inv.Message.ReplyToID != 0 {
			return kit.Fail("被回复的消息没有文字")
		}
		return inv.Edit(ctx, help(inv.Prefix))
	}
	if kit.UTF16Len(input) > maxInput {
		return kit.Failf("文本太长，最多 %d 字", maxInput)
	}
	output, err := op.transform(input)
	if err != nil {
		return kit.Fail(op.title + "失败：" + err.Error())
	}
	return inv.EditPages(ctx, command.HTMLPages(render(op, input, output), command.PageLimit))
}

// render 拼出结果消息。结果放在 <code> 里，点一下就能复制；太长时由 HTMLPages 分页，
// 续页会重新打开 <code>。
func render(op operation, input, output string) string {
	preview := input
	if utf8.RuneCountInString(preview) > previewRunes {
		preview = string([]rune(preview)[:previewRunes]) + "…"
	}
	result := command.Code(output)
	if output == "" {
		result = "（空）"
	}
	return "🔣 <b>" + command.Escape(op.title) + "</b>\n\n<b>原文</b>\n" + command.Code(preview) +
		"\n\n<b>结果</b>\n" + result
}

// decodeBase64 解码 Base64，要求结果是 UTF-8 文本。
//
// 空白（换行、空格）先去掉：从别处复制来的 Base64 常按 76 字一行折过。
// 标准和 URL 安全两种字母表都认，带不带 = 填充都行；但末尾多余的位必须是 0（Strict），
// 和 MiBox 一样，不接受「解得出来但编码回去对不上」的输入。
func decodeBase64(input string) (string, error) {
	compact := strings.Join(strings.Fields(input), "")
	encoding := base64.StdEncoding
	if strings.ContainsAny(compact, "-_") {
		encoding = base64.URLEncoding
	}
	if !strings.HasSuffix(compact, "=") {
		encoding = encoding.WithPadding(base64.NoPadding)
	}
	decoded, err := encoding.Strict().DecodeString(compact)
	if err != nil || compact == "" {
		return "", errors.New("不是有效的 Base64 字符串")
	}
	if !utf8.Valid(decoded) {
		return "", errors.New("解出来的内容不是 UTF-8 文本")
	}
	return string(decoded), nil
}

// uriUnreserved 是 encodeURIComponent 不转义的字符：字母、数字和 -_.!~*'()。
func uriUnreserved(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	}
	return strings.IndexByte("-_.!~*'()", b) >= 0
}

// encodeURIComponent 和 JavaScript 的同名函数一样：按 UTF-8 逐字节转成大写的 %XX。
// 不用 url.QueryEscape：它把空格写成 +，还会转义 !'()*，和 MiBox 的结果对不上。
func encodeURIComponent(input string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(input); i++ {
		c := input[i]
		if uriUnreserved(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}

// decodeURIComponent 和 JavaScript 的同名函数一样：% 后面必须是两位十六进制，
// 解出来必须是 UTF-8，+ 不当空格。url.PathUnescape 不查 UTF-8，所以另外查。
func decodeURIComponent(input string) (string, error) {
	var out []byte
	for i := 0; i < len(input); i++ {
		if input[i] != '%' {
			out = append(out, input[i])
			continue
		}
		if i+2 >= len(input) || !isHex(input[i+1]) || !isHex(input[i+2]) {
			return "", errors.New("不是有效的 URL 编码")
		}
		out = append(out, unhex(input[i+1])<<4|unhex(input[i+2]))
		i += 2
	}
	if !utf8.Valid(out) {
		return "", errors.New("解出来的内容不是 UTF-8 文本")
	}
	return string(out), nil
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}
