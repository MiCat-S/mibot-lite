package command

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/bot"
)

// userMessage 是带着一句要给用户看的话的错误。实现它的错误由派发器显示成「❌ 这句话」，
// 其余错误只显示 Brief 给出的概括，细节进日志。底层的包（如 media）不依赖本包也能实现它。
type userMessage interface {
	UserMessage() string
}

// userError 是 Fail 和 Failf 返回的错误。
type userError struct{ text string }

func (e userError) Error() string       { return e.text }
func (e userError) UserMessage() string { return e.text }

// Fail 返回一个要原样显示给用户的错误：处理函数 return 它，聊天里就显示「❌ text」。
func Fail(text string) error { return userError{text: text} }

// Failf 同 Fail，文字按 fmt.Sprintf 格式化。
func Failf(format string, args ...any) error { return userError{text: fmt.Sprintf(format, args...)} }

func isUserError(err error) bool {
	_, ok := IsUserError(err)
	return ok
}

// IsUserError 取出错误里要给用户看的那句话。
func IsUserError(err error) (string, bool) {
	var message userMessage
	if errors.As(err, &message) {
		return message.UserMessage(), true
	}
	return "", false
}

// Brief 把错误概括成能发进聊天的一句话，不带 URL、主机名、路径这类细节：
//   - 给用户看的错误（Fail）：原样；
//   - Telegram RPC 错误：只给错误码；
//   - 找不到对话的 access hash、超时：一句固定的说明；
//   - 其余：「内部错误，详情见日志」。Go 的网络错误会带完整 URL（包括用户自己设的 API 地址），
//     不能原样发出去。
//
// 结果最多 80 个字符。
func Brief(err error) string {
	if err == nil {
		return ""
	}
	if text, ok := IsUserError(err); ok {
		return Truncate(text, 80)
	}
	if rpcErr, ok := tgerr.As(err); ok {
		return rpcErr.Message
	}
	switch {
	case errors.Is(err, bot.ErrUnaddressablePeer):
		return "无法定位目标对话"
	case errors.Is(err, context.DeadlineExceeded):
		return "超时"
	}
	// 已经被拼成文字的 RPC 错误（errors.New("…：" + Brief(err)) 之类）只剩错误码可认。
	if match := rpcCode.FindString(err.Error()); match != "" && !strings.Contains(err.Error(), "://") {
		return match
	}
	return "内部错误，详情见日志"
}

var rpcCode = regexp.MustCompile(`\b[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+\b`)

// Truncate 把 text 截到最多 n 个字符（按 rune 数，不会切开一个字），截掉了就在末尾加「…」。
func Truncate(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n]) + "…"
}
