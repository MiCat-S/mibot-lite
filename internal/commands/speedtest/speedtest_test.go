package speedtest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// 服务器的公网地址会和测速结果一起发进聊天，所以要打码，不能原样公开。
func TestMaskAddress(t *testing.T) {
	if got := maskAddress("43.153.150.179"); got != "43.153.x.x" {
		t.Fatalf("maskAddress = %q", got)
	}
	if got := maskAddress("2400:cb00:1234:5678::1"); !strings.HasSuffix(got, "…") || strings.Contains(got, "5678") {
		t.Fatalf("IPv6 was not masked: %q", got)
	}
	if got := maskAddress(""); got != "未知" {
		t.Fatalf("empty address = %q", got)
	}
	if got := maskAddress("nonsense"); got != "…" {
		t.Fatalf("unparseable address should not be echoed, got %q", got)
	}
}

func TestFormatSpeed(t *testing.T) {
	for bits, want := range map[float64]string{
		2.5e9: "2.50 Gbps", 118.4e6: "118.4 Mbps", 5400: "5.4 Kbps", 12: "12 bps",
	} {
		if got := formatSpeed(bits); got != want {
			t.Errorf("formatSpeed(%v) = %q, want %q", bits, got, want)
		}
	}
}

// 被篡改的压缩包绝不能落盘，更不能被执行。
func TestExtractOoklaRejectsJunk(t *testing.T) {
	if _, err := extractOokla([]byte("not a gzip stream"), t.TempDir()); err == nil {
		t.Fatal("garbage should not extract")
	}
}

// 结果图片只从 Speedtest 自己的结果页获取，而且拿回来的确实是 PNG 才用。
func TestResultImageRefusesOtherSources(t *testing.T) {
	for _, link := range []string{"", "http://evil.example/x.png", "https://example.com/result/c/1",
		"https://www.speedtest.net.evil.com/result/c/1"} {
		if image := resultImage(context.Background(), link); image != nil {
			t.Errorf("%q should not be fetched", link)
		}
	}
}

// 服务器列表是得知 ID 的唯一途径，所以它必须显示 ID、长度适合放进
// 聊天，还要标出当前固定的是哪一个。
func TestRenderServersMarksThePinnedOne(t *testing.T) {
	servers := make([]speedServer, 0, speedListLimit+5)
	for i := 0; i < speedListLimit+5; i++ {
		servers = append(servers, speedServer{ID: 1000 + i, Name: "Server", Location: "Tokyo", Country: "Japan"})
	}
	text := renderServers(servers, 1003, ".")
	if !strings.Contains(text, "1003") || !strings.Contains(text, "✅") {
		t.Errorf("the pinned server is not marked:\n%s", text)
	}
	if strings.Contains(text, strconv.Itoa(1000+speedListLimit)) {
		t.Errorf("the list ran past its limit:\n%s", text)
	}
	if !strings.Contains(text, "1000") {
		t.Errorf("the nearest server is missing:\n%s", text)
	}
	// 每个服务器占一行，否则列表又会变回一大片文字：表头、空行、
	// speedListLimit 个服务器、空行、页脚。
	if got := len(strings.Split(text, "\n")); got != speedListLimit+4 {
		t.Errorf("the list is %d lines, want %d", got, speedListLimit+4)
	}
	// 所有服务器共同的国家放在表头，只出现一次。
	if strings.Count(text, "Japan") != 1 {
		t.Errorf("the country is repeated per line:\n%s", text)
	}
	// 页脚必须是读者能直接复制使用的内容。
	if !strings.Contains(text, ".speedtest 1003") {
		t.Errorf("the footer has no usable example:\n%s", text)
	}
}

// 服务器分属不同国家时，列表要标明各自是哪个国家。
func TestRenderServersKeepsMixedCountries(t *testing.T) {
	text := renderServers([]speedServer{
		{ID: 1, Name: "A", Location: "Tokyo", Country: "Japan"},
		{ID: 2, Name: "B", Location: "Seoul", Country: "Korea"},
	}, 0, ".")
	if !strings.Contains(text, "Japan") || !strings.Contains(text, "Korea") {
		t.Errorf("a mixed list lost its countries:\n%s", text)
	}
}

// 失败原因在两路输出里的样子，都是真实抓到的：测到一半被对端断开时只有 stdout 上的
// error 字段，stderr 是空的；被限流时是 stderr 上带时间戳的几行纯文本；其余是 stderr
// 上的 JSON 日志记录。聊天里要的是其中那句话，不是外面的包装。
func TestFailureDetailReadsBothStreams(t *testing.T) {
	if got := failureDetail(`{"error":"Cannot write: "}`, ""); got != "Cannot write:" {
		t.Errorf("the reason on stdout was lost: %q", got)
	}
	limited := "[2026-09-26 09:23:48.288] [error] Limit reached:\n\nSpeedtest CLI. Too many requests received. " +
		"To maintain a fair and stable environment,\nplease review and adjust the frequency of your requests.\n"
	if got := failureDetail("", limited); !strings.HasPrefix(got, "Limit reached: Speedtest CLI. Too many requests") ||
		!strings.HasSuffix(got, "frequency of your requests.") {
		t.Errorf("the plain-text reason was not put back together: %q", got)
	}
	record := `{"type":"log","timestamp":"2026-09-22T10:53:22Z","message":"Could not retrieve or read configuration","level":"error"}`
	if got := failureDetail("", record); got != "Could not retrieve or read configuration" {
		t.Errorf("failureDetail = %q", got)
	}
	// 进度记录和 info 日志不是原因；同一句话出现两次只留一次。
	progress := `{"type":"testStart","server":{"id":1}}` + "\n" + `{"type":"ping","ping":{"latency":1}}` + "\n" + `{"error":"Cannot read: "}`
	info := `{"type":"log","message":"Server selected","level":"info"}` + "\n" + `{"type":"log","message":"Cannot read:","level":"error"}`
	if got := failureDetail(progress, info); got != "Cannot read:" {
		t.Errorf("failureDetail = %q", got)
	}
	if got := failureDetail("", "ookla failed: signal: aborted"); got != "ookla failed: signal: aborted" {
		t.Errorf("a plain line should survive unchanged, got %q", got)
	}
	if got := failureDetail("  \n", "\n\n"); got != "" {
		t.Errorf("no output should give no reason, got %q", got)
	}
}

// CLI 的措辞准确，但对用户没什么用。这几条都是在部署环境里真实遇到过的，
// 所以把它们的翻译和要不要重试固定下来。
func TestExplainCLI(t *testing.T) {
	for detail, want := range map[string]string{
		"Configuration - Could not retrieve or read configuration (ConfigurationError)": "过一阵再试",
		"Limit reached: Speedtest CLI. Too many requests received.":                     "限流",
		"Error: [0] Cannot read from socket: ":                                          "换一个 ID",
		"Cannot write:":                                                                 "断开了连接",
		"Cannot read:":                                                                  "断开了连接",
		"Configuration - No servers defined (NoServersException)":                       "找不到指定的测速服务器",
	} {
		if got := explainCLI(detail); !strings.Contains(got, want) {
			t.Errorf("explainCLI(%q) = %q, want it to mention %q", detail, got, want)
		}
	}
	if got := explainCLI("signal: aborted"); got != "signal: aborted" {
		t.Errorf("an unknown reason should pass through, got %q", got)
	}
	// 限流是整台机器的事，换服务器也一样；断开、连不上、原因不明才值得再测一次。
	for detail, want := range map[string]bool{
		"Limit reached:": false, "Configuration - Could not retrieve or read configuration (ConfigurationError)": false,
		"Cannot write:": true, "Latency test failed": true, "signal: aborted": true, "": true,
	} {
		failure := &cliFailure{Kind: "ookla", Exit: errors.New("exit status 2"), Detail: detail}
		if got := failure.retryable(); got != want {
			t.Errorf("retryable(%q) = %v, want %v", detail, got, want)
		}
	}
	if (&cliFailure{Exit: errors.New("signal: killed"), TimedOut: true}).retryable() {
		t.Error("a test that already ran out of time should not be run again")
	}
}

// flakyServer 冒充 Ookla CLI：自动挑选总是挑中 28910，而 28910 测到一半就断开；
// 指定 48463 能测完。HOME 下有 limited 时一律报限流。每次调用的参数记在 HOME/calls。
const flakyServer = `#!/bin/sh
echo "$*" >> "$HOME/calls"
if [ -f "$HOME/limited" ]; then
  printf '[2026-09-26 09:23:48.288] [error] Limit reached:\n\nSpeedtest CLI. Too many requests received.\n' >&2
  exit 173
fi
case " $* " in
  *" -L "*)
    echo '{"type":"serverList","servers":[{"id":28910,"name":"fdcservers.net","location":"Tokyo","country":"Japan"},{"id":48463,"name":"IPA CyberLab 400G","location":"Tokyo","country":"Japan"}]}'
    exit 0 ;;
  *" -s 48463 "*)
    echo '{"type":"testStart","server":{"id":48463,"name":"IPA CyberLab 400G","location":"Tokyo"}}'
    echo '{"type":"result","ping":{"latency":1.5},"download":{"bandwidth":25000000},"upload":{"bandwidth":3700000},"server":{"id":48463,"name":"IPA CyberLab 400G","location":"Tokyo"},"result":{"url":""}}'
    exit 0 ;;
esac
echo '{"type":"testStart","server":{"id":28910,"name":"fdcservers.net","location":"Tokyo"}}'
echo '{"type":"ping","ping":{"latency":1.6,"progress":1}}'
echo '{"error":"Cannot write: "}'
exit 2
`

func flakyTester(t *testing.T) (*speedtester, string, *command.Invocation) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("用 sh 脚本冒充 CLI")
	}
	tester := &speedtester{dataDir: t.TempDir()}
	if err := os.MkdirAll(tester.home(), 0o700); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(tester.home(), "speedtest")
	if err := os.WriteFile(tool, []byte(flakyServer), 0o700); err != nil {
		t.Fatal(err)
	}
	inv := &command.Invocation{Prefix: ".", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return tester, tool, inv
}

func calls(t *testing.T, tester *speedtester) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(tester.home(), "calls"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// 自动挑中的服务器测到一半断开时，再自动挑一次还是它，所以要换列表里的另一台，
// 并且在结果里说明换过。
func TestMeasureMovesOffAServerThatDropped(t *testing.T) {
	tester, tool, inv := flakyTester(t)
	result, note, err := tester.measure(context.Background(), inv, tool, "ookla", 0)
	if err != nil {
		t.Fatalf("measure failed: %v", err)
	}
	if result.ServerID != 48463 {
		t.Errorf("measured on %d, want the other server", result.ServerID)
	}
	if !strings.Contains(note, "fdcservers.net Tokyo") || !strings.Contains(note, "28910") {
		t.Errorf("the note does not say which server dropped: %q", note)
	}
	got := calls(t, tester)
	if len(got) != 3 || strings.Contains(got[0], "-s") || !strings.Contains(got[1], "-L") || !strings.Contains(got[2], "-s 48463") {
		t.Errorf("calls = %q", got)
	}
}

// 失败原因要能进聊天和日志：以前 stdout 上的原因被丢掉，日志里只剩 exit status 2。
func TestRunExternalKeepsTheReasonFromStdout(t *testing.T) {
	tester, tool, _ := flakyTester(t)
	_, err := runExternal(context.Background(), tool, "ookla", tester.home(), 0)
	if text, ok := kit.IsUserError(err); !ok || !strings.Contains(text, "断开了连接") {
		t.Errorf("the chat would not be told why: %q (%v)", text, err)
	}
	for _, want := range []string{"server 28910", "exit status 2", "Cannot write:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the log line %q is missing %q", err.Error(), want)
		}
	}
	if !strings.Contains(calls(t, tester)[0], "-f jsonl") {
		t.Error("without jsonl there is no testStart to say which server was used")
	}
}

// 被限流时再测只会被限得更久，所以只跑一次，并照实告诉用户。
func TestMeasureDoesNotRetryWhenThrottled(t *testing.T) {
	tester, tool, inv := flakyTester(t)
	os.WriteFile(filepath.Join(tester.home(), "limited"), nil, 0o600)
	_, _, err := tester.measure(context.Background(), inv, tool, "ookla", 0)
	if text, ok := kit.IsUserError(err); !ok || !strings.Contains(text, "限流") {
		t.Errorf("throttling was not explained: %q (%v)", text, err)
	}
	if got := calls(t, tester); len(got) != 1 {
		t.Errorf("a throttled test was retried: %q", got)
	}
}

// 每个子命令都必须出现在帮助里，否则跟不存在没什么两样：.speedtest list
// 当初就是这样漏掉的。
func TestSpeedtestHelpDocumentsEverySubcommand(t *testing.T) {
	text := speedtestHelp(t.TempDir(), ".", 0)
	for _, wanted := range []string{".speedtest list", ".speedtest set", ".speedtest clear", ".speedtest config", ".speedtest diagnose", ".speedtest fix", "update", ".st"} {
		if !strings.Contains(text, wanted) {
			t.Errorf("the help never mentions %q", wanted)
		}
	}
	if !strings.Contains(text, "自动挑选") {
		t.Error("the help does not say which server is in use")
	}
	if pinned := speedtestHelp(t.TempDir(), ".", 48463); !strings.Contains(pinned, "48463") {
		t.Error("the help does not show the pinned server")
	}
}
