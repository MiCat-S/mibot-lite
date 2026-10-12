package bot

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// flaky 前 failures 次回 FLOOD_WAIT_<seconds>（seconds 为负时回服务端错误），之后成功。
type flaky struct {
	failures, seconds, calls int
	other                    string
}

func (f *flaky) Invoke(context.Context, bin.Encoder, bin.Decoder) error {
	f.calls++
	if f.calls <= f.failures {
		switch {
		case f.other != "":
			return tgerr.New(400, f.other)
		case f.seconds < 0:
			return tgerr.New(500, "RPC_CALL_FAIL")
		}
		return tgerr.New(420, "FLOOD_WAIT_"+strconv.Itoa(f.seconds))
	}
	return nil
}

func TestRetrier(t *testing.T) {
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
		{"服务端错误等 2 秒重试", 2, -1, 3, false, 6 * time.Second},
	} {
		var slept time.Duration
		now := time.Now()
		waiter := Retrier{MaxWait: 30 * time.Second, Attempts: 3,
			sleep: func(_ context.Context, d time.Duration) error { slept += d; now = now.Add(d); return nil },
			now:   func() time.Time { return now }, jitter: func(time.Duration) time.Duration { return 0 }}
		fake := &flaky{failures: c.failures, seconds: c.seconds}
		err := waiter.Handle(fake)(context.Background(), &tg.MessagesGetMessagesRequest{}, nil)
		if (err != nil) != c.wantErr || fake.calls != c.wantCalls || slept != c.wantSlept {
			t.Errorf("%s：err=%v 调用 %d 次（应 %d） 等了 %v（应 %v）", c.name, err, fake.calls, c.wantCalls, slept, c.wantSlept)
		}
	}
	fake := &flaky{failures: 1, other: "USER_NOT_PARTICIPANT"}
	waiter := Retrier{MaxWait: 30 * time.Second, Attempts: 3, sleep: func(context.Context, time.Duration) error { return nil }}
	if err := waiter.Handle(fake)(context.Background(), &tg.MessagesGetMessagesRequest{}, nil); err == nil || fake.calls != 1 {
		t.Errorf("普通错误不该重试：err=%v 调用 %d 次", err, fake.calls)
	}
	// 更新引擎补抓群组更新的请求失败了就原样交回：它自己会再补，这里一等就卡住那个群组。
	for _, fake := range []*flaky{{failures: 1, seconds: 3}, {failures: 1, seconds: -1}} {
		slept := time.Duration(0)
		waiter := Retrier{MaxWait: 30 * time.Second, Attempts: 3, sleep: func(_ context.Context, d time.Duration) error { slept += d; return nil }}
		if err := waiter.Handle(fake)(context.Background(), &tg.UpdatesGetChannelDifferenceRequest{}, nil); err == nil || fake.calls != 1 || slept != 0 {
			t.Errorf("getChannelDifference 不该重试：err=%v 调用 %d 次 等了 %v", err, fake.calls, slept)
		}
	}
	if got := methodName(&tg.ChannelsGetMessagesRequest{}); got != "channels.getMessages" {
		t.Errorf("methodName = %q", got)
	}
}

func TestRetrierStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waiter := Retrier{MaxWait: 30 * time.Second, Attempts: 3}
	fake := &flaky{failures: 5, seconds: 1}
	start := time.Now()
	if err := waiter.Handle(fake)(ctx, &tg.MessagesGetMessagesRequest{}, nil); !errors.Is(err, context.Canceled) || fake.calls != 0 || time.Since(start) > time.Second {
		t.Errorf("取消后应立刻返回 context.Canceled：err=%v 调用 %d 次", err, fake.calls)
	}
}

func TestRetrierCancellationDuringSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	waiter := Retrier{MaxWait: time.Minute, Attempts: 3, sleep: func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }}
	fake := &flaky{failures: 2, seconds: 1}
	if err := waiter.Handle(fake)(ctx, &tg.MessagesGetMessagesRequest{}, nil); !errors.Is(err, context.Canceled) || fake.calls != 1 {
		t.Fatalf("lost sleep cancellation: %v calls=%d", err, fake.calls)
	}
}

func TestRetrierSharesMethodCooldown(t *testing.T) {
	now := time.Now()
	slept := time.Duration(0)
	waiter := Retrier{MaxWait: time.Minute, Attempts: 0, now: func() time.Time { return now }, sleep: func(_ context.Context, d time.Duration) error { slept += d; now = now.Add(d); return nil }}
	fake := &flaky{failures: 1, seconds: 2}
	invoke := waiter.Handle(fake)
	if err := invoke(context.Background(), &tg.MessagesGetMessagesRequest{}, nil); err == nil {
		t.Fatal("expected flood wait")
	}
	if err := invoke(context.Background(), &tg.ContactsResolveUsernameRequest{}, nil); err != nil || slept != 0 {
		t.Fatal("unrelated method was blocked")
	}
	if err := invoke(context.Background(), &tg.MessagesGetMessagesRequest{}, nil); err != nil || slept != 3*time.Second {
		t.Fatalf("same method bypassed cooldown: %v slept=%s", err, slept)
	}
}

func TestRetrierRetainsSendRandomID(t *testing.T) {
	request := &tg.MessagesSendMessageRequest{RandomID: 123, Message: "x", Peer: &tg.InputPeerSelf{}}
	calls := 0
	fake := telegram.InvokeFunc(func(_ context.Context, input bin.Encoder, _ bin.Decoder) error {
		calls++
		if input != request || input.(*tg.MessagesSendMessageRequest).RandomID != 123 {
			t.Fatal("send identity changed")
		}
		if calls == 1 {
			return tgerr.New(500, "RPC_CALL_FAIL")
		}
		return nil
	})
	waiter := Retrier{Attempts: 1, sleep: func(context.Context, time.Duration) error { return nil }}
	if err := waiter.Handle(fake)(context.Background(), request, nil); err != nil || calls != 2 {
		t.Fatalf("retry failed: %v", err)
	}
}
