// Package botapi 是可选的 HTTP Bot API 推送（目前只有 .checkin 的汇总用它）。
// 主连接是 MTProto userbot，不能套用这里按 Bot API 指导限额设的限流。
package botapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/delivery"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

// ErrQueueFull 表示同时等着发送的调用已经满了。
var ErrQueueFull = errors.New("Bot API 推送排队已满")

// APIError 是 Bot API 明确拒绝的响应，带着限流等待时间，不带含 Token 的 URL。
type APIError struct {
	Code        int
	Description string
	RetryAfter  time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Bot API 返回 %d：%s", e.Code, e.Description)
}

// unsent 判断传输错误是否发生在请求发出之前：连不上、域名解析失败都说明对方没收到。
func unsent(err error) bool {
	var dns *net.DNSError
	var op *net.OpError
	return errors.As(err, &dns) || (errors.As(err, &op) && op.Op == "dial")
}

type budget struct {
	global, cooldown time.Time
	chats            map[string]time.Time
}

// Client 可以并发使用。每个 Token 有自己的全局额度；同时最多接纳 32 个调用
// （包括正在等额度的），多出来的直接返回 ErrQueueFull，不另开协程。
type Client struct {
	mu      sync.Mutex
	budgets map[[32]byte]*budget
	slots   chan struct{}
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
	post    func(context.Context, string, map[string]string, any, time.Duration, int64) (httpx.Response, error)
}

func New() *Client {
	return &Client{budgets: make(map[[32]byte]*budget), slots: make(chan struct{}, 32),
		now: time.Now, sleep: sleep, post: postJSON}
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// wait 等到能发为止，能发时才记下这次占用的额度，所以中途取消不会白占以后的额度。
// 429 的冷却对同一个 Token 的所有聊天都生效。
func (c *Client) wait(ctx context.Context, key [32]byte, chat string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		now := c.now()
		for id, b := range c.budgets {
			for chat, until := range b.chats {
				if !until.After(now) {
					delete(b.chats, chat)
				}
			}
			if len(b.chats) == 0 && !b.global.After(now) && !b.cooldown.After(now) {
				delete(c.budgets, id)
			}
		}
		b := c.budgets[key]
		if b == nil {
			b = &budget{chats: make(map[string]time.Time)}
			c.budgets[key] = b
		}
		until := b.global
		if b.chats[chat].After(until) {
			until = b.chats[chat]
		}
		if b.cooldown.After(until) {
			until = b.cooldown
		}
		wait := until.Sub(now)
		if wait <= 0 {
			b.global = now.Add((time.Second + 29) / 30)
			interval := time.Second
			// 群组、超级群组的数字 ID 是负数。频道和 @用户名 也按群组的保守间隔算：
			// 用户名也可能指向群组。
			if strings.HasPrefix(chat, "-") || strings.HasPrefix(chat, "@") {
				interval = 3 * time.Second // 每分钟不超过 20 条
			}
			b.chats[chat] = now.Add(interval)
		}
		c.mu.Unlock()
		if wait <= 0 {
			return nil
		}
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

func (c *Client) pause(key [32]byte, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.budgets[key]
	if b == nil {
		b = &budget{chats: make(map[string]time.Time)}
		c.budgets[key] = b
	}
	if until := c.now().Add(d); until.After(b.cooldown) {
		b.cooldown = until
	}
}

// SendHTML 发一条 HTML 消息，只对明确的 429 拒绝有限重试。网络错误和 5xx 时消息
// 可能已经发出去了，Bot API 没有 random_id 去重，盲目重试会发出重复消息；
// 这类结果用 delivery.Unknown 标出，由调用方决定怎么办。
func (c *Client) SendHTML(ctx context.Context, endpoint, token, chat, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		return ErrQueueFull
	}
	key := sha256.Sum256([]byte(endpoint + "\x00" + token))
	request := struct {
		ChatID             string `json:"chat_id"`
		Text               string `json:"text"`
		ParseMode          string `json:"parse_mode"`
		LinkPreviewOptions struct {
			Disabled bool `json:"is_disabled"`
		} `json:"link_preview_options"`
	}{ChatID: chat, Text: text, ParseMode: "HTML"}
	request.LinkPreviewOptions.Disabled = true
	for attempt := 0; ; attempt++ {
		if err := c.wait(ctx, key, chat); err != nil {
			return err
		}
		response, err := c.post(ctx, strings.TrimRight(endpoint, "/")+"/bot"+token+"/sendMessage", nil, request, 15*time.Second, 1<<20)
		if err != nil {
			if ctx.Err() != nil {
				return delivery.Unknown(ctx.Err())
			}
			// URL 错误的文字里有 Token，不能原样往外传。
			failure := errors.New("Bot API: " + httpx.Reason(err))
			if unsent(err) {
				return failure
			}
			return delivery.Unknown(failure)
		}
		var envelope struct {
			OK          bool   `json:"ok"`
			Code        int    `json:"error_code"`
			Description string `json:"description"`
			Parameters  struct {
				RetryAfter int64 `json:"retry_after"`
			} `json:"parameters"`
		}
		if err := json.Unmarshal(response.Body, &envelope); err != nil {
			// 代理或网关出错时常返回 HTML 页面，这时报状态码比报「不是 JSON」有用。
			apiErr := &APIError{Code: response.Status, Description: "响应不是 JSON"}
			if response.Status >= 500 || response.OK() {
				return delivery.Unknown(apiErr)
			}
			return apiErr
		}
		if response.OK() && envelope.OK {
			return nil
		}
		code := envelope.Code
		if code == 0 {
			code = response.Status
		}
		apiErr := &APIError{Code: code, Description: strings.ReplaceAll(envelope.Description, token, "[redacted]")}
		if code >= 500 {
			return delivery.Unknown(apiErr)
		}
		if code != 429 {
			return apiErr
		}
		// 先排除离谱的秒数，再换算成 Duration，免得溢出。
		seconds := envelope.Parameters.RetryAfter
		if seconds < 0 || seconds > 86400 {
			return apiErr
		}
		apiErr.RetryAfter = time.Duration(seconds) * time.Second
		delay := time.Second << min(attempt, 3)
		wait := max(apiErr.RetryAfter, delay) + time.Duration(rand.Int64N(int64(250*time.Millisecond)))
		c.pause(key, wait)
		if attempt >= 3 || apiErr.RetryAfter > time.Minute {
			return apiErr
		}
	}
}

// postJSON 发送 JSON 请求。URL 里有 Token、正文是消息，都不能跟随重定向转给别的地址。
func postJSON(ctx context.Context, target string, headers map[string]string, payload any, timeout time.Duration, limit int64) (httpx.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return httpx.Response{}, err
	}
	merged := map[string]string{"Content-Type": "application/json"}
	for key, value := range headers {
		merged[key] = value
	}
	return httpx.Do(ctx, httpx.Request{Method: "POST", URL: target, Headers: merged, Body: body, Timeout: timeout, MaxBytes: limit, NoRedirect: true})
}
