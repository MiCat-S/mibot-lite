# mibot-lite 逐文件代码审查

审查对象为提交 `0fb5f41` 上的本地工作树。共 242 个文件，其中有 129 个 Go 实现文件、105 个 Go 测试文件、4 个 Shell 脚本、1 个 Python 安装测试、1 个 systemd 单元和 2 个 Go 模块文件。

按文件核对函数入口、条件分支、调用关系和读写位置，直接展开协议、分页、配置发布、文件保存和定时调度等实现，结合测试断言确认行为。下表给出每个实现文件检查的行为，并列出每个测试文件中的代表用例。

## 验证结果

- macOS arm64 的 `go test ./...` 共覆盖 68 个包，66 个有测试的包通过，2 个包没有测试文件。按顶层用例计，587 个通过，1 个因缺少外部动画素材跳过（`TestRealAnimationAssets`）。
- `go vet ./...`、4 个 Shell 脚本的语法检查、5 个 Python 安装测试及工作树差异的空白检查均通过。构建成功的平台为 macOS arm64、Linux amd64 和 Linux arm64。macOS 二进制运行 `--version` 输出 `v0.3.10-dirty`；Linux 结果是交叉构建。
- 独立模块缓存的 `go mod verify` 全部通过。本机原有缓存中的 `go.opentelemetry.io/otel@v1.44.0` 缺少归档附带的 `CLAUDE.md`，因此直接检查原有缓存会报目录已修改。
- AI 验证使用本地 HTTP 服务接收实际请求，并返回 JSON、流式事件和错误响应。核对 Chat Completions 的 `messages` 与 Responses 的 `input`，以及两套接口各自的输出限额字段、图片格式和响应解析。[Chat Completions 接口](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create) · [Responses 接口](https://developers.openai.com/api/reference/resources/responses/methods/create)
- Telegram 消息和任务通过模拟 RPC 验证。真实账号收发、线上模型网关和 Linux systemd 启停仍缺少本次实机验证。

## 使用时仍需留意

同时发送两条修改同一插件配置的命令时，`.alias` 和 `.privacy` 的写盘顺序与运行时应用顺序可能不同。等上一条回执后再修改即可避开；重启会加载磁盘里的最终值。

## 启动、配置、存储与安装（56 个文件）

| 文件 | 核对内容 |
| --- | --- |
| [cmd/mibot-lite/main.go](../cmd/mibot-lite/main.go) | CLI 模式冲突、根目录锁、只读预检、版本和自检退出码。 |
| [cmd/mibot-lite/main_test.go](../cmd/mibot-lite/main_test.go) | `TestInvalidModeCombinationsAreRejectedBeforeBackup`（共 1 个顶层用例） |
| [deploy/mibot-lite.service](../deploy/mibot-lite.service) | 核对 ExecStart、可写目录、网络顺序、重启策略与媒体子进程内存上限。 |
| [go.mod](../go.mod) | 核对直接依赖、间接依赖和 Go 工具链声明。 |
| [go.sum](../go.sum) | 在独立模块缓存中校验锁定的依赖归档；项目使用的版本与 go.mod 配套。 |
| [internal/backup/backup.go](../internal/backup/backup.go) | 创建端现在拒绝缺账号的会话、无效账号配置、过多条目，以及包括 tar 头在内的超限归档。恢复先暂存再发布账号。 |
| [internal/backup/backup_test.go](../internal/backup/backup_test.go) | `TestRoundTripKeepsConfigurationAndDropsTheRest`、`TestOversizedArchiveIsRefused`、`TestCreateRefusesUnrestorableDeployment`（共 16 个顶层用例） |
| [internal/commands/alias/alias.go](../internal/commands/alias/alias.go) | 多词别名的分界和展开、写盘后发布。如果两次配置并发，注册表可能保留较旧的值，重启会从磁盘恢复。 |
| [internal/commands/alias/alias_test.go](../internal/commands/alias/alias_test.go) | `TestSplitAlias`、`TestRenderAliases`（共 2 个顶层用例） |
| [internal/commands/core/core.go](../internal/commands/core/core.go) | 根命令、帮助分组、别名查找、HTTPS 探测与目标校验。 |
| [internal/commands/core/core_test.go](../internal/commands/core/core_test.go) | `TestLookupForHelp`、`TestHelpListAndGroups`、`TestHelpRich`（共 4 个顶层用例） |
| [internal/commands/core/status.go](../internal/commands/core/status.go) | 资源采样、状态卡绘制互斥、图片发送和失败显示。 |
| [internal/commands/import_test.go](../internal/commands/import_test.go) | `TestImportMiBoxRefusesLockedDeployment`（共 1 个顶层用例） |
| [internal/commands/kit/kit.go](../internal/commands/kit/kit.go) | 统一错误、回执、限流等待、目标 ID 和 UTF-16 取段的调用约定。 |
| [internal/commands/kit/kit_test.go](../internal/commands/kit/kit_test.go) | `TestUTF16Helpers`（共 1 个顶层用例） |
| [internal/commands/kit/numbers_test.go](../internal/commands/kit/numbers_test.go) | `TestIsDigitsAndNumericID`（共 1 个顶层用例） |
| [internal/commands/log/log.go](../internal/commands/log/log.go) | 日志级别、筛选、条数上限和文件发送。 |
| [internal/commands/prefix/prefix.go](../internal/commands/prefix/prefix.go) | 前缀校验、去重、写入环境配置，以及运行时与环境变量来源的关系。 |
| [internal/commands/prefix/prefix_test.go](../internal/commands/prefix/prefix_test.go) | `TestCheckPrefix`、`TestNextPrefixes`（共 2 个顶层用例） |
| [internal/commands/privacy/privacy.go](../internal/commands/privacy/privacy.go) | 从文件读取并发布 IP 掩码。同一时间执行两条配置命令，运行时可能保留较旧策略，重启会从磁盘恢复。 |
| [internal/commands/privacy/privacy_test.go](../internal/commands/privacy/privacy_test.go) | `TestDescribe`（共 1 个顶层用例） |
| [internal/commands/readme_test.go](../internal/commands/readme_test.go) | `TestREADMEMatchesRegistry`、`TestCommandsHaveGroupAndDescription`（共 2 个顶层用例） |
| [internal/commands/register.go](../internal/commands/register.go) | 插件注册、MiBox 导入锁、只读启动和跳过目标已有设置。 |
| [internal/commands/register_test.go](../internal/commands/register_test.go) | `TestTextCommandsOnlyShowHelpForStandaloneHelp`、`TestImportMiBox`、`TestReadOnlyRegistrationWritesNothing`（共 4 个顶层用例） |
| [internal/commands/style_test.go](../internal/commands/style_test.go) | `TestCommandTextStyle`（共 1 个顶层用例） |
| [internal/config/config.go](../internal/config/config.go) | 账号与代理配置解析、文件大小、环境变量优先级和前缀写回。 |
| [internal/config/config_test.go](../internal/config/config_test.go) | `TestParseAcceptsMiBoxConfig`、`TestEnvFileAndPrefixes`、`TestSetEnvAppendsOrCreates`（共 6 个顶层用例） |
| [internal/fsutil/fsutil.go](../internal/fsutil/fsutil.go) | 同目录临时文件、权限、同步、关闭与原子替换的错误处理。 |
| [internal/fsutil/fsutil_test.go](../internal/fsutil/fsutil_test.go) | `TestWriteFileAtomicConcurrent`（共 1 个顶层用例） |
| [internal/login/login.go](../internal/login/login.go) | 交互提示前获取部署锁并校验代理，更换账号时清理旧会话状态。 |
| [internal/login/login_test.go](../internal/login/login_test.go) | `TestLoginRefusesLockedDeployment`、`TestWriteAccountReplacesSessionAndClearsAccountState`、`TestWriteAccountPreservesConfigOnSessionRemovalFailure`（共 4 个顶层用例） |
| [internal/logtail/logtail.go](../internal/logtail/logtail.go) | 供 .log 使用的日志环、脱敏、稳定匿名标识、轮转与级别筛选。 |
| [internal/logtail/logtail_test.go](../internal/logtail/logtail_test.go) | `TestNothingSensitiveReachesTheRing`、`TestRingWrapsOldestFirst`、`TestEnabledFollowsTheWrappedHandler`（共 9 个顶层用例） |
| [internal/mibox/sqlite.go](../internal/mibox/sqlite.go) | SQLite 页、记录、变长整数、溢出页及别名表导入。 |
| [internal/mibox/sqlite_test.go](../internal/mibox/sqlite_test.go) | `TestRejectsImpossibleSQLiteLengths`、`TestRejectsNonSQLite`、`TestVarint`（共 4 个顶层用例） |
| [internal/session/file.go](../internal/session/file.go) | gotd 会话首次读文件的错误语义与原子写回。 |
| [internal/session/gramjs.go](../internal/session/gramjs.go) | GramJS 会话编码、历史地址格式和 auth key 长度，导入已有会话时的处理。 |
| [internal/session/gramjs_test.go](../internal/session/gramjs_test.go) | `TestRoundTrip`、`TestDataCarriesKeyAndAddress`、`TestResolveAddressSkipsSpecialEndpoints`（共 7 个顶层用例） |
| [internal/store/store.go](../internal/store/store.go) | 成功写盘后才替换快照。只读消费者与需要修改副本的调用方使用不同的读取方法。 |
| [internal/store/store_test.go](../internal/store/store_test.go) | `TestReadMissingFileReturnsDefaults`、`TestFailedUpdateLeavesFileAlone`、`TestSnapshotRemainsValidDuringUpdate`（共 5 个顶层用例） |
| [internal/sysinfo/disk_other.go](../internal/sysinfo/disk_other.go) | 非 Unix 平台磁盘容量不可用时的返回值。 |
| [internal/sysinfo/disk_unix.go](../internal/sysinfo/disk_unix.go) | Statfs 成功与失败时的磁盘容量换算。 |
| [internal/sysinfo/format_test.go](../internal/sysinfo/format_test.go) | `TestFormatUptimeAndBytes`（共 1 个顶层用例） |
| [internal/sysinfo/kernel_linux.go](../internal/sysinfo/kernel_linux.go) | uname 返回的内核版本和字符串结束位置。 |
| [internal/sysinfo/kernel_other.go](../internal/sysinfo/kernel_other.go) | 非 Linux 平台的内核信息来源。 |
| [internal/sysinfo/resources.go](../internal/sysinfo/resources.go) | CPU 采样窗口、取消、内存、交换空间与系统发行版读取。 |
| [internal/sysinfo/sysinfo.go](../internal/sysinfo/sysinfo.go) | RSS、进程与主机信息来源，运行时长和单位换算。 |
| [internal/tgstate/cursor.go](../internal/tgstate/cursor.go) | 游标代号、过期游标、按时间写完整检查点和失败后重试。 |
| [internal/tgstate/cursor_test.go](../internal/tgstate/cursor_test.go) | `TestCursorFlushRecoversWithoutClose`、`TestCursorWriteVolume`、`TestRuntimeCheckpointsRecoveredIdleCursor`（共 8 个顶层用例） |
| [internal/tgstate/state.go](../internal/tgstate/state.go) | 游标与长期 access hash 的持久化、关闭后拒绝写入、失败时重试。 |
| [internal/tgstate/state_test.go](../internal/tgstate/state_test.go) | `TestStateSurvivesRestart`、`TestForEachChannelsIsOrdered`、`TestFailedFlushRetriesAndCallbackMayReadState`（共 7 个顶层用例） |
| [scripts/build.sh](../scripts/build.sh) | 核对静态构建、版本注入、输出路径和发布脚本调用方式。 |
| [scripts/install-service.sh](../scripts/install-service.sh) | 候选程序预检、单元模板渲染和目标部署归属。候选文件经过验证再替换，允许从部署目录里的同名程序安装。 |
| [scripts/install.sh](../scripts/install.sh) | 核对下载、候选预检、现有服务归属与切换失败恢复。只安装文件的模式现在无需 systemctl。 |
| [scripts/install_test.py](../scripts/install_test.py) | 5 个安装用例核对临时部署的预检、替换、启动失败恢复，以及只安装文件时对 systemd 的调用。 |
| [scripts/release.sh](../scripts/release.sh) | 检查三个发布平台、输出目录清理和更新器需要的校验清单。 |

## 运行时、命令派发与 Telegram 通信（43 个文件）

| 文件 | 核对内容 |
| --- | --- |
| [internal/app/app.go](../internal/app/app.go) | 准备部署、授权、绑定当前账号、双路更新派发和停机时的任务收尾。 |
| [internal/app/live.go](../internal/app/live.go) | 实时与补差更新共用消息去重，区分聊天、消息编号及进程启动秒。 |
| [internal/app/live_test.go](../internal/app/live_test.go) | `TestDispatchIdentityAndDroppedCommandLog`、`TestSentBeforeStart`、`TestLiveHandlerDispatchesAheadOfTheEngine`（共 4 个顶层用例） |
| [internal/app/prepare_test.go](../internal/app/prepare_test.go) | `TestReadOnlyPrepareCreatesNoFiles`、`TestPrepareLocksBeforeSessionImport`、`TestFailedPrepareReleasesLock`（共 4 个顶层用例） |
| [internal/app/subscribe.go](../internal/app/subscribe.go) | 重连后的订阅失败按次数退避，服从 Telegram 限流及 context 取消。 |
| [internal/app/subscribe_test.go](../internal/app/subscribe_test.go) | `TestSubscribeGuardBacksOffAndResets`、`TestSubscribeGuardHonoursFloodWait`、`TestSubscribeRetryDelayCapsAtOneMinute`（共 5 个顶层用例） |
| [internal/bot/client.go](../internal/bot/client.go) | 消息读写、回复话题、MTProto 投递错误和按对话过滤返回消息。 |
| [internal/bot/dialogs.go](../internal/bot/dialogs.go) | 已修复不足百条的分片提前结束。下一页游标按同一对话的消息编号和日期生成。 |
| [internal/bot/dialogs_test.go](../internal/bot/dialogs_test.go) | `TestEachDialog`、`TestEachDialogCursorUsesExactPeerAndServiceMessage`（共 2 个顶层用例） |
| [internal/bot/download.go](../internal/bot/download.go) | 媒体大小限制、资料照片选择、下载迁移到其他 DC 和临时文件重置。 |
| [internal/bot/entities.go](../internal/bot/entities.go) | RPC 返回的实体进入缓存，提及用户时按缓存解析 access hash。 |
| [internal/bot/entities_test.go](../internal/bot/entities_test.go) | `TestEntityRecorder`、`TestParseHTMLMentions`（共 2 个顶层用例） |
| [internal/bot/ipprivacy.go](../internal/bot/ipprivacy.go) | 检查 IP 掩码、链接过滤和 UTF-16 实体偏移经过编辑、发图及相册请求的传递。 |
| [internal/bot/ipprivacy_test.go](../internal/bot/ipprivacy_test.go) | `TestMaskIPs`、`TestIPRedactorRichMessages`、`TestIPRedactor`（共 4 个顶层用例） |
| [internal/bot/message.go](../internal/bot/message.go) | 核对收藏夹、用户与频道发送者、转发来源、选中引用和讨论话题。 |
| [internal/bot/message_test.go](../internal/bot/message_test.go) | `TestGetMessagesReturnsOnlyRequestedDialog`、`TestEscapeAndParseHTML`、`TestEnvelopeKeepsOtherPeopleIncoming`（共 14 个顶层用例） |
| [internal/bot/peers.go](../internal/bot/peers.go) | 缓存实体时区分 min 与完整对象，读取长期 access hash 并检查并发刷新路径。 |
| [internal/bot/peers_test.go](../internal/bot/peers_test.go) | `TestRememberUsersWritesThrough`、`TestDurableWithoutWriterIsSafe`、`TestInputPeerConcurrentRefresh`（共 6 个顶层用例） |
| [internal/bot/retrier.go](../internal/bot/retrier.go) | 按 RPC 方法共享等待；临时限制临近过期时避免再次提交；检查取消与随机消息编号。 |
| [internal/bot/retrier_test.go](../internal/bot/retrier_test.go) | `TestRetrier`、`TestRetrierCancellationDuringSleep`、`TestRetrierRetainsSendRandomID`（共 6 个顶层用例） |
| [internal/bot/rich.go](../internal/bot/rich.go) | 富消息的获取、发送、长度限制，以及确定拒绝时的纯文本处理。 |
| [internal/bot/rich_privacy.go](../internal/bot/rich_privacy.go) | 富文本节点、分段拼出的地址和按钮链接均走发送前的隐私处理。 |
| [internal/bot/rich_test.go](../internal/bot/rich_test.go) | `TestSendRich`、`TestRichFits`、`TestCopyRich`（共 7 个顶层用例） |
| [internal/bot/sent.go](../internal/bot/sent.go) | 从发送回执取实际消息编号；自检只观察本次调用产生的回复。 |
| [internal/bot/sent_test.go](../internal/bot/sent_test.go) | `TestSentRecorder`（共 1 个顶层用例） |
| [internal/botapi/client.go](../internal/botapi/client.go) | 分开处理 429、明确拒绝和投递结果未知，检查分聊天与全局配额。 |
| [internal/botapi/client_test.go](../internal/botapi/client_test.go) | `TestPayloadAndResponseOK`、`TestDoesNotReplayAmbiguousSend`、`TestCancelWhileRateLimited`（共 8 个顶层用例） |
| [internal/command/abort_test.go](../internal/command/abort_test.go) | `TestAbortCancelsRunningCommands`（共 1 个顶层用例） |
| [internal/command/command.go](../internal/command/command.go) | 前缀、别名、派发身份、命令超时和更新前的任务暂停。 |
| [internal/command/command_test.go](../internal/command/command_test.go) | `TestParseRoutes`、`TestAliasExpandsAndKeepsTheArguments`、`TestRawAfter`（共 14 个顶层用例） |
| [internal/command/dispatch_test.go](../internal/command/dispatch_test.go) | `TestDispatchShowsErrors`、`TestDispatchShowsHelp`（共 2 个顶层用例） |
| [internal/command/errors.go](../internal/command/errors.go) | 用户可读错误与内部日志的边界，以及包装错误的原因链。 |
| [internal/command/scheduler.go](../internal/command/scheduler.go) | 任务准入、容量限制、取消时释放资源及空闲等待。 |
| [internal/command/scheduler_test.go](../internal/command/scheduler_test.go) | `TestSchedulerBoundsExecutionAndQueue`、`TestSubmitReportsRejectionOnFullQueue`、`TestQuiesceTimeoutReopens`（共 12 个顶层用例） |
| [internal/command/text.go](../internal/command/text.go) | 按 UTF-16 分页，HTML 实体、链接和跨页标签成对处理。 |
| [internal/command/text_test.go](../internal/command/text_test.go) | `TestEscape`、`TestHTMLPagesReopenTags`、`TestPagesCountUTF16Units`（共 7 个顶层用例） |
| [internal/delivery/delivery.go](../internal/delivery/delivery.go) | 投递结果未知与普通失败的区分，包装后仍可检查原因。 |
| [internal/delivery/delivery_test.go](../internal/delivery/delivery_test.go) | `TestUnknownKeepsMessageAndChain`（共 1 个顶层用例） |
| [internal/httpx/httpx.go](../internal/httpx/httpx.go) | 带凭证重定向、响应大小、总超时与等待 AI 响应头的连接池。 |
| [internal/httpx/httpx_test.go](../internal/httpx/httpx_test.go) | `TestConnectionReuseAndBodyLimit`、`TestRequestCancellation`、`TestSlowHeadersUseRequestTimeout`（共 4 个顶层用例） |
| [internal/httpx/reason_test.go](../internal/httpx/reason_test.go) | `TestReason`（共 1 个顶层用例） |
| [internal/verify/verify.go](../internal/verify/verify.go) | 自检命令与本次产生的回复相绑定；检查轮询、结果分类及清理对象。 |
| [internal/verify/verify_test.go](../internal/verify/verify_test.go) | `TestStatusVerificationTracksOnlyItsOwnAnswer`（共 1 个顶层用例） |

## AI、摘要与翻译（28 个文件）

| 文件 | 核对内容 |
| --- | --- |
| [internal/commands/ai/ai.go](../internal/commands/ai/ai.go) | 服务商增删、模型选择、私聊配置凭证及新建完整 Responses 地址的初始模式。 |
| [internal/commands/ai/answer.go](../internal/commands/ai/answer.go) | 搜索工具、引用来源、会话发送，以及富文本和 HTML 分页的选择。 |
| [internal/commands/ai/check.go](../internal/commands/ai/check.go) | 模型分页与探测认证；账单接口拒绝访问时，仍按模型接口的实际结果判断 Key。 |
| [internal/commands/ai/check_test.go](../internal/commands/ai/check_test.go) | `TestModelsListing`、`TestImportProvider`、`TestOpenAIBillingPermissionIndependentOfModelAccess`（共 10 个顶层用例） |
| [internal/commands/ai/config_test.go](../internal/commands/ai/config_test.go) | `TestConfigSet`、`TestConfigSetRejects`（共 2 个顶层用例） |
| [internal/commands/ai/failure.go](../internal/commands/ai/failure.go) | 保留错误链、请求阶段与超时信息，用户消息和日志中的密钥处理。 |
| [internal/commands/ai/failure_test.go](../internal/commands/ai/failure_test.go) | `TestRequestFailureLogsContextWithoutCredentials`、`TestRequestFailureKeepsTimeoutCause`（共 2 个顶层用例） |
| [internal/commands/ai/images.go](../internal/commands/ai/images.go) | 回复和相册中的图片合并、下载限额、解码及格式转换。 |
| [internal/commands/ai/markdown.go](../internal/commands/ai/markdown.go) | Markdown 到 Telegram HTML、Telegraph 节点，链接、代码块、表格与分页。 |
| [internal/commands/ai/markdown_test.go](../internal/commands/ai/markdown_test.go) | `TestMarkdownToHTML`、`TestHTMLPages`、`TestTelegraphNodes`（共 6 个顶层用例） |
| [internal/commands/ai/protocol_test.go](../internal/commands/ai/protocol_test.go) | `TestSearchProtocols`、`TestNativeProviderDoesNotRepeatFailedSummary`、`TestQueryKeyRedactedOnFailure`（共 6 个顶层用例） |
| [internal/commands/ai/provider.go](../internal/commands/ai/provider.go) | Chat Completions、Responses、Gemini、Anthropic 的端点、请求体和认证；自动识别网关完整原生端点。 |
| [internal/commands/ai/provider_test.go](../internal/commands/ai/provider_test.go) | `TestComposeQuestion`、`TestChatFallsBackToChatCompletions`、`TestImageHelpers`（共 10 个顶层用例） |
| [internal/commands/ai/response.go](../internal/commands/ai/response.go) | JSON 与 SSE 的文本、拒答、终止事件、截断和失败；避免最终快照与增量重复。 |
| [internal/commands/ai/rich.go](../internal/commands/ai/rich.go) | 用同一份回答生成富消息，核对标题、折叠、列表、引用与表格。 |
| [internal/commands/ai/rich_test.go](../internal/commands/ai/rich_test.go) | `TestMarkdownRich`、`TestFoldRichSections`、`TestAnswerRich`（共 3 个顶层用例） |
| [internal/commands/ai/service.go](../internal/commands/ai/service.go) | 供摘要与翻译复用的模型选择、超时、Responses 回退，以及 MiBox 的模式迁移。 |
| [internal/commands/checkapi/checkapi.go](../internal/commands/checkapi/checkapi.go) | 服务商诊断、询问、比较，检查共享 AI 服务的结果和命令回执。 |
| [internal/commands/checkapi/checkapi_test.go](../internal/commands/checkapi/checkapi_test.go) | `TestSubcommands`、`TestMigrateWithoutFile`、`TestMigrateSkipsRejected`（共 7 个顶层用例） |
| [internal/commands/checkapi/migrate.go](../internal/commands/checkapi/migrate.go) | 从旧 Key 记录补齐服务商和模型，处理拒绝条目、重复导入与落盘失败。 |
| [internal/commands/gt/gt.go](../internal/commands/gt/gt.go) | 翻译输入、回复取词、共享 AI 模型、长译文分段和取消。 |
| [internal/commands/sum/config.go](../internal/commands/sum/config.go) | 修改、删除服务商和选择默认服务商时，在同一次存储更新里读取最新记录。 |
| [internal/commands/sum/prompt.go](../internal/commands/sum/prompt.go) | 核对内置 Markdown 提示词、历史默认值的迁移，以及私聊和普通群缺少可跳转链接时的来源说明。 |
| [internal/commands/sum/sum.go](../internal/commands/sum/sum.go) | 完整端点决定请求协议。保留网关前缀；历史分页使用系统消息的编号推进；只为频道和超级群生成来源链接；按字符截断摘要。 |
| [internal/commands/sum/sum_test.go](../internal/commands/sum/sum_test.go) | `TestSumCronParsing`、`TestSumAutoGeminiFallbackUsesLegacyChatPath`、`TestSumFailedConfigWriteKeepsSchedule`（共 23 个顶层用例） |
| [internal/commands/sum/tasks.go](../internal/commands/sum/tasks.go) | Cron 解析、任务落盘和调度。删除、停用或重新编号写盘失败时保留原定时项；新增任务注册失败时仅撤回该任务。 |
| [internal/commands/tr/tr.go](../internal/commands/tr/tr.go) | 翻译目标语言、输入方向、两种上游返回格式及长结果发送。 |
| [internal/commands/tr/tr_test.go](../internal/commands/tr/tr_test.go) | `TestParseTranslation`、`TestHasHan`、`TestLanguageName`（共 4 个顶层用例） |

## 定时任务、自动化、搜索与更新（27 个文件）

| 文件 | 核对内容 |
| --- | --- |
| [internal/commands/acn/acn.go](../internal/commands/acn/acn.go) | 查询天气期间配置已变会丢弃旧结果；关闭与正在处理的改名相遇时，用已保存的原名恢复。 |
| [internal/commands/acn/acn_test.go](../internal/commands/acn/acn_test.go) | `TestApplyTextStyle`、`TestComposeNameHonoursDisplayComponents`、`TestClockEmoji`（共 15 个顶层用例） |
| [internal/commands/acn/recovery_test.go](../internal/commands/acn/recovery_test.go) | `TestScheduledUpdateIgnoresForeignAccountConfig`、`TestDisableDuringProfileUpdateRestoresOriginal`（共 2 个顶层用例） |
| [internal/commands/acn/zones.go](../internal/commands/acn/zones.go) | 时区加载、冬夏时制、分钟偏移与未知缩写的显示。 |
| [internal/commands/acron/acron.go](../internal/commands/acron/acron.go) | 定时任务增删、启停、别名递归以及已保存配置与调度器的衔接。 |
| [internal/commands/acron/acron_test.go](../internal/commands/acron/acron_test.go) | `TestParseCron`、`TestExecuteDeleteMatching`、`TestHelpStyle`（共 16 个顶层用例） |
| [internal/commands/acron/data.go](../internal/commands/acron/data.go) | 导入任务、数字形态的 ID、毫秒时间、目标对话和消息链接。 |
| [internal/commands/acron/entities.go](../internal/commands/acron/entities.go) | 往返转换 UTF-16 实体与数字 ID，发送提及时从缓存取用户信息。 |
| [internal/commands/acron/run.go](../internal/commands/acron/run.go) | 任务执行前重新读取配置，核对目标身份、话题、消息来源和维护期间的准入。 |
| [internal/commands/bf/bf.go](../internal/commands/bf/bf.go) | 从部署根目录生成备份，检查发送位置、附件说明和恢复入口。 |
| [internal/commands/bf/bf_test.go](../internal/commands/bf/bf_test.go) | `TestBackupCaptionFits`、`TestBackupHelpSaysWhereItGoesAndHowToRestore`（共 2 个顶层用例） |
| [internal/commands/checkin/checkin.go](../internal/commands/checkin/checkin.go) | 每日计划、跨午夜时段、匹配器与任务准入。设置推送机器人凭证只接受收藏夹命令。 |
| [internal/commands/checkin/checkin_test.go](../internal/commands/checkin/checkin_test.go) | `TestParseClockAndMatcher`、`TestSignOneFailures`、`TestRefusesKeyOutsideSavedMessages`（共 14 个顶层用例） |
| [internal/commands/checkin/recovery_test.go](../internal/commands/checkin/recovery_test.go) | `TestScheduledCheckinTracksAndResumesUntouchedTargets`、`TestCanceledCheckinBeforeAttemptDoesNotConsumeTarget`、`TestCanceledTargetResolutionDoesNotConsumeAttempt`（共 5 个顶层用例） |
| [internal/commands/checkin/sign.go](../internal/commands/checkin/sign.go) | 确认目标后记录尝试，等待回复与按钮，并按投递结果决定能否补发汇总。 |
| [internal/commands/restart/restart.go](../internal/commands/restart/restart.go) | 待重启回执、目标 peer 与话题、启动后更新回执的条件。 |
| [internal/commands/restart/restart_test.go](../internal/commands/restart/restart_test.go) | `TestScheduleKeepsReceiptWhenInterrupted`、`TestNotifyReadySendsIntoTopic`、`TestReceiptSelfMustBeOwnChat`（共 8 个顶层用例） |
| [internal/commands/search/match.go](../internal/commands/search/match.go) | 关键词规整、Unicode 空白、视频、广告过滤及排序。 |
| [internal/commands/search/search.go](../internal/commands/search/search.go) | 搜索顺序、讨论区部分结果、去重和来源配置；清理失效频道时，写盘错误现在进入日志。 |
| [internal/commands/search/search_test.go](../internal/commands/search/search_test.go) | `TestParseOptions`、`TestSearchFallbackAndPrune`、`TestUnicodeSpaces`（共 18 个顶层用例） |
| [internal/commands/search/telegram.go](../internal/commands/search/telegram.go) | 对话查找、关联讨论区、相册、转发与重新上传的临时文件清理。 |
| [internal/commands/service/service.go](../internal/commands/service/service.go) | systemd 单元名、cgroup 解析、受限输出与状态报告。 |
| [internal/commands/service/service_test.go](../internal/commands/service/service_test.go) | `TestUnitName`、`TestReportRunning`、`TestLimitedWriter`（共 7 个顶层用例） |
| [internal/commands/update/auto.go](../internal/commands/update/auto.go) | 每日检查策略、忙碌时等待、维护准入及重启回执。 |
| [internal/commands/update/path_linux_test.go](../internal/commands/update/path_linux_test.go) | `TestUpdateKeepsInstallationPathAfterRename`（共 1 个顶层用例）。构建条件为 `linux` |
| [internal/commands/update/update.go](../internal/commands/update/update.go) | 发布资产和校验清单、候选程序预检、安装路径与上一个可回滚版本。 |
| [internal/commands/update/update_test.go](../internal/commands/update/update_test.go) | `TestReinstallBeforeRestartKeepsRollback`、`TestStrictlyNewer`、`TestQuiesceBlocksWorkAfterIdleCheck`（共 9 个顶层用例） |

## 群管理、消息规则与清理（27 个文件）

| 文件 | 核对内容 |
| --- | --- |
| [internal/commands/aban/aban.go](../internal/commands/aban/aban.go) | 批量封禁、临时禁言和踢出后的解禁；核对目标保护、到期时间及失败统计。 |
| [internal/commands/aban/aban_test.go](../internal/commands/aban/aban_test.go) | `TestParseDuration`（共 1 个顶层用例） |
| [internal/commands/aban/resolve.go](../internal/commands/aban/resolve.go) | 当前对话、近期成员、管理群的查找顺序，以及回复里的发送者身份。 |
| [internal/commands/aban/resolve_test.go](../internal/commands/aban/resolve_test.go) | `TestFindUserFromCacheSendsNothing`、`TestRefusesSelf`、`TestUnresolvedIDMessage`（共 17 个顶层用例） |
| [internal/commands/cleanmember/cleanmember.go](../internal/commands/cleanmember/cleanmember.go) | 筛选条件、删除限额、普通群与频道权限、进度回执。 |
| [internal/commands/cleanmember/cleanmember_test.go](../internal/commands/cleanmember/cleanmember_test.go) | `TestParseRequest`、`TestCleanDoesNotSkipShiftedMembers`、`TestBasicGroup`（共 10 个顶层用例） |
| [internal/commands/cleanmember/members.go](../internal/commands/cleanmember/members.go) | 成员分页、在线状态以及删除导致的页内位置变化。 |
| [internal/commands/cleanmember/report.go](../internal/commands/cleanmember/report.go) | CSV 转义、缓存文件读写和过期清理。 |
| [internal/commands/da/da.go](../internal/commands/da/da.go) | 长任务名额、状态保存、分页删除及取消后的回收。 |
| [internal/commands/da/da_test.go](../internal/commands/da/da_test.go) | `TestIsAdminFallsBackToAdminList`（共 1 个顶层用例） |
| [internal/commands/da/recovery_test.go](../internal/commands/da/recovery_test.go) | `TestStatusDoesNotOverwriteProgress`（共 1 个顶层用例） |
| [internal/commands/da/slot_test.go](../internal/commands/da/slot_test.go) | `TestCanceledBeforeRunReleasesSlot`、`TestStartRefusesBeyondLimit`（共 2 个顶层用例） |
| [internal/commands/dme/dme.go](../internal/commands/dme/dme.go) | 按本人、话题和消息边界执行删除。失败通知现在跟随命令取消，通知也失败时向派发器返回原错误。 |
| [internal/commands/dme/dme_test.go](../internal/commands/dme/dme_test.go) | `TestGeneralTopicDoesNotDeleteOtherTopics`、`TestScrubReuploadsWhenReferenceExpires`、`TestScrubKeepsPhotoOnOtherFailures`（共 4 个顶层用例） |
| [internal/commands/dme/trigger.go](../internal/commands/dme/trigger.go) | 触发词匹配、同一对话的任务名额和维护时跳过执行。 |
| [internal/commands/dme/trigger_test.go](../internal/commands/dme/trigger_test.go) | `TestTriggerIndexMatch`、`TestOfferStartsOneRunPerChat`、`TestTriggersSurviveRestart`（共 6 个顶层用例） |
| [internal/commands/guestban/guestban.go](../internal/commands/guestban/guestban.go) | 配置生效时机，后台清理与封禁前重新确认设置。 |
| [internal/commands/guestban/guestban_test.go](../internal/commands/guestban/guestban_test.go) | `TestGuestBotCleanup`、`TestFailedSettingsWriteDoesNotEnableCleanup`、`TestAllowedAndUsername`（共 6 个顶层用例） |
| [internal/commands/keyword/busy_test.go](../internal/commands/keyword/busy_test.go) | `TestKeywordActionCountsAsBusy`（共 1 个顶层用例） |
| [internal/commands/keyword/keyword.go](../internal/commands/keyword/keyword.go) | 任务配置增删、当前配置展示与注册的监听入口。 |
| [internal/commands/keyword/keyword_test.go](../internal/commands/keyword/keyword_test.go) | `TestParseTask`、`TestOfferContinuesAfterSendFailure`、`TestOfferDropsWhenBusy`（共 12 个顶层用例） |
| [internal/commands/keyword/task.go](../internal/commands/keyword/task.go) | 导入和解析任务，处理正则表达式、动作类型及限制时长。 |
| [internal/commands/keyword/trigger.go](../internal/commands/keyword/trigger.go) | 核对匿名管理员和发送者身份、队列占用、各动作的失败处理。 |
| [internal/commands/sudo/sudo.go](../internal/commands/sudo/sudo.go) | 代发白名单、触发来源、授权撤销，以及排队后再次核对权限。 |
| [internal/commands/sudo/sudo_test.go](../internal/commands/sudo/sudo_test.go) | `TestSureRuleMatching`、`TestDelegationAllowed`、`TestQueuedDelegationHonorsRevocation`（共 3 个顶层用例） |
| [internal/commands/trace/trace.go](../internal/commands/trace/trace.go) | 表情规则现在读取成功写入的快照，落盘失败继续使用原有规则。 |
| [internal/commands/trace/trace_test.go](../internal/commands/trace/trace_test.go) | `TestParse`、`TestOfferReacts`、`TestFailedWriteKeepsActiveReactions`（共 6 个顶层用例） |

## 网络、查询与实用命令（35 个文件）

| 文件 | 核对内容 |
| --- | --- |
| [internal/commands/aff/aff.go](../internal/commands/aff/aff.go) | 旧单条记录与新列表格式的合并、排序、命令参数和展示分页。 |
| [internal/commands/aff/aff_test.go](../internal/commands/aff/aff_test.go) | `TestEntitiesHTML`、`TestEntryFormatsAndJSON`、`TestPagesAndList`（共 7 个顶层用例） |
| [internal/commands/aff/html.go](../internal/commands/aff/html.go) | 从 Telegram 实体生成 HTML，处理重叠、Unicode 偏移和转义。 |
| [internal/commands/bgp/bgp.go](../internal/commands/bgp/bgp.go) | 命令输入解析以及 IP、前缀和 ASN 查询的选择。 |
| [internal/commands/bgp/bgp_test.go](../internal/commands/bgp/bgp_test.go) | `TestParseTarget`、`TestLookupASN`、`TestRegister`（共 14 个顶层用例） |
| [internal/commands/bgp/render.go](../internal/commands/bgp/render.go) | 展示部分公布的网段、不可用数据、ASN 和 PTR 结果。 |
| [internal/commands/bgp/ripestat.go](../internal/commands/bgp/ripestat.go) | 并行查询各数据源，区分部分查询失败与完全无法给出结果。 |
| [internal/commands/bgp/target.go](../internal/commands/bgp/target.go) | IP、CIDR 和 ASN 的输入边界，保留地址与正文提取。 |
| [internal/commands/bin/bin.go](../internal/commands/bin/bin.go) | 从输入提取 BIN，发出的前缀最长八位，处理接口状态和返回字段。 |
| [internal/commands/bin/bin_test.go](../internal/commands/bin/bin_test.go) | `TestBINDigitsNeverSendsMoreThanEight`、`TestRenderBINFromARealResponse`（共 2 个顶层用例） |
| [internal/commands/calc/calc.go](../internal/commands/calc/calc.go) | 算术词法、运算顺序、数值格式和非法输入。 |
| [internal/commands/calc/calc_test.go](../internal/commands/calc/calc_test.go) | `TestCalcEvaluates`、`TestCalcFormatsLikeMiBox`、`TestCalcRejects`（共 3 个顶层用例） |
| [internal/commands/ddg/ddg.go](../internal/commands/ddg/ddg.go) | 搜索参数、HTML 结果解析、来源链接和主搜索受限时的补充查询。 |
| [internal/commands/ddg/ddg_test.go](../internal/commands/ddg/ddg_test.go) | `TestParseArgs`、`TestSearchFallsBackWhenBlocked`、`TestRender`（共 7 个顶层用例） |
| [internal/commands/dig/dig.go](../internal/commands/dig/dig.go) | 参数边界、命令输出上限和执行超时；核对转交用户对 DNS 目标的限制。 |
| [internal/commands/dig/dig_test.go](../internal/commands/dig/dig_test.go) | `TestParseArgsBuildsDigCommandLine`、`TestAddresses`、`TestRenderAndTruncation`（共 11 个顶层用例） |
| [internal/commands/dig/location.go](../internal/commands/dig/location.go) | 对响应中的地址去重、过滤、批量查询位置并附加结果。 |
| [internal/commands/encode/encode.go](../internal/commands/encode/encode.go) | Base64 和 URI 往返、非法编码、结果转义及预览长度。 |
| [internal/commands/encode/encode_test.go](../internal/commands/encode/encode_test.go) | `TestRoundTrip`、`TestDecodeBase64`、`TestRegister`（共 6 个顶层用例） |
| [internal/commands/ids/ids.go](../internal/commands/ids/ids.go) | 参数或回复的身份解析、建号时间估算和数据缺失时的展示。 |
| [internal/commands/ids/ids_test.go](../internal/commands/ids/ids_test.go) | `TestEstimateCreation`、`TestRenderIDsUnknownUser`、`TestResolveArgument`（共 4 个顶层用例） |
| [internal/commands/ip/ip.go](../internal/commands/ip/ip.go) | 地址提取、回复查询，以及返回状态与地理字段的解析。 |
| [internal/commands/ip/ip_test.go](../internal/commands/ip/ip_test.go) | `TestFindAddress`、`TestReplyQuery`、`TestRenderIPFromARealResponse`（共 3 个顶层用例） |
| [internal/commands/rate/currencies.go](../internal/commands/rate/currencies.go) | 法币、加密币与常用别名的数据映射。 |
| [internal/commands/rate/rate.go](../internal/commands/rate/rate.go) | 法币和加密币的直接或中转报价；美元参考价查询失败时不再展示虚假的零价格。 |
| [internal/commands/rate/rate_test.go](../internal/commands/rate/rate_test.go) | `TestParseRateArgs`、`TestFormatPriceAndAmount`（共 2 个顶层用例） |
| [internal/commands/speedtest/report_test.go](../internal/commands/speedtest/report_test.go) | `TestExternalToolSkipsNonOoklaSpeedtest`、`TestRenderShowsTheNewFields`、`TestConvertMiBox`（共 5 个顶层用例） |
| [internal/commands/speedtest/speedtest.go](../internal/commands/speedtest/speedtest.go) | Ookla CLI 识别、归档校验、服务器选择、限流和结果图来源。 |
| [internal/commands/speedtest/speedtest_test.go](../internal/commands/speedtest/speedtest_test.go) | `TestMaskAddress`、`TestFailureDetailReadsBothStreams`、`TestSpeedtestHelpDocumentsEverySubcommand`（共 12 个顶层用例） |
| [internal/commands/subinfo/fetch.go](../internal/commands/subinfo/fetch.go) | 订阅获取、重定向目标、响应体大小和来源信息。 |
| [internal/commands/subinfo/parse.go](../internal/commands/subinfo/parse.go) | 订阅格式、节点名称、流量头和数值溢出。 |
| [internal/commands/subinfo/subinfo.go](../internal/commands/subinfo/subinfo.go) | 并发查询、链接数量、报告截断和长结果作为附件发送。 |
| [internal/commands/subinfo/subinfo_test.go](../internal/commands/subinfo/subinfo_test.go) | `TestParseClashYAML`、`TestFetchGuard`、`TestLongResultBecomesFile`（共 14 个顶层用例） |
| [internal/commands/whois/whois.go](../internal/commands/whois/whois.go) | 域名规整、上游 HTTP 状态、到期日与剩余时间显示。 |
| [internal/commands/whois/whois_test.go](../internal/commands/whois/whois_test.go) | `TestNormalizeDomain`、`TestWhoisQueryReturnsUpstreamStatus`、`TestWhoisReportExtractsFields`（共 4 个顶层用例） |

## 媒体、图像、消息保存与复读（26 个文件）

| 文件 | 核对内容 |
| --- | --- |
| [internal/commands/eatgif/eat.go](../internal/commands/eatgif/eat.go) | 配置读取和重新下载素材时的缓存清除失败现在会报告；核对头像、素材 URL 和印章合成。 |
| [internal/commands/eatgif/eat_test.go](../internal/commands/eatgif/eat_test.go) | `TestEatRoot`、`TestEatAssetURL`、`TestStamp`（共 3 个顶层用例） |
| [internal/commands/eatgif/eatgif.go](../internal/commands/eatgif/eatgif.go) | 素材目录、帧缓存、并发生成上限，以及临时帧交给 ffmpeg 的路径。 |
| [internal/commands/eatgif/eatgif_test.go](../internal/commands/eatgif/eatgif_test.go) | `TestFrameCanvasDecodesPNGAndJPEG`、`TestFetchFreshFallsBackToStaleCopy`、`TestSafeRelative`（共 3 个顶层用例） |
| [internal/commands/re/re.go](../internal/commands/re/re.go) | 从回复开始复读，排除触发消息、保留话题并区分转发拒绝。 |
| [internal/commands/re/re_test.go](../internal/commands/re/re_test.go) | `TestRepeatForwardsFromTheRepliedMessageOn`、`TestRepeatOutsideForumsHasNoTopic`、`TestRepeatExcludesOwnCommandAndTrigger`（共 4 个顶层用例） |
| [internal/commands/save/recovery_test.go](../internal/commands/save/recovery_test.go) | `TestLostAlbumResponseDoesNotReplay`、`TestRestrictedAlbumStillCopiesMessages`、`TestRestrictedRichMessageIsCopied`（共 4 个顶层用例） |
| [internal/commands/save/save.go](../internal/commands/save/save.go) | 单个消息范围、相册投递结果、本地文件名长度和重名保存。创建目标名遇到真实文件系统错误会返回。 |
| [internal/commands/save/save_test.go](../internal/commands/save/save_test.go) | `TestParseLink`、`TestConvertMiBox`、`TestPublishLocalKeepsBothDownloads`（共 9 个顶层用例） |
| [internal/commands/sticker/sticker.go](../internal/commands/sticker/sticker.go) | 回复贴纸解析、查找和创建贴纸包、容量已满时换包。 |
| [internal/commands/sticker/sticker_test.go](../internal/commands/sticker/sticker_test.go) | `TestSaveFindsOrCreatesAPack`、`TestStickerOf`、`TestValidPackName`（共 4 个顶层用例） |
| [internal/commands/tts/tts.go](../internal/commands/tts/tts.go) | 文本清理、音色、合成响应、音频发送和配置持久化。设置语音服务密钥只接受收藏夹命令。 |
| [internal/commands/tts/tts_test.go](../internal/commands/tts/tts_test.go) | `TestCleanText`、`TestReadsMiBoxData`、`TestRefusesKeyOutsideSavedMessages`（共 7 个顶层用例） |
| [internal/commands/yvlu/yvlu.go](../internal/commands/yvlu/yvlu.go) | 引用消息的作者和实体偏移，头像、媒体和消息数量上限。 |
| [internal/commands/yvlu/yvlu_test.go](../internal/commands/yvlu/yvlu_test.go) | `TestDescribeVoiceUnpacksWaveform`、`TestNameHashIsStableAndPositive`、`TestYvluRefusesMoreThanFiveMessages`（共 9 个顶层用例） |
| [internal/imaging/assets_test.go](../internal/imaging/assets_test.go) | `TestRealAnimationAssets`（共 1 个顶层用例） |
| [internal/imaging/imaging.go](../internal/imaging/imaging.go) | 解码前的像素限额、PNG 编码、缩放裁剪、旋转与透明通道。 |
| [internal/imaging/imaging_test.go](../internal/imaging/imaging_test.go) | `TestEncodeDecodeRoundTrip`、`TestBrightnessClampsToAlpha`、`TestOpacity`（共 11 个顶层用例） |
| [internal/media/ffmpeg.go](../internal/media/ffmpeg.go) | 临时工作目录、ffmpeg 参数、超时、编码失败与输出读取。 |
| [internal/media/sticker.go](../internal/media/sticker.go) | 贴纸尺寸、WebP 和 WebM 的 MIME、文档名称与视频属性。 |
| [internal/media/sticker_test.go](../internal/media/sticker_test.go) | `TestResizeSticker`、`TestStickerDocument`（共 2 个顶层用例） |
| [internal/media/webm.go](../internal/media/webm.go) | WebM 容器的属性解析及不完整输入处理。 |
| [internal/media/webm_test.go](../internal/media/webm_test.go) | `TestWebMInfo`（共 1 个顶层用例） |
| [internal/statuscard/card.go](../internal/statuscard/card.go) | 字体加载、资源指标、告警颜色和状态卡输出。 |
| [internal/statuscard/card_test.go](../internal/statuscard/card_test.go) | `TestRender`、`TestHealthThresholds`（共 2 个顶层用例） |
| [internal/statuscard/font_test.go](../internal/statuscard/font_test.go) | `TestFontCoversCardText`（共 1 个顶层用例） |
