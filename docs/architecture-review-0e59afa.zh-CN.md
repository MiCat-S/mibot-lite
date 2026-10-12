# mibot-lite 第二轮架构与并发复查

审查日期：2026-10-12（Asia/Taipei）。审查版本：`0e59afa4781e6d06488b3a64b149e939ef1b222d`，提交标题：`fix: 按架构审查收紧并发上限、限流与生命周期`。对照基线：`d3309cf`。复查开始时工作区干净。

本次是审查：没有修改生产 Go 代码，也没有调用真实 Telegram、删除真实消息、安装更新或重启服务。仅新增本报告，并在第一轮报告中加入复查链接。边界探针通过 Go `-overlay` 从临时目录注入测试，不向源码目录写入测试文件。

**结论：本轮确认 5 个 P2 功能/生命周期问题。已提交的限流、工作池和状态写盘改动有效，但仍存在“排队后取消”“满队列的结果传播”“跨发送路径回退”方面的边界缺口。现有 race 测试与 vet 均通过，不能据此判定这些业务逻辑已经正确。**

## 修复状态（2026-10-12 更新）

五项均已在 `0e59afa` 之后的提交中修复，每项都有正式回归测试（断言正确结果，进 Git 跟踪）：

| 问题 | 修复 | 回归测试 |
| --- | --- | --- |
| R1 | 调度器为接受的任务提供 release 钩子，跑完、panic、排队中被取消都调用一次；`.da` 的群名额在 release 里释放，并核对 slot 身份 | `TestSchedulerReleasesCanceledQueuedWork`、`da.TestCanceledBeforeRunReleasesSlot` |
| R2 | 新增 `Registry.Busy()`：命令、短后台、长任务、到点的延时任务的执行与排队，加上 `Track()` 记下的 `.sum`/`.acron` 定时执行；自动更新改用它判断，装好后重启前再等最多 30 分钟空闲，等不到就不重启 | `TestBusyCountsLongTasksAndTracked`、`update.TestAutoUpdateWaitsForLongTasks`、`TestWaitIdle` |
| R3 | 新增 `Registry.Submit` / `App.SubmitMessage` 返回 NotCommand/Queued/Rejected；`Dispatch` 仍表示“匹配到命令”。`.acron` 记“已提交执行命令”，被拒绝时记错误、不记 deliverySent；`.sudo` 代发被拒绝时回复说明 | `TestSubmitReportsRejectionOnFullQueue`、acron 现有测试 |
| R4 | 新增 `Registry.After`：计时不占协程，上限 4096 个；到点交给独立的延时任务池（4 worker、256 排队），ctx 来自服务生命周期；Abort 时停掉未到点的计时器。keyword 与 aban 的延时删除都改用它 | `TestAfterRunsAndStopsOnAbort` |
| R5 | botapi 把网络错误、请求途中取消、5xx、读不懂的 2xx 标为 `UncertainError`（连不上、DNS 失败、4xx 仍是明确失败）；签到推送返回已发页数，回退途径只发剩下的页，不确定的那页不重发，并在最前面加一页说明 | `TestDefiniteFailuresAreNotUncertain`、`TestRemainingPages`、`TestDeliverDoesNotResendPushedPages` |

附录探针断言的是修复前的行为，且 `LongRunning`、keyword `later` 的签名已变，现在不再能编译；它们保留为复现记录。

## 1. 已修复项目的复查结果

| 上轮问题 | 当前实现 | 复查结果 |
| --- | --- | --- |
| 命令启动 goroutine 后才等名额 | 按需 worker、16 worker + 64 排队 | 已修复；标准上限测试通过 |
| 长时间删除占住短后台池 | `.da` 改用独立 long pool；service.active 限制 4 个群 | 已修复长短任务互相阻塞；仍有 R1 的取消清理问题 |
| 短后台任务无界 | `.sudo/.sure` 使用 4 worker + 16 排队 | 已修复准入上界 |
| WaitGroup Add/Wait 边界与等待协程 | 三个池的 idle channel + 共同空闲检查 | 已改善；嵌套任务与并发取消测试通过 |
| Bot API 忽略 ok 与 retry_after | 独立 botapi.Client、响应信封、429 冷却、有限重试 | 已修复客户端路径；业务回退仍有 R5 |
| 旧版网页预览字段 | link_preview_options.is_disabled | 已替换 |
| token URL / 密钥头重定向 | Bot API NoRedirect；httpx 检查原始请求凭证头和 key 参数 | 已修复当前凭证路径 |
| MTProto 固定退避、旧取消错误 | 同方法冷却、指数退避、抖动、ctx 错误 | 已改善；RandomID 保留测试通过 |
| 持续流量一直重置写盘时间 | 首次修改起一秒触发 flush，失败重试 | 已修复；持续更新/失败恢复测试通过 |
| 只读 Prepare 写文件、导入先于实例锁 | 内存会话验证、先锁后导入、失败释放锁 | 已修复；准备阶段回归测试通过 |

主通信路径仍是 MTProto userbot；HTTP Bot API 仅用于可选签到汇总推送。本次没有重新更改协议模型，也没有添加 getUpdates/webhook 入口。

## 2. 确认的问题

### R1 — [P2] 尚未执行的 `.da` 被取消后，active 名额永久残留

位置：`internal/commands/da/da.go:210–220`，相关路径 `internal/command/scheduler.go:89–103`。

`start` 在提交 long pool 之前把 slot 放进 `s.active`，但 `delete(s.active, id)` 只放在任务回调内部的 defer。如果 `.da stop` 在 worker 开始执行前调用 slot.cancel，scheduler.execute 看到取消后直接返回，不会调用 task.run，因此也不会运行回调内的 defer。队列和 worker 可以完全空闲，service.active 却仍然保存这个群。

结果：同一个群之后 `.da true` 一直被判断为已有任务，并返回 `false, nil`；累计到四个残留名额后，其他群也无法再启动删除。需要重启进程才会重建这个内存 map。这个窗口较短，但在启动后立即停止、系统负载或调度延迟下可达。

离线复现使用 GOMAXPROCS(1) 控制测试顺序，在 start 返回后立即取消 slot，再等 worker 消费取消任务，没有发送 Telegram RPC。实际输出：

```text
canceled before execution: pool idle, active slots=1, restart accepted=false, err=<nil>
```

建议：让调度器对所有已经接纳的任务提供完成/丢弃清理钩子，覆盖执行完成与“取消后未执行”两种路径；`.da` 的名额释放放在这个钩子里。拒绝提交时继续立即清理。释放时可核对 slot 身份，防止旧任务删除新任务的记录。不要简单在任意 cancel 时立即删除 active：已经在跑的删除 RPC 可能尚未退出，提前释放会允许同群重复启动。

这是本次有界调度与 `.da` 预登记状态之间的集成缺口，现有 TestSchedulerAbortDropsQueuedWork 只验证“未执行”，没有验证业务侧资源已经释放。

### R2 — [P2] 自动更新把运行中的长任务判断为空闲，仍会掐断 `.da`

位置：`internal/commands/update/auto.go:177`；计数来源 `internal/command/command.go:590–592`。

自动更新通过 `Registry.Running()` 决定是否等待任务结束，但该方法仅返回命令池 r.pool 的 running，忽略 r.long、r.tasks 以及其他池之外的业务工作。`.da true` 的交互命令很快结束，删除却继续在 long pool 运行，此时 Running()==0，decide 就进入 autoCheck。发现新版本后可以安装并重启，将正在删除的任务取消。

这与 auto.go 的 maxWait 注释“自动更新不该把正在跑的 .da、.sum 之类掐断”不一致。计数语义不完整的问题在旧版本也存在；独立长任务池并没有自动修复这个决策调用方。

复现启动一个等待 ctx 取消的长任务，等待它实际进入执行，再在 04:00 调用真实 decide：

```text
long task active, Running=0, update action=3 (autoCheck=3)
```

建议：提供面向维护动作的 Busy/ActivitySnapshot，统计命令、短后台、长任务的执行与排队，并纳入需要保护的 cron 业务执行。长期常驻的调度器循环本身不应算“忙”，否则自动更新永远无法运行。更新下载完成、准备重启前再次检查，或显式进入停止新任务准入的维护状态；仅在开始下载前读一次计数仍有时间窗口。

### R3 — [P2] 满队列拒绝命令后，`.acron` 仍记录“已执行命令”

位置：`internal/command/command.go:468–469`，调用方 `internal/commands/acron/run.go:342–345`。

Registry.run 在队列满时只记录 command.rejected，返回值没有传回 Dispatch。Dispatch 按当前注释返回“是否匹配到命令”，所以即使没有入队也返回 true。这个 API 本身的注释与实现一致；问题是 `.acron` 把 true 当成“命令已执行”，返回该文本，execute 又在 run.go:217 把 Delivery 写为 deliverySent。

在 16 个 worker 都被占用、64 个排队位置都满时，定时命令没有执行，却会被记录为成功。`.sudo/.sure` 的 DispatchFor 同样没有暴露接纳失败，需要一并处理结果传播。

复现先占满 16 worker，再放满 64 个目标命令，然后多提交一个：

```text
full queue: Dispatch=true, accepted target executions=64 (queued=64, rejected extra=1)
```

建议：区分 Matched、Accepted、Completed，给调用方返回结构化接纳结果或提供 TryDispatch；更新 `.acron` 和代发调用方。接纳成功但异步执行时，应显示“已提交执行”；只有收到执行完成结果才能宣称“已执行”。拒绝入队不能记录为 deliverySent，应保留队列满的原因与后续策略。直接把 Dispatch 的 bool 语义改掉前，应检查 app 日志与 verify 等调用方，避免把“队列满”误报成“命令不存在”。

这是引入可拒绝的有界队列后新暴露的调用契约问题。

### R4 — [P2] 关键词延时删除仍脱离服务生命周期，退出后可继续发删除 RPC

位置：`internal/commands/keyword/trigger.go:266–279`。

正常关键词动作得到 Registry.Lifecycle 派生的 ctx，但 deleteAfter 不接受该 ctx；计时回调直接创建 context.Background() 的 30 秒期限。该回调不在注册表三个池中，Abort/Wait 看不到它，也没有取消待触发 timer。

结果：服务退出取消后、客户端真正关闭前，timer 仍可能发起删除；已触发的删除可能继续等待限流。关键词动作的 4 个 slots 仅限制匹配动作，不限制已安排的 timer 数量，也不限制多个 timer 同时到期后的 RPC 并发。`.aban` 的 deleteLater 有同类生命周期问题，第一轮报告已经将它列为剩余项。

复现给关键词 apply 传服务生命周期，安排删除后取消生命周期，再触发已保存的 timer 回调，模拟 RPC 记录：

```text
after lifecycle abort, delayed delete RPCs=[delete [77]]
```

建议：延时任务持有服务生命周期，取消时停止尚未触发的 timer；到期后向有界后台池提交；请求期限从生命周期派生。安排数量也要设上限。结束日志、恢复和释放应覆盖取消/拒绝等路径。不能只把后台池改成更大而保留所有 timer 无界提交。

这项是上轮明确保留的剩余问题，本次补充了取消后仍调用 RPC 的具体证据。

### R5 — [P2] Bot API 分页部分成功后，签到回退会重发已送达页面

位置：`internal/commands/checkin/sign.go:199–206`，相关路径 sign.go:239–243。

botapi.Client 正确避免自动重试结果未知的 sendMessage，但 deliver 的业务回退绕开了这项保护。pushViaBot 逐页发送，只返回 error，不返回已成功的页数。第一页成功、第二页失败后，deliver 会把完整 pages 再交给 LogChat 的 MTProto 发送；PushChatID 与 LogChat 指向同一聊天时，第一页确定重复。没有 LogChat 时，回退到收藏夹/命令消息也会产生跨目的地重复汇总。

如果失败页发生在服务端已收消息但响应丢失的情况下，失败页本身也可能在回退中再次送达。客户端“只重试明确 429”不能单独保证整个投递流程不会重放。

复现设置 Bot API 与 LogChat 为同一 chat，让模拟 HTTP 服务第一页成功、第二页返回 500，再记录模拟 MTProto 发送：

```text
same chat: bot delivered pages=1; MTProto fallback pages=2; first page duplicated=true
```

建议：返回已确认发送进度、明确拒绝与结果未知三类信息。部分成功后的回退需要单独策略：继续明确未发送的页并说明切换目的地，或者报告部分成功而停止；结果未知时不能默认整份重发。需要最终送达保证时，持久化每页状态和业务幂等键。目的地使用稳定 chat ID，避免同一聊天的 username/数字 ID 别名掩盖重复目的地。

这项延续了第一轮“分页中途失败与持久投递”剩余风险，本次已验证具体重复行为。

## 3. 验证记录与边界

本轮执行：

```text
go test -race ./...             通过
go vet ./...                   通过
5 个临时 overlay 边界探针       均复现预期问题
```

探针使用 `-count=1` 强制实际执行。标准 race 测试对内容未变化的包会正常使用 Go 测试缓存。探针的 PASS 是因为断言确认了缺陷表现，并非表示对应功能正确；修复后的正式回归测试应改为断言正确结果。

生产代码和正式测试未修改。没有测量生产 RSS、吞吐或真实 Telegram 网络行为；没有执行自动更新、systemd 重启或任何真实删除。上述五项均属于可在离线模拟中验证的逻辑缺口，race detector 不会自动识别。

第一轮提出的 PeerCache 总量、锁内 DC 建连/状态 fsync、cron 调度规模、媒体资源预算等优化事项仍是后续工作。本轮将它们与已复现的功能缺陷分开，避免把性能建议误当成新增 bug。

修复顺序建议：先处理 R1 的任务清理和 R3 的结果传播；随后补全 R2 的维护忙碌状态；再统一 R4 的延时任务生命周期和 R5 的分页投递策略。每项都应以本文触发条件为基础补正式回归测试。

## 附录：可重放的离线探针

以下完整 Go 测试通过临时 overlay 注入，不需要把它们复制进正式源码。它们复用当前仓库的非联网测试 fixture，并断言问题存在。

在仓库根目录执行下面的脚本，即可从本报告提取源码、创建临时 overlay 并运行探针。脚本只在系统临时目录写文件；Go 标准构建/测试缓存照常使用。

```bash
python3 <<'PY'
from pathlib import Path
import json, re, subprocess, tempfile
repo = Path.cwd()
report = repo / 'docs/architecture-review-0e59afa.zh-CN.md'
text = report.read_text()
probes = re.findall(r'<!-- review-probe-package: ([^ ]+) -->\n\n```go\n(.*?)\n```', text, re.S)
if len(probes) != 5:
    raise SystemExit('expected five probes')
output = Path(tempfile.mkdtemp(prefix='mibot-lite-review-0e59afa-'))
replace = {}
packages = []
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

### internal/commands/da

<!-- review-probe-package: internal/commands/da -->

```go
package da

import (
	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"log/slog"
	"runtime"
	"testing"
	"time"
)

func TestReviewProbeCanceledDaRetainsSlot(t *testing.T) {
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	r := command.New([]string{"."}, slog.New(slog.DiscardHandler))
	defer r.Abort()
	s := &daService{a: &app.App{Registry: r}, active: map[string]*daSlot{}}
	inv := &command.Invocation{Message: &bot.Message{ChatID: "-1001", ID: 1}}
	accepted, err := s.start(inv)
	if !accepted || err != nil {
		t.Fatalf("start: %v %v", accepted, err)
	}
	s.mu.Lock()
	s.active["-1001"].cancel()
	s.mu.Unlock()
	if !r.Wait(time.Second) {
		t.Fatal("pool did not drain")
	}
	s.mu.Lock()
	retained := len(s.active)
	s.mu.Unlock()
	again, err := s.start(inv)
	t.Logf("canceled before execution: pool idle, active slots=%d, restart accepted=%v, err=%v", retained, again, err)
	if retained != 1 || again || err != nil {
		t.Fatal("probe did not reproduce retained slot")
	}
}
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

func TestReviewProbeAutoUpdateIgnoresLongTask(t *testing.T) {
	r := command.New([]string{"."}, slog.New(slog.DiscardHandler))
	defer r.Abort()
	started := make(chan struct{})
	r.LongRunning(r.Lifecycle(), func(ctx context.Context) { close(started); <-ctx.Done() })
	<-started
	action, _ := decide(autoDefaults(), time.Date(2026, 10, 12, 4, 0, 0, 0, shanghai), r.Running())
	t.Logf("long task active, Running=%d, update action=%d (autoCheck=%d)", r.Running(), action, autoCheck)
	if action != autoCheck {
		t.Fatal("probe did not reproduce idle classification")
	}
	r.Abort()
	if !r.Wait(time.Second) {
		t.Fatal("long task lingered")
	}
}
```

### internal/command

<!-- review-probe-package: internal/command -->

```go
package command

import (
	"context"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"sync/atomic"
	"testing"
	"time"
)

func TestReviewProbeDispatchReportsSuccessOnFullQueue(t *testing.T) {
	r := registry()
	defer r.Abort()
	started := make(chan struct{}, commandWorkers)
	release := make(chan struct{})
	var called atomic.Int32
	r.Register(&Command{Name: "blocked", Handle: func(context.Context, *Invocation) error { started <- struct{}{}; <-release; return nil }}, &Command{Name: "target", Handle: func(context.Context, *Invocation) error { called.Add(1); return nil }})
	for range commandWorkers {
		r.Dispatch(r.Lifecycle(), nil, &bot.Message{Text: ".blocked", ChatID: "1"})
		<-started
	}
	for range commandQueue {
		r.Dispatch(r.Lifecycle(), nil, &bot.Message{Text: ".target", ChatID: "1"})
	}
	reported := r.Dispatch(r.Lifecycle(), nil, &bot.Message{Text: ".target", ChatID: "1"})
	close(release)
	if !r.Wait(time.Second) {
		t.Fatal("did not drain")
	}
	t.Logf("full queue: Dispatch=%v, accepted target executions=%d (queued=%d, rejected extra=1)", reported, called.Load(), commandQueue)
	if !reported || called.Load() != commandQueue {
		t.Fatal("probe did not reproduce false success")
	}
}
```

### internal/commands/keyword

<!-- review-probe-package: internal/commands/keyword -->

```go
package keyword

import (
	"context"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"testing"
	"time"
)

func TestReviewProbeDeleteRunsAfterLifecycleAbort(t *testing.T) {
	s, fake, client, _ := newFixture(t)
	r := command.New(nil, client.Logger())
	var delayed func()
	s.later = func(_ time.Duration, run func()) { delayed = run }
	s.apply(r.Lifecycle(), client, groupMessage(channelID, 77, "ad"), task{ID: 1, DeleteSource: true, DeleteSourceAfter: 1})
	r.Abort()
	if r.Lifecycle().Err() != context.Canceled {
		t.Fatal("service not canceled")
	}
	delayed()
	calls := fake.take()
	t.Logf("after lifecycle abort, delayed delete RPCs=%v", calls)
	if len(calls) != 1 {
		t.Fatal("probe did not reproduce post-shutdown delete")
	}
}
```

### internal/commands/checkin

<!-- review-probe-package: internal/commands/checkin -->

```go
package checkin

import (
	"context"
	"encoding/json"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/botapi"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReviewProbeBotPushFailoverDuplicatesFirstPage(t *testing.T) {
	var botCalls atomic.Int32
	botTexts := make(chan string, 1)
	var userTexts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if botCalls.Add(1) == 1 {
			botTexts <- body.Text
			w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(500)
		w.Write([]byte(`{"ok":false,"error_code":500}`))
	}))
	defer server.Close()
	oldURL, oldClient := botAPI, botPusher
	botAPI, botPusher = server.URL, botapi.New()
	defer func() { botAPI, botPusher = oldURL, oldClient }()
	peers := bot.NewPeerCache()
	peers.SetSelf(100)
	target := &tg.User{ID: 200}
	target.SetAccessHash(1)
	peers.RememberUsers([]tg.UserClass{target})
	fake := telegram.InvokeFunc(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
		if request, ok := input.(*tg.MessagesSendMessageRequest); ok {
			userTexts = append(userTexts, request.Message)
			output.(*tg.UpdatesBox).Updates = &tg.UpdateShortSentMessage{ID: len(userTexts)}
			return nil
		}
		t.Fatalf("unexpected RPC: %T", input)
		return nil
	})
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 100}, slog.New(slog.DiscardHandler))
	if err := deliver(context.Background(), client, config{BotToken: "TEST", PushChatID: "200", LogChat: "200"}, strings.Repeat("x", command.PageLimit+100), nil); err != nil {
		t.Fatal(err)
	}
	botFirst := <-botTexts
	t.Logf("same chat: bot delivered pages=%d; MTProto fallback pages=%d; first page duplicated=%v", 1, len(userTexts), len(userTexts) > 0 && botFirst == userTexts[0])
	if len(userTexts) != 2 || botFirst != userTexts[0] {
		t.Fatal("probe did not reproduce duplicate first page")
	}
}
```
