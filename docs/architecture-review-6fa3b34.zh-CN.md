# mibot-lite 第三轮复查：6fa3b34

审查日期：2026-10-12（Asia/Taipei）。当前提交：`6fa3b341de1274db5d252d045ee2f82cf0032b85`，标题：`fix: 修复第二轮复查确认的五个边界问题`。对照提交：`0e59afa`。开始时工作区干净。

本轮检查该提交的生产代码与测试变更，沿上一轮 R1–R5 的调用链继续核查，并运行全仓 race 测试、vet 和临时离线边界探针。没有修改生产 Go 代码或正式测试；没有连接真实 Telegram、安装更新、执行 systemd 重启或删除真实消息。

**结论：R1、R3、R4 的直接触发路径已修复；R2 与 R5 的修复有效但尚未覆盖整个业务调用链。本轮确认 2 项 P2 问题，另有 1 项新引入的 P3 计数错误。**

## 修复状态（2026-10-12 更新）

三项均已在 `6fa3b34` 之后的提交中修复，每项都有断言正确结果的正式回归测试：

| 问题 | 修复 | 回归测试 |
| --- | --- | --- |
| R6 | 注册表新增共用的维护闸门：`Quiesce` 先关闸门（之后提交的命令、短后台、长任务、到点的延时任务和 `Track` 一律拒绝），再等已接受的工作结束；等不到就重新打开。闸门关上后工作只减不增，此时的 Busy==0 才作为重启依据。自动更新先在闸门开着时等空闲，再关闸门确认（最多 30 秒，不成功就打开后继续等），确认后才发进度、提交重启；提交失败立即 `Resume`，提交成功但 2 分钟后进程仍在也会 `Resume`。`Track` 改为返回 ok，维护或退出期间 `.sum`/`.acron` 定时执行跳过；keyword 动作、guestban、trace 的每次处理都 `Track`（keyword 的协程顺带兜住 panic）。Busy 仍保留作观测，注释说明它在闸门开着时不是一致快照 | `TestQuiesceClosesAdmission`、`TestQuiesceWaitsForAcceptedWork`、`TestQuiesceTimeoutReopens`、`update.TestQuiesceBlocksWorkAfterIdleCheck`、`keyword.TestKeywordActionCountsAsBusy` |
| R7 | 新增 `internal/delivery`，Bot API 与 MTProto 共用“结果不确定”的分类。`bot.Client` 发送消息时，4xx 类 RPC 错误（含限流）是明确失败；5xx、连接断开、读响应失败、途中取消、拿到结果却认不出消息 ID 都标为不确定（错误文字不变）。签到汇总对话失败时同样跳过不确定的那页，只发说明 | `TestDeliverDoesNotReplayUncertainMTProtoSend`、`delivery.TestUnknownKeepsMessageAndChain` |
| R8 | `After` 先判断生命周期再申请名额，只有真正加过的才回退 | `TestAfterRunsAndStopsOnAbort` 在拒绝后检查计数为 0 |

附录探针断言的是修复前的行为，`Track` 签名也已改变，保留为复现记录。

## 1. 上一轮五项问题的修复状态

| 第二轮问题 | 当前修复 | 本轮判断 |
| --- | --- | --- |
| R1：排队中的 `.da` 被取消，active 名额残留 | scheduler.admit 提供 release；execute 在取消检查之前登记 release defer；`.da` 核对 slot 后释放 | 直接问题已修复；正式测试覆盖取消后未执行的释放路径 |
| R2：自动更新忽略长任务 | Busy 统计四个池、Track 统计 sum/acron；安装后再次 waitIdle | 已修复“长任务实际运行而计数为零”的原触发条件；仍缺维护准入屏障及部分业务计数，见 R6 |
| R3：拒绝入队仍被记录为已执行 | Submit / SubmitMessage / Result；acron 使用 Queued/Rejected 并显示“已提交执行命令” | 直接问题已修复；sudo 也对命令池拒绝补了提示 |
| R4：延时删除使用 Background，退出后继续删除 | Registry.After、timer 上限、延时任务池；keyword/aban 从服务生命周期派生删除请求 | 延时删除的直接问题已修复；After 拒绝路径计数错误见 R8 |
| R5：Bot API 分页部分成功后整份回退 | 返回成功页数、UncertainError、remaining 跳过已发送及未知页面 | Bot API 到下一途径的回退已改善；下一途径 MTProto 的未知结果仍被重发，见 R7 |

本轮现有测试通过：TestCanceledBeforeRunReleasesSlot、TestSchedulerReleasesCanceledQueuedWork、TestSubmitReportsRejectionOnFullQueue、TestBusyCountsLongTasksAndTracked、TestAfterRunsAndStopsOnAbort、TestAutoUpdateWaitsForLongTasks、TestDeliverDoesNotResendPushedPages 等。不能把下面的剩余缺口描述成整项修复完全无效。

## 2. 仍需处理的问题

### R6 — [P2] 自动更新仍没有可靠的“停止接入并等待空闲”边界

主要位置：`internal/commands/update/auto.go:248–257`。相关位置：`internal/command/command.go:302–303`、`internal/commands/keyword/trigger.go:91,163–170`。

目前 autoInstall 在 waitIdle 返回后继续发进度 RPC，然后调用 Restarter.Schedule。waitIdle 只读一次 Busy==0 就返回，期间没有改变任务准入状态。新的 `.da`、普通命令或定时任务可以在“观察到空闲”之后、重启真正提交之前开始。进度 RPC 可能发生限流或网络等待，这个窗口不只是几条本地指令的时间。于是上一轮要求保护的长任务仍可能被自动重启取消。

新增 Busy 也不是一个一致的活动快照：四个池分别加锁读取，Track 另行原子读取。短后台任务把命令交给普通池再结束时，Busy 可以先读到旧的命令池 0，后读到新的短后台池 0，最终得出 0，而实际存在未完成命令。这个计数适合一般指标观测，不足以作为严格的维护准入条件。

此外，keyword.offer 仍使用自己的 `go work()` 与四个 slots，没有进入注册表池，也没有 Track。已进入关键词回复/处置 RPC 时 Busy 仍然为零。延时删除已经迁移，并不等于整条关键词动作都完成了活动跟踪。

本轮分别验证了三个方面：

```text
waitIdle=true, new long task accepted=true, Busy before restart=1
handoff snapshot model: observed Busy=0, current Busy=1
keyword reply RPC active, Registry.Busy=0
```

证据边界：第一个探针调用真实 waitIdle 和 LongRunning，验证等待结束后仍开放准入；未调用真实重启。第二个是白盒状态交接模型，在锁下模拟实际可达的“短后台 -> 命令”状态变化，证明独立读取可能漏计；不声称测到了生产环境发生率。第三个使用真正的 keyword.Register / App.OfferForeign，让模拟回复 RPC 阻塞，直接检查 Busy 的漏计。

建议：引入共享的维护准入状态，原子地停止新的业务提交/Track/到期任务接入，再等待已接纳工作结束，最后提交自动重启。若延期或提交失败，应恢复准入。统一登记需要保护的业务执行；长期常驻的调度器循环本身不计入活动。可以保留 Busy 作为指标，但安全决策应使用维护屏障及一致的活动登记。只在 Schedule 之前再多调用一次 Busy 仍然留下检查与动作之间的窗口。

正式测试应覆盖：空闲确认后尝试启动长任务、后台向命令交接、关键词动作执行中申请维护、维护失败恢复准入。

### R7 — [P2] MTProto 汇总发送结果未知时，下一次回退仍以新 RandomID 重发

位置：`internal/commands/checkin/sign.go:209–214`；相关函数 sign.go:252–266。

Bot API 的 UncertainError 已进入 remaining，但 LogChat 的 MTProto 路径在任何 sendPages 错误后仍固定调用 `remaining(pages, sent, false)`。sendPages 的失败页仅返回 error，没有结果未知分类。

如果 Telegram 接受了 MTProto sendMessage，但响应丢失，sent 只统计前面拿到明确结果的页。当前失败页被当成“确定没发出”，然后回退到命令消息或收藏夹。再次调用 SendHTML 会创建新的 RandomID，服务器无法把两次业务发送关联为同一条；当 LogChat 是 me、定时签到的最终回退也是 me 时，同账号同聊天可以收到重复页面。

模拟服务端接受第一条请求后返回 io.ErrUnexpectedEOF，再让后续调用成功。deliver 返回 nil，实际模拟记录如下：

```text
MTProto response lost: sends=2, identical texts=true, different RandomID=true, delivery error=<nil>
```

这不否定 MTProto 中间件保持 RandomID 的正确性：中间件重试的是同一请求对象；这里是业务回退重新构造了发送请求，已经换了 RandomID。

建议：把投递分类放到覆盖所有途径的业务/Telegram 适配层，而不仅是 Bot API 包。明确区分“发送前失败”“服务端明确拒绝”“已确认送达”“结果未知”。MTProto 失败页未知时同样不默认重新发送；如果要在同账号、同目的地可靠重试，应保留原请求的 RandomID/发送标识。跨目的地回退的语义应另行决定，并把未知页的情况显示给用户。

正式测试应新增 LogChat=me、第一条 MTProto 请求已接受但读响应失败的端到端离线场景，断言不会使用新的 RandomID 重发同页。

### R8 — [P3] 退出后调用 After 会减掉未增加的计时器名额

位置：`internal/command/command.go:263–264`。

判断式 `r.life.Err() != nil || r.timers.Add(1) > MaxTimers` 使用短路求值。生命周期已取消时，右侧 Add(1) 根本不会执行，但错误分支仍无条件 Add(-1)。因此每个在退出后被拒绝的 After 调用都会额外减一。

直接复现：

```text
aborted After: accepted=false, timers before=0, after=-1
```

该生命周期不会恢复，所以本轮没有证据表明它能在正常运行时绕过 MaxTimers 上限；主要影响是计数不变量与退出后的统计/测试正确性，故按 P3 列出，不夸大为活跃期无界计时器问题。

建议：把生命周期检查与名额申请分开，只有确实执行过 Add(1) 的失败路径才回退 Add(-1)。扩充现有 After 测试，在退出后拒绝新任务之后再次检查计数，而不仅在拒绝调用之前检查为零。

## 3. 验证与审查边界

```text
go test -race ./...          通过
go vet ./...                通过
临时 overlay 探针           5 个测试均复现上述现象
```

四个 overlay 包中共五个测试：R6 有准入窗口、交接快照模型、实际关键词漏计三个探针；R7、R8 各一个。它们通过 Go -overlay 从临时目录注入，未向源码目录添加正式测试。探针使用 -count=1 强制执行。PASS 表示断言确认了缺口；修复后的正式回归测试应断言相反的正确行为。

本轮仅新增本 Markdown，并给前两轮文档加入当前复查链接。生产 Go 代码、配置、账号状态和正式测试保持原样；未提交或发布审查文档。

没有发现证据需要推翻现有共享 HTTP 连接池、MTProto 按方法冷却、状态定期写盘或实例锁顺序的既有修复。本轮聚焦新增修复的集成边界，没有进行生产性能基准或真实 Telegram API 兼容性验收。缓存总量、媒体资源预算、cron 并发规模等第一轮建议仍应按实际负载逐步推进。

## 附录：完整离线探针与重放脚本

从仓库根目录执行以下脚本，可提取本报告中的探针代码，在临时目录生成 Go overlay 并运行。脚本只在系统临时目录写入测试文件，Go 构建/测试缓存照常使用。

```bash
python3 <<'PY'
from pathlib import Path
import json, re, subprocess, tempfile
repo = Path.cwd()
report = repo / 'docs/architecture-review-6fa3b34.zh-CN.md'
probes = re.findall(r'<!-- review-probe-package: ([^ ]+) -->\n\n```go\n(.*?)\n```', report.read_text(), re.S)
if len(probes) != 4:
    raise SystemExit('expected four probe packages')
output = Path(tempfile.mkdtemp(prefix='mibot-lite-review-6fa3b34-'))
replace, packages = {}, []
for package, source in probes:
    target = output / (package.replace('/', '_') + '_test.go')
    target.write_text(source + '\n')
    replace[str(repo / package / 'review_overlay_test.go')] = str(target)
    packages.append('./' + package)
overlay = output / 'overlay.json'
overlay.write_text(json.dumps({'Replace': replace}, indent=2))
print('overlay:', overlay, flush=True)
subprocess.run(['go', 'test', '-race', '-overlay=' + str(overlay), *packages,
                '-run', '^TestReviewProbe', '-count=1', '-v'], check=True)
PY
```

### internal/commands/update

<!-- review-probe-package: internal/commands/update -->

```go
package update

import (
	"context"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"log/slog"
	"testing"
	"time"
)

func TestReviewProbeNewWorkAcceptedAfterIdleCheck(t *testing.T) {
	r := command.New(nil, slog.New(slog.DiscardHandler))
	defer r.Abort()
	idle := waitIdle(r.Lifecycle(), r.Busy, time.Second)
	started := make(chan struct{})
	accepted := r.LongRunning(r.Lifecycle(), func(ctx context.Context) { close(started); <-ctx.Done() }, nil)
	if accepted {
		<-started
	}
	t.Logf("waitIdle=%v, new long task accepted=%v, Busy before restart=%d", idle, accepted, r.Busy())
	if !idle || !accepted || r.Busy() != 1 {
		t.Fatal("did not reproduce admission window")
	}
	r.Abort()
	if !r.Wait(time.Second) {
		t.Fatal("task did not stop")
	}
}
```

### internal/command

<!-- review-probe-package: internal/command -->

```go
package command

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestReviewProbeAfterRejectedCallDecrementsUnreservedTimer(t *testing.T) {
	r := registry()
	r.Abort()
	before := r.timers.Load()
	accepted := r.After(time.Hour, func(context.Context) {})
	t.Logf("aborted After: accepted=%v, timers before=%d, after=%d", accepted, before, r.timers.Load())
	if accepted || r.timers.Load() != -1 {
		t.Fatal("underflow not reproduced")
	}
}

// 模拟实际存在的交接：短后台代发提交命令，再结束自身。整个过程总工作量保持 1。
func TestReviewProbeBusyCanObserveFalseIdleDuringHandoff(t *testing.T) {
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	r := registry()
	defer r.Abort()
	r.tasks.mu.Lock()
	r.tasks.running = 1
	result := make(chan int, 1)
	go func() { result <- r.Busy() }()
	runtime.Gosched() // Busy 已读到命令池 0，然后阻塞在短后台池 mutex。
	r.pool.mu.Lock()
	r.pool.running = 1
	r.pool.mu.Unlock()
	r.tasks.running = 0
	r.tasks.mu.Unlock()
	observed := <-result
	current := r.Busy()
	t.Logf("handoff snapshot model: observed Busy=%d, current Busy=%d", observed, current)
	if observed != 0 || current != 1 {
		t.Fatal("false idle snapshot not reproduced")
	}
	r.pool.mu.Lock()
	r.pool.running = 0
	r.pool.mu.Unlock()
}
```

### internal/commands/keyword

<!-- review-probe-package: internal/commands/keyword -->

```go
package keyword

import (
	"context"
	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/store"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewProbeKeywordReplyNotCountedByBusy(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	r := command.New(nil, logger)
	defer r.Abort()
	a := &app.App{Root: t.TempDir(), Registry: r, Logger: logger}
	Register(a)
	saved := store.New(filepath.Join(a.DataDir(), "keyword.json"), defaults)
	if err := saved.Update(func(doc *document) error {
		doc.Tasks = append(doc.Tasks, task{ID: 1, ChatID: "200", Key: "ad", Response: "reply", Include: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	fake := telegram.InvokeFunc(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		if _, ok := input.(*tg.MessagesSendMessageRequest); !ok {
			t.Errorf("unexpected RPC %T", input)
			return nil
		}
		close(started)
		defer close(finished)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
		}
		output.(*tg.UpdatesBox).Updates = &tg.UpdateShortSentMessage{ID: 9}
		return nil
	})
	peers := bot.NewPeerCache()
	peers.SetSelf(100)
	target := &tg.User{ID: 200}
	target.SetAccessHash(1)
	peers.RememberUsers([]tg.UserClass{target})
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 100}, logger)
	a.OfferForeign(r.Lifecycle(), client, &bot.Message{ID: 1, ChatID: "200", Peer: &tg.PeerUser{UserID: 200}, Sender: &tg.PeerUser{UserID: 200}, Text: "ad"})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("keyword reply did not start")
	}
	busy := r.Busy()
	t.Logf("keyword reply RPC active, Registry.Busy=%d", busy)
	close(release)
	<-finished
	if busy != 0 {
		t.Fatal("probe did not reproduce missing activity tracking")
	}
}
```

### internal/commands/checkin

<!-- review-probe-package: internal/commands/checkin -->

```go
package checkin

import (
	"context"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"io"
	"log/slog"
	"testing"
)

func TestReviewProbeMTProtoFallbackReplaysUnknownSend(t *testing.T) {
	var texts []string
	var ids []int64
	fake := telegram.InvokeFunc(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
		request := input.(*tg.MessagesSendMessageRequest)
		texts = append(texts, request.Message)
		ids = append(ids, request.RandomID)
		// 模拟服务端接受第一条，响应却在读取时丢失。
		if len(texts) == 1 {
			return io.ErrUnexpectedEOF
		}
		output.(*tg.UpdatesBox).Updates = &tg.UpdateShortSentMessage{ID: len(texts)}
		return nil
	})
	peers := bot.NewPeerCache()
	peers.SetSelf(100)
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 100}, slog.New(slog.DiscardHandler))
	err := deliver(context.Background(), client, config{LogChat: "me"}, "test summary", nil)
	t.Logf("MTProto response lost: sends=%d, identical texts=%v, different RandomID=%v, delivery error=%v", len(texts), len(texts) == 2 && texts[0] == texts[1], len(ids) == 2 && ids[0] != ids[1], err)
	if err != nil || len(texts) != 2 || texts[0] != texts[1] || ids[0] == ids[1] {
		t.Fatal("unknown send was not replayed as expected")
	}
}
```
