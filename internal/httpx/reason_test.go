package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

type userFacing string

func (e userFacing) Error() string       { return string(e) }
func (e userFacing) UserMessage() string { return string(e) }

// Reason 的结果会发进聊天：用户错误照原话，传输失败给一句概括，不带 URL。
func TestReason(t *testing.T) {
	if got := Reason(fmt.Errorf("load: %w", userFacing("素材目录格式不对"))); got != "素材目录格式不对" {
		t.Errorf("a user error was hidden: %q", got)
	}
	if got := Reason(&StatusError{Status: 429}); !strings.Contains(got, "频繁") {
		t.Errorf("429: %q", got)
	}
	if got := Reason(fmt.Errorf("x: %w", context.DeadlineExceeded)); !strings.Contains(got, "超时") {
		t.Errorf("deadline: %q", got)
	}
	leak := &url.Error{Op: "Get", URL: "https://raw.example.com/secret/path", Err: errors.New("connection refused")}
	if got := Reason(leak); strings.Contains(got, "example") || strings.Contains(got, "http") {
		t.Errorf("a URL leaked: %q", got)
	}
}
