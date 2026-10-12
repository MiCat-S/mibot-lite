package bot

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// Retrier 是连接中间件，补上 MiBox 所用的 teleproto 对每个请求都做、gotd 不做的两种重试：
//
//   - FLOOD_WAIT：要求等的时间不超过 MaxWait 时等够再重试（teleproto 的 floodSleepThreshold，默认 60 秒）。
//   - Telegram 服务端内部错误（5xx、RPC_CALL_FAIL、RPC_MCGET_FAIL）：采用有上限的指数退避和抖动。
//
// 两种都最多重试 Attempts 次（teleproto 的 requestRetries，默认 5），之后把错误交给调用方。
// FLOOD_WAIT 会让同方法的并发调用共享冷却时间。发送请求重试时保留 RandomID；
// 5xx 不保证任意写操作都幂等，新增写方法时必须单独评估重试语义。
//
// updates.getChannelDifference 例外，见 selfRecovering。
type Retrier struct {
	Logger   *slog.Logger
	MaxWait  time.Duration
	Attempts int
	// sleep 默认是按 ctx 可取消地等待，测试里换掉。
	sleep  func(ctx context.Context, d time.Duration) error
	now    func() time.Time
	jitter func(time.Duration) time.Duration
}

// serverFailure 是 teleproto 等 2 秒重试的那类错误。
func serverFailure(err error) bool {
	rpcErr, ok := tgerr.As(err)
	return ok && (rpcErr.Code >= 500 || rpcErr.IsOneOf("RPC_CALL_FAIL", "RPC_MCGET_FAIL"))
}

// selfRecovering 判断请求是不是 gotd 更新引擎自己会补救的那种，这种请求原样交出失败，不重试。
//
// 更新引擎给每个群组开一个协程补抓漏掉的更新（updates.getChannelDifference），失败了它会
// 在下一次发现缺口或空闲轮询时再补，不需要这里重试；而这里一重试，那个协程就被卡住：
// 服务端错误要等 5 次 × 2 秒，限流更是每次等几秒到一分钟。协程卡住时它那个群组只有
// 10 格缓冲，塞满后会挡住所有对话的更新，收藏夹里的命令也跟着没反应。2026-09-27
// Telegram 连续十个小时对部分群组回 PERSISTENT_TIMESTAMP_OUTDATED，那天记了 1230 次这样的重试，
// 整个账号时不时卡住。
func selfRecovering(input bin.Encoder) bool {
	_, ok := input.(*tg.UpdatesGetChannelDifferenceRequest)
	return ok
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
	now := f.now
	if now == nil {
		now = time.Now
	}
	jitter := f.jitter
	if jitter == nil {
		jitter = func(d time.Duration) time.Duration { return time.Duration(rand.Int64N(int64(d/4) + 1)) }
	}
	var mu sync.Mutex
	type cooldown struct {
		until time.Time
		err   error
	}
	blocked := make(map[string]cooldown)
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if selfRecovering(input) {
			return next.Invoke(ctx, input, output)
		}
		method := methodName(input)
		serverErrors := 0
		for attempt := 0; ; attempt++ {
			// 睡醒后要再查一遍：等的这段时间里别的调用可能又延长了冷却。
			// 网络调用期间绝不持有 mu。
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				mu.Lock()
				entry := blocked[method]
				wait := entry.until.Sub(now())
				if wait <= 0 {
					delete(blocked, method)
				}
				mu.Unlock()
				if wait <= 0 {
					break
				}
				if wait > f.MaxWait+time.Second {
					return entry.err
				}
				if err := sleep(ctx, wait); err != nil {
					return err
				}
			}
			err := next.Invoke(ctx, input, output)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == nil {
				return nil
			}
			if flood, flooded := tgerr.AsFloodWait(err); flooded {
				wait := flood + time.Second
				mu.Lock()
				at := now()
				for name, entry := range blocked {
					if !entry.until.After(at) {
						delete(blocked, name)
					}
				}
				until := at.Add(wait)
				if until.After(blocked[method].until) {
					blocked[method] = cooldown{until: until, err: err}
				}
				mu.Unlock()
				if attempt >= f.Attempts || flood > f.MaxWait {
					return err
				}
				f.log("rpc.flood_wait", input, wait, attempt+1)
				continue
			}
			if !serverFailure(err) || attempt >= f.Attempts {
				return err
			}
			// 依次等 2、4、8、16 秒，再加最多 25% 的随机抖动，免得并发失败的请求同时重试。
			delay := 2 * time.Second << min(serverErrors, 3)
			serverErrors++
			wait := delay + jitter(delay)
			f.log("rpc.server_error", input, wait, attempt+1)
			if err := sleep(ctx, wait); err != nil {
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
