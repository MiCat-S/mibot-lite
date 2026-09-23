# MiBot Lite

一个小的 Telegram UserBot：单个静态 Go 二进制，命令用 Go 写死在程序里，
状态存成 JSON 文件。它是 [MiBox](https://github.com/MiCat-S/Mi-Box) 的省内存版本——相同的账号、
相同的命令名，但没有插件系统、没有 JavaScript 运行时、没有 SQLite。

## 为什么另起一个

MiBox 的宿主要装载任意 TypeScript 插件，所以它必须常驻一个 JS 引擎
（Node 里是 V8，Go 里是 goja）、一个 SQLite 和一整套插件生命周期。这是
它内存占用的主要来源。

MiBot Lite 换掉的是这个前提：命令不再是可热装的构件，而是编译进二进制的
Go 函数。代价是装新命令要重新编译；换来的是一个更小的二进制、更小的常驻
内存，和一份不需要沙箱的代码。

| | MiBox（Node） | MiBox（Go 宿主） | MiBot Lite |
|---|---|---|---|
| 二进制 / 依赖 | node_modules 约 237 个包 | 57 MB 单文件 | **18 MB 单文件** |
| 插件运行时 | V8 | goja | 无 |
| 数据库 | better-sqlite3 | modernc sqlite | JSON 文件 |
| 图像处理 | sharp（libvips） | sharp（libvips） | 标准库 + x/image |
| 加命令 | `.tpm install` | `.tpm install` | 重新编译 |

### 图像与视频

`.yvlu` 和 `.eatgif` 要合成图片，MiBox 用的是 sharp，它绑定 libvips——一个共享库、
一份图像缓存和一个线程池，为了一天用几次的功能常驻整个进程。

这里换成标准库的 `image/draw` 加 `golang.org/x/image` 的缩放器，二进制只多 0.7 MB，
闲置时不占内存。唯一的外部依赖是 **ffmpeg**：Telegram 的视频贴纸必须是 VP9，
纯 Go 编码不现实，而 ffmpeg 是子进程，不跑的时候不占内存。

不占内存指的是常驻；**跑的时候它要一百多 MB**。systemd 单元里因此把
`MemoryMax` 设在 512M 而不是贴着常驻值设，并且**没有** `MemoryHigh`——
后者不会失败，只会反复强制回收把进程拖慢，一次编码里触发了一万四千多次，
结果是编码没跑完就撞上自己的超时，还不留任何错误信息。

两处和 MiBox 的取舍差异：

- **eatgif 不再先编码 GIF**。原实现把帧编码成 GIF 再转 VP9，中间那步会把每帧压到
  256 色。这里直接把 PNG 帧序列交给 ffmpeg 的 concat，省掉一次有损中转。
- **yvlu 不支持 tgs 动画贴纸转换**。那条路要 python 的 `rlottie-python`，
  生产机上本来就没装，移植它等于移植一个当前不工作的功能。其余行为对着原版
  逐条对齐过：多条消息走真实历史而不是连续 ID、转发归属原作者、部分引用、
  管理员头衔、emoji 状态、头像大图兜底。

## 命令

```text
.ping .status .sysinfo .memory .version .ver .help .h .restart .update .log .bf .alias .prefix
.calc .rate .whois .tr .speedtest .ip .bin .ids .dc .save
.ai .gt .sum .re .dme .da
.ban .unban .kick .mute .unmute .sb .unsb .refresh .aban
.acn .autochangename .yvlu .eatgif
.sudo .sure
```

每条命令的用法见 `.help 命令`。

### 借用账号：.sudo 和 .sure

两者都是让名单里的人通过你的账号执行命令：对方在群里发消息，账号以你的身份
在同一个对话里发出命令、回复同一个目标并执行。`.sudo` 让对方直接发命令；
`.sure` 更窄，对方的消息要和你设的规则对上，还能重定向，比如把群友发的
`/sb` 变成 `.ban`。

和 MiBox 不同的是**能借出去的范围是白名单**。MiBox 让名单里的人执行任何命令，
包括 `.sudo add` 本身，被授权的人可以再去授权别人。这里只有查询类命令
（`.ping` `.rate` `.calc` `.whois` `.ip` 等）、`.ai` `.sum` `.gt` `.tr` `.yvlu` `.eatgif` `.re`
和单群的 `.ban` `.kick` `.mute` 这一类能借；改设置的子命令（`.ai config` 之类）、
授权管理、删消息、改昵称或前缀别名、备份与日志、`.save`、重启更新、跨所有群的
`.sb`、`.sysinfo` 都只限本人。判断按别名展开之后的真实命令来，起个别名绕不过去；
以后新加的命令默认也不能借。名单里的人发了不能借的命令，账号只回一句没有权限。

名单存在 `data/sudo.json` 和 `data/sure.json`，`.bf` 备份时会一起带上。

## 部署

```sh
bash <(curl -fsSL https://raw.githubusercontent.com/MiCat-S/mibot-lite/main/scripts/install.sh)
```

下载、校验、登录、装服务一步到位，重跑即升级。分步流程见 [INSTALL.md](INSTALL.md)。已经有一个 MiBox 部署目录时，
`--import-mibox` 可以把 ai、sum、whois 等命令的配置直接搬过来：

```sh
./mibot-lite --import-mibox /root/mibot --root /root/mibot-lite
```

重装系统或换机器：在 Telegram 里发 `.bf`，配置会备份到收藏夹；新机器上给安装脚本加
`--restore 备份文件` 就能原样恢复，不用重新登录。详见 [INSTALL.md 第 10 节](INSTALL.md#10-备份与恢复重装系统换机器)。

登录、会话和 `config.json` 的格式与 MiBox 完全一致，所以同一个账号在两套
程序之间可以来回切——但**不能同时跑**，Telegram 的同账号并发会互相顶掉。

## 开发

```sh
go test ./...          # 全部测试
go vet ./...
bash scripts/build.sh  # 带版本号构建
```

连上真账号验证（往收藏夹发命令、核对回复、删掉，只跑只读命令）：

```sh
./mibot-lite --verify --root /部署目录
```

用真实素材验证图像合成（默认跳过）：

```sh
MIBOT_EATGIF_ASSETS=/path/to/eatgif go test ./internal/imaging/ -run RealAnimation -v
```

- 入口：`cmd/mibot-lite/main.go`
- 连接与更新分发：`internal/app`
- Telegram 封装：`internal/bot`
- 命令注册与分发：`internal/command`
- 命令实现：`internal/commands/<命令>/`，每个子目录一个命令，或共用一套数据的一组命令
  （如 `aban` 是全部封禁命令，`ids` 含 `.dc`，`sudo` 含 `.sure`）
- 命令共用的小工具：`internal/commands/kit`

新增一条命令：建一个 `internal/commands/xxx/` 目录，写一个 `Register(a *app.App)`，
调用 `a.Registry.Register(&command.Command{...})`，再在
`internal/commands/register.go` 的 `RegisterAll` 里挂上。

仓库里的测试只覆盖不依赖外部环境的纯逻辑。用假 Telegram 跑的行为测试、快照测试和
连真服务的测试（文件名 `*_local_test.go`，连同 `internal/commands/testkit` 和各命令的
`testdata`）只留在维护者本地，已写进 `.gitignore`。

## 许可

LGPL-2.1，与 MiBox 一致。
