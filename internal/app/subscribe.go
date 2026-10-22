package app

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// subscribeGuard 处理重连后订阅更新失败的情况。
//
// gotd 每次连上都调一次 Self()，借这个需要授权的请求让 Telegram 往这条连接推更新。
// 这一步失败时 gotd 只记一条警告就算了：连接照样 ping 得通、看着在线，却收不到任何
// 更新，要等更新引擎 15 分钟空闲轮询那次 getDifference 才重新订阅上。这里把错误交回
// gotd，让它断开重连、重新订阅。
//
// gotd 每次连上就把重连退避清零，Self() 要是一直失败，原样交回错误就成了每 100 毫秒
// 重连一次。所以交回之前先自己等一会儿：连续失败越多等得越久，最长一分钟；限流要求
// 等更久就等够。Self() 成功一次计数就清零。Self() 经过 Retrier，60 秒以内的限流和
// 服务端内部错误到这里之前已经重试过了。
type subscribeGuard struct {
	logger   *slog.Logger
	failures atomic.Int32
	// sleep 默认是 sleepFor，测试里换掉。
	sleep func(ctx context.Context, d time.Duration) error
}

// onError 实现 telegram.Options.OnSelfError。返回错误 gotd 就断开重连，返回 nil 就留着这条连接。
func (g *subscribeGuard) onError(ctx context.Context, err error) error {
	// 没授权是登录之前的正常情况，gotd 自己会把它当成永久错误；连接已经在关就不用管了。
	if auth.IsUnauthorized(err) || ctx.Err() != nil {
		return nil
	}
	failures := int(g.failures.Add(1))
	delay := subscribeRetryDelay(failures)
	if wait, flooded := tgerr.AsFloodWait(err); flooded && wait > delay {
		delay = wait
	}
	g.logger.Warn("updates.subscribe_failed", slog.String("error", err.Error()),
		slog.Int("failures", failures), slog.Duration("reconnect_in", delay))
	sleep := g.sleep
	if sleep == nil {
		sleep = sleepFor
	}
	if sleep(ctx, delay) != nil {
		return nil
	}
	return err
}

// onSuccess 实现 telegram.Options.OnSelfSuccess。
func (g *subscribeGuard) onSuccess(*tg.User) { g.failures.Store(0) }

// subscribeRetryDelay 是连续第 n 次失败后、重连之前等多久：2 秒起，每次翻倍，最长一分钟。
func subscribeRetryDelay(n int) time.Duration {
	delay := 2 * time.Second
	for range n - 1 {
		if delay >= time.Minute {
			break
		}
		delay *= 2
	}
	return min(delay, time.Minute)
}
