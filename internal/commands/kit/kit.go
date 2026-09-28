// Package kit 是各命令共用的小工具：显示给用户的错误和用法、进行中的提示、按命令存放的
// JSON 文档、分页发送、长时间限流的重试、按 UTF-16 计算长度等。
package kit

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/store"
	"github.com/MiCat-S/mibot-lite/internal/sysinfo"
)

// Fail 返回要原样显示给用户的错误，见 command.Fail。
func Fail(text string) error { return command.Fail(text) }

// Failf 同 Fail，文字按 fmt.Sprintf 格式化。
func Failf(format string, args ...any) error { return command.Failf(format, args...) }

// FailWith 返回「text（错误码）」这样给用户看的错误，原始错误进日志，见 command.FailWith。
func FailWith(text string, cause error) error { return command.FailWith(text, cause) }

// IsUserError 取出错误里要给用户看的那句话，见 command.IsUserError。
func IsUserError(err error) (string, bool) { return command.IsUserError(err) }

// Feedback 生成一行状态（STYLE.md 的第三层）：状态 emoji 加标题，标题不加粗；
// detail 不为空时另起一行。state 是 success、error 或 working。
func Feedback(state, title, detail string) string {
	icon := "⏳"
	switch state {
	case "success":
		icon = "✅"
	case "error":
		icon = "❌"
	}
	text := icon + " " + command.Escape(title)
	if detail != "" {
		text += "\n" + command.Escape(detail)
	}
	return text
}

// dataPath 是一类命令存放其 JSON 文档的位置。
func dataPath(a *app.App, name string) string {
	return filepath.Join(a.DataDir(), name)
}

// NewStore 打开一类命令的文档。
func NewStore[T any](a *app.App, name string, defaults func() T) *store.Store[T] {
	return store.New(dataPath(a, name), defaults)
}

// SendPages 把命令消息编辑成第一页，其余各页作为回复发出。
func SendPages(ctx context.Context, inv *command.Invocation, pages []string) error {
	for index, page := range pages {
		if err := ctx.Err(); err != nil {
			return err
		}
		if index == 0 {
			if err := inv.Edit(ctx, page); err != nil {
				return err
			}
			continue
		}
		if err := inv.Reply(ctx, page); err != nil {
			return err
		}
	}
	return nil
}

// Warn 在 err 不为空时记一条带 error 的警告。用于「失败了也不影响这次命令，但不能悄悄吞掉」
// 的地方，比如存进度、写缓存。a 或它的日志器为空（测试里）时什么也不做。
func Warn(a *app.App, event string, err error) {
	if err != nil && a != nil && a.Logger != nil {
		a.Logger.Warn(event, "error", err.Error())
	}
}

// Working 是「⏳ 正在…」这样的进行中提示（纯文本，调用方用 EditText 发）。text 以「正在」开头，
// 不用自己加省略号。
func Working(text string) string { return "⏳ " + strings.TrimSuffix(text, "…") + "…" }

// Usage 返回「用法：<前缀><用法>」这样给用户看的错误，前缀取用户实际用的那个，照抄就能用。
func Usage(prefix, usage string) error { return command.Fail("用法：" + prefix + usage) }

// OnOff 解析 on|off。
func OnOff(value string) (bool, error) {
	switch strings.ToLower(value) {
	case "on":
		return true, nil
	case "off":
		return false, nil
	}
	return false, Fail("请输入 on 或 off")
}

// Sleep 等待 d，ctx 结束时提前返回。
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// RetryFlood 执行 fn：遇到 FLOOD_WAIT 就等够规定的时间，遇到其他错误则
// 退避，最多重试 attempts 次。
//
// 连接层的 bot.Retrier 已经会等 60 秒以内的限流、重试服务端错误；这里是给 .dme 批量删除用的：
// 删几千条消息时限流常常超过 60 秒，别的暂时性错误也值得再试，重试次数照 MiBox 由配置决定。
func RetryFlood(ctx context.Context, attempts int, fn func() error) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil || ctx.Err() != nil || attempt >= attempts {
			return err
		}
		wait := time.Duration(2*(attempt+1)) * time.Second
		if flood, ok := bot.FloodWait(err); ok {
			wait = flood + time.Second
		}
		if err := Sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// UTF16Slice 按 UTF-16 偏移截取字符串，Telegram 的 entity 用的就是这个单位。
func UTF16Slice(text string, offset, length int) string {
	units := utf16.Encode([]rune(text))
	if offset < 0 || length <= 0 || offset >= len(units) {
		return ""
	}
	end := offset + length
	if end > len(units) {
		end = len(units)
	}
	return string(utf16.Decode(units[offset:end]))
}

// UTF16Len 是按 Telegram 的算法得出的消息长度，见 command.UTF16Len。
func UTF16Len(text string) int { return command.UTF16Len(text) }

// PeerKey 把 peer 写成 "kind:id"，dme 移植版就是按这个来比较的。
func PeerKey(peer tg.PeerClass) string {
	switch value := peer.(type) {
	case *tg.PeerUser:
		return "userId:" + strconv.FormatInt(value.UserID, 10)
	case *tg.PeerChannel:
		return "channelId:" + strconv.FormatInt(value.ChannelID, 10)
	case *tg.PeerChat:
		return "chatId:" + strconv.FormatInt(value.ChatID, 10)
	}
	return ""
}

// TruncateRunes 把 text 截到最多 n 个 rune。
func TruncateRunes(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n])
}

// MessageID 是任意一种消息（普通、服务、空）的编号。
func MessageID(item tg.MessageClass) int {
	switch value := item.(type) {
	case *tg.Message:
		return value.ID
	case *tg.MessageService:
		return value.ID
	case *tg.MessageEmpty:
		return value.ID
	}
	return 0
}

// Clamp 把 value 限制在 [low, high] 里。
func Clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// OrDefault 在 value 只有空白时返回 fallback。
func OrDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// OnOffText 是开关状态的显示：开启或关闭（参数照旧收 on/off，显示用中文，见 STYLE.md）。
func OnOffText(value bool) string {
	if value {
		return "开启"
	}
	return "关闭"
}

// FormatBytes 按 1024 进位写字节数，见 sysinfo.FormatBytes。
func FormatBytes(size int64) string {
	if size < 0 {
		size = 0
	}
	return sysinfo.FormatBytes(uint64(size))
}

// Version 是运行中的版本号，开发构建没有版本号时写「未知」。
func Version(a *app.App) string {
	if a.Version == "" {
		return "未知"
	}
	return a.Version
}

// OrDash 在 value 只有空白时返回「—」，给查询结果里没有的字段用。
func OrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}
