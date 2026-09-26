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

常用选项：`--root 目录`（默认 `/root/mibot-lite`）、`--no-service`（只装二进制不碰 systemd）、
`--restore 备份文件`（用 `.bf` 的备份装，不用重新登录，见第 10 节）。

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

`.eatgif`、`.eat`、`.eat2`、`.t` 需要主机装有 ffmpeg（要带 libvpx-vp9、libwebp、libopus、libmp3lame，Debian/Ubuntu 的 ffmpeg 包都带），`.yvlu` 引用视频时也要用：

```sh
apt install -y ffmpeg
```

没装的话这几条命令会明确报「ffmpeg 不可用」（`.yvlu` 改为用文字描述视频），其余命令不受影响。

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

服务单元里只设了 `MemoryMax=512M`，没有 `MemoryHigh`。程序常驻 30–50 MB，限额留的余量是给
生成贴纸时临时起的 ffmpeg 子进程的，它跑几秒钟就要一百多 MB；真撞到 512M 说明确实出了问题，
这时进程会被杀掉重启，而不是让机器开始换页。

不设 `MemoryHigh` 是踩过坑的：它超了不会失败，只会反复强制回收，把进程拖到跑不完。以前设过
`MemoryHigh=128M`，一次 `.eatgif` 编码里触发了一万四千多次，编码没跑完就超时了，也没留下错误信息。

实际占用用 `.memory` 或 `.status` 看，机器侧用：

```sh
systemctl show mibot-lite -p MemoryCurrent -p MemoryPeak
```

## 10. 备份与恢复（重装系统、换机器）

**备份**：在 Telegram 里发 `.bf`。配置会打成一个 `.tar.gz` 发到本账号的收藏夹，
无论在哪个对话里执行都只发到收藏夹。机器连不上 Telegram 时，也可以在服务器上：

```sh
/root/mibot-lite/mibot-lite --backup /root/backup.tar.gz --root /root/mibot-lite
```

备份里有：`config.json` 和 `gotd-session.json`（登录会话）、`.env`、`data/` 下每个命令的
JSON 配置。没有：eatgif 素材和测速 CLI 这类缓存（用到时自己重新下载）、`updates.json`
（这台机器的连接状态，换机器没有意义）、程序本身。

**这个文件等同于你的账号**，拿到它的人能直接登录。不要转发给别人；要给别人看问题，
发 `.log`，那个是脱敏的。

**恢复到新机器**：先把旧机器上的服务停掉（同一账号不能两处同时在线），把备份文件
传到新服务器上，然后：

```sh
bash <(curl -fsSL https://raw.githubusercontent.com/MiCat-S/mibot-lite/main/scripts/install.sh) --restore /root/backup.tar.gz
```

安装脚本会先恢复配置，看到已有会话就跳过登录，直接装服务启动。

`--restore` 只用于全新的目录。目录里已经有账号时它会拒绝，并告诉你怎么覆盖：

```sh
systemctl stop mibot-lite
/root/mibot-lite/mibot-lite --restore /root/backup.tar.gz --root /root/mibot-lite --force
systemctl start mibot-lite
```

恢复前会把整个文件读完并校验：不是 mibot-lite 的备份、文件被截断、里面有备份不该
有的路径，都会在写入任何东西之前拒绝。覆盖时如果备份里没有 `gotd-session.json`，
旧的那个会被删掉——否则启动时它会压过 `config.json`，悄悄继续用旧账号。
