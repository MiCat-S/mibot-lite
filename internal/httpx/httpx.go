// Package httpx 是带限制的 HTTP 客户端，所有要联网的命令都走它：
// 共用一个 transport，每个请求有超时，响应体有字节上限，重定向最多三次。
package httpx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// UserAgent 是默认发送的 User-Agent，请求可以自己覆盖。
const UserAgent = "MiBot-Lite/1"

// ErrTooLarge 表示响应体超过了请求设定的字节上限。
var ErrTooLarge = errors.New("response body exceeds the size limit")

var shared = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}
		return nil
	},
}

// Request 描述一次调用。
type Request struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
	// Timeout 限制整个调用的时长。零值表示 30 秒。
	Timeout time.Duration
	// MaxBytes 限制响应体大小。零值表示 2 MB。
	MaxBytes int64
}

// Response 是返回的结果。
type Response struct {
	Status int
	Body   []byte
}

// OK 判断状态码是不是 2xx。
func (r Response) OK() bool { return r.Status >= 200 && r.Status < 300 }

// Do 执行请求。非 2xx 状态码不算错误：404 意味着什么由调用方决定。
func Do(ctx context.Context, request Request) (Response, error) {
	timeout := request.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	limit := request.MaxBytes
	if limit <= 0 {
		limit = 2 << 20
	}
	method := request.Method
	if method == "" {
		method = http.MethodGet
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body io.Reader
	if request.Body != nil {
		body = bytes.NewReader(request.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, request.URL, body)
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("User-Agent", UserAgent)
	for key, value := range request.Headers {
		req.Header.Set(key, value)
	}
	resp, err := shared.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return Response{Status: resp.StatusCode}, err
	}
	if int64(len(data)) > limit {
		return Response{Status: resp.StatusCode}, ErrTooLarge
	}
	return Response{Status: resp.StatusCode, Body: data}, nil
}

// GetJSON 获取并解码一个 JSON 文档。
func GetJSON(ctx context.Context, url string, timeout time.Duration, maxBytes int64, out any) error {
	response, err := Do(ctx, Request{URL: url, Timeout: timeout, MaxBytes: maxBytes})
	if err != nil {
		return err
	}
	if !response.OK() {
		return &StatusError{Status: response.Status}
	}
	if err := json.Unmarshal(response.Body, out); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// PostJSON 发送 JSON 请求体，返回原始响应。
func PostJSON(ctx context.Context, url string, headers map[string]string, payload any, timeout time.Duration, maxBytes int64) (Response, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Response{}, err
	}
	merged := map[string]string{"Content-Type": "application/json"}
	for key, value := range headers {
		merged[key] = value
	}
	return Do(ctx, Request{Method: http.MethodPost, URL: url, Headers: merged, Body: encoded, Timeout: timeout, MaxBytes: maxBytes})
}

// StatusError 表示非 2xx 的响应。
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return fmt.Sprintf("HTTP %d", e.Status) }

// Reason 把传输失败转成能发到聊天里的说明，不泄露 URL 和主机名。
//
// 给用户看的错误（实现 UserMessage，如 command.Fail）照原话返回：调用方常常把整段
// 「下载 + 解析 + 处理」的错误都交给它，以前「素材目录格式不对」、ffmpeg 失败这些
// 都被说成「网络请求失败」。
func Reason(err error) string {
	var message interface{ UserMessage() string }
	if errors.As(err, &message) {
		return message.UserMessage()
	}
	var status *StatusError
	switch {
	case errors.As(err, &status):
		if status.Status == 429 {
			return "请求过于频繁，稍后再试"
		}
		return fmt.Sprintf("服务返回 HTTP %d", status.Status)
	case errors.Is(err, context.DeadlineExceeded):
		return "请求超时，稍后再试"
	case errors.Is(err, ErrTooLarge):
		return "响应数据过大"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "请求超时，稍后再试"
	}
	return "网络请求失败，稍后再试"
}

// DownloadFile 把 url 下载到 target（边下边写，不整个读进内存），返回内容的 SHA-256。
// 超过 limit 字节或状态码不是 200 都算失败。和 Do 共用一个 transport，超时 5 分钟。
// 用于下载新版本的程序文件这类几十 MB 的东西。
func DownloadFile(ctx context.Context, url, target string, limit int64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", UserAgent)
	response, err := shared.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", &StatusError{Status: response.StatusCode}
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
		return "", ErrTooLarge
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
