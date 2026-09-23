package speedtest

import (
	"context"
	"strconv"
	"strings"
	"testing"
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

// CLI 给出的失败原因是一条 JSON 日志记录；聊天里需要的是其中那句话，
// 而不是外面那层包装。
func TestLastLineUnwrapsTheCLIRecord(t *testing.T) {
	raw := `{"type":"log","timestamp":"2026-09-22T10:53:22Z","message":"Could not retrieve or read configuration","level":"error"}`
	if got := lastLine(raw); got != "Could not retrieve or read configuration" {
		t.Errorf("lastLine = %q", got)
	}
	if got := lastLine("ookla failed: signal: aborted"); got != "ookla failed: signal: aborted" {
		t.Errorf("a plain line should survive unchanged, got %q", got)
	}
	if got := lastLine("  \n\n plain \n\n"); got != "plain" {
		t.Errorf("lastLine = %q", got)
	}
}

// CLI 的措辞准确，但对用户没什么用。这两条是在部署环境里真实遇到过的，
// 所以把它们的翻译固定下来。
func TestExplainCLI(t *testing.T) {
	if got := explainCLI("Configuration - Could not retrieve or read configuration (ConfigurationError)"); !strings.Contains(got, "过一阵再试") {
		t.Errorf("throttling was not explained: %q", got)
	}
	if got := explainCLI("Error: [0] Cannot read from socket: "); !strings.Contains(got, "换一个 ID") {
		t.Errorf("an unreachable server was not explained: %q", got)
	}
	if got := explainCLI("signal: aborted"); got != "signal: aborted" {
		t.Errorf("an unknown reason should pass through, got %q", got)
	}
}

// 每个子命令都必须出现在帮助里，否则跟不存在没什么两样：.speedtest list
// 当初就是这样漏掉的。
func TestSpeedtestHelpDocumentsEverySubcommand(t *testing.T) {
	text := speedtestHelp(t.TempDir(), ".", 0)
	for _, wanted := range []string{".speedtest list", ".speedtest set", ".speedtest clear", ".speedtest config", ".st"} {
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
