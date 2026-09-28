package command

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
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

// FailWith 返回一个给用户看的错误：text 说明什么失败了，括号里附上 Brief(cause)
// （RPC 错误码、网络概况之类；内部错误只说「详情见日志」）。cause 本身就是给用户看的
// 错误时，直接用它那句更具体的话。cause 留在错误链里，派发器把它完整记进日志。
// cause 为 nil 时返回 nil，所以可以直接包住一个处理函数的返回值。
func FailWith(text string, cause error) error {
	if cause == nil {
		return nil
	}
	return failure{text: text, cause: cause}
}

type failure struct {
	text  string
	cause error
}

func (e failure) Error() string { return e.text + ": " + e.cause.Error() }
func (e failure) Unwrap() error { return e.cause }
func (e failure) UserMessage() string {
	if text, ok := IsUserError(e.cause); ok {
		return text
	}
	if brief := Brief(e.cause); brief != internalError {
		return e.text + "（" + brief + "）"
	}
	return e.text + "，详情见日志"
}

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
//   - 给用户看的错误（Fail）：原样，超过 80 个字截断；
//   - Telegram RPC 错误：只给错误码；
//   - 找不到对话的 access hash、超时：一句固定的说明；
//   - HTTP 请求失败：httpx.Reason 的概括（超时、HTTP 状态码、网络不通）；
//   - 已经拼成文字、里面还认得出错误码的：只给错误码；
//   - 其余：「内部错误，详情见日志」。Go 的网络错误会带完整 URL（包括用户自己设的 API 地址），
//     不能原样发出去。
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
		// 要在 isNetwork 之前：context.DeadlineExceeded 也实现了 net.Error。
		return "超时"
	case isNetwork(err):
		return httpx.Reason(err)
	}
	// 已经被拼成文字的 RPC 错误（errors.New("…：" + Brief(err)) 之类）只剩错误码可认。
	if match := rpcCode.FindString(err.Error()); match != "" && !strings.Contains(err.Error(), "://") {
		return match
	}
	return internalError
}

const internalError = "内部错误，详情见日志"

// isNetwork 判断是不是 HTTP 请求本身失败了（连不上、超时、状态码不对、响应太大）。
func isNetwork(err error) bool {
	var status *httpx.StatusError
	var urlErr *url.Error
	var netErr net.Error
	return errors.As(err, &status) || errors.Is(err, httpx.ErrTooLarge) || errors.As(err, &urlErr) || errors.As(err, &netErr)
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
