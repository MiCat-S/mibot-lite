package botapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/delivery"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

func virtualClient() (*Client, *time.Duration) {
	c := New()
	now := time.Unix(1800000000, 0)
	elapsed := new(time.Duration)
	c.now = func() time.Time { return now }
	c.sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		*elapsed += d
		now = now.Add(d)
		return nil
	}
	c.post = func(context.Context, string, map[string]string, any, time.Duration, int64) (httpx.Response, error) {
		return httpx.Response{Status: 200, Body: []byte(`{"ok":true}`)}, nil
	}
	return c, elapsed
}

func TestPayloadAndResponseOK(t *testing.T) {
	c, _ := virtualClient()
	c.post = func(_ context.Context, url string, _ map[string]string, payload any, _ time.Duration, _ int64) (httpx.Response, error) {
		if !strings.HasSuffix(url, "/botSECRET/sendMessage") {
			t.Fatal("wrong method URL")
		}
		body, _ := json.Marshal(payload)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		if parsed["disable_web_page_preview"] != nil || parsed["link_preview_options"].(map[string]any)["is_disabled"] != true {
			t.Fatalf("obsolete payload: %s", body)
		}
		return httpx.Response{Status: 200, Body: []byte(`{"ok":false,"error_code":400,"description":"bad SECRET"}`)}, nil
	}
	err := c.SendHTML(context.Background(), "https://example.test", "SECRET", "1", "<b>text</b>")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 400 || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("invalid or leaking error: %v", err)
	}
}

func Test429RetriesAfterServerWait(t *testing.T) {
	c, elapsed := virtualClient()
	calls := 0
	c.post = func(context.Context, string, map[string]string, any, time.Duration, int64) (httpx.Response, error) {
		calls++
		if calls == 1 {
			return httpx.Response{Status: 429, Body: []byte(`{"ok":false,"error_code":429,"parameters":{"retry_after":7}}`)}, nil
		}
		return httpx.Response{Status: 200, Body: []byte(`{"ok":true}`)}, nil
	}
	if err := c.SendHTML(context.Background(), "https://example.test", "T", "1", "x"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || *elapsed < 7*time.Second || *elapsed > 8*time.Second {
		t.Fatalf("calls=%d elapsed=%s", calls, *elapsed)
	}
}

func TestChatGroupAndGlobalBudgets(t *testing.T) {
	for _, test := range []struct {
		chat, next string
		minimum    time.Duration
	}{{"1", "1", time.Second}, {"-1001", "-1001", 3 * time.Second}, {"@group", "@group", 3 * time.Second}, {"1", "2", (time.Second + 29) / 30}} {
		c, elapsed := virtualClient()
		_ = c.SendHTML(context.Background(), "https://example.test", "T", test.chat, "x")
		_ = c.SendHTML(context.Background(), "https://example.test", "T", test.next, "x")
		if *elapsed != test.minimum {
			t.Errorf("%s -> %s slept %s, want %s", test.chat, test.next, *elapsed, test.minimum)
		}
		before := *elapsed
		_ = c.SendHTML(context.Background(), "https://example.test", "OTHER", test.chat, "x")
		if *elapsed != before {
			t.Fatal("separate tokens share a budget")
		}
	}
}

func Test429ExhaustionAndLongWait(t *testing.T) {
	for _, seconds := range []int{0, 61} {
		c, elapsed := virtualClient()
		calls := 0
		c.post = func(context.Context, string, map[string]string, any, time.Duration, int64) (httpx.Response, error) {
			calls++
			body, _ := json.Marshal(map[string]any{"ok": false, "error_code": 429, "parameters": map[string]int{"retry_after": seconds}})
			return httpx.Response{Status: 429, Body: body}, nil
		}
		err := c.SendHTML(context.Background(), "https://example.test", "T", "1", "x")
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("lost 429: %v", err)
		}
		if seconds == 0 && calls != 4 {
			t.Fatalf("unbounded retries: %d", calls)
		}
		if seconds == 61 && (calls != 1 || *elapsed != 0) {
			t.Fatal("long flood wait was retried")
		}
	}
}

func TestDoesNotReplayAmbiguousSend(t *testing.T) {
	c, _ := virtualClient()
	calls := 0
	c.post = func(context.Context, string, map[string]string, any, time.Duration, int64) (httpx.Response, error) {
		calls++
		return httpx.Response{Status: 500, Body: []byte(`{"ok":false,"error_code":500}`)}, nil
	}
	if err := c.SendHTML(context.Background(), "https://example.test", "T", "1", "x"); !delivery.Uncertain(err) || calls != 1 {
		t.Fatalf("ambiguous send retried or not marked uncertain: calls=%d err=%v", calls, err)
	}
}

// 明确的拒绝（4xx、连不上）不算「不确定」，调用方可以放心改走别的途径。
func TestDefiniteFailuresAreNotUncertain(t *testing.T) {
	c, _ := virtualClient()
	c.post = func(context.Context, string, map[string]string, any, time.Duration, int64) (httpx.Response, error) {
		return httpx.Response{Status: 400, Body: []byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`)}, nil
	}
	if err := c.SendHTML(context.Background(), "https://example.test", "T", "1", "x"); err == nil || delivery.Uncertain(err) {
		t.Fatalf("400 marked uncertain: %v", err)
	}
	c.post = func(context.Context, string, map[string]string, any, time.Duration, int64) (httpx.Response, error) {
		return httpx.Response{}, &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	}
	if err := c.SendHTML(context.Background(), "https://example.test", "T", "1", "x"); err == nil || delivery.Uncertain(err) {
		t.Fatalf("dial failure marked uncertain: %v", err)
	}
	c.post = func(context.Context, string, map[string]string, any, time.Duration, int64) (httpx.Response, error) {
		return httpx.Response{}, &net.OpError{Op: "read", Err: errors.New("connection reset")}
	}
	if err := c.SendHTML(context.Background(), "https://example.test", "T", "1", "x"); !delivery.Uncertain(err) {
		t.Fatalf("read failure not marked uncertain: %v", err)
	}
}

func TestBoundedAdmission(t *testing.T) {
	c := New()
	release := make(chan struct{})
	started := make(chan struct{}, 32)
	c.post = func(context.Context, string, map[string]string, any, time.Duration, int64) (httpx.Response, error) {
		started <- struct{}{}
		<-release
		return httpx.Response{Status: 200, Body: []byte(`{"ok":true}`)}, nil
	}
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			if err := c.SendHTML(context.Background(), "https://example.test", string(rune('a'+i)), "1", "x"); err != nil {
				t.Error(err)
			}
		})
	}
	for range 32 {
		<-started
	}
	if err := c.SendHTML(context.Background(), "https://example.test", "extra", "1", "x"); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("overload not rejected: %v", err)
	}
	close(release)
	wg.Wait()
}

func TestCancelWhileRateLimited(t *testing.T) {
	c := New()
	c.post = func(context.Context, string, map[string]string, any, time.Duration, int64) (httpx.Response, error) {
		return httpx.Response{Status: 200, Body: []byte(`{"ok":true}`)}, nil
	}
	_ = c.SendHTML(context.Background(), "https://example.test", "T", "1", "x")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.SendHTML(ctx, "https://example.test", "T", "1", "x"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation lost: %v", err)
	}
}
