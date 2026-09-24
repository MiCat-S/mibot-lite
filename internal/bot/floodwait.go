package bot

import (
	"context"
	"log/slog"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// FloodWaiter 是处理 FLOOD_WAIT 的连接中间件：Telegram 要求等的时间不超过 maxWait 时，
// 等够再重试，最多 attempts 次；等得更久就把错误交给调用方。
//
// 被限流的请求 Telegram 并没有执行，所以重试不会重复操作。放在连接层，是因为限流
// 可能落在任何一次请求上——读被回复的消息、改进度提示、查成员——各个命令不可能
// 在每一处都自己处理，以前没处理到的地方会让整条命令直接失败。
type FloodWaiter struct {
	Logger   *slog.Logger
	MaxWait  time.Duration
	Attempts int
	// sleep 默认是按 ctx 可取消地等待，测试里换掉。
	sleep func(ctx context.Context, d time.Duration) error
}

// Handle 实现 telegram.Middleware。
func (f FloodWaiter) Handle(next tg.Invoker) telegram.InvokeFunc {
	sleep := f.sleep
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
	}
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		for attempt := 1; ; attempt++ {
			err := next.Invoke(ctx, input, output)
			wait, flooded := tgerr.AsFloodWait(err)
			if !flooded || attempt > f.Attempts || wait > f.MaxWait {
				return err
			}
			if f.Logger != nil {
				f.Logger.Info("rpc.flood_wait", slog.String("method", methodName(input)),
					slog.Duration("wait", wait), slog.Int("attempt", attempt))
			}
			// 多等一秒：Telegram 给的秒数是取整后的，掐着点重试常常又被限。
			if sleep(ctx, wait+time.Second) != nil {
				return err
			}
		}
	}
}

// methodName 是请求的 TL 名称，如 channels.getMessages，只用于日志。
func methodName(input bin.Encoder) string {
	if named, ok := input.(interface{ TypeName() string }); ok {
		return named.TypeName()
	}
	return "unknown"
}
