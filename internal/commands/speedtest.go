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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/httpx"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/store"
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
func runExternal(ctx context.Context, path, kind, home string, server int) (*reading, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	var arguments []string
	switch kind {
	case "ookla":
		arguments = []string{"-f", "json", "--accept-license", "--accept-gdpr"}
		if server > 0 {
			arguments = append(arguments, "-s", strconv.Itoa(server))
		}
	default:
		arguments = []string{"--json", "--secure"}
		if server > 0 {
			arguments = append(arguments, "--server", strconv.Itoa(server))
		}
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
			return nil, failf("%s", explainCLI(detail))
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
		where := strings.TrimSpace(parsed.Server.Name + " " + parsed.Server.Location)
		return &reading{
			Source: "Ookla Speedtest", Latency: durationFromMillis(parsed.Ping.Latency),
			Jitter: durationFromMillis(parsed.Ping.Jitter),
			// Ookla reports bytes per second.
			Download: parsed.Download.Bandwidth * 8, Upload: parsed.Upload.Bandwidth * 8,
			Server: where, ISP: parsed.ISP, Link: parsed.Result.URL,
			ExternalIP: parsed.Interface.ExternalIP,
		}, nil
	}
	var parsed pythonResult
	if err := json.Unmarshal(line, &parsed); err != nil {
		return nil, fail("无法解析 speedtest-cli 的输出")
	}
	where := strings.TrimSpace(parsed.Server.Sponsor + " " + parsed.Server.Name)
	return &reading{
		Source: "speedtest-cli", Latency: durationFromMillis(parsed.Ping),
		Download: parsed.Download, Upload: parsed.Upload,
		Server: where, ISP: parsed.Client.ISP, Link: parsed.Share,
		ExternalIP: parsed.Client.IP,
	}, nil
}

// speedServer is one entry of `speedtest -f json -L`.
type speedServer struct {
	ID       int    `json:"id"`
	Host     string `json:"host"`
	Name     string `json:"name"`
	Location string `json:"location"`
	Country  string `json:"country"`
}

// listServers asks the CLI which servers it can see from here.
//
// Ookla orders them by its own idea of proximity, so the first entries are
// the ones worth pinning; the whole list is hundreds long and no use in a
// chat.
func listServers(ctx context.Context, path, home string) ([]speedServer, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	tool := exec.CommandContext(ctx, path, "-f", "json", "-L", "--accept-license", "--accept-gdpr")
	tool.Env = append(os.Environ(), "HOME="+home)
	var out, complaint bytes.Buffer
	tool.Stdout = &out
	tool.Stderr = &complaint
	if err := tool.Run(); err != nil {
		if detail := lastLine(complaint.String()); detail != "" {
			return nil, fail("取服务器列表失败：" + explainCLI(detail))
		}
		return nil, fmt.Errorf("speedtest -L failed: %w", err)
	}
	var parsed struct {
		Servers []speedServer `json:"servers"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		return nil, fail("无法解析服务器列表")
	}
	if len(parsed.Servers) == 0 {
		return nil, fail("没有可用的测速服务器")
	}
	return parsed.Servers, nil
}

// speedListLimit is how many servers a chat message can usefully hold.
const speedListLimit = 12

// speedNameLimit keeps one server on one line.
const speedNameLimit = 28

// renderServers lays the list out one server per line.
//
// Two lines each with an indented location was 20 lines for 10 servers and
// read as a wall. A monospace table would align the ids, but the names
// come back in Japanese as often as not and CJK widths do not align in a
// pre block anyway, so the id is simply first and the rest follows it.
func renderServers(servers []speedServer, pinned int, prefix string) string {
	shown := servers
	if len(shown) > speedListLimit {
		shown = shown[:speedListLimit]
	}
	// When they are all in one country, saying so once is enough.
	common := ""
	for index, server := range shown {
		if index == 0 {
			common = server.Country
		} else if server.Country != common {
			common = ""
			break
		}
	}
	header := "🌐 <b>可用测速服务器</b>"
	if common != "" {
		header += " · " + command.Escape(common)
	}
	lines := []string{header, ""}
	for _, server := range shown {
		name := server.Name
		if runes := []rune(name); len(runes) > speedNameLimit {
			name = string(runes[:speedNameLimit]) + "…"
		}
		line := command.Code(strconv.Itoa(server.ID)) + " " + command.Escape(name)
		if where := strings.TrimSpace(server.Location); where != "" {
			line += " · " + command.Escape(where)
		}
		if common == "" && server.Country != "" {
			line += " " + command.Escape(server.Country)
		}
		if server.ID == pinned {
			line += " ✅"
		}
		lines = append(lines, line)
	}
	// A worked example beats a placeholder: the id below can be copied.
	sample := strconv.Itoa(shown[0].ID)
	if pinned > 0 {
		sample = strconv.Itoa(pinned)
	}
	lines = append(lines, "",
		"<i>"+command.Escape(prefix+"speedtest "+sample)+" 测这一台，"+
			command.Escape(prefix+"speedtest set "+sample)+" 设为默认</i>")
	return strings.Join(lines, "\n")
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
//
// Ookla writes that line as a JSON log record. Its message field is a
// sentence a person can act on ("Could not retrieve or read
// configuration"); the surrounding JSON is noise in a chat, so it is
// unwrapped when it parses and kept verbatim when it does not.
func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var record struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &record); err == nil && record.Message != "" {
			line = record.Message
		}
		if len(line) > 200 {
			line = line[:200] + "…"
		}
		return line
	}
	return ""
}

// explainCLI turns the CLI's own wording into something actionable.
//
// Both of these were met here for real. Running the test many times in an
// afternoon got this host refused by Ookla's configuration endpoint, and
// the message for that says "ConfigurationError", which reads like a
// broken install rather than "wait a while". A listed server that will
// not answer says "Cannot read from socket", which reads like a local
// network fault rather than "pick another one".
func explainCLI(detail string) string {
	switch {
	case strings.Contains(detail, "Configuration"):
		return "Speedtest 暂时拒绝了这台机器的请求，通常是短时间内测得太频繁，过一阵再试"
	case strings.Contains(detail, "Cannot read from socket"), strings.Contains(detail, "Latency test failed"):
		return "这个服务器现在连不上，换一个 ID 或用自动挑选"
	case strings.Contains(detail, "NoServersException"):
		return "找不到可用的测速服务器"
	}
	return detail
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

func speedtestHelp(dataDir, prefix string, pinned int) string {
	p := command.Escape(prefix)
	tool, kind := externalTool(dataDir)
	source := "未安装，首次运行会自动下载 Ookla 官方 CLI"
	if tool != "" {
		source = "使用 " + kind + "：" + tool
	}
	server := "自动挑选最近的服务器"
	if pinned > 0 {
		server = "固定用 " + strconv.Itoa(pinned)
	}
	return "🚀 <b>网络测速</b>\n\n测量这台服务器的出口带宽。\n\n• <code>" + p +
		"speedtest</code> 完整测速\n• <code>" + p + "st</code> 同上，简写\n• <code>" + p +
		"speedtest list</code> 列出可用服务器\n• <code>" + p +
		"speedtest &lt;ID&gt;</code> 只这一次用指定服务器\n• <code>" + p +
		"speedtest set &lt;ID&gt;</code> 设为默认服务器\n• <code>" + p +
		"speedtest clear</code> 恢复自动挑选\n• <code>" + p +
		"speedtest config</code> 看当前设置\n\n<b>当前来源</b>\n" + command.Escape(source) +
		"\n\n<b>当前服务器</b>\n" + command.Escape(server) +
		"\n\n用 Ookla 官方 CLI 测：它自己挑就近的测速服务器，报得出 ISP，还会给一张结果图。没装的话首次" +
		"运行会把官方静态构件下载到部署目录（校验 SHA-256，不写系统目录），之后直接复用。\n\n" +
		"指定的服务器测不通时会自动退回自动挑选，并在结果里说明。输出里的出口地址会打码。"
}

// render lays out a finished reading.
func render(result *reading, elapsed time.Duration, note string) string {
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
	if note != "" {
		lines = append(lines, "<i>"+command.Escape(note)+"</i>")
	}
	if result.Link != "" {
		lines = append(lines, "🔗 <a href=\""+command.Escape(result.Link)+"\">详细结果</a>")
	}
	lines = append(lines, "<i>来源 "+command.Escape(result.Source)+" · 用时 "+
		command.Escape(fmt.Sprintf("%.1f 秒", elapsed.Seconds()))+"</i>")
	return strings.Join(lines, "\n")
}

// speedtestDocument 记住固定的测速服务器。
type speedtestDocument struct {
	Server int `json:"server,omitempty"`
}

// speedtester 放 .speedtest 各个子命令共用的东西。
type speedtester struct {
	dataDir  string
	settings *store.Store[speedtestDocument]
	// running 保证同一时间只跑一次测速：两次同时跑会互相抢带宽，测出来的都不准。
	running sync.Mutex
}

// home 是给 CLI 当 HOME 用的目录，它要在里面记授权状态。
func (s *speedtester) home() string { return filepath.Join(s.dataDir, "speedtest") }

func (s *speedtester) pinned() int {
	current, err := s.settings.Read()
	if err != nil {
		return 0
	}
	return current.Server
}

func (s *speedtester) help(prefix string) string {
	return speedtestHelp(s.dataDir, prefix, s.pinned())
}

// ensureTool 找到 CLI，第一次用时顺手装上。
func (s *speedtester) ensureTool(ctx context.Context, inv *command.Invocation) (string, string, error) {
	if tool, kind := externalTool(s.dataDir); tool != "" {
		return tool, kind, nil
	}
	if err := inv.Edit(ctx, "🚀 <b>网络测速</b>\n\n首次运行，正在下载 Ookla 官方 CLI…"); err != nil {
		return "", "", err
	}
	installed, err := installOokla(ctx, s.dataDir)
	if err != nil {
		inv.Log.Warn("speedtest.install_failed", "error", err.Error())
		return "", "", err
	}
	return installed, "ookla", nil
}

func installProblem(ctx context.Context, inv *command.Invocation, err error) error {
	detail, ok := isUserError(err)
	if !ok {
		detail = "网络不通或构件无法校验"
	}
	return inv.EditText(ctx, "❌ 无法安装 Speedtest CLI："+detail+"\n手动装好 speedtest 后再试")
}

// setting 处理 help、config、clear、set。第一个返回值表示参数是不是这几个之一。
func (s *speedtester) setting(ctx context.Context, inv *command.Invocation) (bool, error) {
	switch strings.ToLower(inv.Arg(0)) {
	case "help", "h", "config":
		return true, inv.Edit(ctx, s.help(inv.Prefix))
	case "clear", "auto", "自动":
		if err := s.settings.Update(func(value *speedtestDocument) error { value.Server = 0; return nil }); err != nil {
			return true, err
		}
		return true, inv.Edit(ctx, "✅ 已恢复自动挑选服务器\n<i>想再固定一台，"+
			command.Escape(inv.Prefix+"speedtest list")+" 看有哪些</i>")
	case "set":
		id, err := strconv.Atoi(inv.Arg(1))
		if err != nil || id <= 0 {
			return true, inv.EditText(ctx, "用法：set 后面跟服务器 ID，ID 用 list 查")
		}
		if err := s.settings.Update(func(value *speedtestDocument) error { value.Server = id; return nil }); err != nil {
			return true, err
		}
		return true, inv.Edit(ctx, "✅ 默认服务器已设为 "+command.Code(strconv.Itoa(id))+
			"\n<i>测不通会自动退回自动挑选；取消固定用 "+command.Escape(inv.Prefix+"speedtest clear")+"</i>")
	}
	return false, nil
}

// list 列出 CLI 能看到的测速服务器。
func (s *speedtester) list(ctx context.Context, inv *command.Invocation) error {
	if err := inv.Edit(ctx, "🌐 正在取服务器列表…"); err != nil {
		return err
	}
	tool, kind, err := s.ensureTool(ctx, inv)
	if err != nil {
		return installProblem(ctx, inv, err)
	}
	if kind != "ookla" {
		return inv.EditText(ctx, "只有 Ookla 官方 CLI 能列出服务器，当前用的是 speedtest-cli")
	}
	servers, err := listServers(ctx, tool, s.home())
	if detail, ok := isUserError(err); ok {
		return inv.EditText(ctx, "❌ "+detail)
	}
	if err != nil {
		return err
	}
	return inv.Edit(ctx, renderServers(servers, s.pinned(), inv.Prefix))
}

// measure 跑一次测速，失败了重试一次。
//
// CLI 偶尔会中途丢掉服务器（报 "Latency test failed"，这台机器上见过两次），
// 固定的服务器也可能已经下线，所以重试时顺便放弃固定、改用自动挑选，
// 而不是因为几天前选的服务器不在了就整个失败。返回的 note 说明发生过这种退回。
func (s *speedtester) measure(ctx context.Context, inv *command.Invocation, tool, kind string, server int) (*reading, string, error) {
	result, err := runExternal(ctx, tool, kind, s.home(), server)
	if err == nil {
		return result, "", nil
	}
	inv.Log.Warn("speedtest.retrying", "tool", kind, "server", server, "error", err.Error())
	note := ""
	if server > 0 {
		note = "服务器 " + strconv.Itoa(server) + " 没测通，已改用自动挑选。换一台用 " +
			inv.Prefix + "speedtest list，取消固定用 " + inv.Prefix + "speedtest clear"
	}
	result, err = runExternal(ctx, tool, kind, s.home(), 0)
	return result, note, err
}

// deliver 把结果连同 Speedtest 的结果图一起发出去，数字作为图说明；
// 图发不出去就退回成改文字。
//
// 两种失败各记各的原因：聊天禁止发媒体、对话解析不出来、网络出错是三种不同的问题，
// 群里出现过一条什么原因都没带的 photo_failed，那条日志什么也回答不了。
func deliver(ctx context.Context, inv *command.Invocation, text, link string) error {
	image := resultImage(ctx, link)
	if image == nil {
		return inv.Edit(ctx, text)
	}
	peer, err := inv.Client.InputPeer(inv.Message.Peer)
	if err != nil {
		inv.Log.Info("speedtest.peer_unresolved", "error", err.Error())
		return inv.Edit(ctx, text)
	}
	if err := inv.Client.SendPhoto(ctx, peer, "speedtest.png", image, text, 0); err != nil {
		inv.Log.Info("speedtest.photo_failed", "error", err.Error())
		return inv.Edit(ctx, text)
	}
	return inv.Client.DeleteMessage(ctx, inv.Message)
}

func (s *speedtester) handle(ctx context.Context, inv *command.Invocation) error {
	if handled, err := s.setting(ctx, inv); handled {
		return err
	}
	if !s.running.TryLock() {
		return inv.EditText(ctx, "已有一个测速在进行，请稍候")
	}
	defer s.running.Unlock()

	first := strings.ToLower(inv.Arg(0))
	if first == "list" || first == "servers" || first == "列表" {
		return s.list(ctx, inv)
	}
	// 单独一个数字表示只这一次用那台服务器，不改默认设置。
	server, once := s.pinned(), false
	if first != "" {
		id, err := strconv.Atoi(first)
		if err != nil || id <= 0 {
			return inv.Edit(ctx, s.help(inv.Prefix))
		}
		server, once = id, true
	}

	started := time.Now()
	tool, kind, err := s.ensureTool(ctx, inv)
	if err != nil {
		return installProblem(ctx, inv, err)
	}
	where := "，约需一分钟…"
	if server > 0 {
		where = "（服务器 " + strconv.Itoa(server) + "），约需一分钟…"
	}
	if err := inv.Edit(ctx, "🚀 <b>网络测速</b>\n\n正在通过 "+command.Escape(kind)+" 测速"+command.Escape(where)); err != nil {
		return err
	}
	result, note, err := s.measure(ctx, inv, tool, kind, server)
	if err != nil {
		inv.Log.Warn("speedtest.external_failed", "tool", kind, "error", err.Error())
		if detail, ok := isUserError(err); ok {
			return inv.EditText(ctx, "❌ "+detail)
		}
		return inv.EditText(ctx, "❌ 测速失败，请稍后再试")
	}
	if once && note == "" {
		note = "本次指定了服务器，未改动默认设置"
	}
	return deliver(ctx, inv, render(result, time.Since(started), note), result.Link)
}

// Speedtest 注册 .speedtest 和它的简写 .st。
func Speedtest(a *app.App) {
	tester := &speedtester{dataDir: a.DataDir(),
		settings: newStore(a, "speedtest.json", func() speedtestDocument { return speedtestDocument{} })}
	a.Registry.Register(
		&command.Command{Name: "speedtest", Description: "测量服务器网络速度", Usage: "[list|set ID|clear|ID]",
			Help: tester.help, Timeout: 5 * time.Minute, Handle: tester.handle},
		&command.Command{Name: "st", Description: "speedtest 的简写", Hidden: true,
			Help: tester.help, Timeout: 5 * time.Minute, Handle: tester.handle},
	)
}
