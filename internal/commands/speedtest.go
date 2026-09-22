package commands

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/httpx"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
)

// Speed is measured by Ookla's official Speedtest CLI: it picks a nearby
// server, knows the ISP, and publishes a result page that everyone already
// recognises. When the CLI is not installed this command installs it once,
// into its own data directory, and reuses it from then on.

// reading is one complete measurement, however it was taken.
type reading struct {
	// Source names what produced it, for the line that says so.
	Source     string
	Latency    time.Duration
	Jitter     time.Duration
	Download   float64
	Upload     float64
	Server     string
	ISP        string
	Link       string
	ExternalIP string
}

// ooklaResult is `speedtest -f json` from the official CLI. Its bandwidth
// figures are bytes per second.
type ooklaResult struct {
	Ping struct {
		Latency float64 `json:"latency"`
		Jitter  float64 `json:"jitter"`
	} `json:"ping"`
	Download struct {
		Bandwidth float64 `json:"bandwidth"`
	} `json:"download"`
	Upload struct {
		Bandwidth float64 `json:"bandwidth"`
	} `json:"upload"`
	ISP    string `json:"isp"`
	Server struct {
		Name     string `json:"name"`
		Location string `json:"location"`
		Country  string `json:"country"`
	} `json:"server"`
	Result struct {
		URL string `json:"url"`
	} `json:"result"`
	Interface struct {
		ExternalIP string `json:"externalIp"`
	} `json:"interface"`
}

// pythonResult is `speedtest-cli --json`. Its figures are bits per second.
type pythonResult struct {
	Download float64 `json:"download"`
	Upload   float64 `json:"upload"`
	Ping     float64 `json:"ping"`
	Server   struct {
		Name    string `json:"name"`
		Country string `json:"country"`
		Sponsor string `json:"sponsor"`
	} `json:"server"`
	Client struct {
		ISP string `json:"isp"`
		IP  string `json:"ip"`
	} `json:"client"`
	Share string `json:"share"`
}

// Ookla publishes a static binary that needs no package manager and no
// root. The version and its digest are pinned here: this downloads an
// executable and then runs it, so "it came over HTTPS from the right
// domain" is not enough on its own.
const ooklaVersion = "1.2.0"

var ooklaDigests = map[string]string{
	"amd64": "5690596c54ff9bed63fa3732f818a05dbc2db19ad36ed68f21ca5f64d5cfeeb7",
	"arm64": "3953d231da3783e2bf8904b6dd72767c5c6e533e163d3742fd0437affa431bd3",
}

var ooklaArchives = map[string]string{"amd64": "x86_64", "arm64": "aarch64"}

// externalTool finds an installed speedtest command, preferring Ookla's
// own, and including the copy this command may have installed itself.
func externalTool(dataDir string) (string, string) {
	if path, err := exec.LookPath("speedtest"); err == nil {
		return path, "ookla"
	}
	local := filepath.Join(dataDir, "speedtest", "speedtest")
	if info, err := os.Stat(local); err == nil && info.Mode()&0o111 != 0 {
		return local, "ookla"
	}
	if path, err := exec.LookPath("speedtest-cli"); err == nil {
		return path, "python"
	}
	return "", ""
}

// installOokla fetches the official static binary into the deployment's
// own data directory.
//
// It goes there rather than into /usr/local/bin because this program
// should not be editing the system on its own initiative, and because a
// deployment that is deleted should take everything it installed with it.
func installOokla(ctx context.Context, dataDir string) (string, error) {
	archive, ok := ooklaArchives[runtime.GOARCH]
	digest, hasDigest := ooklaDigests[runtime.GOARCH]
	if !ok || !hasDigest {
		return "", failf("没有 %s 架构的 Ookla CLI 构件", runtime.GOARCH)
	}
	url := fmt.Sprintf("https://install.speedtest.net/app/cli/ookla-speedtest-%s-linux-%s.tgz", ooklaVersion, archive)
	response, err := httpx.Do(ctx, httpx.Request{URL: url, Timeout: 90 * time.Second, MaxBytes: 16 << 20})
	if err != nil {
		return "", err
	}
	if !response.OK() {
		return "", failf("下载 Ookla CLI 失败：HTTP %d", response.Status)
	}
	sum := sha256.Sum256(response.Body)
	if hex.EncodeToString(sum[:]) != digest {
		return "", fail("Ookla CLI 的 SHA-256 与预期不符，已丢弃")
	}

	target := filepath.Join(dataDir, "speedtest")
	if err := os.MkdirAll(target, 0o700); err != nil {
		return "", err
	}
	binary, err := extractOokla(response.Body, target)
	if err != nil {
		return "", err
	}
	return binary, nil
}

// extractOokla pulls just the speedtest executable out of the tarball.
func extractOokla(archive []byte, target string) (string, error) {
	stream, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return "", err
	}
	defer stream.Close()
	reader := tar.NewReader(stream)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		// Only the one file, matched by its exact name: an archive entry
		// is untrusted input, and a path from one has no business
		// deciding where this writes.
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != "speedtest" || strings.ContainsRune(header.Name, os.PathSeparator) {
			continue
		}
		path := filepath.Join(target, "speedtest")
		file, err := os.OpenFile(path+".part", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(file, io.LimitReader(reader, 64<<20))
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			os.Remove(path + ".part")
			return "", err
		}
		if err := os.Rename(path+".part", path); err != nil {
			return "", err
		}
		return path, nil
	}
	return "", fail("Ookla CLI 压缩包里没有可执行文件")
}

// runExternal drives the installed speedtest tool.
//
// home is where the tool is allowed to keep its own state. It matters:
// the Ookla CLI reads $HOME to find where it recorded the licence, and
// under this service's sandbox $HOME is unset, which it does not survive
// — it aborts on a null string before it measures anything. Handing it a
// writable directory of its own is also what keeps ProtectSystem=strict
// from being the thing that breaks it.
func runExternal(ctx context.Context, path, kind, home string) (*reading, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	var arguments []string
	switch kind {
	case "ookla":
		arguments = []string{"-f", "json", "--accept-license", "--accept-gdpr"}
	default:
		arguments = []string{"--json", "--secure"}
	}
	tool := exec.CommandContext(ctx, path, arguments...)
	tool.Env = append(os.Environ(), "HOME="+home)
	// Both streams are kept: the tool says why it failed on stderr, and
	// "exit status 2" on its own sent me looking in the wrong place.
	var out, complaint bytes.Buffer
	tool.Stdout = &out
	tool.Stderr = &complaint
	if err := tool.Run(); err != nil {
		if detail := lastLine(complaint.String()); detail != "" {
			return nil, fmt.Errorf("%s failed: %w: %s", kind, err, detail)
		}
		return nil, fmt.Errorf("%s failed: %w", kind, err)
	}
	output := out.Bytes()
	// Ookla prints one JSON object per line and ends with the result.
	line := output
	if index := strings.LastIndexByte(strings.TrimSpace(string(output)), '\n'); index >= 0 {
		line = []byte(strings.TrimSpace(string(output))[index+1:])
	}

	if kind == "ookla" {
		var parsed ooklaResult
		if err := json.Unmarshal(line, &parsed); err != nil {
			return nil, fail("无法解析 speedtest 的输出")
		}
		server := strings.TrimSpace(parsed.Server.Name + " " + parsed.Server.Location)
		return &reading{
			Source: "Ookla Speedtest", Latency: durationFromMillis(parsed.Ping.Latency),
			Jitter: durationFromMillis(parsed.Ping.Jitter),
			// Ookla reports bytes per second.
			Download: parsed.Download.Bandwidth * 8, Upload: parsed.Upload.Bandwidth * 8,
			Server: server, ISP: parsed.ISP, Link: parsed.Result.URL,
			ExternalIP: parsed.Interface.ExternalIP,
		}, nil
	}
	var parsed pythonResult
	if err := json.Unmarshal(line, &parsed); err != nil {
		return nil, fail("无法解析 speedtest-cli 的输出")
	}
	server := strings.TrimSpace(parsed.Server.Sponsor + " " + parsed.Server.Name)
	return &reading{
		Source: "speedtest-cli", Latency: durationFromMillis(parsed.Ping),
		Download: parsed.Download, Upload: parsed.Upload,
		Server: server, ISP: parsed.Client.ISP, Link: parsed.Share,
		ExternalIP: parsed.Client.IP,
	}, nil
}

// resultImage fetches the picture Speedtest publishes for a result, or
// nil when there is none to be had. A missing image is not a failure: the
// measurement is the point and the numbers are already in hand.
func resultImage(ctx context.Context, link string) []byte {
	if !strings.HasPrefix(link, "https://www.speedtest.net/result/") {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	response, err := httpx.Do(ctx, httpx.Request{URL: strings.TrimSuffix(link, ".png") + ".png",
		Timeout: 25 * time.Second, MaxBytes: 8 << 20})
	if err != nil || !response.OK() || len(response.Body) < 1024 {
		return nil
	}
	// Only a real PNG is forwarded: an error page rendered as an image
	// would be worse than no image.
	if len(response.Body) < 8 || string(response.Body[1:4]) != "PNG" {
		return nil
	}
	return response.Body
}

// lastLine returns the final non-empty line, which is where these tools
// put the reason they gave up.
func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if len(line) > 200 {
			line = line[:200] + "…"
		}
		return line
	}
	return ""
}

func durationFromMillis(value float64) time.Duration {
	return time.Duration(value * float64(time.Millisecond))
}

// maskAddress hides the host's own address. The result goes into a chat,
// and a server's public IP is not something a speed reading needs to
// publish.
func maskAddress(address string) string {
	if address == "" {
		return "未知"
	}
	if strings.Contains(address, ":") {
		parts := strings.Split(address, ":")
		if len(parts) > 2 {
			return parts[0] + ":" + parts[1] + ":…"
		}
		return "…"
	}
	parts := strings.Split(address, ".")
	if len(parts) != 4 {
		return "…"
	}
	return parts[0] + "." + parts[1] + ".x.x"
}

// formatSpeed renders bits per second at a sensible scale.
func formatSpeed(bitsPerSecond float64) string {
	switch {
	case bitsPerSecond >= 1e9:
		return fmt.Sprintf("%.2f Gbps", bitsPerSecond/1e9)
	case bitsPerSecond >= 1e6:
		return fmt.Sprintf("%.1f Mbps", bitsPerSecond/1e6)
	case bitsPerSecond >= 1e3:
		return fmt.Sprintf("%.1f Kbps", bitsPerSecond/1e3)
	}
	return fmt.Sprintf("%.0f bps", bitsPerSecond)
}

func formatLatency(d time.Duration) string {
	return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
}

func speedtestHelp(dataDir, prefix string) string {
	p := command.Escape(prefix)
	tool, kind := externalTool(dataDir)
	source := "未安装，首次运行会自动下载 Ookla 官方 CLI"
	if tool != "" {
		source = "使用 " + kind + "：" + tool
	}
	return "🚀 <b>网络测速</b>\n\n测量这台服务器的出口带宽。\n\n• <code>" + p +
		"speedtest</code> 完整测速\n• <code>" + p + "st</code> 同上，简写\n\n<b>当前来源</b>\n" +
		command.Escape(source) +
		"\n\n用 Ookla 官方 CLI 测：它自己挑就近的测速服务器，报得出 ISP，还会给一张结果图。没装的话首次运行会把" +
		"官方静态构件下载到部署目录（校验 SHA-256，不写系统目录），之后直接复用。\n\n输出里的出口地址会打码。"
}

// render lays out a finished reading.
func render(result *reading, elapsed time.Duration) string {
	lines := []string{"🚀 <b>网络测速</b>", ""}
	if result.Server != "" {
		lines = append(lines, "📍 节点: "+command.Code(result.Server))
	}
	if result.ISP != "" {
		lines = append(lines, "🏢 运营商: "+command.Code(result.ISP))
	}
	if result.ExternalIP != "" {
		lines = append(lines, "🌐 出口: "+command.Code(maskAddress(result.ExternalIP)))
	}
	lines = append(lines, "")
	latency := "⏱ 延迟: " + command.Code(formatLatency(result.Latency))
	if result.Jitter > 0 {
		latency += "（抖动 " + command.Escape(formatLatency(result.Jitter)) + "）"
	}
	lines = append(lines,
		latency,
		"⬇️ 下载: "+command.Code(formatSpeed(result.Download)),
		"⬆️ 上传: "+command.Code(formatSpeed(result.Upload)),
		"")
	if result.Link != "" {
		lines = append(lines, "🔗 <a href=\""+command.Escape(result.Link)+"\">详细结果</a>")
	}
	lines = append(lines, "<i>来源 "+command.Escape(result.Source)+" · 用时 "+
		command.Escape(fmt.Sprintf("%.1f 秒", elapsed.Seconds()))+"</i>")
	return strings.Join(lines, "\n")
}

// Speedtest registers .speedtest and its short form.
func Speedtest(a *app.App) {
	var running sync.Mutex
	handle := func(ctx context.Context, inv *command.Invocation) error {
		dataDir := a.DataDir()
		switch strings.ToLower(inv.Arg(0)) {
		case "help", "h":
			return inv.Edit(ctx, speedtestHelp(dataDir, inv.Prefix))
		}
		// Running two at once would have them measure each other.
		if !running.TryLock() {
			return inv.EditText(ctx, "已有一个测速在进行，请稍候")
		}
		defer running.Unlock()

		started := time.Now()
		tool, kind := externalTool(dataDir)
		if tool == "" {
			// Nothing installed: fetch the official CLI once and keep it.
			if err := inv.Edit(ctx, "🚀 <b>网络测速</b>\n\n首次运行，正在下载 Ookla 官方 CLI…"); err != nil {
				return err
			}
			installed, err := installOokla(ctx, dataDir)
			if err != nil {
				inv.Log.Warn("speedtest.install_failed", "error", err.Error())
				detail, ok := isUserError(err)
				if !ok {
					detail = "网络不通或构件无法校验"
				}
				return inv.EditText(ctx, "❌ 无法安装 Speedtest CLI："+detail+"\n手动装好 speedtest 后再试")
			}
			tool, kind = installed, "ookla"
		}
		if err := inv.Edit(ctx, "🚀 <b>网络测速</b>\n\n正在通过 "+command.Escape(kind)+" 测速，约需一分钟…"); err != nil {
			return err
		}
		home := filepath.Join(dataDir, "speedtest")
		result, err := runExternal(ctx, tool, kind, home)
		if err != nil {
			// One retry. The CLI occasionally loses its server part way
			// through — "Latency test failed", seen twice here — and there
			// is no second path to fall back on any more.
			inv.Log.Warn("speedtest.retrying", "tool", kind, "error", err.Error())
			result, err = runExternal(ctx, tool, kind, home)
		}
		if err != nil {
			inv.Log.Warn("speedtest.external_failed", "tool", kind, "error", err.Error())
			if detail, ok := isUserError(err); ok {
				return inv.EditText(ctx, "❌ "+detail)
			}
			return inv.EditText(ctx, "❌ 测速失败，请稍后再试")
		}
		text := render(result, time.Since(started))
		// Speedtest publishes a picture of every result; sending it is what
		// people expect to see, and the numbers ride along as the caption.
		if image := resultImage(ctx, result.Link); image != nil {
			// Both failures carry their reason. A bare "photo_failed" was
			// logged once from a group and said nothing at all: a chat
			// that forbids media, a peer that will not resolve and a
			// network error are three different problems.
			peer, peerErr := inv.Client.InputPeer(inv.Message.Peer)
			if peerErr != nil {
				inv.Log.Info("speedtest.peer_unresolved", "error", peerErr.Error())
			} else if sendErr := inv.Client.SendPhoto(ctx, peer, "speedtest.png", image, text, 0); sendErr != nil {
				inv.Log.Info("speedtest.photo_failed", "error", sendErr.Error())
			} else {
				return inv.Client.DeleteMessage(ctx, inv.Message)
			}
		}
		return inv.Edit(ctx, text)
	}
	a.Registry.Register(
		&command.Command{Name: "speedtest", Description: "测量服务器网络速度",
			Help: func(prefix string) string { return speedtestHelp(a.DataDir(), prefix) }, Timeout: 5 * time.Minute, Handle: handle},
		&command.Command{Name: "st", Description: "speedtest 的简写", Hidden: true,
			Help: func(prefix string) string { return speedtestHelp(a.DataDir(), prefix) }, Timeout: 5 * time.Minute, Handle: handle},
	)
}
