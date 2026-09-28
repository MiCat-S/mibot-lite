package commands

import (
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
)

// isEmoji 粗略判断一个字符是不是 emoji：符号、箭头、杂项符号、图形符号这几段，
// 以及变体选择符和零宽连接符（组合 emoji 用的）。
func isEmoji(r rune) bool {
	return (r >= 0x2190 && r <= 0x2BFF) || (r >= 0x1F000 && r <= 0x1FAFF) || r == 0xFE0F || r == 0x200D || r == 0x20E3
}

// leadingEmoji 取出开头连续的 emoji 字符。
func leadingEmoji(text string) string {
	end := 0
	for end < len(text) {
		r, size := utf8.DecodeRuneInString(text[end:])
		if !isEmoji(r) {
			break
		}
		end += size
	}
	return text[:end]
}

var (
	// hanThenASCII 是中文后面紧跟半角冒号或左括号，STYLE.md 要求中文句子里用全角。
	hanThenASCII = regexp.MustCompile(`\p{Han}[:(]`)
	// emojiInBold 是把 emoji 放进了 <b> 里：顶层标题的 emoji 在 <b> 外，小标题不带 emoji。
	emojiInBold = regexp.MustCompile(`<b>[\x{2190}-\x{2BFF}\x{1F000}-\x{1FAFF}]`)
	statusEmoji = []string{"✅", "❌", "⏳", "⚠️", "⚠"}
)

// 命令的说明、用法和帮助按 STYLE.md 写。这里查能机器检查的几条；以前每个命令各写各的，
// 冒号半角全角混用、标题有没有 emoji、用法怎么写都不一样。
func TestCommandTextStyle(t *testing.T) {
	a := &app.App{Root: t.TempDir(), Registry: command.New([]string{"."}, slog.New(slog.NewTextHandler(io.Discard, nil)))}
	RegisterAll(a)
	owner := map[string]string{} // 标题 emoji → 用它的帮助（几条命令可以共用一份帮助）
	for _, cmd := range a.Registry.Commands() {
		name := "." + cmd.Name
		description := []rune(cmd.Description)
		if len(description) < 4 || len(description) > 16 {
			t.Errorf("%s 的说明 %q 有 %d 个字，应为 4–16", name, cmd.Description, len(description))
		}
		if strings.ContainsAny(cmd.Description, "，,。") || strings.HasSuffix(cmd.Description, ".") {
			t.Errorf("%s 的说明 %q 带了逗号或句号", name, cmd.Description)
		}
		if strings.Contains(cmd.Usage, " | ") || strings.Contains(cmd.Usage, "...") {
			t.Errorf("%s 的用法 %q：几选一写 a|b，不留空格，不用 ...", name, cmd.Usage)
		}
		if cmd.Help == nil {
			t.Errorf("%s 没有帮助", name)
			continue
		}
		help := cmd.Help(".")
		emoji := leadingEmoji(help)
		switch {
		case emoji == "" || !strings.HasPrefix(help[len(emoji):], " <b>"):
			t.Errorf("%s 的帮助没有以「emoji <b>标题</b>」开头：%q", name, firstLine(help))
		case contains(statusEmoji, emoji):
			t.Errorf("%s 的帮助标题用了状态 emoji %s", name, emoji)
		case owner[emoji] != "" && owner[emoji] != help:
			t.Errorf("%s 的标题 emoji %s 和别的命令重复了", name, emoji)
		default:
			owner[emoji] = help
		}
		for _, text := range []string{help, cmd.Description} {
			if strings.Contains(text, "...") {
				t.Errorf("%s：省略号用「…」：%q", name, around(text, "..."))
			}
			if match := hanThenASCII.FindString(text); match != "" {
				t.Errorf("%s：中文后面用了半角标点 %q：%q", name, match, around(text, match))
			}
			if match := emojiInBold.FindString(text); match != "" {
				t.Errorf("%s：emoji 放进了 <b> 里：%q", name, around(text, match))
			}
			if strings.Contains(text, "</code> - ") {
				t.Errorf("%s：条目写成「• <code>命令</code> 说明」，中间不加「-」：%q", name, around(text, "</code> - "))
			}
		}
	}
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// around 取 needle 前后一小段，报错时好找。
func around(text, needle string) string {
	index := strings.Index(text, needle)
	if index < 0 {
		return ""
	}
	start, end := max(0, index-24), min(len(text), index+len(needle)+24)
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '⏎'
		}
		return r
	}, text[start:end])
}
