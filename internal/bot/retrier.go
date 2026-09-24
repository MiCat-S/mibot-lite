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

// Retrier 是连接中间件，补上 MiBox 所用的 teleproto 对每个请求都做、gotd 不做的两种重试：
//
//   - FLOOD_WAIT：要求等的时间不超过 MaxWait 时等够再重试（teleproto 的 floodSleepThreshold，默认 60 秒）。
//   - Telegram 服务端内部错误（5xx、RPC_CALL_FAIL、RPC_MCGET_FAIL）：等 2 秒重试。
//
// 两种都最多重试 Attempts 次（teleproto 的 requestRetries，默认 5），之后把错误交给调用方。
// 这两种错误都表示请求没被执行，重试不会重复操作。从 MiBox 移植来的命令都默认有这层
// 保护，以前没有时，几秒的限流就会让整条命令失败。
type Retrier struct {
	Logger   *slog.Logger
	MaxWait  time.Duration
	Attempts int
	// sleep 默认是按 ctx 可取消地等待，测试里换掉。
	sleep func(ctx context.Context, d time.Duration) error
}

// serverFailure 是 teleproto 等 2 秒重试的那类错误。
func serverFailure(err error) bool {
	rpcErr, ok := tgerr.As(err)
	return ok && (rpcErr.Code >= 500 || rpcErr.IsOneOf("RPC_CALL_FAIL", "RPC_MCGET_FAIL"))
}

// Handle 实现 telegram.Middleware。
func (f Retrier) Handle(next tg.Invoker) telegram.InvokeFunc {
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
			if err == nil || attempt > f.Attempts {
				return err
			}
			var wait time.Duration
			if flood, flooded := tgerr.AsFloodWait(err); flooded {
				if flood > f.MaxWait {
					return err
				}
				// 多等一秒：Telegram 给的秒数是取整后的，掐着点重试常常又被限。
				wait = flood + time.Second
				f.log("rpc.flood_wait", input, flood, attempt)
			} else if serverFailure(err) {
				wait = 2 * time.Second
				f.log("rpc.server_error", input, wait, attempt)
			} else {
				return err
			}
			if sleep(ctx, wait) != nil {
				return err
			}
		}
	}
}

func (f Retrier) log(event string, input bin.Encoder, wait time.Duration, attempt int) {
	if f.Logger != nil {
		f.Logger.Info(event, slog.String("method", methodName(input)), slog.Duration("wait", wait), slog.Int("attempt", attempt))
	}
}

// methodName 是请求的 TL 名称，如 channels.getMessages，只用于日志。
func methodName(input bin.Encoder) string {
	if named, ok := input.(interface{ TypeName() string }); ok {
		return named.TypeName()
	}
	return "unknown"
}
