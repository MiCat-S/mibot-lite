package commands

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

func fail(text string) error { return userError{text: text} }

func failf(format string, args ...any) error { return userError{text: fmt.Sprintf(format, args...)} }

// isUserError 判断 err 是否带着要发到聊天里的消息。
func isUserError(err error) (string, bool) {
	var ue userError
	if errors.As(err, &ue) {
		return ue.text, true
	}
	return "", false
}

// feedback 按 MiBox 的 ui.renderFeedback 的样式生成一行状态。
func feedback(state, title, detail string) string {
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

// newStore 打开一类命令的文档。
func newStore[T any](a *app.App, name string, defaults func() T) *store.Store[T] {
	return store.New(dataPath(a, name), defaults)
}

// sendPages 把命令消息编辑成第一页，其余各页作为回复发出。
func sendPages(ctx context.Context, inv *command.Invocation, pages []string) error {
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

// onOff 解析 on|off。
func onOff(value string) (bool, error) {
	switch strings.ToLower(value) {
	case "on":
		return true, nil
	case "off":
		return false, nil
	}
	return false, fail("请输入 on 或 off")
}

// sleepCtx 等待 d，ctx 结束时提前返回。
func sleepCtx(ctx context.Context, d time.Duration) error {
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

// retryFlood 执行 fn：遇到 FLOOD_WAIT 就等够规定的时间，遇到其他错误则
// 退避，最多重试 attempts 次。
func retryFlood(ctx context.Context, attempts int, fn func() error) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil || ctx.Err() != nil || attempt >= attempts {
			return err
		}
		wait := time.Duration(2*(attempt+1)) * time.Second
		if flood, ok := bot.FloodWait(err); ok {
			wait = flood + time.Second
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return err
		}
	}
}

// rpcCode 从错误里取出 Telegram 错误码，取不到就给一段简短描述。
func rpcCode(err error) string { return command.Brief(err) }

// utf16Slice 按 UTF-16 偏移截取字符串，Telegram 的 entity 用的就是这个单位。
func utf16Slice(text string, offset, length int) string {
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

// utf16Len 是按 Telegram 的算法得出的消息长度。
func utf16Len(text string) int {
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

// peerKey 把 peer 写成 "kind:id"，dme 移植版就是按这个来比较的。
func peerKey(peer tg.PeerClass) string {
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

// truncateRunes 把 text 截到最多 n 个 rune。
func truncateRunes(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n])
}

// chatHTMLError 把处理函数返回的错误转成要在聊天里显示的消息；
// 一般性的失败返回 ""。
func chatHTMLError(err error) string {
	if text, ok := isUserError(err); ok {
		return command.Escape(text)
	}
	return ""
}

func messageID(item tg.MessageClass) int {
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

func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func onOffText(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

func formatBytes(size int) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(size)/(1<<10))
	}
	return strconv.Itoa(size) + " B"
}

func download(ctx context.Context, url, target string, limit int64) (string, error) {
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

func versionOf(a *app.App) string {
	if a.Version == "" {
		return "未知"
	}
	return a.Version
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}
