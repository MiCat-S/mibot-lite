package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

// Gemini 的 Key 在 URL 中：传输失败后原因和阶段进日志，凭证和请求 URL 不进日志或聊天。
func TestRequestFailureLogsContextWithoutCredentials(t *testing.T) {
	for _, stage := range []string{"response_headers", "response_body", "decode"} {
		t.Run(stage, func(t *testing.T) {
			secret := "sk+secret/123"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("key") != secret {
					t.Error("请求没有带上测试凭证")
				}
				if stage == "decode" {
					fmt.Fprint(w, "invalid JSON")
					return
				}
				if stage == "response_body" {
					w.Header().Set("Content-Length", "1000")
					fmt.Fprint(w, "partial")
					w.(http.Flusher).Flush()
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				conn.Close()
			}))
			defer server.Close()
			cfg := testConfig(aiProvider{URL: server.URL, Key: secret, Type: "gemini"})
			_, err := chatText(context.Background(), cfg, cfg.selection("chat"), "问题", "", chatOptions{})
			if err == nil {
				t.Fatal("应当返回请求失败")
			}
			var logs strings.Builder
			slog.New(slog.NewTextHandler(&logs, nil)).Warn("command.refused", "error", kit.FailWith("AI 操作失败", err).Error())
			line := logs.String()
			for _, want := range []string{"tag=", "model=", "stream=false", "responses=false", "timeout=30s", "took=", "stage=" + stage} {
				if !strings.Contains(line, want) {
					t.Errorf("日志缺少 %q：%s", want, line)
				}
			}
			message, _ := kit.IsUserError(err)
			for _, private := range []string{secret, url.QueryEscape(secret), server.URL} {
				if strings.Contains(line, private) || strings.Contains(message, private) {
					t.Fatalf("日志或聊天泄露请求信息：%s / %s", line, message)
				}
			}
			if stage != "decode" && !strings.Contains(line, "EOF") {
				t.Fatalf("日志丢失原始传输原因：%s", line)
			}
		})
	}
}

func TestRequestFailureKeepsTimeoutCause(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	request := &aiRequest{Model: "m", URL: server.URL, Body: map[string]any{"stream": true},
		Provider: aiProvider{Tag: "p"}, Timeout: 50 * time.Millisecond, Responses: true}
	_, err := aiCall(context.Background(), request)
	var network *httpx.RequestError
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &network) || network.Stage != "response_headers" {
		t.Fatalf("原始超时或阶段丢失：%v", err)
	}
	if message, _ := kit.IsUserError(err); message != "AI 请求失败：请求超时，稍后再试" {
		t.Fatalf("聊天提示改变：%q", message)
	}
	for _, want := range []string{`tag="p"`, `model="m"`, "stream=true", "responses=true", "context deadline exceeded"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误缺少 %q：%v", want, err)
		}
	}
}
