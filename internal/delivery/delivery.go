// Package delivery 区分消息发送失败的两种情况：确定没发出去，和不确定有没有送达。
//
// 后者是请求可能已经被 Telegram 接受、只是没拿到明确结果（连接断开、读响应失败、
// 等待途中被取消、服务端内部错误）。这时换个途径、或者重新构造请求再发，都可能重复：
// MTProto 重新发送会换一个 RandomID，Bot API 根本没有去重。MTProto 的 bot.Client 和
// Bot API 的 botapi.Client 都用这里的类型报告，业务层据此决定要不要改走别的途径。
package delivery

import "errors"

// UncertainError 包着「不确定有没有送达」的发送错误。错误文字保持原样，
// errors.Is / errors.As 照常能看到里面的错误。
type UncertainError struct{ Err error }

func (e *UncertainError) Error() string { return e.Err.Error() }
func (e *UncertainError) Unwrap() error { return e.Err }

// Unknown 把 err 标成「不确定有没有送达」；err 为 nil 时返回 nil。
func Unknown(err error) error {
	if err == nil {
		return nil
	}
	return &UncertainError{Err: err}
}

// Uncertain 判断 err 是不是「不确定有没有送达」。
func Uncertain(err error) bool {
	var uncertain *UncertainError
	return errors.As(err, &uncertain)
}
