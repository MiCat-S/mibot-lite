package commands

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// fakeSpeedtest 是一个冒充 Ookla CLI 的脚本，放在 CLI 本该在的位置。
//
// 它认得 -L（列服务器）和 -s（指定服务器）；服务器 111 永远连不上；
// HOME 下有 fail-once 就失败一次，有 fail-always 就一直失败——用来走到重试和退回的分支。
// 脚本只用 shell 内建的东西：测试把 PATH 清空了，rm 这类外部命令找不到。
const fakeSpeedtest = `#!/bin/sh
case " $* " in
  *" -L "*)
    echo '{"type":"serverList","servers":[{"id":48463,"host":"a","name":"IPA CyberLab 400G","location":"Tokyo","country":"Japan"},{"id":111,"host":"b","name":"Dead","location":"Osaka","country":"Japan"}]}'
    exit 0 ;;
  *" -s 111 "*)
    echo '{"type":"log","message":"Error: [0] Cannot read from socket: ","level":"error"}' >&2
    exit 2 ;;
esac
if [ -f "$HOME/fail-always" ]; then
  echo '{"type":"log","message":"Configuration - Could not retrieve or read configuration (ConfigurationError)","level":"error"}' >&2
  exit 2
fi
if [ -f "$HOME/fail-once" ] && [ ! -f "$HOME/failed" ]; then
  : > "$HOME/failed"
  echo '{"type":"log","message":"Latency test failed","level":"error"}' >&2
  exit 2
fi
echo '{"type":"result","ping":{"latency":1.5,"jitter":0.2},"download":{"bandwidth":25000000},"upload":{"bandwidth":3700000},"isp":"Tencent","interface":{"externalIp":"43.153.150.179"},"server":{"name":"IPA CyberLab 400G","location":"Tokyo","country":"Japan"},"result":{"url":""}}'
`

type speedtestScenario struct {
	name  string
	lines []string
	// flag 是放进 CLI 的 HOME 的标记文件，控制它失败一次还是一直失败。
	flag string
}

func speedtestScenarios() []speedtestScenario {
	return []speedtestScenario{
		{name: "帮助和设置", lines: []string{"help", "config", "set abc", "set 0", "set 48463", "config", "clear", "weird"}},
		{name: "列服务器", lines: []string{"list", "set 48463", "list"}},
		{name: "正常测速", lines: []string{""}},
		{name: "固定的服务器连不上就退回自动", lines: []string{"set 111", ""}},
		{name: "只这一次指定服务器", lines: []string{"48463", "config"}},
		{name: "偶尔失败，重试成功", lines: []string{""}, flag: "fail-once"},
		{name: "一直失败", lines: []string{""}, flag: "fail-always"},
	}
}

// 用时是现测的，数字换掉再比。
var elapsed = regexp.MustCompile(`用时 [0-9.]+ 秒`)

func runSpeedtest(t *testing.T, scenario speedtestScenario) ([]string, string) {
	t.Helper()
	// PATH 里不能有真的 speedtest，否则会绕过假的那个。
	t.Setenv("PATH", t.TempDir())
	dataDir := t.TempDir()
	home := filepath.Join(dataDir, "speedtest")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "speedtest"), []byte(fakeSpeedtest), 0o700); err != nil {
		t.Fatal(err)
	}
	if scenario.flag != "" {
		os.WriteFile(filepath.Join(home, scenario.flag), nil, 0o600)
	}
	fake := newFakeTelegram(t, dmeSelf)
	client := fakeClient(fake, otherUser, nil)
	path := filepath.Join(dataDir, "speedtest.json")
	settings := store.New(path, func() speedtestDocument { return speedtestDocument{} })
	tester := &speedtester{dataDir: dataDir, settings: settings}
	for index, line := range scenario.lines {
		message := &bot.Message{ID: dmeCommand + index, Peer: dmePrivate, ChatID: bot.PeerID(dmePrivate), Out: true}
		inv := fakeInvocation(client, message, strings.Fields(line)...)
		if err := tester.handle(context.Background(), inv); err != nil {
			fake.log("error %v", err)
		}
	}
	calls := make([]string, len(fake.calls))
	for index, call := range fake.calls {
		calls[index] = elapsed.ReplaceAllString(strings.ReplaceAll(call, dataDir, "<data>"), "用时 # 秒")
	}
	raw, _ := os.ReadFile(path)
	return calls, string(raw)
}

// TestSpeedtestSnapshot 把每个场景的回复和存下的设置与快照比较。
func TestSpeedtestSnapshot(t *testing.T) {
	var out strings.Builder
	for _, scenario := range speedtestScenarios() {
		calls, stored := runSpeedtest(t, scenario)
		out.WriteString(section(scenario.name, calls, stored))
	}
	golden(t, "speedtest", out.String())
}
