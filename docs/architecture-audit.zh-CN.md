# mibot-lite 架构审查与核心重构报告

> 更新：对提交 `0e59afa` 的第二轮复查已完成，确认 5 个可复现的 P2 边界问题。详见 [第二轮复查报告](architecture-review-0e59afa.zh-CN.md)。本文件保留第一轮诊断与实现记录。

审查日期：2026-10-12（Asia/Taipei）。基线：`d3309cf`，`github.com/MiCat-S/mibot-lite`，Go 1.26，gotd/td v0.162.0。本报告对应的重构已提交为 `0e59afa`，附录代码与该提交的源文件一致；之后的修复见第二轮复查报告。

审查范围：基线 Git 跟踪的 116 个非测试 Go 文件，以及入口、部署脚本和依赖配置；对全仓网络调用、goroutine 启动、共享 map、锁、context 脱离和重试路径进行静态扫描，深入检查核心模块及有关命令。测试也包括工作区已有的本地测试。动态验证使用模拟 RPC、httptest 和 race detector；未连接真实 Telegram 账号，未执行删除、封禁或真实签到。静态扫描与测试覆盖不能证明所有第三方服务和所有 Telegram RPC 在生产环境都没有问题。

## 1. 架构诊断与痛点总结

### 1.1 首先区分两种 Telegram 接口

主服务是 **MTProto userbot**，不是 HTTP Bot API 机器人：

- `internal/app/app.go` 使用 `telegram.NewClient(APIID, APIHash, ...)`、StringSession、`Auth().Status()`、`Self()`。
- 发消息走 `tg.MessagesSendMessageRequest`，媒体走 `messages.sendMedia`，删消息走 `channels.deleteMessages` / `messages.deleteMessages`。
- 更新来自 MTProto 长连接的服务端推送，gotd 的 updates.Manager 管理 pts/qts/seq 和补差。
- `.checkin` 可选的汇总推送是唯一发现的直接 HTTP Bot API 调用，使用 bot token 调用 `sendMessage`。

这意味着不能把 30 messages/sec 限流器安装到所有 MTProto RPC 上，也不能把 MTProto 的 `FLOOD_WAIT` 当成 HTTP `parameters.retry_after`。官方的单聊天约 1 条/秒、群组 20 条/分钟、免费广播约 30 条/秒是 Bot API 机器人发送消息的指导限额，并非所有 Telegram 方法统一的严格 QPS。参见 [Bots FAQ](https://core.telegram.org/bots/faq#my-bot-is-hitting-limits-how-do-i-avoid-this)。

本次在线查看的 Bot API 文档最近更新为 2026-08-24 的 10.3。该版本号用于核查 HTTP 推送路径，不能据此判断 gotd 的 MTProto TL layer 是否匹配；本地 gotd 的生成代码使用 layer 229。参见 [Bot API 官方文档](https://core.telegram.org/bots/api)。

### 1.2 主要问题与处理结果

优先级：P1 为可能导致服务失去响应、内存增长、状态丢失或凭证泄露的问题；P2 为可靠性、运维或扩展性问题。

| 优先级 | 原始问题与证据 | 影响 | 本次结果 |
| --- | --- | --- | --- |
| P1 | `Registry.run` 先 `go func()`，再等待容量 16 的 semaphore | 限制运行数，没有限制排队协程；等待 Telegram 限流时可堆积大量 goroutine | 改为按需 worker pool：命令最多 16 worker、64 排队，提交满时立刻拒绝并记日志 |
| P1 | `.sudo/.sure` 的 `inBackground` 每次命中都启动 goroutine；`.da` 脱离命令 ctx 后不挂退出信号 | 授权用户刷屏或多个群启动长期删除时，后台工作不受统一约束 | 短后台池：4 worker、16 排队，承载代发；`.da` 另用长任务池（最多 4 个群同时删除，超出时直接提示），长任务占满也不挡代发；都随服务生命周期取消；Wait 包括后台转命令等嵌套提交 |
| P1 | `.checkin` 只检查 HTTP 状态，不检查 JSON `ok`，不解析 `retry_after` | HTTP 200 + `ok:false` 被误判成功；429 直接失败并走汇总回退 | 专用 botapi.Client 校验响应信封、处理 429、共享 token 冷却、有限重试与并发准入 |
| P1 | 同一方法的多个 MTProto 请求各自睡眠，未共享 FLOOD_WAIT 状态 | 其他并发调用继续冲击被限流的方法 | Retrier 在一次 Handle 创建的中间件内共享按方法冷却状态；等待不持锁 |
| P1 | `tgstate.markDirty` 在每次更新上 Reset 一秒计时器 | 持续流量下长期不落盘，崩溃后 pts/hash 丢失窗口扩大 | 改为首次修改启动计时器，期间合并更新；失败自动重试；关闭后拒绝写入 |
| P1 | httpx 允许跨站重定向；第三方 API 请求携带 `x-api-key` 等自定义头 | Go 不会自动清除所有自定义密钥头，307/308 可转发原请求体 | 检查原始请求凭证头/常用 key 参数，阻止变更 origin；Bot API token URL 完全禁用重定向 |
| P2 | MTProto 重试 sleep 被取消时返回旧 RPC 错误，固定 2 秒重试服务端错误 | 调用方无法识别取消；并发故障时请求同步重试 | 返回 `ctx.Err()` / sleep 错误；服务端退避 2/4/8/16 秒，附加 0–25% 抖动 |
| P2 | `Registry.Wait` 每次创建 goroutine 等 WaitGroup，Add/Wait 生命周期缺乏完整边界 | 等待超时后留下 waiter；并发提交和等待可能违反 WaitGroup 约束 | worker 状态由锁保护；用每批次 idle channel 等待，无单独 waiter goroutine |
| P2 | 命令 panic / timeout 错误反馈使用 `WithoutCancel` 且没有额外期限 | 反馈 RPC 本身可继续阻塞，拖住退出 | 反馈最长 5 秒并保留服务取消；panic 日志带 stack |
| P2 | App 启动 jobs 后不 join；AfterReady 启动两条无恢复 goroutine；`.sum` cron 不恢复 panic | 关闭客户端/状态时仍有工作；后台 panic 可终止进程 | jobs 和 ready hook 恢复 panic、取消并有界等待；sum 使用 cron.Recover，并等已启动任务结束 |
| P2 | `Prepare` 在取得单实例锁之前导入会话；`ReadOnly` 仍可能写 gotd-session/data | 第二个进程在被锁挡住之前修改状态；自更新的只读预检具有副作用 | 先锁后导入；失败释放锁；只读会话使用内存 storage，不创建部署文件 |
| P2 | HTTP transport 总空闲连接仅 4 个，默认单 host 空闲连接仅 2 个 | 多 API、多命令并发时容易淘汰可复用的空闲连接 | 调整为全局空闲 32、每 host 空闲 8、每 host 连接 16、idle 90 秒 |
| P2 | `eachGroupLimit` 占 semaphore 的发送无法响应 ctx 取消 | 取消后等待名额仍可阻塞，甚至开始新的群任务 | 占名额使用 select，并在占到后再次检查取消 |

### 1.3 协议与 payload 细节

**HTTP Bot API 推送**：`chat_id`、`text`、`parse_mode:HTML` 原本正确；原来的 `disable_web_page_preview` 不在目前 sendMessage 参数表中，本次替换为 `link_preview_options:{is_disabled:true}`。普通签到汇总不需要 business_connection_id、ephemeral_message_parameters、reply_markup 等可选字段，不能把未发送所有可选参数描述为“字段遗漏”。若要推送到论坛指定话题或频道 direct messages topic，当前配置缺少对应 topic 参数，应单独扩展 SendOptions 与配置。文本当前由 HTMLPages 按 PageLimit 分页，实体长度工具使用 UTF-16；后续新调用点同样必须控制 1–4096 的解析后文本长度。参见 [sendMessage](https://core.telegram.org/bots/api#sendmessage)、[LinkPreviewOptions](https://core.telegram.org/bots/api#linkpreviewoptions)。

**MTProto 消息**：检查了 Peer、RandomID、Entities、ReplyTo/TopMsgID、Silent、NoWebpage 的构造。现有代码正确使用 `SetEntities`、`SetReplyTo` 等方法设置可选 TL 字段；需要 access hash 的寻址由 PeerCache 负责，min 实体不被当成可直接寻址的 hash。Retrier 重试相同请求对象，因此 RandomID 保持不变，回归测试已覆盖。官方要求 random_id 用来防止重发；但此结论只适用于带这种去重机制的消息方法，不能推论所有 RPC 写操作都幂等。参见 [messages.sendMessage](https://core.telegram.org/method/messages.sendMessage)。

**两种限流语义**：MTProto 的 `FLOOD_WAIT_X` / `FLOOD_PREMIUM_WAIT_X` 用 RPC 420 表示等待；HTTP Bot API 用响应信封的 error_code 和 parameters.retry_after。SLOWMODE_WAIT 是按聊天限制的另外一种 MTProto 错误，本次未把它合并进按方法共享的 FLOOD_WAIT 控制器，以免一个慢速群阻塞其他聊天。参见 [MTProto 错误处理](https://core.telegram.org/api/errors)、[ResponseParameters](https://core.telegram.org/bots/api#responseparameters)。

### 1.4 更新通信模型

主服务没有 getUpdates 轮询器，也没有 HTTP webhook handler。Telegram 在已授权 MTProto 连接上推送更新；gotd 更新引擎负责补差。本项目另有 liveHandler，在排序/补差之前先执行新消息路由，再交给 gotd，以避免某个群的补差失败拖住即时命令。seenMessages 和 SentRecorder 用 peer/message ID 去重，并阻止账号自动发出的文本被补差重新当成命令执行；这两层也是授权边界，不能为了减少代码而删除。参见 [Working with Updates](https://core.telegram.org/api/updates)。

该设计的取舍是：live 路径不保证业务处理遵守完整 pts 顺序；更新回调本身的 goroutine 由 gotd 管理，命令池限制的是业务工作，不是底层全部更新 goroutine。seenMessages 是两代时间窗口去重，进程重启前的消息被主动过滤。它提供的是“在线去重 + 尽快执行”，不提供持久的恰好一次处理保证。应通过更新补差失败指标和队列拒绝指标观察，而不是简单改成 webhook：userbot 的通信方式与 Bot API webhook 不能直接互换。

若未来添加真正的 Bot API 机器人入口，getUpdates 与 webhook 互斥。长轮询需要正数 timeout、limit 1–100、按接收/持久化成功后的 update_id 更新 offset；webhook 应校验 X-Telegram-Bot-Api-Secret-Token，限制请求体，把更新可靠入队后再返回 2xx，并处理重复更新。现有仓库没有这些入口，所以本次没有添加无调用方的新 server。参见 [getUpdates](https://core.telegram.org/bots/api#getupdates)、[setWebhook](https://core.telegram.org/bots/api#setwebhook)。

### 1.5 共享状态、锁和持久化

- Registry、PeerCache、Store、seenMessages、logtail.Ring 的共享 map 已有 Mutex/RWMutex；注册后只读的命令和监听器依赖“只在启动注册”约定，运行中修改应改为显式快照或加锁。
- Store.Read 使用 JSON 深拷贝，Update 在副本上修改并原子写回，因此调用方不会直接拿到内部 map；不能仅为了提速改成返回缓存内部指针。每次 JSON marshal/unmarshal 的成本、持锁 fsync 的成本都随文档大小增加。
- tgstate 的整份 JSON 编码和 fsync 仍在状态锁内，保障写入顺序，但大型状态文件会拖住并发更新。进一步优化应使用版本化快照和单写者；不能简单解锁后同时写同一个状态文件，后完成的旧快照可能覆盖新快照。
- PeerCache 的用户/频道 map 没有总量上限，maxPersistBatch 只限制单次持久化批次，不限制内存缓存总量；扫描大群后可能保持大量实体。应评估 LRU/TTL 和持久 hash 兜底，先测真实命中率再更改寻址语义。
- mediaDC 在 dcMu 内执行最长 20 秒的网络建连，能避免重复连接，但会串行化不同 DC 的初始化，锁等待不能直接用 ctx 打断。下一步适合按 DC singleflight、将建连移出全局锁，并跟踪关闭状态。
- `.keyword` 动作已有 4 个非阻塞准入名额；但延时删除仍使用 AfterFunc + Background，未进入本次新后台池。`.acron` 同任务避免重入、不同任务 serial Mutex 串行；不同任务过多时 cron 回调可积压。二者是后续统一调度的明确剩余项。

现有日志、UTF-16 分页、媒体流式下载、外部子进程 CommandContext、订阅抓取 SSRF 防护、原子配置写入都属于有价值的现有设计。本次不把它们误报成缺失。

## 2. 核心模块重构方案与代码

本节描述的代码已经写入对应 Go 源文件。附录 A–E 包含完整文件源码，不是仅展示核心分支的伪代码。

### 2.1 HTTP Bot API 客户端：internal/botapi/client.go

- 一个并发安全 Client，在 `.checkin` 中长期复用；连接 transport 继续使用 httpx 的共享池。
- token/endpoint 组合用 SHA-256 标识限流账户，预算表不保存明文 token。
- 同 token 全局约 30 条/秒，正数私聊 ID 至少 1 秒间隔，负数 ID 和 @username 保守设 3 秒间隔以覆盖群组 20 条/分钟；频道也采用保守间隔。
- 最多 32 个发送调用包括等待者；额外调用立即返回 ErrQueueFull，客户端不启动后台 goroutine。
- 429 等待 max(retry_after, 1/2/4 秒退避) 加少量正抖动，初始调用之后最多重试 3 次；超过 60 秒的服务端等待返回 APIError，仍记下冷却时间。单次 HTTP 请求最长 15 秒，整个操作还受调用方 context 限制。
- 校验 HTTP 状态和 JSON ok；错误描述脱敏 token；禁止所有重定向，防止 token URL 和正文泄露。
- 不自动重试网络错误和 5xx sendMessage，因为发送可能已成功；不会假装 HTTP Bot API 具备 MTProto RandomID 去重能力。

这是**有界等待与有限重试**，不是跨重启的持久投递队列，也不是严格 FIFO。同一 chat 以不同 ID/username 表示可能绕开本地每 chat 预算，后续应规范为稳定 chat ID；同 token 在多个进程或其他程序中共享时，需要集中限流。长时间 429、迁移群 ID、分页中途失败和持久重放策略需要业务层决策。

### 2.2 MTProto 重试：internal/bot/retrier.go

按方法维护共享冷却，不把 Bot API 数值限额套到 MTProto 上。等待时不持 mutex，不创建新 goroutine。sleep 与所有网络调用使用同一个 ctx；取消优先返回 context 错误。服务端错误的基础等待为 2/4/8/16 秒，附加 0–25% 正抖动，最长每次约 20 秒。FLOOD_WAIT 等待服务端要求的时间并额外 1 秒；超过 MaxWait 返回错误。更新引擎自行恢复的 getChannelDifference 保留绕过中间件重试的行为，避免把补差协程卡住。

不自动重试普通 400/401/403、网络传输错误。新增带业务副作用的 RPC 时，应把本层“所有 5xx 可重试”的历史行为进一步收敛为明确安全的方法清单。

### 2.3 命令调度：internal/command/scheduler.go 与 command.go

调度器在提交时将任务放进有界 channel，最多按需创建 16 个 worker。worker 在同一批次内循环消费；队列空了后退出，闲置时没有常驻命令 worker。短后台任务（`.sudo/.sure` 代发）使用另外 4 worker + 16 排队的池；`.da` 这类按小时计的任务再用独立的长任务池（4 worker），由 `.da` 自己把同时删除的群数限制在 4 个以内，超出时直接回复失败原因而不是静默排队。三个池分开，避免长任务堵住交互命令或代发。

拒绝提交时记录 command.rejected / task.rejected。命令的 Dispatch 返回值仍表示是否识别到命令，而非任务已执行完成；调用方不要把它当执行结果。提交在更新回调内不等待队列空位，不以新 goroutine 保存待提交工作。

Abort 取消注册表生命周期，关闭继续准入，运行任务消费取消信号，排队任务被跳过。Wait 使用 idle channel 等待，检查三个池共同空闲，覆盖“后台代发提交命令”和“命令启动后台任务”的情况。

### 2.4 HTTP 连接池：internal/httpx/httpx.go

Transport 参数：MaxIdleConns=32、MaxIdleConnsPerHost=8、MaxConnsPerHost=16、IdleConnTimeout=90 秒、拨号/TLS 10 秒、ResponseHeaderTimeout=30 秒、HTTP/2 启用、TCP keepalive 30 秒。

每请求通过 NewRequestWithContext 和 WithTimeout 限制总时长；正常有限响应读完并关闭，测试验证同 host 三次请求只建立一条连接。超出 body 上限时关闭连接而非无界 drain，这是保护资源和复用之间的合理取舍。

httpx.NoRedirect 只复制 Client 的配置，仍共享 Transport，不重新创建连接池。订阅抓取使用独立 guardedTransport 是为了 SSRF 检查和不同的重定向策略；不能为了“全局一个 Client”把它的安全策略删除。单次构造 Client 但复用同一 Transport 不等于每次新建连接。

### 2.5 生命周期与状态：app.go、tgstate/state.go、有关命令

Prepare 先取得实例锁，再导入文件会话；失败通过 defer 释放锁。ReadOnly 在内存会话上校验并构造依赖，注册过程不创建 data/。可复用的命令生命周期替代更新回调的 WithoutCancel，让命令可以比单个更新处理活得久，又仍在 Abort 时取消。

App 对注册 jobs 和 AfterReady hook 统一记录 panic/stack。退出时取消 jobs 与命令，命令池最长等 2 秒、jobs 最长等 2 秒，之后关闭额外 DC 连接、清空已连接客户端并由 Close 刷状态、释放锁。`.sum` 使用服务生命周期，cron Stop 等已启动 callback 返回。

这是**有界取消式退出**，符合项目 `.restart` 要快速掐断卡住命令的既有设计；不是“等所有长任务自然完成”。Go 无法强制终止忽略 ctx 的任意 goroutine，超时仍记 lingering 警告；文件系统 syscall 也不是因为接收了 Context 就自动可中断。需要完整投递保证的业务应结合 outbox/checkpoint，而不是无限等关机。

## 3. 最佳实践建议

1. **保持协议边界**：MTProto 账号与 Bot API token 各用自己的 adapter、限流器和错误类型；依赖 TL layer、Bot API schema 更新分别检查。
2. **定义重试安全性**：读取可重试；消息重试保持 RandomID；其他写 RPC 必须逐一评估。Bot API 仅对明确拒绝的 429 有限重试，结果未知的 sendMessage 返回可观测错误，业务层决定补发。
3. **把流量和资源分开限额**：QPS、同时请求数、排队数、同 chat 顺序、媒体编码内存分别控制。16 个 command worker 并不代表可并行跑 16 个 100 MB ffmpeg；媒体编码应另设小容量权重 semaphore，并用真实素材基准测试。
4. **所有后台工作有归属**：继续迁移 keyword 延时删除、aban 的 AfterFunc、acron 的 serial 等待、sum 多任务并行到统一生命周期和有界调度；每种队列明确满时丢弃/拒绝/持久化策略。避免重新引入 go func 再占 semaphore 的模式。
5. **控制缓存规模**：PeerCache 加总量上限与持久寻址兜底；定时任务数、延时计时器数也要有限额。Store 深拷贝和状态整份 fsync 如成为热点，使用 pprof 和文件大小数据证明后再改存储设计。
6. **保持可取消语义**：取消返回 context.Canceled/DeadlineExceeded；不要用 WithoutCancel 作为通用漏斗。需要脱离命令超时的工作挂服务生命周期；用户反馈也有单独短超时。
7. **继续缩小耦合**：当前已有 app / bot / command / commands / store 分层，并非所有逻辑都在 handler 中；但 commands 依赖整个 App，很多命令直接调原始 tg.Client。可逐步引入窄接口 TelegramSender、PeerResolver、Store、TaskRunner 和 clock；避免为轻量单账号应用过早引入大型框架。
8. **统一错误与恢复**：内部使用 fmt.Errorf("operation: %w", err)，由 UI 边界生成用户提示；对 token URL 特殊脱敏。goroutine/cron 边界各自 recover，main 的 recover 不能捕获其他 goroutine。对密码、token、session 和付费发送字段不写敏感日志、不自动开启。
9. **持久投递需要明确保证**：当前队列是进程内有界队列；如要保证签到汇总最终送达，应加入带幂等业务键的 outbox、每页发送状态、失败记录和人工/自动补发策略。Bot API 429 之外的请求结果未知问题仍需显式处理。
10. **加入公开 CI**：go test ./...、go test -race ./...、go vet ./...、Linux amd64/arm64 构建。新的安全/并发回归测试应进入 Git 跟踪，不能只留在 *_local_test.go。外部服务、真实账号和破坏性命令测试保持显式选择，真实 Telegram 接入由独立测试账号完成。
11. **可观测性优先**：记录 command/task queue depth、拒绝计数、运行中任务、FLOOD_WAIT/429 等待与次数、状态 flush 错误、补差失败、连接复用、退出残留；日志标签避免 chat ID 或 token 带来指标高基数。
12. **生命周期测试持续维护**：服务启动失败释放锁、只读检查零副作用、运行/排队同时取消、ready hook 失败、长任务 shutdown、状态写盘持续流量和错误恢复都应可在离线测试中验证。

### 验证结果

- 修改前：go test ./... 通过。
- 修改后：go test ./...、go test -race ./...、go vet ./... 通过。
- bash scripts/build.sh /tmp/mibot-lite-audit-20261012 成功，CGO_ENABLED 默认 0，构建 darwin/arm64 当前宿主平台静态配置的二进制。
- git diff --check 通过。
- 新回归覆盖：10,000 次超额提交的队列拒绝、worker 并发上限、排队取消、嵌套任务等待、并发 Dispatch/Abort/Wait；Bot API payload、200/ok:false、429 retry_after、限流账户隔离、群组/聊天/全局间隔、超长等待、发送结果不确定不重放、并发准入上限和取消；MTProto 同方法冷却与 RandomID 保留；HTTP 连接复用、字节限制与密钥重定向；状态持续变更落盘、失败自动重试、回调读状态不死锁、关闭后写入；只读 Prepare 和实例锁顺序。

验证未提供生产吞吐、真实 Telegram 延迟或 RSS 降幅数据；本次性能结论是资源上界得到约束和连接复用已离线验证，不能据此声称吞吐提高某个百分比。


## 附录 A：HTTP Bot API 客户端完整代码

源文件：`internal/botapi/client.go`。

```go
// Package botapi 是可选的 HTTP Bot API 推送（目前只有 .checkin 的汇总用它）。
// 主连接是 MTProto userbot，不能套用这里按 Bot API 指导限额设的限流。
package botapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

// ErrQueueFull 表示同时等着发送的调用已经满了。
var ErrQueueFull = errors.New("Bot API 推送排队已满")

// APIError 是 Bot API 明确拒绝的响应，带着限流等待时间，不带含 Token 的 URL。
type APIError struct {
	Code        int
	Description string
	RetryAfter  time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Bot API 返回 %d：%s", e.Code, e.Description)
}

type budget struct {
	global, cooldown time.Time
	chats            map[string]time.Time
}

// Client 可以并发使用。每个 Token 有自己的全局额度；同时最多接纳 32 个调用
// （包括正在等额度的），多出来的直接返回 ErrQueueFull，不另开协程。
type Client struct {
	mu      sync.Mutex
	budgets map[[32]byte]*budget
	slots   chan struct{}
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
	post    func(context.Context, string, map[string]string, any, time.Duration, int64) (httpx.Response, error)
}

func New() *Client {
	return &Client{budgets: make(map[[32]byte]*budget), slots: make(chan struct{}, 32),
		now: time.Now, sleep: sleep, post: postJSON}
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

// wait 等到能发为止，能发时才记下这次占用的额度，所以中途取消不会白占以后的额度。
// 429 的冷却对同一个 Token 的所有聊天都生效。
func (c *Client) wait(ctx context.Context, key [32]byte, chat string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		now := c.now()
		for id, b := range c.budgets {
			for chat, until := range b.chats {
				if !until.After(now) {
					delete(b.chats, chat)
				}
			}
			if len(b.chats) == 0 && !b.global.After(now) && !b.cooldown.After(now) {
				delete(c.budgets, id)
			}
		}
		b := c.budgets[key]
		if b == nil {
			b = &budget{chats: make(map[string]time.Time)}
			c.budgets[key] = b
		}
		until := b.global
		if b.chats[chat].After(until) {
			until = b.chats[chat]
		}
		if b.cooldown.After(until) {
			until = b.cooldown
		}
		wait := until.Sub(now)
		if wait <= 0 {
			b.global = now.Add((time.Second + 29) / 30)
			interval := time.Second
			// 群组、超级群组的数字 ID 是负数。频道和 @用户名 也按群组的保守间隔算：
			// 用户名也可能指向群组。
			if strings.HasPrefix(chat, "-") || strings.HasPrefix(chat, "@") {
				interval = 3 * time.Second // 每分钟不超过 20 条
			}
			b.chats[chat] = now.Add(interval)
		}
		c.mu.Unlock()
		if wait <= 0 {
			return nil
		}
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

func (c *Client) pause(key [32]byte, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.budgets[key]
	if b == nil {
		b = &budget{chats: make(map[string]time.Time)}
		c.budgets[key] = b
	}
	if until := c.now().Add(d); until.After(b.cooldown) {
		b.cooldown = until
	}
}

// SendHTML 发一条 HTML 消息，只对明确的 429 拒绝有限重试。网络错误和 5xx 时消息
// 可能已经发出去了，Bot API 没有 random_id 去重，盲目重试会发出重复消息。
func (c *Client) SendHTML(ctx context.Context, endpoint, token, chat, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		return ErrQueueFull
	}
	key := sha256.Sum256([]byte(endpoint + "\x00" + token))
	request := struct {
		ChatID             string `json:"chat_id"`
		Text               string `json:"text"`
		ParseMode          string `json:"parse_mode"`
		LinkPreviewOptions struct {
			Disabled bool `json:"is_disabled"`
		} `json:"link_preview_options"`
	}{ChatID: chat, Text: text, ParseMode: "HTML"}
	request.LinkPreviewOptions.Disabled = true
	for attempt := 0; ; attempt++ {
		if err := c.wait(ctx, key, chat); err != nil {
			return err
		}
		response, err := c.post(ctx, strings.TrimRight(endpoint, "/")+"/bot"+token+"/sendMessage", nil, request, 15*time.Second, 1<<20)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// URL 错误的文字里有 Token，不能原样往外传。
			return errors.New("Bot API: " + httpx.Reason(err))
		}
		var envelope struct {
			OK          bool   `json:"ok"`
			Code        int    `json:"error_code"`
			Description string `json:"description"`
			Parameters  struct {
				RetryAfter int64 `json:"retry_after"`
			} `json:"parameters"`
		}
		if err := json.Unmarshal(response.Body, &envelope); err != nil {
			// 代理或网关出错时常返回 HTML 页面，这时报状态码比报「不是 JSON」有用。
			if !response.OK() {
				return &APIError{Code: response.Status, Description: "响应不是 JSON"}
			}
			return errors.New("Bot API 返回的不是 JSON")
		}
		if response.OK() && envelope.OK {
			return nil
		}
		code := envelope.Code
		if code == 0 {
			code = response.Status
		}
		apiErr := &APIError{Code: code, Description: strings.ReplaceAll(envelope.Description, token, "[redacted]")}
		if code != 429 {
			return apiErr
		}
		// 先排除离谱的秒数，再换算成 Duration，免得溢出。
		seconds := envelope.Parameters.RetryAfter
		if seconds < 0 || seconds > 86400 {
			return apiErr
		}
		apiErr.RetryAfter = time.Duration(seconds) * time.Second
		delay := time.Second << min(attempt, 3)
		wait := max(apiErr.RetryAfter, delay) + time.Duration(rand.Int64N(int64(250*time.Millisecond)))
		c.pause(key, wait)
		if attempt >= 3 || apiErr.RetryAfter > time.Minute {
			return apiErr
		}
	}
}

// postJSON 发送 JSON 请求。URL 里有 Token、正文是消息，都不能跟随重定向转给别的地址。
func postJSON(ctx context.Context, target string, headers map[string]string, payload any, timeout time.Duration, limit int64) (httpx.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return httpx.Response{}, err
	}
	merged := map[string]string{"Content-Type": "application/json"}
	for key, value := range headers {
		merged[key] = value
	}
	return httpx.Do(ctx, httpx.Request{Method: "POST", URL: target, Headers: merged, Body: body, Timeout: timeout, MaxBytes: limit, NoRedirect: true})
}
```

## 附录 B：有界调度器完整代码

源文件：`internal/command/scheduler.go`。

```go
package command

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

const (
	// 命令池：最多 16 个同时跑，再多 64 个排队。
	commandWorkers = 16
	commandQueue   = 64
	// 短后台任务池：最多 4 个同时跑，再多 16 个排队。
	taskWorkers = 4
	taskQueue   = 16
)

type scheduledWork struct {
	ctx context.Context
	run func(context.Context)
}

// scheduler 是按需启动的有界工作池：最多 limit 个 worker，队列空了 worker 就退出，
// 闲着时不留常驻协程。提交从不阻塞更新回调，排队中的任务也不占协程；队列满了直接拒绝。
type scheduler struct {
	mu      sync.Mutex
	queue   chan scheduledWork
	limit   int
	workers int
	running int
	// idle 在 worker 全部退出时关闭，有新任务时换一个新的。
	idle   chan struct{}
	life   context.Context
	logger *slog.Logger
}

func newScheduler(life context.Context, logger *slog.Logger, workers, queued int) *scheduler {
	idle := make(chan struct{})
	close(idle)
	return &scheduler{queue: make(chan scheduledWork, queued), limit: workers, idle: idle, life: life, logger: logger}
}

func (s *scheduler) submit(ctx context.Context, run func(context.Context)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.life.Err() != nil || ctx.Err() != nil {
		return false
	}
	select {
	case s.queue <- scheduledWork{ctx: ctx, run: run}:
	default:
		return false
	}
	if s.workers == 0 {
		s.idle = make(chan struct{})
	}
	if s.workers < s.limit {
		s.workers++
		go s.work()
	}
	return true
}

func (s *scheduler) work() {
	for {
		s.mu.Lock()
		var task scheduledWork
		select {
		case task = <-s.queue:
			s.running++
		default:
			s.workers--
			if s.workers == 0 {
				close(s.idle)
			}
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		s.execute(task)
		s.mu.Lock()
		s.running--
		s.mu.Unlock()
	}
}

func (s *scheduler) execute(task scheduledWork) {
	if s.life.Err() != nil || task.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(task.ctx)
	stop := context.AfterFunc(s.life, cancel)
	defer cancel()
	defer stop()
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logger.Error("worker.panic", slog.Any("panic", recovered), slog.String("stack", string(debug.Stack())))
		}
	}()
	if ctx.Err() == nil && s.life.Err() == nil {
		task.run(ctx)
	}
}

func (s *scheduler) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *scheduler) wait(timeout time.Duration) bool {
	s.mu.Lock()
	idle := s.idle
	s.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-idle:
		return true
	case <-timer.C:
		return false
	}
}
```

## 附录 C：MTProto 重试中间件完整代码

源文件：`internal/bot/retrier.go`。

```go
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
```

## 附录 D：HTTP 连接复用客户端完整代码

源文件：`internal/httpx/httpx.go`。

```go
// Package httpx 是带限制的 HTTP 客户端，所有要联网的命令都走它：
// 共用一个 transport，每个请求有超时，响应体有字节上限，重定向最多三次。
package httpx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// UserAgent 是默认发送的 User-Agent，请求可以自己覆盖。
const UserAgent = "MiBot-Lite/1"

// ErrTooLarge 表示响应体超过了请求设定的字节上限。
var ErrTooLarge = errors.New("response body exceeds the size limit")

var shared = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		MaxConnsPerHost:       16,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	},
	CheckRedirect: checkRedirect,
}

// ErrCredentialRedirect 表示带凭证的请求被重定向到了别的源，已拒绝跟随。
var ErrCredentialRedirect = errors.New("refusing credential redirect to another origin")

// credentialHeaders 是各家 API 放密钥的请求头。Go 跨域重定向时只会清掉 Authorization、
// Cookie 等标准头，x-api-key 这类自定义头会原样带到新地址，307/308 还会连正文一起转发。
var credentialHeaders = []string{"Authorization", "Cookie", "X-Api-Key", "Api-Key", "X-Goog-Api-Key"}

// checkRedirect 最多跟随三次重定向；最初的请求带了凭证（请求头或 URL 里的 key 参数）时，
// 只允许重定向到同一个源（scheme + host）。
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return errors.New("too many redirects")
	}
	first := via[0]
	credential := first.URL.Query().Get("key") != ""
	for _, name := range credentialHeaders {
		credential = credential || first.Header.Get(name) != ""
	}
	if credential && (!strings.EqualFold(req.URL.Host, first.URL.Host) || req.URL.Scheme != first.URL.Scheme) {
		return ErrCredentialRedirect
	}
	return nil
}

// Request 描述一次调用。
type Request struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
	// NoRedirect 不跟随重定向，用于 URL 里带凭证、或者正文不能转给别处的请求。
	NoRedirect bool
	// Timeout 限制整个调用的时长。零值表示 30 秒。
	Timeout time.Duration
	// MaxBytes 限制响应体大小。零值表示 2 MB。
	MaxBytes int64
}

// Response 是返回的结果。
type Response struct {
	Status int
	Body   []byte
}

// OK 判断状态码是不是 2xx。
func (r Response) OK() bool { return r.Status >= 200 && r.Status < 300 }

// Do 执行请求。非 2xx 状态码不算错误：404 意味着什么由调用方决定。
func Do(ctx context.Context, request Request) (Response, error) {
	timeout := request.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	limit := request.MaxBytes
	if limit <= 0 {
		limit = 2 << 20
	}
	method := request.Method
	if method == "" {
		method = http.MethodGet
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body io.Reader
	if request.Body != nil {
		body = bytes.NewReader(request.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, request.URL, body)
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("User-Agent", UserAgent)
	for key, value := range request.Headers {
		req.Header.Set(key, value)
	}
	client := shared
	if request.NoRedirect {
		copy := *shared // 只换重定向策略，Transport 和连接池仍然共用。
		copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &copy
	}
	resp, err := client.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return Response{Status: resp.StatusCode}, err
	}
	if int64(len(data)) > limit {
		return Response{Status: resp.StatusCode}, ErrTooLarge
	}
	return Response{Status: resp.StatusCode, Body: data}, nil
}

// GetJSON 获取并解码一个 JSON 文档。
func GetJSON(ctx context.Context, url string, timeout time.Duration, maxBytes int64, out any) error {
	response, err := Do(ctx, Request{URL: url, Timeout: timeout, MaxBytes: maxBytes})
	if err != nil {
		return err
	}
	if !response.OK() {
		return &StatusError{Status: response.Status}
	}
	if err := json.Unmarshal(response.Body, out); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// PostJSON 发送 JSON 请求体，返回原始响应。
func PostJSON(ctx context.Context, url string, headers map[string]string, payload any, timeout time.Duration, maxBytes int64) (Response, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Response{}, err
	}
	merged := map[string]string{"Content-Type": "application/json"}
	for key, value := range headers {
		merged[key] = value
	}
	return Do(ctx, Request{Method: http.MethodPost, URL: url, Headers: merged, Body: encoded, Timeout: timeout, MaxBytes: maxBytes})
}

// StatusError 表示非 2xx 的响应。
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return fmt.Sprintf("HTTP %d", e.Status) }

// Reason 把传输失败转成能发到聊天里的说明，不泄露 URL 和主机名。
//
// 给用户看的错误（实现 UserMessage，如 command.Fail）照原话返回：调用方常常把整段
// 「下载 + 解析 + 处理」的错误都交给它，一律说成「网络请求失败」的话，「素材目录格式不对」、
// ffmpeg 失败这些就都说错了。
func Reason(err error) string {
	var message interface{ UserMessage() string }
	if errors.As(err, &message) {
		return message.UserMessage()
	}
	var status *StatusError
	switch {
	case errors.As(err, &status):
		if status.Status == 429 {
			return "请求过于频繁，稍后再试"
		}
		return fmt.Sprintf("服务返回 HTTP %d", status.Status)
	case errors.Is(err, context.DeadlineExceeded):
		return "请求超时，稍后再试"
	case errors.Is(err, ErrTooLarge):
		return "响应数据过大"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "请求超时，稍后再试"
	}
	return "网络请求失败，稍后再试"
}

// DownloadFile 把 url 下载到 target（边下边写，不整个读进内存），返回内容的 SHA-256。
// 超过 limit 字节或状态码不是 200 都算失败。和 Do 共用一个 transport，超时 5 分钟。
// 用于下载新版本的程序文件这类几十 MB 的东西。
func DownloadFile(ctx context.Context, url, target string, limit int64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", UserAgent)
	response, err := shared.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", &StatusError{Status: response.StatusCode}
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, limit+1))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if written > limit {
		return "", ErrTooLarge
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
```

## 附录 E：更新状态存储完整代码

源文件：`internal/tgstate/state.go`。

```go
// Package tgstate 保存 gotd 的更新引擎跨重启需要的东西：账号的更新位置、
// 每个频道的 pts，以及账号见过的所有对象的 access hash。全放在一个 JSON
// 文件里，每秒最多写一次。这样重启后能从停下的地方接着收更新；某个对话
// 在那之后即使再没人说话，也还能编辑其中的消息。
package tgstate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gotd/td/telegram/updates"

	"github.com/MiCat-S/mibot-lite/internal/fsutil"
)

var (
	errNoState = errors.New("update state does not exist")
	ErrClosed  = errors.New("update state is closed")
)

type document struct {
	UserID   int64            `json:"userId"`
	Pts      int              `json:"pts"`
	Qts      int              `json:"qts"`
	Date     int              `json:"date"`
	Seq      int              `json:"seq"`
	HasState bool             `json:"hasState"`
	Channels map[string]int   `json:"channels"`
	Chans    map[string]int64 `json:"channelHashes"`
	Users    map[string]int64 `json:"userHashes"`
}

// State 基于一个 JSON 文件实现 updates.StateStorage、
// updates.ChannelAccessHasher 和 updates.UserAccessHasher。
type State struct {
	path   string
	mu     sync.Mutex
	doc    document
	dirty  bool
	timer  *time.Timer
	closed bool
	// OnFlushError 在后台写盘失败时调用（Open 之后、开始使用之前设置）。写不进去的话，
	// 重启后会从旧的 pts 补抓，或者丢掉 access hash，这不能悄悄发生。
	OnFlushError func(error)
}

// Open 加载文件；文件不存在就从空状态开始。
func Open(path string) (*State, error) {
	state := &State{path: path, doc: document{Channels: map[string]int{}, Chans: map[string]int64{}, Users: map[string]int64{}}}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &state.doc); err != nil {
			return nil, err
		}
		if state.doc.Channels == nil {
			state.doc.Channels = map[string]int{}
		}
		if state.doc.Chans == nil {
			state.doc.Chans = map[string]int64{}
		}
		if state.doc.Users == nil {
			state.doc.Users = map[string]int64{}
		}
	}
	return state, nil
}

// Close 把尚未写入的改动写到磁盘。
func (s *State) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	return s.flushLocked()
}

// Flush 立即写入尚未写入的改动。
func (s *State) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

// markDirty 在第一次改动时定一次一秒后的写盘，期间的改动合并进去；不在每次改动时
// 重新计时，否则更新不断时就一直不落盘。调用方持有 mu。
func (s *State) markDirty() {
	s.dirty = true
	if s.timer != nil || s.closed {
		return
	}
	s.timer = time.AfterFunc(time.Second, func() {
		s.mu.Lock()
		s.timer = nil
		if s.closed {
			s.mu.Unlock()
			return
		}
		err := s.flushLocked()
		if err != nil {
			// 写失败了就再定一次，即使之后没有新的改动也会重试。
			s.markDirty()
		}
		onError := s.OnFlushError
		s.mu.Unlock()
		if err != nil && onError != nil {
			onError(err)
		}
	})
}

func (s *State) writable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed {
		return ErrClosed
	}
	return nil
}

func (s *State) flushLocked() error {
	if !s.dirty {
		return nil
	}
	encoded, err := json.Marshal(s.doc)
	if err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(s.path, encoded, 0o600); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

func key(id int64) string { return strconv.FormatInt(id, 10) }

func (s *State) GetState(ctx context.Context, userID int64) (updates.State, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return updates.State{}, false, err
	}
	if !s.doc.HasState || s.doc.UserID != userID {
		return updates.State{}, false, nil
	}
	return updates.State{Pts: s.doc.Pts, Qts: s.doc.Qts, Date: s.doc.Date, Seq: s.doc.Seq}, true, nil
}

func (s *State) SetState(ctx context.Context, userID int64, state updates.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(ctx); err != nil {
		return err
	}
	s.doc.UserID, s.doc.HasState = userID, true
	s.doc.Pts, s.doc.Qts, s.doc.Date, s.doc.Seq = state.Pts, state.Qts, state.Date, state.Seq
	s.doc.Channels = map[string]int{}
	s.markDirty()
	return nil
}

func (s *State) setField(ctx context.Context, userID int64, set func()) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(ctx); err != nil {
		return err
	}
	if !s.doc.HasState || s.doc.UserID != userID {
		return errNoState
	}
	set()
	s.markDirty()
	return nil
}

func (s *State) SetPts(ctx context.Context, userID int64, pts int) error {
	return s.setField(ctx, userID, func() { s.doc.Pts = pts })
}
func (s *State) SetQts(ctx context.Context, userID int64, qts int) error {
	return s.setField(ctx, userID, func() { s.doc.Qts = qts })
}
func (s *State) SetDate(ctx context.Context, userID int64, date int) error {
	return s.setField(ctx, userID, func() { s.doc.Date = date })
}
func (s *State) SetSeq(ctx context.Context, userID int64, seq int) error {
	return s.setField(ctx, userID, func() { s.doc.Seq = seq })
}
func (s *State) SetDateSeq(ctx context.Context, userID int64, date, seq int) error {
	return s.setField(ctx, userID, func() { s.doc.Date, s.doc.Seq = date, seq })
}

func (s *State) GetChannelPts(ctx context.Context, userID, channelID int64) (int, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if s.doc.UserID != userID {
		return 0, false, nil
	}
	pts, ok := s.doc.Channels[key(channelID)]
	return pts, ok, nil
}

func (s *State) SetChannelPts(ctx context.Context, userID, channelID int64, pts int) error {
	return s.setField(ctx, userID, func() { s.doc.Channels[key(channelID)] = pts })
}

func (s *State) ForEachChannels(ctx context.Context, userID int64, visit func(ctx context.Context, channelID int64, pts int) error) error {
	s.mu.Lock()
	type entry struct {
		id  int64
		pts int
	}
	var entries []entry
	if s.doc.UserID == userID {
		for id, pts := range s.doc.Channels {
			parsed, err := strconv.ParseInt(id, 10, 64)
			if err == nil {
				entries = append(entries, entry{parsed, pts})
			}
		}
	}
	s.mu.Unlock()
	sort.Slice(entries, func(a, b int) bool { return entries[a].id < entries[b].id })
	for _, item := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(ctx, item.id, item.pts); err != nil {
			return err
		}
	}
	return nil
}

func (s *State) SetChannelAccessHash(ctx context.Context, userID, channelID, accessHash int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(ctx); err != nil {
		return err
	}
	if s.doc.Chans[key(channelID)] != accessHash {
		s.doc.Chans[key(channelID)] = accessHash
		s.markDirty()
	}
	return nil
}

func (s *State) GetChannelAccessHash(ctx context.Context, userID, channelID int64) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	hash, ok := s.doc.Chans[key(channelID)]
	return hash, ok, nil
}

func (s *State) SetUserAccessHash(ctx context.Context, userID, targetUserID, accessHash int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(ctx); err != nil {
		return err
	}
	if s.doc.Users[key(targetUserID)] != accessHash {
		s.doc.Users[key(targetUserID)] = accessHash
		s.markDirty()
	}
	return nil
}

func (s *State) GetUserAccessHash(ctx context.Context, userID, targetUserID int64) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	hash, ok := s.doc.Users[key(targetUserID)]
	return hash, ok, nil
}

// Dir 返回文件所在的目录。
func (s *State) Dir() string { return filepath.Dir(s.path) }
```

完整入口与注册表集成见 `internal/app/app.go`、`internal/command/command.go`；`cmd/mibot-lite/main.go` 已有 signal.NotifyContext，本次沿用。完整实现以工作区源文件为准。
