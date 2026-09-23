package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
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

// Update 注册 .update。
func Update(a *app.App) {
	repo := a.Env.Get("MIBOT_UPDATE_REPO", "MiCat-S/mibot-lite")
	a.Registry.Register(&command.Command{Name: "update", Description: "检查并更新程序", Usage: "[check|run|rollback]", Timeout: 10 * time.Minute,
		Help: func(prefix string) string {
			return "<b>程序更新</b>\n" + command.Code(prefix+"update") + " 当前版本与回滚状态\n" + command.Code(prefix+"update check") + " 读取 GitHub Releases 检查新版本\n" +
				command.Code(prefix+"update run") + " 下载、校验、试跑、替换并重启\n" + command.Code(prefix+"update rollback") + " 换回上一版本并重启\n发布仓库由 " + command.Code("MIBOT_UPDATE_REPO") + " 指定。"
		},
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			binary, err := os.Executable()
			if err == nil {
				binary, _ = filepath.EvalSymlinks(binary)
			}
			switch strings.ToLower(inv.Arg(0)) {
			case "", "ver", "status":
				rows := []string{"<b>更新状态</b>", "当前版本：" + command.Code(versionOf(a)), "程序文件：" + command.Code(binary), "发布仓库：" + command.Code(repo)}
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
					return inv.Edit(ctx, "<b>已是最新版本</b>\n当前："+command.Code(versionOf(a))+"\n最新："+command.Code(latest.TagName))
				}
				notes := strings.TrimSpace(latest.Body)
				if len(notes) > 800 {
					notes = notes[:800] + "…"
				}
				text := "<b>发现新版本</b>\n当前：" + command.Code(versionOf(a)) + "\n最新：" + command.Code(latest.TagName) + "\n执行 " + command.Code(inv.Prefix+"update run") + " 安装。"
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
				if serviceRestarter == nil {
					return inv.EditText(ctx, "重启组件不可用")
				}
				swap := binary + ".rollback"
				if err := os.Rename(binary, swap); err != nil {
					return err
				}
				if err := os.Rename(previous, binary); err != nil {
					_ = os.Rename(swap, binary)
					return err
				}
				_ = os.Rename(swap, previous)
				return serviceRestarter.command(ctx, inv, "update", "<b>MiBot Lite 回滚</b>\n已换回上一版本，正在重启…", "回滚后重启失败。")
			}
			return inv.EditText(ctx, "用法："+inv.Prefix+"update [check|run|rollback]")
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

func runUpdate(ctx context.Context, a *app.App, inv *command.Invocation, repo, binary string) error {
	if serviceRestarter == nil {
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
		return inv.Edit(ctx, "<b>已是最新版本</b>\n当前："+command.Code(versionOf(a)))
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
	digest, err := download(ctx, assetURL, candidate, 200<<20)
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
		tail := strings.TrimSpace(string(output))
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
		return inv.Edit(ctx, "<b>更新失败</b>\n新版本无法读取当前部署，已丢弃。\n<pre>"+command.Escape(tail)+"</pre>\n当前运行的版本未被改动。")
	}
	previous := binary + ".previous"
	os.Remove(previous)
	if err := os.Rename(binary, previous); err != nil {
		os.Remove(candidate)
		return err
	}
	if err := os.Rename(candidate, binary); err != nil {
		_ = os.Rename(previous, binary)
		return err
	}
	return serviceRestarter.command(ctx, inv, "update", "<b>MiBot Lite 更新</b>\n已安装 "+command.Code(latest.TagName)+"，正在重启…", "更新后重启失败，可手动重启服务。")
}

func download(ctx context.Context, url, target string, limit int64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", httpx.UserAgent)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", &httpx.StatusError{Status: response.StatusCode}
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
		return "", httpx.ErrTooLarge
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
