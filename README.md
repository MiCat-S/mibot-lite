# MiBot Lite

一个小的 Telegram UserBot：单个静态 Go 二进制，命令用 Go 写死在程序里，
状态存成 JSON 文件。它是 [MiBox](../MiBox) 的省内存版本——相同的账号、
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
| 加命令 | `.tpm install` | `.tpm install` | 重新编译 |

## 命令

```text
.ping .status .sysinfo .memory .version .ver .help .h .restart .update
.calc .rate .whois .bgp .ai .gt .sum .re .dme .da
.ban .unban .kick .mute .unmute .sb .unsb .refresh .aban
.acn .autochangename
```

每条命令的用法见 `.help 命令`。

## 部署

从零部署见 [INSTALL.md](INSTALL.md)。已经有一个 MiBox 部署目录时，
`--import-mibox` 可以把 ai、sum、whois 等命令的配置直接搬过来：

```sh
./mibot-lite --import-mibox /root/mibot --root /root/mibot-lite
```

登录、会话和 `config.json` 的格式与 MiBox 完全一致，所以同一个账号在两套
程序之间可以来回切——但**不能同时跑**，Telegram 的同账号并发会互相顶掉。

## 开发

```sh
go test ./...          # 全部测试
go vet ./...
bash scripts/build.sh  # 带版本号构建
```

- 入口：`cmd/mibot-lite/main.go`
- 连接与更新分发：`internal/app`
- Telegram 封装：`internal/bot`
- 命令注册与分发：`internal/command`
- 命令实现：`internal/commands`

新增一条命令：在 `internal/commands` 里写一个 `Xxx(a *app.App)` 函数，
调用 `a.Registry.Register(&command.Command{...})`，再在
`internal/commands/register.go` 的 `RegisterAll` 里挂上。

## 许可

LGPL-2.1，与 MiBox 一致。
