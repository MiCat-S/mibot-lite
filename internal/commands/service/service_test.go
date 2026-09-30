package service

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

func TestUnitName(t *testing.T) {
	for in, want := range map[string]string{
		"ssh":                "ssh.service",
		"mibot-lite.service": "mibot-lite.service",
		"certbot.timer":      "certbot.timer",
		"getty@tty1.service": "getty@tty1.service",
		"docker.socket":      "docker.socket",
		"nginx.conf":         "nginx.conf.service",
		`systemd-fsck@dev-disk-by\x2duuid-1234.service`: `systemd-fsck@dev-disk-by\x2duuid-1234.service`,
		"dbus-org.freedesktop.login1":                   "dbus-org.freedesktop.login1.service",
	} {
		if got, ok := unitName(in); !ok || got != want {
			t.Errorf("unitName(%q) = %q, %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "-H", "--help", "--host=x", ".service", ".hidden", "a b", "a;b", "a/b", "../x", "$(id)", "`id`", "x*", "ssh\n", strings.Repeat("a", 257)} {
		if got, ok := unitName(bad); ok {
			t.Errorf("unitName(%q) = %q, want it rejected", bad, got)
		}
	}
}

// 单元名放在 -- 后面，而且是最后一个参数。
func TestShowArgs(t *testing.T) {
	args := showArgs("ssh.service")
	if len(args) < 3 || args[0] != "show" || args[len(args)-2] != "--" || args[len(args)-1] != "ssh.service" {
		t.Errorf("showArgs = %q", args)
	}
	for _, arg := range args {
		if strings.Contains(arg, "FragmentPath") || strings.Contains(arg, "ExecStart=") {
			t.Errorf("asked for a property that carries paths: %q", arg)
		}
	}
}

func TestCgroupUnit(t *testing.T) {
	for content, want := range map[string]string{
		"0::/system.slice/mibot-lite.service\n":                                                 "mibot-lite.service",
		"12:memory:/system.slice/x.service\n1:name=systemd:/system.slice/mibot.service\n0::/\n": "mibot.service",
		"0::/system.slice/system-getty.slice/getty@tty1.service":                                "getty@tty1.service",
	} {
		if got, ok := cgroupUnit(content); !ok || got != want {
			t.Errorf("cgroupUnit(%q) = %q, %v, want %q", content, got, ok, want)
		}
	}
	for _, content := range []string{
		"",
		"0::/",
		"0::/user.slice/user-1000.slice/session-3.scope",
		"0::/user.slice/user-1000.slice/user@1000.service/app.slice/mibot.service",
		"0::/docker/0123456789abcdef",
		"12:memory:/system.slice/x.service",
	} {
		if got, ok := cgroupUnit(content); ok {
			t.Errorf("cgroupUnit(%q) = %q, want nothing", content, got)
		}
	}
}

// running 是 systemctl show 对一个运行中服务的输出（属性顺序照 systemd 的，多了一行没要的也不碍事）。
const running = `Id=mibot-lite.service
Description=MiBot Lite
LoadState=loaded
ActiveState=active
SubState=running
UnitFileState=enabled
Result=success
MainPID=4242
NRestarts=2
ActiveEnterTimestampMonotonic=3600000000
TasksCurrent=12
TasksMax=4915
MemoryCurrent=31457280
MemoryPeak=47185920
MemoryHigh=infinity
MemoryMax=536870912
CPUUsageNSec=36000000000
ExecMainCode=0
ExecMainStatus=0
Unknown=value
`

func TestReportRunning(t *testing.T) {
	text := report("mibot-lite.service", "当前进程", parseShow(running), 2*time.Hour)
	for _, wanted := range []string{
		"⚙️ <b>服务状态</b>\n<code>mibot-lite.service</code>（当前进程）\nMiBot Lite",
		"• 状态：活跃（运行中）",
		"• 运行时间：<code>01:00:00</code>",
		"• 主进程 PID：<code>4242</code>",
		"• 任务数：<code>12 / 4915</code>",
		"• 内存：<code>30.0 MB</code>（峰值 45.0 MB · 上限 512.0 MB）",
		"• CPU 时间：<code>00:00:36</code>（平均 1.0%）",
		"• 自动重启：<code>2 次</code>",
		"• 开机启动：开启",
	} {
		if !strings.Contains(text, wanted) {
			t.Errorf("report lost %q:\n%s", wanted, text)
		}
	}
	if strings.Contains(text, "限流线") || strings.Contains(text, "上次结果") {
		t.Errorf("unexpected rows:\n%s", text)
	}
	if _, _, err := bot.ParseHTML(text); err != nil {
		t.Errorf("report does not parse: %v", err)
	}
}

// 失败、停止的服务：说明结果和退出码，不显示运行时间；没开统计的属性不显示。
func TestReportStoppedAndFailed(t *testing.T) {
	failed := parseShow("Id=x.service\nDescription=/srv/app/run.sh wrapper\nLoadState=loaded\nActiveState=failed\nSubState=failed\n" +
		"Result=exit-code\nMainPID=0\nActiveEnterTimestampMonotonic=100\nMemoryCurrent=[not set]\nCPUUsageNSec=18446744073709551615\n" +
		"TasksCurrent=18446744073709551615\nExecMainStatus=1\nUnitFileState=disabled\n")
	text := report("x.service", "", failed, time.Hour)
	for _, wanted := range []string{"• 状态：失败", "• 上次结果：<code>exit-code</code>（退出码 1）", "• 开机启动：关闭"} {
		if !strings.Contains(text, wanted) {
			t.Errorf("failed report lost %q:\n%s", wanted, text)
		}
	}
	for _, unwanted := range []string{"/srv", "运行时间", "PID", "内存", "CPU", "任务数"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("failed report shows %q:\n%s", unwanted, text)
		}
	}
	killed := report("k.service", "", parseShow("Id=k.service\nLoadState=loaded\nActiveState=failed\nSubState=failed\nResult=signal\nExecMainCode=2\nExecMainStatus=9\n"), time.Hour)
	if !strings.Contains(killed, "<code>signal</code>（信号 9）") {
		t.Errorf("killed report:\n%s", killed)
	}
	dead := report("y.service", "", parseShow("Id=y.service\nLoadState=loaded\nActiveState=inactive\nSubState=dead\nResult=success\nUnitFileState=static\n"), time.Hour)
	if !strings.Contains(dead, "• 状态：未运行（已停止）") || strings.Contains(dead, "上次结果") || !strings.Contains(dead, "静态") {
		t.Errorf("dead report:\n%s", dead)
	}
	// 读不到主机开机时长、或者时间对不上（比如容器里）时不显示运行时间，也不算平均 CPU。
	odd := report("z.service", "", parseShow(running), 30*time.Minute)
	if strings.Contains(odd, "运行时间") || strings.Contains(odd, "平均") {
		t.Errorf("an impossible uptime was shown:\n%s", odd)
	}
}

func TestInspect(t *testing.T) {
	defer func(old func(context.Context, []string) (string, error), oldUptime func() time.Duration) {
		systemctl, uptime = old, oldUptime
	}(systemctl, uptime)
	uptime = func() time.Duration { return 2 * time.Hour }
	var asked []string
	answer := func(output string, err error) {
		systemctl = func(_ context.Context, args []string) (string, error) {
			asked = args
			return output, err
		}
	}

	answer(running, nil)
	text, err := inspect(context.Background(), ".", "mibot-lite.service", "当前进程")
	if err != nil || !strings.Contains(text, "运行时间：<code>01:00:00</code>") || !reflect.DeepEqual(asked, showArgs("mibot-lite.service")) {
		t.Errorf("inspect = %v:\n%s\nasked %q", err, text, asked)
	}

	for _, c := range []struct {
		output string
		err    error
		origin string
		want   string
	}{
		{"Id=nope.service\nLoadState=not-found\nActiveState=inactive\n", nil, "", "没有叫 nope.service 的服务"},
		{"Id=nope.service\nLoadState=not-found\n", nil, "默认", ".service 服务名 指定"},
		{"", errNoSystemctl, "", "没有 systemctl"},
		{"", errTimeout, "", "没有应答"},
		{"", errors.New("systemctl: exit status 1: System has not been booted with systemd as init system (PID 1). Can't operate."), "", "连不上 systemd"},
		{"", errors.New("systemctl: exit status 1: Access denied at /run/x"), "", "读取服务状态失败"},
		{"garbage", nil, "", "没有返回服务状态"},
	} {
		answer(c.output, c.err)
		_, err := inspect(context.Background(), ".", "nope.service", c.origin)
		text, ok := kit.IsUserError(err)
		if !ok || !strings.Contains(text, c.want) || strings.Contains(text, "/run") {
			t.Errorf("%q / %v → %q, want it to mention %q", c.output, c.err, text, c.want)
		}
	}
}

func TestLimitedWriter(t *testing.T) {
	var sink strings.Builder
	w := &limited{buf: new(bytes.Buffer), limit: 5}
	for _, chunk := range []string{"abc", "defg", "hij"} {
		if n, err := w.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
		sink.WriteString(chunk)
	}
	if w.buf.String() != "abcde" {
		t.Errorf("kept %q of %q", w.buf.String(), sink.String())
	}
}
