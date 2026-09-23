// Package verify exercises the commands against the live account.
//
// The unit tests cover the parsing and the pure logic; what they cannot
// cover is whether a command, run against a real connection, actually
// edits a real message. This sends each command to Saved Messages, hands
// it to the dispatcher, waits for the handler to rewrite it, checks what
// came back and deletes it.
//
// It dispatches the message itself rather than waiting for an update,
// because a process cannot receive an update for its own action: Telegram
// reports it in the RPC result, and for plain text that result carries
// only an id and a pts — no peer, no text. So the update plumbing is the
// one layer this does not cover; everything above it, from prefix routing
// to the edit that lands in the chat, is the real thing.
//
// Only read-only commands are listed. Nothing here deletes a chat's
// history, bans anyone or restarts the service.
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

// Case is one command and what its answer must contain.
type Case struct {
	// Command is the text sent, without the prefix.
	Command string
	// Expect is a substring the edited message must contain. It must not
	// appear in the command's own progress line, or the case passes
	// before the command has done anything.
	Expect string
	// Network marks a case that talks to a third party, so a failure is
	// reported as a warning rather than a defect in this program.
	Network bool
	// Wait overrides the default per-case timeout.
	Wait time.Duration
}

// Cases is the default set: every command that answers without changing
// anything.
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

// Result is what one case did.
type Result struct {
	Case    Case
	Passed  bool
	Skipped bool
	Detail  string
	Took    time.Duration
}

// Dispatch offers a message to the command registry and reports whether a
// command matched.
type Dispatch func(ctx context.Context, message *tg.Message) bool

// Run sends every case and reports what happened. It returns the number of
// failures that are this program's own, ignoring third-party outages.
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

// run sends one command and waits for the handler to rewrite it.
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
	// The message is removed whatever happens, so a verification run
	// leaves Saved Messages as it found it.
	defer func() {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_ = client.Delete(removeCtx, peer, []int{id})
	}()

	// Read the message back in full: what the send returned cannot be
	// dispatched, and this is also a first check that the account can see
	// what it just wrote.
	sent, err := client.GetMessages(ctx, peer, []int{id})
	if err != nil || len(sent) == 0 {
		return Result{Case: item, Detail: "could not read the sent message back", Took: time.Since(started)}
	}
	if !dispatch(ctx, sent[0]) {
		return Result{Case: item, Detail: "no command matched " + text, Took: time.Since(started)}
	}

	// Wait for the answer, not merely for a change: several commands edit
	// once to say they are working and again with the result. Taking the
	// first edit would pass on the progress line and, worse, delete the
	// message out from under the command still writing to it.
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
	// A third party being down is not a defect in this program, and a
	// verification run must be able to say which is which.
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
