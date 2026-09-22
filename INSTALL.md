# 从零部署 MiBot Lite

## 一键安装

服务器上执行（Linux + systemd + root）：

```sh
bash <(curl -fsSL https://raw.githubusercontent.com/MiCat-S/mibot-lite/main/scripts/install.sh)
```

它会检测架构、下载对应构件、按发布自带的 `checksums.txt` 校验 SHA-256、
没有会话就先带你登录、装好 systemd 单元并启动。重跑即升级，`config.json`
和 `data/` 原样保留。

**用 `bash <(curl …)` 而不是 `curl … | bash`**：登录要输手机号和验证码，
管道进来的脚本没有键盘。拿不到终端时它会装好二进制并告诉你下一步命令。

常用选项：`--root 目录`（默认 `/root/mibot-lite`）、`--no-service`（只装二进制不碰 systemd）。

下面是手动分步的流程，想清楚每一步做了什么再看。

---

每一步都注明在哪执行。服务器需要 Linux + systemd + root。

## 1. 本机：编译

任意装了 Go 1.26 的机器都能交叉编译，产物是静态单文件，服务器上不需要 Go。

```sh
GOOS=linux GOARCH=amd64 bash scripts/build.sh /tmp/mibot-lite
```

**用这个脚本，不要直接 `go build`。** 版本号是链接期注入的，直接 build 会
留空，`.version` 报 `dev`，`.update` 也认不出自己是不是已经最新。

传到服务器：

```sh
scp -P 22 /tmp/mibot-lite root@你的服务器:/tmp/mibot-lite
```

## 2. 服务器：准备目录

```sh
mkdir -p /root/mibot-lite && chmod 700 /root/mibot-lite
chmod +x /tmp/mibot-lite
```

## 3. 服务器：登录 Telegram

需要 [my.telegram.org](https://my.telegram.org) 上申请的 API ID 和 API hash。

```sh
/tmp/mibot-lite --login --root /root/mibot-lite
```

依次询问 API ID、API hash、手机号（含 `+` 和国家码）、验证码，开了两步验证
还会问密码。**全部校验通过之前不写任何文件**：会话先在内存里建好，Telegram
确认身份后才落盘，中途退出不会留下半个会话。

成功后目录里有 `config.json`（gramjs 格式，MiBox 也读得懂）和
`gotd-session.json`。

已经有 MiBox 部署时可以跳过这一步，直接复制它的 `config.json` 过来：

```sh
cp /root/mibot/config.json /root/mibot-lite/config.json
```

## 4. 服务器：迁移已有配置（可选）

把 MiBox 里 ai、sum、whois、aban、acn、da、dme、yvlu 的数据搬过来：

```sh
/tmp/mibot-lite --import-mibox /root/mibot --root /root/mibot-lite
```

已存在的文件不会被覆盖。

## 5. 服务器：先前台试跑

```sh
/tmp/mibot-lite --serve --root /root/mibot-lite
```

看到 `msg=runtime.ready` 后，在自己的 Telegram「收藏夹」发 `.ping` 和
`.help`，确认有响应，再按 Ctrl+C 停掉。

同一个 Telegram 账号不能同时跑两个实例。如果服务器上还跑着 MiBox，先
`systemctl stop mibot`。

### 自动验证

想一次跑完全部只读命令，不用手动一条条发：

```sh
/tmp/mibot-lite --verify --root /root/mibot-lite
```

它会连上账号，往**收藏夹**依次发送 `.ping`、`.calc`、`.whois` 等命令，等每条
被处理后回读结果、核对内容，然后把消息删掉——跑完收藏夹和跑之前一样。
输出会区分三种结果：`pass` 是通过，`FAIL` 是程序自己的问题，`warn` 是第三方
服务（汇率、WHOIS 等）当时不可用，不算缺陷。

只列了只读命令。删消息、封禁、重启这些都不在里面。

## 6. 服务器：装成后台服务

需要仓库里的 `deploy/` 和 `scripts/`，所以先把仓库克隆到服务器，或者把这
两个目录一起传上去：

```sh
git clone https://github.com/MiCat-S/mibot-lite.git /tmp/mibot-lite-src
bash /tmp/mibot-lite-src/scripts/install-service.sh \
  --binary /tmp/mibot-lite --root /root/mibot-lite
```

安装器会先用新二进制跑一次 `--check`（只读、不联网），再渲染并
`systemd-analyze verify` 服务文件，最后 `enable --now` 并等 journal 出现
`runtime.ready`。

看到 `MiBot Lite is ready and enabled at boot.` 后，去 Telegram 发 `.ping`
验证，然后就可以关掉 SSH 了。

```sh
journalctl -u mibot-lite -f     # 看日志
systemctl status mibot-lite     # 看状态
```

## 7. 配置

可选设置写在 `<部署目录>/.env`，一行一个 `KEY=value`，改完重启服务生效。
程序自己读这个文件，不经过 systemd 的 `EnvironmentFile=`。

| 变量 | 含义 | 默认 |
|---|---|---|
| `MIBOT_PREFIX` | 命令前缀，空格分隔 | `. 。 $` |
| `MIBOT_SERVICE` | `.restart` 要重启的 unit 名 | `mibot-lite.service` |
| `MIBOT_UPDATE_REPO` | `.update` 读的 GitHub 仓库 | `MiCat-S/mibot-lite` |

`.eatgif` 和 `.yvlu` 生成视频贴纸需要主机装有 ffmpeg（带 libvpx-vp9）：

```sh
apt install -y ffmpeg
```

没装的话这两条命令会明确报「ffmpeg 不可用」，其余命令不受影响。

AI、汇率等命令的配置在 Telegram 里用命令完成，见 `.help ai`、`.help sum`。
**涉及 API Key 的命令请在「收藏夹」里执行**，别在群里。

## 8. 更新

```text
.update          查看当前版本和能否回滚
.update check    读 GitHub Releases 看有没有新版本
.update run      下载 → 校验 SHA-256 → 试跑 → 原子替换 → 重启
.update rollback 换回上一版本并重启
```

`.update run` 在三项校验（下载完整、哈希匹配、新二进制能读当前部署）全过
之前不动任何东西，失败时消息里会写明「当前运行的版本未被改动」。

发布方要用 `bash scripts/release.sh <版本号>` 产出带 `checksums.txt` 的
构件——没有校验文件的发布，`.update run` 会拒绝安装。

## 9. 内存

服务单元里设了 `MemoryHigh=128M`、`MemoryMax=192M`。正常运行远低于这个数，
限额的作用是把泄漏变成一次重启，而不是让机器开始换页。

实际占用用 `.memory` 或 `.status` 看，机器侧用：

```sh
systemctl show mibot-lite -p MemoryCurrent -p MemoryPeak
```
