// Package update 实现 .update：从 GitHub Releases 检查、安装和回滚版本。
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// Register 注册 .update。
func Register(a *app.App) {
	repo := a.Env.Get("MIBOT_UPDATE_REPO", "MiCat-S/mibot-lite")
	a.Registry.Register(&command.Command{Name: "update", Description: "检查并更新程序", Usage: "[check|run|rollback]", Timeout: 10 * time.Minute,
		Help: func(prefix string) string {
			return "<b>程序更新</b>\n" + command.Code(prefix+"update") + " 当前版本与回滚状态\n" + command.Code(prefix+"update check") + " 读取 GitHub Releases 检查新版本\n" +
				command.Code(prefix+"update run") + " 下载、校验、试跑、替换并重启\n" + command.Code(prefix+"update rollback") + " 换回上一版本并重启\n发布仓库由 " + command.Code("MIBOT_UPDATE_REPO") + " 指定。"
		},
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			binary, located := executablePath()
			// 下载、替换程序文件、回滚都只能一个一个来：两个同时跑会互相覆盖下载的文件，
			// 或者把留作回滚的上一版本弄丢。
			if sub := strings.ToLower(inv.Arg(0)); sub == "run" || sub == "apply" || sub == "rollback" {
				if located != nil {
					inv.Log.Warn("update.locate_failed", "error", located.Error())
					return kit.Fail("找不到程序文件在哪里，不能替换它")
				}
				if !busy.TryLock() {
					return kit.Fail("已有一个更新或回滚正在进行，请稍候")
				}
				defer busy.Unlock()
			}
			switch strings.ToLower(inv.Arg(0)) {
			case "", "ver", "status":
				where := command.Code(binary)
				if located != nil {
					where = "无法确定"
				}
				rows := []string{"<b>更新状态</b>", "当前版本：" + command.Code(kit.Version(a)), "程序文件：" + where, "发布仓库：" + command.Code(repo)}
				if info, err := os.Stat(binary + ".previous"); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
					rows = append(rows, "可回滚到上一版本："+command.Code(inv.Prefix+"update rollback"))
				} else {
					rows = append(rows, "暂无可回滚的上一版本")
				}
				return inv.Edit(ctx, strings.Join(rows, "\n"))
			case "check":
				if err := inv.Edit(ctx, "<b>MiBot Lite 更新</b>\n正在读取发布信息…"); err != nil {
					return err
				}
				latest, err := fetchRelease(ctx, repo)
				if err != nil {
					return inv.Edit(ctx, "<b>更新检查失败</b>\n"+command.Escape(httpx.Reason(err)))
				}
				if !newer(a.Version, latest.TagName) {
					return inv.Edit(ctx, "<b>已是最新版本</b>\n当前："+command.Code(kit.Version(a))+"\n最新："+command.Code(latest.TagName))
				}
				notes := command.Truncate(strings.TrimSpace(latest.Body), 800)
				text := "<b>发现新版本</b>\n当前：" + command.Code(kit.Version(a)) + "\n最新：" + command.Code(latest.TagName) + "\n执行 " + command.Code(inv.Prefix+"update run") + " 安装。"
				if notes != "" {
					text += "\n\n<blockquote expandable>" + command.Escape(notes) + "</blockquote>"
				}
				return inv.Edit(ctx, text)
			case "run", "apply":
				return runUpdate(ctx, a, inv, repo, binary)
			case "rollback":
				previous := binary + ".previous"
				if info, err := os.Stat(previous); err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
					return inv.EditText(ctx, "暂无可回滚的上一版本")
				}
				if !restart.Available() {
					return inv.EditText(ctx, "重启组件不可用")
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
				return restart.Now(ctx, inv, "rollback", "<b>MiBot Lite 回滚</b>\n已换回上一版本，正在重启…", "回滚后重启失败。")
			}
			return kit.Usage(inv.Prefix, "update [check|run|rollback]")
		}})
}

func fetchRelease(ctx context.Context, repo string) (*release, error) {
	response, err := httpx.Do(ctx, httpx.Request{URL: "https://api.github.com/repos/" + repo + "/releases/latest",
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

// busy 让更新和回滚同一时间只有一个在跑。
var busy sync.Mutex

func runUpdate(ctx context.Context, a *app.App, inv *command.Invocation, repo, binary string) error {
	if !restart.Available() {
		return inv.EditText(ctx, "重启组件不可用")
	}
	progress := func(text string) error { return inv.Edit(ctx, "<b>MiBot Lite 更新</b>\n"+text) }
	if err := progress("正在读取发布信息…"); err != nil {
		return err
	}
	latest, err := fetchRelease(ctx, repo)
	if err != nil {
		return inv.Edit(ctx, "<b>更新失败</b>\n"+command.Escape(httpx.Reason(err))+"\n当前运行的版本未被改动。")
	}
	if !newer(a.Version, latest.TagName) {
		return inv.Edit(ctx, "<b>已是最新版本</b>\n当前："+command.Code(kit.Version(a)))
	}
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
		return inv.Edit(ctx, "<b>更新失败</b>\n发布 "+command.Code(latest.TagName)+" 没有 "+command.Code(assetName)+" 构建。\n当前运行的版本未被改动。")
	}
	if sumsURL == "" {
		return inv.Edit(ctx, "<b>更新失败</b>\n发布缺少 checksums.txt，拒绝安装未校验的二进制。\n当前运行的版本未被改动。")
	}
	if err := progress("正在下载 " + command.Code(latest.TagName) + fmt.Sprintf("（%.1f MB）…", float64(assetSize)/(1<<20))); err != nil {
		return err
	}
	sums, err := httpx.Do(ctx, httpx.Request{URL: sumsURL, Timeout: 30 * time.Second, MaxBytes: 64 << 10})
	if err != nil || !sums.OK() {
		return inv.Edit(ctx, "<b>更新失败</b>\n无法读取校验文件。\n当前运行的版本未被改动。")
	}
	expected := ""
	for _, line := range strings.Split(string(sums.Body), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.TrimPrefix(fields[len(fields)-1], "*") == assetName {
			expected = strings.ToLower(fields[0])
		}
	}
	if len(expected) != 64 {
		return inv.Edit(ctx, "<b>更新失败</b>\n校验文件里没有 "+command.Code(assetName)+" 的哈希。\n当前运行的版本未被改动。")
	}
	candidate := binary + ".download"
	digest, err := kit.Download(ctx, assetURL, candidate, 200<<20)
	if err != nil {
		os.Remove(candidate)
		return inv.Edit(ctx, "<b>更新失败</b>\n下载失败："+command.Escape(httpx.Reason(err))+"\n当前运行的版本未被改动。")
	}
	if digest != expected {
		os.Remove(candidate)
		return inv.Edit(ctx, "<b>更新失败</b>\nSHA-256 不匹配，已丢弃下载。\n当前运行的版本未被改动。")
	}
	if err := os.Chmod(candidate, 0o755); err != nil {
		os.Remove(candidate)
		return err
	}
	if err := progress("校验通过，正在试跑新版本…"); err != nil {
		return err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	output, err := exec.CommandContext(checkCtx, candidate, "--check", "--root", a.Root).CombinedOutput()
	cancel()
	if err != nil {
		os.Remove(candidate)
		tail := []rune(strings.TrimSpace(string(output)))
		if len(tail) > 400 {
			tail = append([]rune("…"), tail[len(tail)-400:]...)
		}
		return inv.Edit(ctx, "<b>更新失败</b>\n新版本无法读取当前部署，已丢弃。\n<pre>"+command.Escape(string(tail))+"</pre>\n当前运行的版本未被改动。")
	}
	previous := binary + ".previous"
	os.Remove(previous)
	if err := swapBinary(os.Rename, binary, candidate, previous); err != nil {
		inv.Log.Error("update.swap_failed", "error", err.Error())
		os.Remove(candidate)
		return err
	}
	return restart.Now(ctx, inv, "update", "<b>MiBot Lite 更新</b>\n已安装 "+command.Code(latest.TagName)+"，正在重启…", "更新后重启失败，可手动重启服务。")
}

// executablePath 是正在运行的程序文件的真实路径（解开符号链接）。
// 以前两个错误都被丢掉：EvalSymlinks 失败时路径变成空字符串，后面就去改名一个空路径。
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
