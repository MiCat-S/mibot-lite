package commands

import (
	"context"
	"errors"
	"fmt"
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
)

// userError is a message meant for the chat, not the log.
type userError struct{ text string }

func (e userError) Error() string { return e.text }

func fail(text string) error { return userError{text: text} }

func failf(format string, args ...any) error { return userError{text: fmt.Sprintf(format, args...)} }

// isUserError reports whether err carries a chat-facing message.
func isUserError(err error) (string, bool) {
	var ue userError
	if errors.As(err, &ue) {
		return ue.text, true
	}
	return "", false
}

// feedback renders a status line the way MiBox's ui.renderFeedback did.
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

// dataPath is where a command family keeps its JSON document.
func dataPath(a *app.App, name string) string {
	return filepath.Join(a.DataDir(), name)
}

// newStore opens a command family's document.
func newStore[T any](a *app.App, name string, defaults func() T) *store.Store[T] {
	return store.New(dataPath(a, name), defaults)
}

// sendPages edits the command message with the first page and replies with
// the rest.
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

// onOff parses on|off.
func onOff(value string) (bool, error) {
	switch strings.ToLower(value) {
	case "on":
		return true, nil
	case "off":
		return false, nil
	}
	return false, fail("请输入 on 或 off")
}

// sleepCtx waits or returns when ctx ends.
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

// retryFlood runs fn, waiting out FLOOD_WAIT and backing off on other
// errors, up to attempts retries.
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

// rpcCode extracts the Telegram error code from an error, else a short
// description.
func rpcCode(err error) string { return command.Brief(err) }

// utf16Slice cuts a string by UTF-16 offsets, the unit Telegram entities use.
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

// utf16Len is the length Telegram counts a message in.
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

// peerKey renders a peer as "kind:id", the comparison the dme port makes.
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

// truncateRunes cuts text to at most n runes.
func truncateRunes(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n])
}

// chatHTMLError turns a handler error into the chat message shown for it,
// or "" for a generic failure.
func chatHTMLError(err error) string {
	if text, ok := isUserError(err); ok {
		return command.Escape(text)
	}
	return ""
}
