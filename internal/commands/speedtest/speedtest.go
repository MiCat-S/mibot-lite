// Package speedtest 实现 .speedtest（简写 .st）：用 Ookla 的 Speedtest CLI 测速。
package speedtest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/httpx"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// 测速用的是 Ookla 官方的 Speedtest CLI：它会自己挑就近的服务器，认得出
// ISP，还会发布一个大家都认得的结果页。没装 CLI 时，本命令会把它装一次到
// 自己的数据目录里，之后一直复用。

// reading 是一次完整的测量结果，不管是用哪种方式测的。
type reading struct {
	// Source 是产生这次结果的工具名，用在注明来源的那一行里。
	Source   string
	Latency  time.Duration
	Jitter   time.Duration
	Download float64
	Upload   float64
	// DownloadBytes、UploadBytes 是测速过程实际收发的字节数，工具没报时为 0。
	DownloadBytes float64
	UploadBytes   float64
	Server        string
	ServerID      int
	ISP           string
	Link          string
	ExternalIP    string
	// Timestamp 是工具记录的测速时刻，原样保留，显示时再换成 UTC。
	Timestamp string
	// ASN 和 Country 是出口地址所属的自治系统和国家代码，另外查询得到，查不到为空。
	ASN     string
	Country string
}

// ooklaResult 对应官方 CLI 的 `speedtest -f json` 输出，其中的带宽数值
// 单位是字节每秒。
type ooklaResult struct {
	Timestamp string `json:"timestamp"`
	Ping      struct {
		Latency float64 `json:"latency"`
		Jitter  float64 `json:"jitter"`
	} `json:"ping"`
	Download struct {
		Bandwidth float64 `json:"bandwidth"`
		Bytes     float64 `json:"bytes"`
	} `json:"download"`
	Upload struct {
		Bandwidth float64 `json:"bandwidth"`
		Bytes     float64 `json:"bytes"`
	} `json:"upload"`
	ISP    string `json:"isp"`
	Server struct {
		ID       int    `json:"id"`
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

// pythonResult 对应 `speedtest-cli --json` 的输出，其中的数值单位是比特每秒。
type pythonResult struct {
	Download      float64 `json:"download"`
	Upload        float64 `json:"upload"`
	Ping          float64 `json:"ping"`
	Timestamp     string  `json:"timestamp"`
	BytesSent     float64 `json:"bytes_sent"`
	BytesReceived float64 `json:"bytes_received"`
	Server        struct {
		// speedtest-cli 把服务器 ID 写成字符串。
		ID      json.Number `json:"id"`
		Name    string      `json:"name"`
		Country string      `json:"country"`
		Sponsor string      `json:"sponsor"`
	} `json:"server"`
	Client struct {
		ISP string `json:"isp"`
		IP  string `json:"ip"`
	} `json:"client"`
	Share string `json:"share"`
}

// Ookla 发布了静态编译的二进制，不需要包管理器，也不需要 root。版本号和
// 摘要都固定在这里：这段代码会下载一个可执行文件然后运行它，光凭「它是
// 从正确的域名经 HTTPS 下载的」还不够。
const ooklaVersion = "1.2.0"

var ooklaDigests = map[string]string{
	"amd64": "5690596c54ff9bed63fa3732f818a05dbc2db19ad36ed68f21ca5f64d5cfeeb7",
	"arm64": "3953d231da3783e2bf8904b6dd72767c5c6e533e163d3742fd0437affa431bd3",
}

var ooklaArchives = map[string]string{"amd64": "x86_64", "arm64": "aarch64"}

// errNotOokla 表示一个叫 speedtest 的程序不是 Ookla 官方 CLI。
var errNotOokla = errors.New("不是 Ookla 官方 CLI")

// probeVersion 运行 path --version，返回它报的第一行。输出里没有
// "Speedtest by Ookla" 就返回 errNotOokla：Python 版 speedtest-cli 也会装一个
// 叫 speedtest 的命令，参数和输出格式都不同，当成官方 CLI 调用只会失败。
func probeVersion(path, home string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	tool := exec.CommandContext(ctx, path, "--version")
	tool.Env = append(os.Environ(), "HOME="+home)
	output, err := tool.Output()
	first, _, _ := strings.Cut(strings.TrimSpace(string(output)), "\n")
	first = strings.TrimSpace(first)
	if len(first) > 160 {
		first = first[:160]
	}
	if err != nil {
		return first, err
	}
	if !strings.Contains(strings.ToLower(string(output)), "speedtest by ookla") {
		return first, errNotOokla
	}
	return first, nil
}

// ooklaChecks 缓存 PATH 上各个 speedtest 是不是官方 CLI，键是路径、大小和修改时间，
// 同一个文件只运行一次 --version。
var ooklaChecks sync.Map

func isOokla(path, home string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	key := path + "|" + strconv.FormatInt(info.Size(), 10) + "|" + info.ModTime().String()
	if known, ok := ooklaChecks.Load(key); ok {
		return known.(bool)
	}
	_, err = probeVersion(path, home)
	ooklaChecks.Store(key, err == nil)
	return err == nil
}

// externalTool 查找已安装的 speedtest 命令，优先用 Ookla 官方的，
// 本命令自己装的那份也算在内。PATH 上的 speedtest 要先确认是官方 CLI 才用。
func externalTool(dataDir string) (string, string) {
	if path, err := exec.LookPath("speedtest"); err == nil && isOokla(path, filepath.Join(dataDir, "speedtest")) {
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

// installOokla 把官方静态二进制下载到本部署自己的数据目录。
//
// 放在这里而不是 /usr/local/bin，是因为这个程序不该自作主张去改系统，
// 而且部署被删掉时，它装过的东西也应该跟着一起消失。
func installOokla(ctx context.Context, dataDir string) (string, error) {
	archive, ok := ooklaArchives[runtime.GOARCH]
	digest, hasDigest := ooklaDigests[runtime.GOARCH]
	if !ok || !hasDigest {
		return "", kit.Failf("没有 %s 架构的 Ookla CLI 构件", runtime.GOARCH)
	}
	url := fmt.Sprintf("https://install.speedtest.net/app/cli/ookla-speedtest-%s-linux-%s.tgz", ooklaVersion, archive)
	response, err := httpx.Do(ctx, httpx.Request{URL: url, Timeout: 90 * time.Second, MaxBytes: 16 << 20})
	if err != nil {
		return "", err
	}
	if !response.OK() {
		return "", kit.Failf("下载 Ookla CLI 失败：HTTP %d", response.Status)
	}
	sum := sha256.Sum256(response.Body)
	if hex.EncodeToString(sum[:]) != digest {
		return "", kit.Fail("Ookla CLI 的 SHA-256 与预期不符，已丢弃")
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

// extractOokla 只从压缩包里取出 speedtest 这一个可执行文件。
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
		// 只要这一个文件，并且按确切的文件名匹配：压缩包里的条目是
		// 不可信输入，不能让条目里的路径决定这里往哪儿写。
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
	return "", kit.Fail("Ookla CLI 压缩包里没有可执行文件")
}

// cliFailure 是测速工具以非零状态退出。
//
// Explained 是发进聊天的那句话，其余字段写进日志：以前日志里只剩一句
// "ookla failed: exit status 2"，是哪台服务器、为什么失败都看不出来。
type cliFailure struct {
	Kind string
	Exit error
	// Detail 是从两路输出里拣出来的原因原文，可能为空。
	Detail string
	// Tested、TestedName 是 CLI 实际连上的服务器，取自 jsonl 的 testStart；没走到那一步时为零值。
	Tested     int
	TestedName string
	// TimedOut 表示是本命令等不及把它杀掉的，不是它自己退出的。
	TimedOut bool
	// Explained 为空时，聊天里只说「测速失败」。
	Explained string
}

func (e *cliFailure) Error() string {
	text := e.Kind + " failed"
	if e.Tested > 0 {
		text += " on server " + strconv.Itoa(e.Tested)
	}
	text += ": " + e.Exit.Error()
	if e.Detail != "" {
		text += ": " + e.Detail
	}
	return text
}

// Unwrap 让 kit.IsUserError 取得 Explained，errors.Is 也还能认出原来的退出错误。
func (e *cliFailure) Unwrap() []error {
	if e.Explained == "" {
		return []error{e.Exit}
	}
	return []error{e.Exit, kit.Fail(e.Explained)}
}

// retryable 判断换一次服务器再测有没有意义：被限流、等超时之后再测，只会更糟、更慢。
func (e *cliFailure) retryable() bool {
	if e.TimedOut {
		return false
	}
	reason, ok := reasonFor(e.Detail)
	return !ok || reason.retry
}

// runExternal 调用已安装的 speedtest 工具。
//
// home 是允许该工具存放自身状态的目录。这一点很关键：Ookla CLI 要读
// $HOME 来找它记录许可协议的位置，而在本服务的沙箱里 $HOME 没有设置，
// 它扛不住——还没开始测就因为一个空字符串直接中止。给它一个自己可写的
// 目录，也能避免 ProtectSystem=strict 把它搞坏。
//
// Ookla 用 jsonl 格式跑：结果还是最后一行，前面多了进度记录，其中 testStart 记着
// 它自动挑中的服务器。测到一半失败时，要靠这个知道该避开哪一台。
func runExternal(ctx context.Context, path, kind, home string, server int) (*reading, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	var arguments []string
	switch kind {
	case "ookla":
		arguments = []string{"-f", "jsonl", "--accept-license", "--accept-gdpr"}
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
	var out, complaint bytes.Buffer
	tool.Stdout = &out
	tool.Stderr = &complaint
	if err := tool.Run(); err != nil {
		failure := &cliFailure{Kind: kind, Exit: err, Detail: failureDetail(out.String(), complaint.String())}
		failure.Tested, failure.TestedName = testedServer(out.Bytes())
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			failure.TimedOut = true
			failure.Explained = "测速两分钟还没测完，已经中止，稍后再试或换一台服务器"
		case failure.Detail != "":
			failure.Explained = explainCLI(failure.Detail)
		}
		return nil, failure
	}
	return parseResult(kind, out.Bytes())
}

// testedServer 从 jsonl 输出的 testStart 记录里取出 CLI 连的服务器。
func testedServer(output []byte) (int, string) {
	for _, line := range strings.Split(string(output), "\n") {
		var record struct {
			Type   string `json:"type"`
			Server struct {
				ID       int    `json:"id"`
				Name     string `json:"name"`
				Location string `json:"location"`
			} `json:"server"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &record) == nil && record.Type == "testStart" {
			return record.Server.ID, strings.TrimSpace(record.Server.Name + " " + record.Server.Location)
		}
	}
	return 0, ""
}

// parseResult 把工具的输出换成 reading。kind 是 "ookla" 或 "python"。
func parseResult(kind string, output []byte) (*reading, error) {
	// Ookla 每行输出一个 JSON 对象，type 为 result 的那行是结果，正常情况下就是最后一行。
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	line := []byte(strings.TrimSpace(lines[len(lines)-1]))
	for i := len(lines) - 1; i >= 0; i-- {
		var record struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(lines[i])), &record) == nil && record.Type == "result" {
			line = []byte(strings.TrimSpace(lines[i]))
			break
		}
	}

	if kind == "ookla" {
		var parsed ooklaResult
		if err := json.Unmarshal(line, &parsed); err != nil {
			return nil, kit.Fail("无法解析 speedtest 的输出")
		}
		where := strings.TrimSpace(parsed.Server.Name + " " + parsed.Server.Location)
		return &reading{
			Source: "Ookla Speedtest", Latency: durationFromMillis(parsed.Ping.Latency),
			Jitter: durationFromMillis(parsed.Ping.Jitter),
			// Ookla 报的是字节每秒。
			Download: parsed.Download.Bandwidth * 8, Upload: parsed.Upload.Bandwidth * 8,
			DownloadBytes: parsed.Download.Bytes, UploadBytes: parsed.Upload.Bytes,
			Server: where, ServerID: parsed.Server.ID, ISP: parsed.ISP, Link: parsed.Result.URL,
			ExternalIP: parsed.Interface.ExternalIP, Timestamp: parsed.Timestamp,
		}, nil
	}
	var parsed pythonResult
	if err := json.Unmarshal(line, &parsed); err != nil {
		return nil, kit.Fail("无法解析 speedtest-cli 的输出")
	}
	where := strings.TrimSpace(parsed.Server.Sponsor + " " + parsed.Server.Name)
	serverID, _ := strconv.Atoi(parsed.Server.ID.String())
	return &reading{
		Source: "speedtest-cli", Latency: durationFromMillis(parsed.Ping),
		Download: parsed.Download, Upload: parsed.Upload,
		DownloadBytes: parsed.BytesReceived, UploadBytes: parsed.BytesSent,
		Server: where, ServerID: serverID, ISP: parsed.Client.ISP, Link: parsed.Share,
		ExternalIP: parsed.Client.IP, Timestamp: parsed.Timestamp,
	}, nil
}

// speedServer 是 `speedtest -f json -L` 输出里的一项。
type speedServer struct {
	ID       int    `json:"id"`
	Host     string `json:"host"`
	Name     string `json:"name"`
	Location string `json:"location"`
	Country  string `json:"country"`
}

// listServers 问 CLI 从这台机器能看到哪些服务器。
//
// Ookla 按它自己判断的远近排序，所以排在前面的才值得固定；
// 完整列表有好几百项，放进聊天里没什么用。
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
		if detail := failureDetail(out.String(), complaint.String()); detail != "" {
			return nil, kit.Fail("取服务器列表失败：" + explainCLI(detail))
		}
		return nil, fmt.Errorf("speedtest -L failed: %w", err)
	}
	var parsed struct {
		Servers []speedServer `json:"servers"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		return nil, kit.Fail("无法解析服务器列表")
	}
	if len(parsed.Servers) == 0 {
		return nil, kit.Fail("没有可用的测速服务器")
	}
	return parsed.Servers, nil
}

// speedListLimit 是一条聊天消息里值得列出的服务器数量上限。
const speedListLimit = 12

// speedNameLimit 保证每台服务器只占一行。
const speedNameLimit = 28

// renderServers 把列表排成每台服务器一行。
//
// 以前每台占两行、第二行缩进写位置，10 台就是 20 行，读起来像一堵墙。
// 等宽表格能让 ID 对齐，但服务器名经常是日文，而 CJK 字符在 pre 块里
// 本来就对不齐，所以干脆把 ID 放在最前面，其余内容跟在后面。
func renderServers(servers []speedServer, pinned int, prefix string) string {
	shown := servers
	if len(shown) > speedListLimit {
		shown = shown[:speedListLimit]
	}
	// 全都在同一个国家时，只说一次就够了。
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
	// 给现成的例子比给占位符好：下面的 ID 可以直接复制。
	sample := strconv.Itoa(shown[0].ID)
	if pinned > 0 {
		sample = strconv.Itoa(pinned)
	}
	lines = append(lines, "",
		"<i>"+command.Escape(prefix+"speedtest "+sample)+" 测这一台，"+
			command.Escape(prefix+"speedtest set "+sample)+" 设为默认</i>")
	return strings.Join(lines, "\n")
}

// resultImage 获取 Speedtest 为结果发布的图片，拿不到时返回 nil。
// 没有图片不算失败：测量本身才是重点，数字已经到手了。
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
	// 只转发真正的 PNG：把错误页当成图片发出去，还不如不发。
	if len(response.Body) < 8 || string(response.Body[1:4]) != "PNG" {
		return nil
	}
	return response.Body
}

// logStamp 是 Ookla 纯文本日志行开头的 "[2026-09-26 09:23:48.288] [error] "。
var logStamp = regexp.MustCompile(`^(\[[^\]]*\]\s*)+`)

// failureDetail 从 CLI 的两路输出里拣出失败原因，按出现顺序去重后用「; 」连起来。
//
// Ookla 的原因不总在同一个地方：多数错误是 stderr 上的一条 JSON 日志记录；被限流时
// 是 stderr 上的几行纯文本；测到一半连接被对端重置时，只在 stdout 打一行
// {"error":"Cannot write: "}，stderr 是空的。以前只看 stderr 的最后一行，最后这种
// 失败就只剩一句 exit status 2，聊天里也只能回「测速失败」。
//
// JSON 记录只取 error 字段或 message 字段，外面那层包装在聊天里只是噪音；进度记录
// 和 info 级别的日志不算原因。
func failureDetail(stdout, stderr string) string {
	var reasons, prose []string
	add := func(text string) {
		if text = strings.TrimSpace(text); text != "" && !slices.Contains(reasons, text) {
			reasons = append(reasons, text)
		}
	}
	for _, line := range strings.Split(stdout+"\n"+stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record struct {
			Error   string `json:"error"`
			Message string `json:"message"`
			Level   string `json:"level"`
		}
		if json.Unmarshal([]byte(line), &record) != nil {
			prose = append(prose, logStamp.ReplaceAllString(line, ""))
			continue
		}
		switch {
		case record.Error != "":
			add(record.Error)
		case record.Message != "" && record.Level != "info" && record.Level != "debug":
			add(record.Message)
		}
	}
	// 纯文本的一段话被折成了好几行，拼回一句。
	add(strings.Join(prose, " "))
	detail := strings.Join(reasons, "; ")
	if runes := []rune(detail); len(runes) > 300 {
		detail = string(runes[:300]) + "…"
	}
	return detail
}

// cliReason 是一类已知的失败：markers 里任何一句出现在原因里就算，retry 表示换台服务器
// 再测有没有意义。
type cliReason struct {
	markers []string
	text    string
	retry   bool
}

// cliReasons 把 CLI 自己的措辞换成能照着做的提示，按顺序匹配，排在前面的优先。
//
// 这几种都真实遇到过：
//   - 短时间内测得太多，Ookla 退出码 173，stderr 上写 "Limit reached"；更早的版本
//     是配置接口拒绝，报 "ConfigurationError"，看起来像是安装坏了，其实是「等一会儿再试」。
//     这两种都是整台机器被限流，换服务器也一样，所以不重试。
//   - 指定的 ID 不存在时报 "Configuration - No servers defined (NoServersException)"，
//     也带着 Configuration，所以要排在限流那条前面，否则会被说成限流。
//   - 列表里的服务器不应答时报 "Cannot read from socket"，看起来像本地网络故障，
//     其实是「换一台」。
//   - 测到一半对端重置连接（errno 104）时只有 "Cannot read: " 或 "Cannot write: "，
//     某些服务器约三四次就有一次，重测多半又自动挑中它，所以要换一台。
var cliReasons = []cliReason{
	{markers: []string{"Limit reached", "Too many requests"}, text: "测得太频繁，被 Speedtest 限流了，过一阵再试"},
	{markers: []string{"NoServersException", "No servers defined"}, text: "找不到指定的测速服务器，ID 可能不对或已经下线", retry: true},
	{markers: []string{"Configuration"}, text: "Speedtest 暂时拒绝了这台机器的请求，通常是短时间内测得太频繁，过一阵再试"},
	{markers: []string{"Cannot read from socket", "Latency test failed"}, text: "这个服务器现在连不上，换一个 ID 或用自动挑选", retry: true},
	{markers: []string{"Cannot read:", "Cannot write:", "Connection reset", "Broken pipe"}, text: "测速服务器测到一半断开了连接，稍后再试，或换一台服务器固定下来", retry: true},
}

func reasonFor(detail string) (cliReason, bool) {
	for _, reason := range cliReasons {
		for _, marker := range reason.markers {
			if strings.Contains(detail, marker) {
				return reason, true
			}
		}
	}
	return cliReason{}, false
}

// explainCLI 返回原因对应的提示，认不出来就原样给出原因。
func explainCLI(detail string) string {
	if reason, ok := reasonFor(detail); ok {
		return reason.text
	}
	return detail
}

func durationFromMillis(value float64) time.Duration {
	return time.Duration(value * float64(time.Millisecond))
}

// maskAddress 隐藏主机自己的地址。结果是要发到聊天里的，
// 而测速结果没必要公开服务器的公网 IP。
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

// formatSpeed 把比特每秒换算成合适的量级显示。
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

// formatVolume 把字节数按 1000 进位显示，和带宽的单位一致。
func formatVolume(bytes float64) string {
	switch {
	case bytes >= 1e9:
		return fmt.Sprintf("%.2f GB", bytes/1e9)
	case bytes >= 1e6:
		return fmt.Sprintf("%.1f MB", bytes/1e6)
	case bytes >= 1e3:
		return fmt.Sprintf("%.1f KB", bytes/1e3)
	}
	return fmt.Sprintf("%.0f B", bytes)
}

// formatTimestamp 把工具报的时刻换成 UTC 显示；解析不了就原样给出。
func formatTimestamp(value string) string {
	value = strings.TrimSpace(value)
	if when, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return when.UTC().Format("2006-01-02 15:04:05") + " UTC"
	}
	if len(value) > 40 {
		value = value[:40]
	}
	return value
}

// countryFlag 把两个大写字母的国家代码换成国旗表情，代码不对时返回空。
func countryFlag(code string) string {
	if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		return ""
	}
	return string([]rune{0x1F1E6 + rune(code[0]-'A'), 0x1F1E6 + rune(code[1]-'A')})
}

// egress 是出口地址所属的自治系统和国家。
type egress struct {
	ASN     string
	Country string
}

// lookupEgress 向 ip-api.com 查出口地址的 ASN 和国家代码，查不到返回空值。
// ip-api 的免费接口只有明文 HTTP；发出去的只有这台机器自己的出口地址。
func lookupEgress(ctx context.Context, address string) egress {
	if net.ParseIP(address) == nil {
		return egress{}
	}
	var answer struct {
		Status      string `json:"status"`
		AS          string `json:"as"`
		CountryCode string `json:"countryCode"`
	}
	endpoint := "http://ip-api.com/json/" + address + "?fields=status,as,countryCode"
	if err := httpx.GetJSON(ctx, endpoint, 8*time.Second, 64<<10, &answer); err != nil || answer.Status != "success" {
		return egress{}
	}
	asn, _, _ := strings.Cut(strings.TrimSpace(answer.AS), " ")
	if len(asn) > 32 {
		asn = asn[:32]
	}
	code := strings.ToUpper(strings.TrimSpace(answer.CountryCode))
	if countryFlag(code) == "" {
		code = ""
	}
	return egress{ASN: asn, Country: code}
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
		"speedtest config</code> 看当前设置\n• <code>" + p +
		"speedtest diagnose</code> 检查 CLI 能否运行\n• <code>" + p +
		"speedtest fix</code> / <code>update</code> 重新下载官方 CLI（版本固定为 " + ooklaVersion + "）\n\n<b>当前来源</b>\n" + command.Escape(source) +
		"\n\n<b>当前服务器</b>\n" + command.Escape(server) +
		"\n\n用 Ookla 官方 CLI 测：它自己挑就近的测速服务器，报得出 ISP，还会给一张结果图。没装的话首次" +
		"运行会把官方静态构件下载到部署目录（校验 SHA-256，不写系统目录），之后直接复用。\n\n" +
		"指定的服务器测不通时会自动退回自动挑选；自动挑到的服务器测到一半断开时，会换最近的另一台重测，" +
		"两种情况都会在结果里说明。被 Speedtest 限流时不重试。输出里的出口地址会打码。"
}

// render 排版一次完成的测量结果。
func render(result *reading, elapsed time.Duration, note string) string {
	lines := []string{"🚀 <b>网络测速</b>", ""}
	if result.Server != "" {
		server := "📍 节点: " + command.Code(result.Server)
		if result.ServerID > 0 {
			server += " · ID " + command.Code(strconv.Itoa(result.ServerID))
		}
		lines = append(lines, server)
	}
	if isp := strings.TrimSpace(result.ISP + " " + result.ASN); isp != "" {
		lines = append(lines, "🏢 运营商: "+command.Code(isp))
	}
	if result.ExternalIP != "" {
		address := "🌐 出口: " + command.Code(maskAddress(result.ExternalIP))
		if flag := countryFlag(result.Country); flag != "" {
			address += " " + flag + " " + result.Country
		}
		lines = append(lines, address)
	}
	lines = append(lines, "")
	latency := "⏱ 延迟: " + command.Code(formatLatency(result.Latency))
	if result.Jitter > 0 {
		latency += "（抖动 " + command.Escape(formatLatency(result.Jitter)) + "）"
	}
	transfer := func(label string, bits, bytes float64) string {
		line := label + command.Code(formatSpeed(bits))
		if bytes > 0 {
			line += "（共 " + command.Escape(formatVolume(bytes)) + "）"
		}
		return line
	}
	lines = append(lines,
		latency,
		transfer("⬇️ 下载: ", result.Download, result.DownloadBytes),
		transfer("⬆️ 上传: ", result.Upload, result.UploadBytes))
	if result.Timestamp != "" {
		lines = append(lines, "🕒 时间: "+command.Code(formatTimestamp(result.Timestamp)))
	}
	lines = append(lines, "")
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
	// egress 查出口地址的 ASN 和国家；为 nil 时不查（测试里不连外网）。
	egress func(ctx context.Context, address string) egress
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
	detail, ok := kit.IsUserError(err)
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
	if detail, ok := kit.IsUserError(err); ok {
		return inv.EditText(ctx, "❌ "+detail)
	}
	if err != nil {
		return err
	}
	return inv.Edit(ctx, renderServers(servers, s.pinned(), inv.Prefix))
}

// measure 跑一次测速，失败了换台服务器重试一次。
//
// CLI 偶尔会中途丢掉服务器（报 "Latency test failed"，这台机器上见过两次），
// 固定的服务器也可能已经下线，所以重试时顺便放弃固定、改用自动挑选，
// 而不是因为几天前选的服务器不在了就整个失败。
//
// 本来就是自动挑选时，再自动挑一次多半还是同一台：它按延迟挑，刚才那台照样最快。
// 有的服务器三四次里就有一次测到一半断开，两次都挑中它，就两次都失败。所以
// 知道刚才连的是哪台时，改测列表里离得最近的另一台。
//
// 被限流或等超时的失败不重试。返回的 note 说明发生过的退回或换台。
func (s *speedtester) measure(ctx context.Context, inv *command.Invocation, tool, kind string, server int) (*reading, string, error) {
	result, err := runExternal(ctx, tool, kind, s.home(), server)
	if err == nil {
		return result, "", nil
	}
	var failure *cliFailure
	if errors.As(err, &failure) && !failure.retryable() {
		return nil, "", err
	}
	next, note := 0, ""
	switch {
	case server > 0:
		note = "服务器 " + strconv.Itoa(server) + " 没测通，已改用自动挑选。换一台用 " +
			inv.Prefix + "speedtest list，取消固定用 " + inv.Prefix + "speedtest clear"
	case failure != nil && failure.Tested > 0 && kind == "ookla":
		if servers, err := listServers(ctx, tool, s.home()); err == nil {
			for _, candidate := range servers {
				if candidate.ID != failure.Tested {
					next = candidate.ID
					break
				}
			}
		}
		if next > 0 {
			dropped := failure.TestedName
			if dropped == "" {
				dropped = "服务器"
			}
			note = "自动挑到的 " + dropped + "（" + strconv.Itoa(failure.Tested) + "）没测完，已换一台重测"
		}
	}
	inv.Log.Warn("speedtest.retrying", "tool", kind, "server", server, "next", next, "error", err.Error())
	result, err = runExternal(ctx, tool, kind, s.home(), next)
	return result, note, err
}

// deliver 把结果连同 Speedtest 的结果图一起发出去，数字作为图说明；
// 图发不出去就退回成改文字。
//
// 命令本身是回复某条消息时，结果图也回复那条消息，与 MiBox 一致。命令消息随后会删掉，
// 所以不回复命令自己；在话题里 ReplyToID 是话题的首条消息，图也就留在同一个话题里。
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
	if err := inv.Client.SendPhoto(ctx, peer, "speedtest.png", image, text, inv.Message.ReplyToID); err != nil {
		inv.Log.Info("speedtest.photo_failed", "error", err.Error())
		return inv.Edit(ctx, text)
	}
	return inv.Client.DeleteMessage(ctx, inv.Message)
}

// diagnose 列出能找到的每个 speedtest 程序、它能不能运行，以及测速时会用哪一个。
func (s *speedtester) diagnose(ctx context.Context, inv *command.Invocation) error {
	lines := []string{"🩺 <b>Speedtest 诊断</b>", ""}
	describe := func(label, path string) {
		version, err := probeVersion(path, s.home())
		switch {
		case err == nil:
			lines = append(lines, label+": "+command.Code(path)+"\n  "+command.Escape(version))
		case errors.Is(err, errNotOokla):
			lines = append(lines, label+": "+command.Code(path)+"\n  不是 Ookla 官方 CLI（可能是 Python 版 speedtest-cli），不会当官方 CLI 用")
		default:
			lines = append(lines, label+": "+command.Code(path)+"\n  无法运行："+command.Escape(command.Brief(err)))
		}
	}
	if path, err := exec.LookPath("speedtest"); err == nil {
		describe("系统 speedtest", path)
	} else {
		lines = append(lines, "系统 speedtest: 未安装")
	}
	local := filepath.Join(s.home(), "speedtest")
	if _, err := os.Stat(local); err == nil {
		describe("本地 CLI", local)
	} else {
		lines = append(lines, "本地 CLI: 未安装，首次测速或 "+command.Code(inv.Prefix+"speedtest fix")+" 时下载")
	}
	if path, err := exec.LookPath("speedtest-cli"); err == nil {
		lines = append(lines, "speedtest-cli: "+command.Code(path))
	}
	lines = append(lines, "")
	if tool, kind := externalTool(s.dataDir); tool != "" {
		lines = append(lines, "测速时使用 "+command.Escape(kind)+"："+command.Code(tool))
	} else {
		lines = append(lines, "测速时会先下载 Ookla 官方 CLI "+ooklaVersion)
	}
	lines = append(lines, "<i>本地 CLI 无法运行时用 "+command.Escape(inv.Prefix+"speedtest fix")+" 重新下载</i>")
	return inv.Edit(ctx, strings.Join(lines, "\n"))
}

// reinstall 重新下载本地 CLI，覆盖可能已经损坏的那份。下载和校验都通过之后才替换，
// 失败时原来的文件不动。版本由摘要固定，所以 update 和 fix 做的是同一件事。
func (s *speedtester) reinstall(ctx context.Context, inv *command.Invocation, done string) error {
	if err := inv.Edit(ctx, "🚀 <b>网络测速</b>\n\n正在重新下载 Ookla 官方 CLI "+ooklaVersion+"…"); err != nil {
		return err
	}
	path, err := installOokla(ctx, s.dataDir)
	if err != nil {
		inv.Log.Warn("speedtest.install_failed", "error", err.Error())
		return installProblem(ctx, inv, err)
	}
	version, err := probeVersion(path, s.home())
	if err != nil {
		return inv.EditText(ctx, "❌ 下载好的 CLI 无法运行："+command.Brief(err))
	}
	return inv.Edit(ctx, "✅ Ookla CLI "+done+"\n路径："+command.Code(path)+"\n"+command.Escape(version))
}

func (s *speedtester) handle(ctx context.Context, inv *command.Invocation) error {
	if handled, err := s.setting(ctx, inv); handled {
		return err
	}
	first := strings.ToLower(inv.Arg(0))
	if first == "diagnose" {
		return s.diagnose(ctx, inv)
	}
	if !s.running.TryLock() {
		return inv.EditText(ctx, "已有一个测速在进行，请稍候")
	}
	defer s.running.Unlock()

	switch first {
	case "list", "servers", "列表":
		return s.list(ctx, inv)
	case "fix":
		return s.reinstall(ctx, inv, "已重新下载")
	case "update":
		return s.reinstall(ctx, inv, "已更新到 "+ooklaVersion)
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
		if detail, ok := kit.IsUserError(err); ok {
			return inv.EditText(ctx, "❌ "+detail)
		}
		return inv.EditText(ctx, "❌ 测速失败，请稍后再试")
	}
	if once && note == "" {
		note = "本次指定了服务器，未改动默认设置"
	}
	if s.egress != nil && result.ExternalIP != "" {
		found := s.egress(ctx, result.ExternalIP)
		result.ASN, result.Country = found.ASN, found.Country
	}
	return deliver(ctx, inv, render(result, time.Since(started), note), result.Link)
}

// Register 注册 .speedtest 和它的简写 .st。
func Register(a *app.App) {
	tester := &speedtester{dataDir: a.DataDir(), egress: lookupEgress,
		settings: kit.NewStore(a, "speedtest.json", func() speedtestDocument { return speedtestDocument{} })}
	a.Registry.Register(
		&command.Command{Name: "speedtest", Description: "测量服务器网络速度", Usage: "[list|set ID|clear|ID|diagnose|fix|update]",
			Help: tester.help, Timeout: 5 * time.Minute, Handle: tester.handle},
		&command.Command{Name: "st", Description: "speedtest 的简写", Hidden: true,
			Help: tester.help, Timeout: 5 * time.Minute, Handle: tester.handle},
	)
}

// ConvertMiBox 把 MiBox 的测速设置换成本命令的 speedtest.json。
//
// MiBox v2 的设置在 assets/speedtest/v2-config.json，v1 的在 assets/speedtest/speedtest.json，
// 两者都用 default_server_id 记默认服务器（数字，没设时为 null 或不存在），这里只取这一项。
// 两版还有一项首选消息类型（photo/sticker/file/txt），本命令只发图片或文字，没有对应设置。
func ConvertMiBox(raw []byte) ([]byte, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, fmt.Errorf("MiBox 测速设置不是 JSON 对象：%w", err)
	}
	var document speedtestDocument
	if value, ok := source["default_server_id"]; ok {
		var number json.Number
		if json.Unmarshal(value, &number) == nil {
			if id, err := strconv.Atoi(number.String()); err == nil && id > 0 {
				document.Server = id
			}
		}
	}
	encoded, err := json.MarshalIndent(document, "", " ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}
