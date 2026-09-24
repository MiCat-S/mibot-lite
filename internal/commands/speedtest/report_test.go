package speedtest

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeTool 在一个新目录里放一个名为 name 的脚本，--version 时输出 version。
func fakeTool(t *testing.T, name, version string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then\n  echo '" + version + "'\n  exit 0\nfi\nexit 2\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Python 版 speedtest-cli 也装了一个叫 speedtest 的命令。PATH 上的 speedtest 不是
// Ookla 官方 CLI 时不能拿来用，要接着找本地下载的那份。
func TestExternalToolSkipsNonOoklaSpeedtest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 sh 脚本冒充 CLI")
	}
	dataDir := t.TempDir()
	t.Setenv("PATH", fakeTool(t, "speedtest", "speedtest-cli 2.1.3\nPython 3.11.2"))
	if tool, kind := externalTool(dataDir); tool != "" {
		t.Fatalf("the Python speedtest was taken for Ookla: %s (%s)", tool, kind)
	}

	local := filepath.Join(dataDir, "speedtest", "speedtest")
	if err := os.WriteFile(local, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if tool, kind := externalTool(dataDir); tool != local || kind != "ookla" {
		t.Fatalf("should fall back to the downloaded CLI, got %q (%s)", tool, kind)
	}

	official := fakeTool(t, "speedtest", "Speedtest by Ookla 1.2.0.84 (ea6b6773cf) Linux/x86_64-linux-musl")
	t.Setenv("PATH", official)
	if tool, kind := externalTool(dataDir); tool != filepath.Join(official, "speedtest") || kind != "ookla" {
		t.Fatalf("the official CLI on PATH should win, got %q (%s)", tool, kind)
	}
}

// 结果里要有服务器 ID（设默认服务器要用）、收发的字节数和测速时刻。
func TestParseOoklaResultKeepsIDBytesAndTime(t *testing.T) {
	output := `{"type":"log","message":"x"}
{"type":"result","timestamp":"2026-09-22T10:53:22Z","ping":{"latency":1.5,"jitter":0.2},` +
		`"download":{"bandwidth":25000000,"bytes":298520000},"upload":{"bandwidth":3700000,"bytes":41000000},` +
		`"isp":"Tencent","interface":{"externalIp":"43.153.150.179"},` +
		`"server":{"id":48463,"name":"IPA CyberLab 400G","location":"Tokyo","country":"Japan"},` +
		`"result":{"url":"https://www.speedtest.net/result/c/abc"}}`
	result, err := parseResult("ookla", []byte(output))
	if err != nil {
		t.Fatal(err)
	}
	if result.ServerID != 48463 || result.DownloadBytes != 298520000 || result.UploadBytes != 41000000 ||
		result.Timestamp != "2026-09-22T10:53:22Z" || result.Download != 200e6 {
		t.Fatalf("parsed = %+v", result)
	}

	python := `{"download":93000000,"upload":12000000,"ping":20.5,"timestamp":"2026-09-22T10:53:22.123456Z",` +
		`"bytes_sent":15000000,"bytes_received":117000000,"server":{"id":"12345","name":"Tokyo","sponsor":"Foo"},` +
		`"client":{"ip":"1.2.3.4","isp":"Bar"},"share":null}`
	parsed, err := parseResult("python", []byte(python))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ServerID != 12345 || parsed.DownloadBytes != 117000000 || parsed.UploadBytes != 15000000 {
		t.Fatalf("python parsed = %+v", parsed)
	}
}

func TestRenderShowsTheNewFields(t *testing.T) {
	text := render(&reading{
		Source: "Ookla Speedtest", Latency: 1500 * time.Microsecond, Download: 200e6, Upload: 29.6e6,
		DownloadBytes: 298520000, UploadBytes: 41000000, Server: "IPA CyberLab 400G Tokyo", ServerID: 48463,
		ISP: "Tencent", ASN: "AS132203", ExternalIP: "43.153.150.179", Country: "JP",
		Timestamp: "2026-09-22T10:53:22Z",
	}, 30*time.Second, "")
	for _, want := range []string{"ID <code>48463</code>", "Tencent AS132203", "43.153.x.x", "🇯🇵 JP",
		"（共 298.5 MB）", "（共 41.0 MB）", "2026-09-22 10:53:22 UTC"} {
		if !strings.Contains(text, want) {
			t.Errorf("report is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "150.179") {
		t.Errorf("the address leaked:\n%s", text)
	}
	bare := render(&reading{Source: "Ookla Speedtest", Download: 1e6, Upload: 1e6}, time.Second, "")
	if strings.Contains(bare, "共") || strings.Contains(bare, "时间") || strings.Contains(bare, "ID") {
		t.Errorf("fields the tool did not report should be left out:\n%s", bare)
	}
}

func TestCountryFlagAndVolume(t *testing.T) {
	if got := countryFlag("JP"); got != "🇯🇵" {
		t.Errorf("countryFlag = %q", got)
	}
	for _, bad := range []string{"", "jp", "JPN", "1A"} {
		if got := countryFlag(bad); got != "" {
			t.Errorf("countryFlag(%q) = %q", bad, got)
		}
	}
	for bytes, want := range map[float64]string{2.5e9: "2.50 GB", 298.52e6: "298.5 MB", 5400: "5.4 KB", 12: "12 B"} {
		if got := formatVolume(bytes); got != want {
			t.Errorf("formatVolume(%v) = %q, want %q", bytes, got, want)
		}
	}
	if got := formatTimestamp("not a time"); got != "not a time" {
		t.Errorf("an unparseable timestamp should pass through, got %q", got)
	}
}

// MiBox v1 的 speedtest.json 和 v2 的 v2-config.json 都用 default_server_id 记默认服务器。
func TestConvertMiBox(t *testing.T) {
	cases := map[string]string{
		`{"schemaVersion":1,"default_server_id":48463,"preferred_type":"photo","legacyImported":true}`: "{\n \"server\": 48463\n}\n",
		`{"default_server_id":12345,"preferred_type":"txt"}`:                                           "{\n \"server\": 12345\n}\n",
		`{"default_server_id":"777"}`:                                                                  "{\n \"server\": 777\n}\n",
		`{"schemaVersion":1,"default_server_id":null,"preferred_type":null}`:                           "{}\n",
		`{"default_server_id":-5}`:                                                                     "{}\n",
		`{"default_server_id":1.5}`:                                                                    "{}\n",
		`{}`:                                                                                           "{}\n",
	}
	for input, want := range cases {
		got, err := ConvertMiBox([]byte(input))
		if err != nil || string(got) != want {
			t.Errorf("ConvertMiBox(%s) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := ConvertMiBox([]byte("not json")); err == nil {
		t.Error("garbage should be rejected")
	}
}
