// Package update 实现 .update：从 GitHub Releases 检查、安装和回滚版本。
package update

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/commands/restart"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// 从 GitHub Releases 自更新：release 里必须有名为 mibot-lite-<os>-<arch>
// 的文件，以及每行都是 "<sha256>  <name>" 的 checksums.txt。下载的文件
// 通过校验、新二进制也证明了自己能读取这个部署（--check）之前，
// 磁盘上什么都不改。

type release struct {
	TagName string `json:"tag_name"`
	Body    string `json:"body"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

// updater 装新版本、回滚，以及每天的自动更新。
type updater struct {
	a         *app.App
	restarter *restart.Restarter
	repo      string
	settings  *store.Store[autoConfig]
}

// Register 注册 .update 和自动更新的后台任务。装好新版本或回滚之后，用 restarter 重启服务。
func Register(a *app.App, restarter *restart.Restarter) {
	u := &updater{a: a, restarter: restarter, repo: a.Env.Get("MIBOT_UPDATE_REPO", "MiCat-S/mibot-lite"),
		settings: kit.NewStore(a, "update.json", autoDefaults)}
	a.Registry.Register(&command.Command{Name: "update", Group: command.GroupSystem, Description: "检查并更新程序", Usage: "[check|run|rollback|auto]", Timeout: 10 * time.Minute,
		Help: help, Handle: u.handle})
	a.Registry.AddJob(u.schedule)
}

func help(prefix string) string {
	c := func(text string) string { return command.Code(prefix + "update" + text) }
	return "⬆️ <b>程序更新</b>\n\n" +
		"• " + c("") + " 当前版本、回滚和自动更新的状态\n" +
		"• " + c(" check") + " 读取 GitHub Releases 检查新版本\n" +
		"• " + c(" run") + " 下载、校验、试跑、替换并重启\n" +
		"• " + c(" rollback") + " 换回上一版本并重启\n" +
		"• " + c(" auto on|off") + " 开启或关闭自动更新\n" +
		"• " + c(" auto time 04:00") + " 自动更新每天检查的时间（北京时间）\n\n" +
		"自动更新默认开启：每天到点检查一次，有新版本就走和 " + c(" run") + " 一样的流程，" +
		"开始和结果都发到收藏夹。它要重启服务，所以等没有命令在跑时再装，最多等两小时，还不空就改天。\n\n" +
		"发布仓库由 " + command.Code("MIBOT_UPDATE_REPO") + " 指定。"
}

func (u *updater) handle(ctx context.Context, inv *command.Invocation) error {
	binary, located := executablePath()
	sub := strings.ToLower(inv.Arg(0))
	// 下载、替换程序文件、回滚都只能一个一个来：两个同时跑会互相覆盖下载的文件，
	// 或者把留作回滚的上一版本弄丢。
	if sub == "run" || sub == "apply" || sub == "rollback" {
		if located != nil {
			inv.Log.Warn("update.locate_failed", "error", located.Error())
			return kit.Fail("找不到程序文件在哪里，不能替换它")
		}
		if u.restarter == nil {
			return kit.Fail("重启组件不可用")
		}
		if !busy.TryLock() {
			return kit.Fail("已有一个更新或回滚正在进行，请稍候")
		}
		defer busy.Unlock()
	}
	switch sub {
	case "", "ver", "status":
		return u.status(ctx, inv, binary, located)
	case "check":
		if err := inv.Edit(ctx, "⬆️ <b>程序更新</b>\n"+command.Escape(kit.Working("正在读取发布信息"))); err != nil {
			return err
		}
		latest, err := fetchRelease(ctx, u.repo)
		if err != nil {
			return kit.FailWith("检查更新失败", err)
		}
		if !newer(u.a.Version, latest.TagName) {
			return inv.Edit(ctx, "✅ 已是最新版本\n当前："+command.Code(kit.Version(u.a))+"\n最新："+command.Code(latest.TagName))
		}
		notes := command.Truncate(strings.TrimSpace(latest.Body), 800)
		text := "⬆️ <b>发现新版本</b>\n当前：" + command.Code(kit.Version(u.a)) + "\n最新：" + command.Code(latest.TagName) + "\n执行 " + command.Code(inv.Prefix+"update run") + " 安装。"
		if notes != "" {
			text += "\n\n<blockquote expandable>" + command.Escape(notes) + "</blockquote>"
		}
		return inv.Edit(ctx, text)
	case "run", "apply":
		progress := func(text string) error { return inv.Edit(ctx, "⬆️ <b>程序更新</b>\n⏳ "+text) }
		if err := progress("正在读取发布信息…"); err != nil {
			return err
		}
		latest, err := fetchRelease(ctx, u.repo)
		if err != nil {
			return kit.FailWith("更新失败，当前运行的版本没有改动", err)
		}
		if !newer(u.a.Version, latest.TagName) {
			return inv.Edit(ctx, "✅ 已是最新版本\n当前："+command.Code(kit.Version(u.a)))
		}
		if err := u.install(ctx, inv.Log, latest, binary, progress); err != nil {
			return err
		}
		return u.restarter.Now(ctx, inv, "update", "⬆️ <b>程序更新</b>\n⏳ 已安装 "+command.Code(latest.TagName)+"，正在重启…", "更新后重启失败，可手动重启服务。")
	case "rollback":
		previous := binary + ".previous"
		if info, err := os.Stat(previous); err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return kit.Fail("没有可回滚的上一版本")
		}
		// 当前版本先挪到 .rollback，上一版本换进来之后，再把它改名成新的 .previous：
		// 回滚之后还能再滚回来。
		aside := binary + ".rollback"
		if err := swapBinary(os.Rename, binary, previous, aside); err != nil {
			inv.Log.Error("update.rollback_swap_failed", "error", err.Error())
			return err
		}
		if err := os.Rename(aside, previous); err != nil {
			inv.Log.Warn("update.keep_previous_failed", "error", err.Error(), "left_at", aside)
		}
		return u.restarter.Now(ctx, inv, "rollback", "⬆️ <b>程序更新</b>\n⏳ 已换回上一版本，正在重启…", "回滚后重启失败。")
	case "auto":
		return u.configureAuto(ctx, inv)
	}
	return kit.Usage(inv.Prefix, "update [check|run|rollback|auto]")
}

func (u *updater) status(ctx context.Context, inv *command.Invocation, binary string, located error) error {
	where := command.Code(binary)
	if located != nil {
		where = "无法确定"
	}
	rows := []string{"⬆️ <b>程序更新</b>", "当前版本：" + command.Code(kit.Version(u.a)), "程序文件：" + where, "发布仓库：" + command.Code(u.repo)}
	if info, err := os.Stat(binary + ".previous"); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		rows = append(rows, "可回滚到上一版本："+command.Code(inv.Prefix+"update rollback"))
	} else {
		rows = append(rows, "暂无可回滚的上一版本")
	}
	cfg, err := u.settings.Read()
	if err != nil {
		return err
	}
	rows = append(rows, "自动更新："+autoSummary(cfg))
	return inv.Edit(ctx, strings.Join(rows, "\n"))
}

// githubAPI 是 GitHub API 的地址，测试里换成本地的假服务。
var githubAPI = "https://api.github.com"

func fetchRelease(ctx context.Context, repo string) (*release, error) {
	response, err := httpx.Do(ctx, httpx.Request{URL: githubAPI + "/repos/" + repo + "/releases/latest",
		Headers: map[string]string{"Accept": "application/vnd.github+json"}, Timeout: 20 * time.Second, MaxBytes: 1 << 20})
	if err != nil {
		return nil, err
	}
	if !response.OK() {
		return nil, &httpx.StatusError{Status: response.Status}
	}
	var parsed release
	if err := json.Unmarshal(response.Body, &parsed); err != nil {
		return nil, err
	}
	if parsed.TagName == "" {
		return nil, errors.New("release has no tag")
	}
	return &parsed, nil
}

func normalizeVersion(value string) string {
	return strings.TrimPrefix(strings.TrimSpace(value), "v")
}

func newer(current, latest string) bool {
	current, latest = normalizeVersion(current), normalizeVersion(latest)
	if current == "" || current == "dev" {
		return true
	}
	return current != latest
}

// busy 让更新、回滚和自动更新同一时间只有一个在跑。
var busy sync.Mutex

// install 把 latest 装到 binary 的位置：下载、核对 SHA-256、让新版本试跑 --check，都过了才
// 替换，原来的版本留作 .previous。不重启。progress 报告进度；返回的错误都是给用户看的一句话，
// 写明当前运行的版本有没有改动。
func (u *updater) install(ctx context.Context, logger *slog.Logger, latest *release, binary string, progress func(string) error) error {
	assetName := "mibot-lite-" + runtime.GOOS + "-" + runtime.GOARCH
	var assetURL, sumsURL string
	var assetSize int64
	for _, asset := range latest.Assets {
		switch asset.Name {
		case assetName:
			assetURL, assetSize = asset.URL, asset.Size
		case "checksums.txt", "SHA256SUMS":
			sumsURL = asset.URL
		}
	}
	if assetURL == "" {
		return kit.Failf("更新失败：发布 %s 里没有 %s。当前运行的版本没有改动", latest.TagName, assetName)
	}
	if sumsURL == "" {
		return kit.Fail("更新失败：发布里缺少 checksums.txt，不安装没有校验过的程序。当前运行的版本没有改动")
	}
	if err := progress("正在下载 " + command.Code(latest.TagName) + "（" + kit.FormatBytes(assetSize) + "）…"); err != nil {
		return err
	}
	sums, err := httpx.Do(ctx, httpx.Request{URL: sumsURL, Timeout: 30 * time.Second, MaxBytes: 64 << 10})
	if err != nil || !sums.OK() {
		return kit.Fail("更新失败：读不到校验文件。当前运行的版本没有改动")
	}
	expected := ""
	for _, line := range strings.Split(string(sums.Body), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.TrimPrefix(fields[len(fields)-1], "*") == assetName {
			expected = strings.ToLower(fields[0])
		}
	}
	if len(expected) != 64 {
		return kit.Failf("更新失败：校验文件里没有 %s 的哈希。当前运行的版本没有改动", assetName)
	}
	candidate := binary + ".download"
	digest, err := httpx.DownloadFile(ctx, assetURL, candidate, 200<<20)
	if err != nil {
		os.Remove(candidate)
		return kit.FailWith("更新失败：下载失败，当前运行的版本没有改动", err)
	}
	if digest != expected {
		os.Remove(candidate)
		return kit.Fail("更新失败：SHA-256 不匹配，已丢弃下载。当前运行的版本没有改动")
	}
	if err := os.Chmod(candidate, 0o755); err != nil {
		os.Remove(candidate)
		return err
	}
	if err := progress("校验通过，正在试跑新版本…"); err != nil {
		return err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	output, err := exec.CommandContext(checkCtx, candidate, "--check", "--root", u.a.Root).CombinedOutput()
	cancel()
	if err != nil {
		os.Remove(candidate)
		// 试跑的输出里有部署目录的路径，只进日志。
		logger.Warn("update.check_failed", "version", latest.TagName, "error", err.Error(), "output", tail(string(output), 400))
		return kit.Fail("更新失败：新版本试跑没通过（详情见日志），已丢弃。当前运行的版本没有改动")
	}
	previous := binary + ".previous"
	os.Remove(previous)
	if err := swapBinary(os.Rename, binary, candidate, previous); err != nil {
		logger.Error("update.swap_failed", "error", err.Error())
		os.Remove(candidate)
		return err
	}
	return nil
}

// tail 是 text 最后 n 个字，前面截掉的写成「…」。
func tail(text string, n int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= n {
		return string(runes)
	}
	return "…" + string(runes[len(runes)-n:])
}

// executablePath 是正在运行的程序文件的真实路径（解开符号链接）。
// 两个错误都要返回：EvalSymlinks 失败时照样往下走的话，路径是空字符串，后面会去改名一个空路径。
func executablePath() (string, error) {
	binary, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(binary)
}

// swapBinary 把 incoming 换到 binary 的位置，原来的 binary 挪到 aside。
//
// 任何一步失败都把 binary 恢复原样。恢复也失败时，binary 那个位置上已经没有程序文件了，
// 服务下次启动会起不来：这时返回的错误写明原来的版本现在在哪里，让人能手动改回去。
// rename 由调用方传入（os.Rename），测试里换成会在指定步骤失败的版本。
func swapBinary(rename func(from, to string) error, binary, incoming, aside string) error {
	if err := rename(binary, aside); err != nil {
		return swapError{text: "无法挪开当前的程序文件，什么都没改", cause: err}
	}
	if err := rename(incoming, binary); err != nil {
		if restoreErr := rename(aside, binary); restoreErr != nil {
			return swapError{text: "程序文件没能换回来，服务下次启动会失败。原来的版本在 " + aside +
				"，请手动把它改名为 " + binary, cause: errors.Join(err, restoreErr)}
		}
		return swapError{text: "无法放入新的程序文件，已恢复原来的版本", cause: err}
	}
	return nil
}

// swapError 给用户一句能照着做的话（它实现 UserMessage），日志里记原始错误。
type swapError struct {
	text  string
	cause error
}

func (e swapError) Error() string       { return e.text + ": " + e.cause.Error() }
func (e swapError) UserMessage() string { return e.text }
func (e swapError) Unwrap() error       { return e.cause }
