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
	"net"
	"net/http"
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

// Speed is measured against Cloudflare's own test endpoints. They need no
// account and no installed tool: MiBox drove the official Ookla CLI, which
// is not on this host and would be one more thing to keep installed.
const speedUserAgent = "MiBot-Lite/1"

const (
	speedDownURL  = "https://speed.cloudflare.com/__down?bytes=%d"
	speedUpURL    = "https://speed.cloudflare.com/__up"
	speedTraceURL = "https://speed.cloudflare.com/cdn-cgi/trace"
)

// The transfers are timed windows rather than fixed sizes, because a short
// fixed size measures the burst and not the line: 10 MB from this host
// reported 647 Mbps while 50 MB reported 120. A window long enough to
// leave the burst behind reports what the connection actually sustains.
//
// One connection, not several. Four parallel streams were measured here at
// 107 Mbps against a single stream's 120 — the link was already saturated,
// and the extra streams only competed with each other.
const (
	speedWindow = 6 * time.Second
	// A single __down request is refused above 100 MB — measured: 99 MB
	// answers, 100 MB is a 403 — so a fast link needs several requests to
	// fill the window rather than one enormous one.
	speedChunk     = 90 << 20
	speedMaxUpload = 100 << 20
)

// speedClient is separate from internal/httpx: that client buffers whole
// responses to bound memory, which is exactly wrong for a transfer that is
// supposed to be large and thrown away.
var speedClient = &http.Client{
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableCompression:  true,
		MaxIdleConns:        4,
	},
}

// counter records how many bytes passed and discards them.
type counter struct{ total int64 }

func (c *counter) Write(p []byte) (int, error) {
	c.total += int64(len(p))
	return len(p), nil
}

// zeroSource feeds the upload without allocating the payload, and stops at
// a deadline so the request ends on its own.
type zeroSource struct {
	deadline time.Time
	limit    int64
	sent     int64
	block    []byte
}

func (z *zeroSource) Read(p []byte) (int, error) {
	if time.Now().After(z.deadline) || z.sent >= z.limit {
		return 0, io.EOF
	}
	if z.block == nil {
		z.block = make([]byte, 64<<10)
	}
	n := len(p)
	if n > len(z.block) {
		n = len(z.block)
	}
	copy(p[:n], z.block[:n])
	z.sent += int64(n)
	return n, nil
}

// measureLatency times several tiny requests and reports the best and the
// spread.
//
// The first request is thrown away: it pays for the TCP and TLS handshake,
// and counting it reported 133 ms of jitter on a link whose real spread
// was a few milliseconds. What is measured is the warm connection, which
// is the one every other command uses too.
func measureLatency(ctx context.Context) (best, jitter time.Duration, err error) {
	var samples []time.Duration
	for attempt := 0; attempt < 7; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		started := time.Now()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(speedDownURL, 0), nil)
		if err != nil {
			return 0, 0, err
		}
		request.Header.Set("User-Agent", speedUserAgent)
		response, err := speedClient.Do(request)
		if err != nil {
			continue
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if attempt == 0 {
			continue
		}
		samples = append(samples, time.Since(started))
	}
	if len(samples) == 0 {
		return 0, 0, fail("无法连通测速节点")
	}
	best, worst := samples[0], samples[0]
	for _, sample := range samples {
		if sample < best {
			best = sample
		}
		if sample > worst {
			worst = sample
		}
	}
	return best, worst - best, nil
}

// measureDownload streams until the window closes and reports bits per
// second. Timing starts at the first byte, so the handshake and the
// server's own think time are not counted as slow network.
func measureDownload(ctx context.Context) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, speedWindow+25*time.Second)
	defer cancel()
	var total int64
	var started, stop time.Time
	for request := 0; request < 12; request++ {
		if !started.IsZero() && !time.Now().Before(stop) {
			break
		}
		read, firstByte, err := downloadChunk(ctx, stop)
		total += read
		if err != nil && total == 0 {
			return 0, err
		}
		if started.IsZero() && !firstByte.IsZero() {
			started = firstByte
			stop = started.Add(speedWindow)
		}
		if err != nil {
			break
		}
	}
	if started.IsZero() || total == 0 {
		return 0, fail("下载测速没有取到数据")
	}
	elapsed := time.Since(started)
	if stop.Before(time.Now()) {
		elapsed = speedWindow
	}
	if elapsed <= 0 {
		return 0, fail("下载测速没有取到数据")
	}
	return float64(total) * 8 / elapsed.Seconds(), nil
}

// downloadChunk streams one request, stopping early when stop passes. It
// reports the bytes read and when the first of them arrived.
func downloadChunk(ctx context.Context, stop time.Time) (int64, time.Time, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(speedDownURL, speedChunk), nil)
	if err != nil {
		return 0, time.Time{}, err
	}
	request.Header.Set("User-Agent", speedUserAgent)
	response, err := speedClient.Do(request)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, time.Time{}, failf("测速节点返回 HTTP %d", response.StatusCode)
	}

	head := make([]byte, 32<<10)
	read, err := io.ReadFull(response.Body, head)
	if read == 0 {
		return 0, time.Time{}, err
	}
	firstByte := time.Now()
	remaining := speedWindow
	if !stop.IsZero() {
		remaining = time.Until(stop)
	}
	if remaining <= 0 {
		return int64(read), firstByte, nil
	}

	sink := &counter{total: int64(read)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(sink, response.Body)
	}()
	select {
	case <-done:
	case <-time.After(remaining):
		response.Body.Close()
		<-done
	}
	return sink.total, firstByte, nil
}

// measureUpload posts zeros for the window and reports bits per second.
func measureUpload(ctx context.Context) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, speedWindow+15*time.Second)
	defer cancel()
	source := &zeroSource{deadline: time.Now().Add(speedWindow), limit: speedMaxUpload}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, speedUpURL, source)
	if err != nil {
		return 0, err
	}
	// An unknown length makes Go send it chunked, which is what lets the
	// body stop at a deadline instead of at a size decided in advance.
	request.ContentLength = -1
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("User-Agent", speedUserAgent)
	started := time.Now()
	response, err := speedClient.Do(request)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	elapsed := time.Since(started)
	if elapsed <= 0 || source.sent == 0 {
		return 0, fail("上传测速没有取到数据")
	}
	return float64(source.sent) * 8 / elapsed.Seconds(), nil
}

// reading is one complete measurement, however it was taken.
type reading struct {
	// Source names what produced it, for the line that says so.
	Source   string
	Latency  time.Duration
	Jitter   time.Duration
	Download float64
	Upload   float64
	// Server, ISP and Link are filled by the external tools, which know
	// things the built-in measurement cannot.
	Server string
	ISP    string
	Link   string
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

// runExternal drives an installed speedtest tool.
//
// It is preferred when present: it picks a nearby Ookla server and knows
// the ISP, which a fixed CDN endpoint cannot. The built-in measurement
// exists because the tool usually is not installed — this host had none of
// the three — not because it is better.
func runExternal(ctx context.Context, path, kind string) (*reading, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var arguments []string
	switch kind {
	case "ookla":
		arguments = []string{"-f", "json", "--accept-license", "--accept-gdpr"}
	default:
		arguments = []string{"--json", "--secure"}
	}
	output, err := exec.CommandContext(ctx, path, arguments...).Output()
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w", kind, err)
	}
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

func durationFromMillis(value float64) time.Duration {
	return time.Duration(value * float64(time.Millisecond))
}

// speedNode is where the test landed.
type speedNode struct {
	Colo     string
	Location string
	IP       string
}

// describeNode reads Cloudflare's trace for the edge that served us.
func describeNode(ctx context.Context) speedNode {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var node speedNode
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, speedTraceURL, nil)
	if err != nil {
		return node
	}
	request.Header.Set("User-Agent", speedUserAgent)
	response, err := speedClient.Do(request)
	if err != nil {
		return node
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	if err != nil {
		return node
	}
	for _, line := range strings.Split(string(body), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "colo":
			node.Colo = value
		case "loc":
			node.Location = value
		case "ip":
			node.IP = value
		}
	}
	return node
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
	source := "未安装 CLI，首次运行会自动下载 Ookla 官方 CLI"
	if tool != "" {
		source = "使用 " + kind + "：" + tool
	}
	return "🚀 <b>网络测速</b>\n\n测量这台服务器的出口带宽。\n\n• <code>" + p +
		"speedtest</code> 完整测速\n• <code>" + p + "st</code> 同上，简写\n• <code>" + p +
		"speedtest cf</code> 强制用内置测速\n\n<b>当前来源</b>\n" + command.Escape(source) +
		"\n\n优先用 Ookla 官方 CLI：它自己挑就近的测速服务器，还能报出 ISP。没装的话首次运行会把官方静态构件" +
		"下载到部署目录（校验 SHA-256，不写系统目录），之后直接复用。下载失败则退回内置测速，各测 " + command.Code(fmt.Sprint(int(speedWindow.Seconds()))+" 秒") +
		"，取持续速率而不是突发峰值。\n\n输出里的出口地址会打码。"
}

// builtinReading measures with the Cloudflare endpoints, reporting
// progress as it goes.
func builtinReading(ctx context.Context, inv *command.Invocation, header string) (*reading, error) {
	if err := inv.Edit(ctx, header+"⏱ 测量延迟…"); err != nil {
		return nil, err
	}
	best, jitter, err := measureLatency(ctx)
	if err != nil {
		return nil, err
	}
	line := "⏱ 延迟: " + command.Code(formatLatency(best)) + "（抖动 " + command.Escape(formatLatency(jitter)) + "）\n"

	if err := inv.Edit(ctx, header+line+"⬇️ 测量下载…"); err != nil {
		return nil, err
	}
	download, err := measureDownload(ctx)
	if err != nil {
		return nil, err
	}
	if err := inv.Edit(ctx, header+line+"⬇️ 下载: "+command.Code(formatSpeed(download))+"\n⬆️ 测量上传…"); err != nil {
		return nil, err
	}
	upload, err := measureUpload(ctx)
	if err != nil {
		return nil, err
	}
	return &reading{Source: "Cloudflare", Latency: best, Jitter: jitter, Download: download, Upload: upload}, nil
}

// render lays out a finished reading.
func render(result *reading, node speedNode, elapsed time.Duration) string {
	lines := []string{"🚀 <b>网络测速</b>", ""}
	if result.Server != "" {
		lines = append(lines, "📍 节点: "+command.Code(result.Server))
	} else if node.Colo != "" {
		lines = append(lines, "📍 节点: "+command.Code(strings.TrimSpace(node.Colo+" "+node.Location)))
	}
	if result.ISP != "" {
		lines = append(lines, "🏢 运营商: "+command.Code(result.ISP))
	}
	if node.IP != "" {
		lines = append(lines, "🌐 出口: "+command.Code(maskAddress(node.IP)))
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
		if err := inv.Edit(ctx, "🚀 <b>网络测速</b>\n\n正在准备…"); err != nil {
			return err
		}
		node := describeNode(ctx)

		forceBuiltin := false
		switch strings.ToLower(inv.Arg(0)) {
		case "cf", "builtin", "内置":
			forceBuiltin = true
		}
		tool, kind := externalTool(dataDir)
		if tool == "" && !forceBuiltin {
			// Nothing installed: fetch the official CLI once and keep it.
			if err := inv.Edit(ctx, "🚀 <b>网络测速</b>\n\n首次运行，正在下载 Ookla 官方 CLI…"); err != nil {
				return err
			}
			installed, err := installOokla(ctx, dataDir)
			if err != nil {
				inv.Log.Warn("speedtest.install_failed", "error", err.Error())
			} else {
				tool, kind = installed, "ookla"
			}
		}
		if tool != "" && !forceBuiltin {
			if err := inv.Edit(ctx, "🚀 <b>网络测速</b>\n\n正在通过 "+command.Escape(kind)+" 测速，约需一分钟…"); err != nil {
				return err
			}
			result, err := runExternal(ctx, tool, kind)
			if err == nil {
				text := render(result, node, time.Since(started))
				// Speedtest publishes a picture of every result; sending it
				// is what people expect to see, and the numbers ride along
				// as the caption.
				if image := resultImage(ctx, result.Link); image != nil {
					peer, peerErr := inv.Client.InputPeer(inv.Message.Peer)
					if peerErr == nil {
						if sendErr := inv.Client.SendPhoto(ctx, peer, "speedtest.png", image, text, 0); sendErr == nil {
							return inv.Client.DeleteMessage(ctx, inv.Message)
						}
						inv.Log.Info("speedtest.photo_failed")
					}
				}
				return inv.Edit(ctx, text)
			}
			// An installed tool that fails is not a reason to report
			// nothing: the built-in path still works.
			inv.Log.Warn("speedtest.external_failed", "tool", kind, "error", err.Error())
		}

		header := "🚀 <b>网络测速</b>\n\n"
		if node.Colo != "" {
			header += "📍 节点: " + command.Code(strings.TrimSpace(node.Colo+" "+node.Location)) + "\n"
		}
		if node.IP != "" {
			header += "🌐 出口: " + command.Code(maskAddress(node.IP)) + "\n"
		}
		header += "\n"
		result, err := builtinReading(ctx, inv, header)
		if err != nil {
			if detail, ok := isUserError(err); ok {
				return inv.EditText(ctx, "❌ "+detail)
			}
			return err
		}
		return inv.Edit(ctx, render(result, node, time.Since(started)))
	}
	a.Registry.Register(
		&command.Command{Name: "speedtest", Description: "测量服务器网络速度", Usage: "[cf]",
			Help: func(prefix string) string { return speedtestHelp(a.DataDir(), prefix) }, Timeout: 5 * time.Minute, Handle: handle},
		&command.Command{Name: "st", Description: "speedtest 的简写", Hidden: true,
			Help: func(prefix string) string { return speedtestHelp(a.DataDir(), prefix) }, Timeout: 5 * time.Minute, Handle: handle},
	)
}
