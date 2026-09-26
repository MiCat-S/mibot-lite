# MiBot Lite

**简体中文** | [English](README.en.md) | [繁體中文](README.zh-TW.md) | [日本語](README.ja.md)

一个省内存的 Telegram UserBot：一个静态 Go 二进制，命令编译在程序里，状态存成 JSON 文件。

它是 [MiBox](https://github.com/MiCat-S/Mi-Box) 的轻量替身：账号、会话、`config.json` 和命令名都一样，
可以从 MiBox 直接切过来，配置也能一条命令搬走。去掉的是插件系统：没有 JavaScript 运行时，
没有 SQLite，常驻内存不到 Node 版 MiBox 的一半。

| | MiBox（Node） | MiBot Lite |
|---|---|---|
| 常驻内存（RSS） | 约 125 MB | **约 30–50 MB** |
| 安装体积 | node_modules，约 237 个包 | **一个约 21 MB 的文件** |
| 插件运行时 | V8 | 无 |
| 数据存储 | better-sqlite3 | JSON 文件 |
| 图像处理 | sharp（libvips） | 标准库 + `golang.org/x/image` |
| 加命令 | `.tpm install` | 改代码、重新编译 |

内存是同一台生产机上实测的：刚连上约 30 MB，连续运行几小时后约 50 MB，其中近 20 MB 是二进制
本身映射进来的页面。生成贴纸时临时起的 ffmpeg 子进程不算在内（见[设计取舍](#设计取舍)）。

## 安装

服务器上执行（Linux amd64 / arm64，systemd，root）：

```sh
bash <(curl -fsSL https://raw.githubusercontent.com/MiCat-S/mibot-lite/main/scripts/install.sh)
```

下载、校验、登录、装成服务一步完成；重跑就是升级，之后也可以在 Telegram 里发 `.update run`。
分步流程、配置项和内存限额见 [INSTALL.md](INSTALL.md)。

- **ffmpeg**：`.eatgif`、`.eat`、`.eat2`、`.t` 要用，`.yvlu` 引用视频时也要用；没装不影响其他命令。
  `apt install -y ffmpeg`。
- **从 MiBox 迁移**：ai、sum、whois、别名、前缀等配置可以直接搬过来：

  ```sh
  ./mibot-lite --import-mibox /root/mibot --root /root/mibot-lite
  ```

  同一个账号可以在两套程序之间来回切，但**不能同时跑**：Telegram 会让同账号的两个连接互相顶掉。
- **重装系统或换机器**：在 Telegram 里发 `.bf`，配置备份到收藏夹；新机器给安装脚本加
  `--restore 备份文件`，原样恢复，不用重新登录。见 [INSTALL.md 第 10 节](INSTALL.md#10-备份与恢复重装系统换机器)。

## 命令

默认前缀是 `.`、`。` 和 `$`，可以用 `.prefix` 改。每条命令的详细用法发 `.help 命令`，
或者在命令后面加 `--help`。

「可借」一栏说的是能不能通过 [`.sudo` / `.sure`](#借用账号sudo-和-sure) 借给别人用：
✓ 是整条命令都能借；**部分**是只能借用来查、用的那部分，改设置的子命令只限本人；空着的是只限本人。

**运行与维护**

| 命令 | 作用 | 可借 |
|---|---|---|
| `.ping [域名]` | Telegram 或某个网站的延迟 | ✓ |
| `.status` | 运行状态卡片（CPU、内存、磁盘、Swap） | ✓ |
| `.memory` | 进程内存 | ✓ |
| `.sysinfo` | 详细系统信息 | |
| `.version` `.ver` | 版本信息 | ✓ |
| `.help` `.h` | 命令列表或单条命令说明 | ✓ |
| `.update [check\|run\|rollback]` | 检查、更新或回滚程序 | |
| `.restart` | 重启服务 | |
| `.log` | 导出运行日志（已脱敏） | |
| `.bf` | 备份配置到收藏夹 | |
| `.prefix` `.alias` | 改命令前缀、给命令起别名 | |
| `.privacy` | 设置输出里 IP 地址的打码方式 | |

**查询与工具**

| 命令 | 作用 | 可借 |
|---|---|---|
| `.calc 表达式` | 四则运算 | ✓ |
| `.rate 货币 [目标] [数量]` | 汇率与换算 | ✓ |
| `.tr [语言] 文本` | 谷歌翻译，不用配置 | ✓ |
| `.gt [语言] 文本` | AI 翻译 | ✓ |
| `.whois 域名` | 域名注册信息，支持批量 | 部分 |
| `.ip [IP\|域名]` | IP 的位置与运营商 | ✓ |
| `.bin 卡号前 6–8 位` | 卡头对应的发卡行 | ✓ |
| `.ids` `.dc` | 用户或对话的资料、所在数据中心 | ✓ |
| `.speedtest` `.st` | 服务器测速（Ookla 官方 CLI） | 部分 |

**AI**

| 命令 | 作用 | 可借 |
|---|---|---|
| `.ai [search] 问题` | AI 对话与联网搜索 | 部分 |
| `.sum [数量]` | 群消息摘要，也能设定时任务 | 部分 |

**消息与贴纸**

| 命令 | 作用 | 可借 |
|---|---|---|
| `.yvlu` | 把消息做成语录贴纸、图片或故事 | 部分 |
| `.eatgif 名称` | 双方头像合成动画贴纸 | 部分 |
| `.eat` `.eat2` | 用头像或图片生成表情包 | 部分 |
| `.sticker` | 把贴纸存进自己的贴纸包 | |
| `.t 文本` | 文字转语音（`.ts` 选角色，`.tk` 设 API Key） | |
| `.re [消息数] [次数]` | 复读回复的消息 | ✓ |
| `.save 链接` | 保存或转发消息，禁止转发的也行 | |
| `.dme 数量` | 删除自己的消息 | |
| `.da` | 批量删除群消息 | |

**群管理**

| 命令 | 作用 | 可借 |
|---|---|---|
| `.ban` `.unban` `.kick` `.mute` `.unmute` | 在当前群封禁、踢出、禁言 | ✓ |
| `.sb` `.unsb` | 在所有管理的群里封禁、解封 | |
| `.refresh` | 刷新管理群缓存（平时一天自动刷新一次） | |
| `.aban` | 以上几条的帮助 | |

**账号**

| 命令 | 作用 | 可借 |
|---|---|---|
| `.acn` `.autochangename` | 按时间、天气自动改昵称 | |
| `.sudo` `.sure` | 把命令借给名单里的人，见下文 | |

### 借用账号：.sudo 和 .sure

两者都是让名单里的人通过你的账号执行命令：对方在群里发消息，账号以你的身份在同一个对话里
发出命令、回复同一个目标。`.sudo` 让对方直接发命令；`.sure` 更窄，对方的消息要和你设的规则
对上，还能改写，比如把群友发的 `/sb` 变成 `.ban`。

和 MiBox 不同，**能借出去的是一张白名单**，也就是上面表里的「可借」一栏。MiBox 让名单里的人
执行任何命令，包括 `.sudo add` 本身，被授权的人可以再去授权别人。这里判断按别名展开之后的
真实命令来，起个别名绕不过去；以后新加的命令、新加的子命令默认都借不出去。名单里的人发了
不能借的命令，账号只回一句没有权限。

名单存在 `data/sudo.json` 和 `data/sure.json`，`.bf` 备份时会一起带上。

### IP 打码：.privacy

和 MiBox v2 一样，账号发出和编辑的每条消息里，IP 地址都会打码：默认 IPv4 遮后 2 段、
IPv6 遮后 4 段，指向 IP 的链接会去掉，文件名里的 IP 也一样。`.privacy ip mask 2 4` 改遮几段，
`.privacy ip hide` 整个换成「[IP已隐藏]」。

打码在连接层做，所有命令的输出都经过它。替别人代发的命令（`.sudo`、`.sure`）不打码，
那些地址本来就是对方自己打出来的。

## 设计取舍

### 为什么不要插件系统

MiBox 的宿主要装载任意 TypeScript 插件，所以它必须常驻一个 JS 引擎、一个 SQLite 和一整套
插件生命周期，这是它内存占用的主要来源。MiBot Lite 换掉的就是这个前提：命令不再是可以热装的
构件，而是编译进二进制的 Go 函数。代价是加命令要重新编译；换来的是更小的常驻内存，和一份
不需要沙箱的代码。

### 图像与视频

`.yvlu` 和 `.eatgif` 要合成图片。MiBox 用的是 sharp，它绑定 libvips：一个共享库、一份图像缓存和
一个线程池，为了一天用几次的功能常驻整个进程。这里换成标准库的 `image/draw` 加
`golang.org/x/image` 的缩放器，二进制只多 0.7 MB，闲置时不占内存。

唯一的外部依赖是 ffmpeg：Telegram 的视频贴纸必须是 VP9，纯 Go 编码不现实。ffmpeg 是子进程，
不跑的时候不占内存，但**跑的时候要一百多 MB**。所以 systemd 单元把 `MemoryMax` 设在 512M，
而不是贴着常驻值设，并且**不设** `MemoryHigh`。后者超了不会失败，只会反复强制回收，把进程
拖慢：一次编码里触发了一万四千多次，编码没跑完就撞上自己的超时，也没留下任何错误信息。

和 MiBox 的两处差异：

- **eatgif 不再先编码 GIF**。原实现把帧编码成 GIF 再转 VP9，中间那步会把每帧压到 256 色。
  这里直接把 PNG 帧序列交给 ffmpeg，省掉一次有损中转。
- **yvlu 不支持 tgs 动画贴纸转换**。那条路要 Python 的 `rlottie-python`，生产机上本来就没装，
  移植它等于移植一个当前不工作的功能。其余行为对着原版逐条对齐过：多条消息走真实历史而不是
  连续 ID、转发归属原作者、部分引用、管理员头衔、emoji 状态、头像大图兜底。

## 开发

```sh
go test ./...          # 全部测试
go vet ./...
bash scripts/build.sh  # 带版本号构建
```

连上真账号自检（往收藏夹发一组只读命令、核对回复、再删掉）。同一个账号不能同时连两次，
所以要先停掉服务：

```sh
./mibot-lite --verify --root /部署目录
```

用真实素材验证图像合成（默认跳过）：

```sh
MIBOT_EATGIF_ASSETS=/path/to/eatgif go test ./internal/imaging/ -run RealAnimation -v
```

代码结构：

| 位置 | 内容 |
|---|---|
| `cmd/mibot-lite` | 入口 |
| `internal/app` | 连接与更新分发 |
| `internal/bot` | Telegram 封装，包括限流重试和 IP 打码这些连接层中间件 |
| `internal/command` | 命令注册与分发 |
| `internal/commands/<命令>/` | 命令实现，一个目录一条命令，或共用一套数据的一组命令 |
| `internal/commands/kit` | 命令共用的小工具 |

成组放在一起的命令：`aban` 是全部群管理命令，`ids` 含 `.dc`，`sudo` 含 `.sure`，`eatgif` 含 `.eat`，
`.t` 在 `tts`，`.status` 等基础命令在 `core`。

新增一条命令：建一个 `internal/commands/xxx/` 目录，写一个 `Register(a *app.App)`，在里面调用
`a.Registry.Register(&command.Command{...})`，再到 `internal/commands/register.go` 的 `RegisterAll`
里挂上。要让它能借给别人，还得加进 `internal/commands/sudo/sudo.go` 的白名单。

仓库里的测试只覆盖不依赖外部环境的纯逻辑。用假 Telegram 跑的行为测试、快照测试和连真服务的
测试（文件名 `*_local_test.go`，连同 `internal/commands/testkit` 和各命令的 `testdata`）只留在
维护者本地，已写进 `.gitignore`。

## 许可

LGPL-2.1，与 MiBox 一致。

状态卡片用的字体是 Noto Sans SC 的子集（`internal/statuscard/NotoSansSC-status-subset.ttf`），
按 SIL Open Font License 1.1 分发，许可证见同目录的 `NotoSansSC-OFL.txt`。
