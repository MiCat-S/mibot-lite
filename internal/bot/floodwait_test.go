package bot

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// flaky 前 failures 次回 FLOOD_WAIT_<seconds>，之后成功。
type flaky struct {
	failures, seconds, calls int
}

func (f *flaky) Invoke(context.Context, bin.Encoder, bin.Decoder) error {
	f.calls++
	if f.calls <= f.failures {
		return tgerr.New(420, "FLOOD_WAIT_"+strconv.Itoa(f.seconds))
	}
	return nil
}

func TestFloodWaiter(t *testing.T) {
	for _, c := range []struct {
		name              string
		failures, seconds int
		wantCalls         int
		wantErr           bool
		wantSlept         time.Duration
	}{
		{"限流一次后成功", 1, 3, 2, false, 4 * time.Second},
		{"连续三次限流仍能成功", 3, 2, 4, false, 9 * time.Second},
		{"超过重试次数就放弃", 5, 2, 4, true, 9 * time.Second},
		{"要等太久的不重试", 1, 60, 1, true, 0},
		{"没限流就不等", 0, 0, 1, false, 0},
	} {
		var slept time.Duration
		waiter := FloodWaiter{MaxWait: 30 * time.Second, Attempts: 3,
			sleep: func(_ context.Context, d time.Duration) error { slept += d; return nil }}
		fake := &flaky{failures: c.failures, seconds: c.seconds}
		err := waiter.Handle(fake)(context.Background(), &tg.MessagesGetMessagesRequest{}, nil)
		if (err != nil) != c.wantErr || fake.calls != c.wantCalls || slept != c.wantSlept {
			t.Errorf("%s：err=%v 调用 %d 次（应 %d） 等了 %v（应 %v）", c.name, err, fake.calls, c.wantCalls, slept, c.wantSlept)
		}
	}
	if got := methodName(&tg.ChannelsGetMessagesRequest{}); got != "channels.getMessages" {
		t.Errorf("methodName = %q", got)
	}
}

func TestFloodWaiterStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waiter := FloodWaiter{MaxWait: 30 * time.Second, Attempts: 3}
	fake := &flaky{failures: 5, seconds: 1}
	start := time.Now()
	if err := waiter.Handle(fake)(ctx, &tg.MessagesGetMessagesRequest{}, nil); err == nil || fake.calls != 1 || time.Since(start) > time.Second {
		t.Errorf("取消后应立刻返回限流错误：err=%v 调用 %d 次", err, fake.calls)
	}
}
