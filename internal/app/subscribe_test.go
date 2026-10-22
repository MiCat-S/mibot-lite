package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// newGuard 造一个不真等、只记下每次要等多久的 subscribeGuard。
func newGuard(waits *[]time.Duration) *subscribeGuard {
	return &subscribeGuard{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		sleep: func(_ context.Context, d time.Duration) error {
			*waits = append(*waits, d)
			return nil
		},
	}
}

// 订阅失败要把错误交回 gotd 让它重连，交回之前等的时间随连续失败加长；成功一次就从头算。
func TestSubscribeGuardBacksOffAndResets(t *testing.T) {
	var waits []time.Duration
	guard := newGuard(&waits)
	failure := tgerr.New(500, "RPC_CALL_FAIL")
	for range 3 {
		if err := guard.onError(context.Background(), failure); !errors.Is(err, failure) {
			t.Fatalf("onError = %v, want the subscription error back", err)
		}
	}
	guard.onSuccess(&tg.User{})
	_ = guard.onError(context.Background(), failure)
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 2 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("wait %d = %v, want %v", i, waits[i], want[i])
		}
	}
}

// 登录之前没授权是正常的，关闭中的连接也不用重连：两种都不等、不交回错误。
func TestSubscribeGuardIgnoresUnauthorizedAndClosing(t *testing.T) {
	var waits []time.Duration
	guard := newGuard(&waits)
	if err := guard.onError(context.Background(), tgerr.New(401, "AUTH_KEY_UNREGISTERED")); err != nil {
		t.Errorf("unauthorized: onError = %v, want nil", err)
	}
	closing, cancel := context.WithCancel(context.Background())
	cancel()
	if err := guard.onError(closing, errors.New("connection dead")); err != nil {
		t.Errorf("closing: onError = %v, want nil", err)
	}
	if len(waits) != 0 {
		t.Errorf("waited %v, want no wait", waits)
	}
}

// 限流要求等得比退避久，就等够限流的时间。
func TestSubscribeGuardHonoursFloodWait(t *testing.T) {
	var waits []time.Duration
	guard := newGuard(&waits)
	_ = guard.onError(context.Background(), tgerr.New(420, "FLOOD_WAIT_120"))
	if len(waits) != 1 || waits[0] != 120*time.Second {
		t.Errorf("waits = %v, want [2m0s]", waits)
	}
}

// 等的时候连接断了，就不再交回错误：gotd 已经在重连。
func TestSubscribeGuardStopsWhenWaitInterrupted(t *testing.T) {
	guard := &subscribeGuard{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		sleep:  func(context.Context, time.Duration) error { return context.Canceled },
	}
	if err := guard.onError(context.Background(), errors.New("timeout")); err != nil {
		t.Errorf("onError = %v, want nil", err)
	}
}

func TestSubscribeRetryDelayCapsAtOneMinute(t *testing.T) {
	for n, want := range map[int]time.Duration{1: 2 * time.Second, 5: 32 * time.Second, 6: time.Minute, 50: time.Minute} {
		if got := subscribeRetryDelay(n); got != want {
			t.Errorf("subscribeRetryDelay(%d) = %v, want %v", n, got, want)
		}
	}
}
