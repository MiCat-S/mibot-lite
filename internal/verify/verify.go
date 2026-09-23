// Package verify 用真实账号把各个命令实际跑一遍。
//
// 单元测试覆盖了解析和纯逻辑；覆盖不到的是：命令在真实连接上运行时，
// 是否真的改写了一条真实的消息。这里把每个命令发到收藏夹，交给分发器，
// 等处理函数改写消息，检查改写的结果，然后把消息删掉。
//
// 这里自己分发消息，而不是等更新推送，因为进程收不到自己操作产生的
// 更新：Telegram 把它放在 RPC 结果里返回，而纯文本消息的结果只带一个
// id 和一个 pts——没有 peer，也没有文本。所以更新处理这一层是这里
// 唯一覆盖不到的；它之上的一切，从前缀路由到最终落到聊天里的那次编辑，
// 都是真实运行的。
//
// 这里只列只读命令。不会清空任何聊天记录，不会封禁任何人，
// 也不会重启服务。
package verify

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/sysinfo"
)

// Case 是一个命令，以及它的回复必须包含的内容。
type Case struct {
	// Command 是要发送的文本，不带前缀。
	Command string
	// Expect 是改写后的消息必须包含的子串。它不能出现在命令自己的进度
	// 提示里，否则命令还没真正做事，这个用例就已经通过了。
	Expect string
	// Network 标记要访问第三方的用例，这类用例失败时报为警告，
	// 而不算本程序的缺陷。
	Network bool
	// Wait 覆盖默认的单个用例超时时间。
	Wait time.Duration
}

// Cases 是默认的用例集：所有不改动任何东西就能给出回复的命令。
var Cases = []Case{
	{Command: "ping", Expect: "Pong"},
	{Command: "version", Expect: "MiBot Lite"},
	{Command: "memory", Expect: "RSS"},
	{Command: "status", Expect: "运行时间"},
	{Command: "sysinfo", Expect: "系统信息"},
	{Command: "help", Expect: "命令列表"},
	{Command: "help calc", Expect: "计算器"},
	{Command: "calc 2+2*5", Expect: "12"},
	{Command: "calc (10-3)*4", Expect: "28"},
	{Command: "update", Expect: "更新状态"},
	{Command: "acn status", Expect: "自动更新"},
	{Command: "yvlu config", Expect: "贴纸包"},
	{Command: "ai config list", Expect: "AI"},
	{Command: "sum config list", Expect: "配置"},
	{Command: "aban", Expect: "封禁管理"},
	{Command: "log debug", Expect: "当前日志级别"},
	{Command: "help log", Expect: "运行日志"},
	{Command: "help bf", Expect: "配置备份"},
	{Command: "alias", Expect: "别名"},
	{Command: "help save", Expect: "保存消息"},
	{Command: "help bin", Expect: "卡头"},
	{Command: "ids", Expect: "注册时间"},
	{Command: "dc", Expect: "数据中心"},
	{Command: "ip 8.8.8.8", Expect: "Google", Network: true, Wait: 30 * time.Second},
	{Command: "eatgif list", Expect: "头像动图", Network: true, Wait: 60 * time.Second},
	{Command: "whois example.com", Expect: "example.com", Network: true, Wait: 60 * time.Second},
	{Command: "rate BTC", Expect: "数据更新", Network: true, Wait: 90 * time.Second},
}

// Result 记录一个用例的运行结果。
type Result struct {
	Case    Case
	Passed  bool
	Skipped bool
	Detail  string
	Took    time.Duration
}

// Dispatch 把一条消息交给命令注册表，返回是否匹配到命令。
type Dispatch func(ctx context.Context, message *tg.Message) bool

// Run 逐个发送用例并报告结果。返回值是本程序自身造成的失败数，
// 第三方服务故障不计在内。
func Run(ctx context.Context, client *bot.Client, prefix string, out io.Writer, cases []Case, dispatch Dispatch) (int, error) {
	self := &tg.InputPeerSelf{}
	fmt.Fprintf(out, "verifying %d commands as %s, in Saved Messages\n\n", len(cases), bot.UserID(client.SelfID()))
	results := make([]Result, 0, len(cases))
	for _, item := range cases {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		result := run(ctx, client, self, prefix, item, dispatch)
		results = append(results, result)
		status := "FAIL"
		switch {
		case result.Passed:
			status = "pass"
		case result.Skipped:
			status = "warn"
		}
		fmt.Fprintf(out, "%-4s  %-22s  %6dms  %s\n", status, prefix+item.Command, result.Took.Milliseconds(), result.Detail)
	}

	passed, failed, warned := 0, 0, 0
	for _, result := range results {
		switch {
		case result.Passed:
			passed++
		case result.Skipped:
			warned++
		default:
			failed++
		}
	}
	memory := sysinfo.Read()
	fmt.Fprintf(out, "\n%d passed, %d failed, %d third-party warnings\n", passed, failed, warned)
	fmt.Fprintf(out, "after the run: RSS %.1f MB, heap %.1f MB, %d goroutines\n",
		sysinfo.Megabytes(memory.RSS), sysinfo.Megabytes(memory.HeapAlloc), memory.Goroutines)
	return failed, nil
}

// run 发送一个命令，等处理函数把它改写。
func run(ctx context.Context, client *bot.Client, peer tg.InputPeerClass, prefix string, item Case, dispatch Dispatch) Result {
	wait := item.Wait
	if wait <= 0 {
		wait = 30 * time.Second
	}
	started := time.Now()
	text := prefix + item.Command
	id, err := client.SendHTML(ctx, peer, bot.Escape(text), bot.SendOptions{})
	if err != nil {
		return Result{Case: item, Detail: "send failed: " + err.Error(), Took: time.Since(started)}
	}
	// 无论结果如何都删掉这条消息，这样验证跑完后，收藏夹和跑之前一样。
	defer func() {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_ = client.Delete(removeCtx, peer, []int{id})
	}()

	// 把消息完整地读回来：发送返回的结果没法拿去分发；这也顺便初步检查了
	// 账号能不能看到自己刚写的内容。
	sent, err := client.GetMessages(ctx, peer, []int{id})
	if err != nil || len(sent) == 0 {
		return Result{Case: item, Detail: "could not read the sent message back", Took: time.Since(started)}
	}
	if !dispatch(ctx, sent[0]) {
		return Result{Case: item, Detail: "no command matched " + text, Took: time.Since(started)}
	}

	// 要等的是答案，而不只是消息有变化：好几个命令会先改一次，说明正在
	// 处理，再改一次给出结果。如果取第一次编辑，就会在进度提示上误判通过；
	// 更糟的是，命令还在往这条消息里写，消息却已经被删掉了。
	deadline := time.Now().Add(wait)
	answer, matched := "", false
	for time.Now().Before(deadline) {
		if err := sleep(ctx, 700*time.Millisecond); err != nil {
			return Result{Case: item, Detail: "cancelled", Took: time.Since(started)}
		}
		found, err := client.GetMessages(ctx, peer, []int{id})
		if err != nil || len(found) == 0 {
			continue
		}
		current := found[0].Message
		if current == text || strings.TrimSpace(current) == "" {
			continue
		}
		answer = current
		if strings.Contains(current, item.Expect) {
			matched = true
			break
		}
	}
	took := time.Since(started)
	if answer == "" {
		return Result{Case: item, Detail: "no answer within " + wait.String(), Took: took}
	}
	snippet := strings.ReplaceAll(firstLine(answer), "\n", " ")
	if matched {
		return Result{Case: item, Passed: true, Detail: snippet, Took: took}
	}
	// 第三方服务挂了不是本程序的缺陷，验证运行必须能分清是哪一种。
	if item.Network {
		return Result{Case: item, Skipped: true, Detail: "third party: " + snippet, Took: took}
	}
	return Result{Case: item, Detail: fmt.Sprintf("expected %q, got %q", item.Expect, snippet), Took: took}
}

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	if len([]rune(text)) > 60 {
		text = string([]rune(text)[:60]) + "…"
	}
	return text
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
