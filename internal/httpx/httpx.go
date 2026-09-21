// Package httpx is the bounded HTTP client every command that talks to the
// web goes through: one shared transport, a per-request timeout, a byte
// limit on the body, and no more than three redirects.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// UserAgent is sent unless a request overrides it.
const UserAgent = "MiBot-Lite/1"

// ErrTooLarge means the body exceeded the request's byte limit.
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

// Request describes one call.
type Request struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
	// Timeout bounds the whole call. Zero means 30 seconds.
	Timeout time.Duration
	// MaxBytes bounds the body. Zero means 2 MB.
	MaxBytes int64
}

// Response is what came back.
type Response struct {
	Status int
	Body   []byte
}

// OK reports a 2xx status.
func (r Response) OK() bool { return r.Status >= 200 && r.Status < 300 }

// Do performs the request. A non-2xx status is not an error: the caller
// decides what a 404 means.
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

// GetJSON fetches and decodes a JSON document.
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

// PostJSON sends a JSON body and returns the raw response.
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

// StatusError is a non-2xx response.
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return fmt.Sprintf("HTTP %d", e.Status) }

// Reason renders a transport failure for a chat message without leaking
// URLs or hosts.
func Reason(err error) string {
	var status *StatusError
	switch {
	case errors.As(err, &status):
		if status.Status == 429 {
			return "请求过于频繁，请稍后重试"
		}
		return fmt.Sprintf("服务返回 HTTP %d", status.Status)
	case errors.Is(err, context.DeadlineExceeded):
		return "请求超时，请稍后重试"
	case errors.Is(err, ErrTooLarge):
		return "响应数据过大"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "请求超时，请稍后重试"
	}
	return "网络请求失败，请稍后重试"
}
