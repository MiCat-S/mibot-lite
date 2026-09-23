// Package kit 是各命令共用的小工具：显示给用户的错误、按命令存放的 JSON 文档、
// 分页发送、FLOOD_WAIT 重试、按 UTF-16 计算长度等。
package kit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// userError 是要显示在聊天里的消息，不是写给日志的。
type userError struct{ text string }

func (e userError) Error() string { return e.text }

func Fail(text string) error { return userError{text: text} }

func Failf(format string, args ...any) error { return userError{text: fmt.Sprintf(format, args...)} }

// IsUserError 判断 err 是否带着要发到聊天里的消息。
func IsUserError(err error) (string, bool) {
	var ue userError
	if errors.As(err, &ue) {
		return ue.text, true
	}
	return "", false
}

// Feedback 按 MiBox 的 ui.renderFeedback 的样式生成一行状态。
func Feedback(state, title, detail string) string {
	icon := "⏳"
	switch state {
	case "success":
		icon = "✅"
	case "error":
		icon = "❌"
	}
	text := icon + " " + command.Bold(title)
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

// RPCCode 从错误里取出 Telegram 错误码，取不到就给一段简短描述。
func RPCCode(err error) string { return command.Brief(err) }

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

// UTF16Len 是按 Telegram 的算法得出的消息长度。
func UTF16Len(text string) int {
	count := 0
	for _, r := range text {
		if r >= 0x10000 {
			count += 2
		} else {
			count++
		}
	}
	return count
}

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

// ChatHTMLError 把处理函数返回的错误转成要在聊天里显示的消息；
// 一般性的失败返回 ""。
func ChatHTMLError(err error) string {
	if text, ok := IsUserError(err); ok {
		return command.Escape(text)
	}
	return ""
}

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

func Clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func OrDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func OnOffText(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

func FormatBytes(size int) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(size)/(1<<10))
	}
	return strconv.Itoa(size) + " B"
}

func Download(ctx context.Context, url, target string, limit int64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", httpx.UserAgent)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", &httpx.StatusError{Status: response.StatusCode}
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, limit+1))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if written > limit {
		return "", httpx.ErrTooLarge
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func Version(a *app.App) string {
	if a.Version == "" {
		return "未知"
	}
	return a.Version
}

func OrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}
