// Package service 实现 .service：查看一个 systemd 服务的状态、运行时间、内存和 CPU。
//
// 读的是 systemctl show 的键值输出，不是 systemctl status 给人看的那一屏：后者的格式随
// systemd 版本和语言变，还带着进程树和完整命令行（里面是服务器路径）。只要指定的几个属性，
// 聊天里就不会出现路径。
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/sysinfo"
)

// runTimeout 是一次 systemctl 的时限，和 MiBox 一样 8 秒。
const runTimeout = 8 * time.Second

// properties 是要读的属性。老版本 systemd 没有的（如 MemoryPeak）systemctl 不输出，不算错。
var properties = []string{
	"Id", "Description", "LoadState", "ActiveState", "SubState", "UnitFileState", "Result",
	"MainPID", "NRestarts", "ActiveEnterTimestampMonotonic", "TasksCurrent", "TasksMax",
	"MemoryCurrent", "MemoryPeak", "MemoryHigh", "MemoryMax", "CPUUsageNSec",
	"ExecMainCode", "ExecMainStatus",
}

// unitPattern 是 systemd 单元名允许的字符（\ 用于 systemd-escape 过的名字，如 \x2d）。
var unitPattern = regexp.MustCompile(`^[A-Za-z0-9:_.@\\-]+$`)

// unitSuffixes 是单元类型的后缀；名字不带其中之一时按服务算，补上 .service（systemctl 也是这么做的）。
var unitSuffixes = []string{".service", ".socket", ".timer", ".target", ".mount", ".automount", ".path",
	".swap", ".slice", ".scope", ".device"}

// unitName 校验并补全单元名。不能以 - 开头（会被当成选项；命令行里另外用 -- 隔开），
// 也不能以 . 开头，长度照 systemd 的上限 256。
func unitName(value string) (string, bool) {
	if value == "" || len(value) > 256 || strings.HasPrefix(value, "-") || strings.HasPrefix(value, ".") || !unitPattern.MatchString(value) {
		return "", false
	}
	for _, suffix := range unitSuffixes {
		if strings.HasSuffix(value, suffix) && len(value) > len(suffix) {
			return value, true
		}
	}
	if len(value)+len(".service") > 256 {
		return "", false
	}
	return value + ".service", true
}

// cgroupUnit 从 /proc/self/cgroup 的内容里找出本进程所在的系统服务，
// 比如 0::/system.slice/mibot-lite.service。cgroup v1 看 name=systemd 那一行。
// 用户服务（路径里有 user@1000.service）和终端里跑的（session-3.scope）都不算：
// 那些不能用系统级的 systemctl 查。
func cgroupUnit(content string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 || (parts[1] != "" && parts[1] != "name=systemd") {
			continue
		}
		path := parts[2]
		if strings.Contains(path, "/user@") {
			return "", false
		}
		last := path[strings.LastIndexByte(path, '/')+1:]
		if !strings.HasSuffix(last, ".service") {
			continue
		}
		if name, ok := unitName(last); ok {
			return name, true
		}
	}
	return "", false
}

// status 是 systemctl show 的输出，属性名 → 值。
type status map[string]string

func parseShow(output string) status {
	values := status{}
	for _, line := range strings.Split(output, "\n") {
		if key, value, ok := strings.Cut(strings.TrimRight(line, "\r"), "="); ok && key != "" {
			values[key] = value
		}
	}
	return values
}

// number 读一个数值属性。未设置（[not set]、infinity）和 systemd 表示「无」的 2^64-1 都当没有。
func (s status) number(key string) (uint64, bool) {
	value, err := strconv.ParseUint(strings.TrimSpace(s[key]), 10, 64)
	if err != nil || value == math.MaxUint64 {
		return 0, false
	}
	return value, true
}

var activeStates = map[string]string{
	"active": "活跃", "reloading": "重新加载中", "inactive": "未运行", "failed": "失败",
	"activating": "启动中", "deactivating": "停止中", "maintenance": "维护中", "refreshing": "刷新中",
}

var subStates = map[string]string{
	"running": "运行中", "exited": "已退出", "dead": "已停止", "failed": "失败", "start": "启动中",
	"start-pre": "启动中", "start-post": "启动中", "auto-restart": "等待自动重启", "stop": "停止中",
	"stop-sigterm": "停止中", "stop-sigkill": "停止中", "stop-post": "停止中", "reload": "重新加载中",
	"listening": "监听中", "waiting": "等待中", "elapsed": "已触发", "mounted": "已挂载", "plugged": "已接入",
	"active": "活跃",
}

var fileStates = map[string]string{
	"enabled": "开启", "enabled-runtime": "开启（仅本次开机）", "disabled": "关闭", "static": "静态（由别的单元拉起）",
	"masked": "已屏蔽", "indirect": "间接", "generated": "自动生成", "transient": "临时", "alias": "别名",
}

// translate 查表翻译，查不到的原样返回（放在 code 里的由调用方处理）。
func translate(table map[string]string, value string) string {
	if chinese, ok := table[value]; ok {
		return chinese
	}
	return value
}

// report 是整理好的状态卡片。booted 是主机开机至今的时长（读不到为 0），用来把
// ActiveEnterTimestampMonotonic（开机后的微秒数）换算成运行时间。
func report(name, origin string, s status, booted time.Duration) string {
	row := func(label, value string) string { return "• " + label + "：" + value }
	title := "⚙️ <b>服务状态</b>\n" + command.Code(kit.OrDefault(s["Id"], name))
	if origin != "" {
		title += "（" + origin + "）"
	}
	// 描述通常就是一句名字；挂载点之类的描述里是路径，不显示。
	if description := strings.TrimSpace(s["Description"]); description != "" && description != s["Id"] && !strings.Contains(description, "/") {
		title += "\n" + command.Escape(description)
	}
	active, sub := s["ActiveState"], s["SubState"]
	state := translate(activeStates, active)
	if sub != "" && translate(subStates, sub) != state {
		state += "（" + translate(subStates, sub) + "）"
	}
	rows := []string{row("状态", command.Escape(state))}
	if active == "failed" || (active == "inactive" && s["Result"] != "" && s["Result"] != "success") {
		detail := command.Code(kit.OrDefault(s["Result"], "未知"))
		if code, ok := s.number("ExecMainStatus"); ok && code != 0 {
			// ExecMainCode 是 waitid 的 si_code：1 正常退出，2、3 被信号杀掉（3 还留了 core）。
			// 被信号杀掉时 ExecMainStatus 是信号编号，不是退出码。
			switch s["ExecMainCode"] {
			case "2", "3":
				detail += "（信号 " + strconv.FormatUint(code, 10) + "）"
			default:
				detail += "（退出码 " + strconv.FormatUint(code, 10) + "）"
			}
		}
		rows = append(rows, row("上次结果", detail))
	}
	running := active == "active" || active == "reloading" || active == "deactivating"
	alive := time.Duration(-1)
	if entered, ok := s.number("ActiveEnterTimestampMonotonic"); ok && running && entered > 0 && booted > 0 {
		if value := booted - time.Duration(entered)*time.Microsecond; value >= 0 {
			alive = value
			rows = append(rows, row("运行时间", command.Code(sysinfo.FormatUptime(alive))))
		}
	}
	if pid, ok := s.number("MainPID"); ok && pid > 0 {
		rows = append(rows, row("主进程 PID", command.Code(strconv.FormatUint(pid, 10))))
	}
	if current, ok := s.number("TasksCurrent"); ok {
		value := strconv.FormatUint(current, 10)
		if limit, ok := s.number("TasksMax"); ok {
			value += " / " + strconv.FormatUint(limit, 10)
		}
		rows = append(rows, row("任务数", command.Code(value)))
	}
	if memory := memoryText(s); memory != "" {
		rows = append(rows, row("内存", memory))
	}
	if cpu, ok := s.number("CPUUsageNSec"); ok {
		value := command.Code(sysinfo.FormatUptime(time.Duration(cpu)))
		if alive > 0 {
			value += fmt.Sprintf("（平均 %.1f%%）", float64(cpu)/float64(alive)*100)
		}
		rows = append(rows, row("CPU 时间", value))
	}
	if restarts, ok := s.number("NRestarts"); ok {
		rows = append(rows, row("自动重启", command.Code(strconv.FormatUint(restarts, 10)+" 次")))
	}
	if file := s["UnitFileState"]; file != "" {
		rows = append(rows, row("开机启动", command.Escape(translate(fileStates, file))))
	}
	return title + "\n\n" + strings.Join(rows, "\n")
}

// memoryText 是「当前（峰值 · 上限）」这样的一格；没开内存统计时为空。
func memoryText(s status) string {
	current, ok := s.number("MemoryCurrent")
	if !ok {
		return ""
	}
	text := command.Code(kit.FormatBytes(int64(current)))
	var notes []string
	if peak, ok := s.number("MemoryPeak"); ok {
		notes = append(notes, "峰值 "+kit.FormatBytes(int64(peak)))
	}
	if high, ok := s.number("MemoryHigh"); ok {
		notes = append(notes, "限流线 "+kit.FormatBytes(int64(high)))
	}
	if limit, ok := s.number("MemoryMax"); ok {
		notes = append(notes, "上限 "+kit.FormatBytes(int64(limit)))
	}
	if len(notes) > 0 {
		text += "（" + strings.Join(notes, " · ") + "）"
	}
	return text
}

// systemctl 执行 systemctl，返回标准输出。测试里换成假的。
var systemctl = runSystemctl

func systemctlPath() (string, error) {
	if _, err := os.Stat("/usr/bin/systemctl"); err == nil {
		return "/usr/bin/systemctl", nil
	}
	return exec.LookPath("systemctl")
}

var (
	errNoSystemctl = errors.New("systemctl not found")
	errTimeout     = errors.New("systemctl timed out")
)

func runSystemctl(ctx context.Context, args []string) (string, error) {
	path, err := systemctlPath()
	if err != nil {
		return "", errNoSystemctl
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	tool := exec.CommandContext(ctx, path, args...)
	// 不继承进程的环境变量（里面可能有 MIBOT_* 的 Key）；systemctl 连 systemd 用不着它们。
	tool.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C", "SYSTEMD_PAGER=", "SYSTEMD_COLORS=0"}
	tool.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	tool.Stdout, tool.Stderr = &limited{buf: &stdout, limit: 64 << 10}, &limited{buf: &stderr, limit: 4 << 10}
	err = tool.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", errTimeout
	}
	if err != nil {
		return "", fmt.Errorf("systemctl: %w: %s", err, kit.TruncateRunes(strings.TrimSpace(stderr.String()), 300))
	}
	return stdout.String(), nil
}

// limited 只留前 limit 字节，多出来的丢掉但照样报告写成功，免得子进程因为管道写不进去而出错。
type limited struct {
	buf   *bytes.Buffer
	limit int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.limit - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// noSystemd 认出「系统不是用 systemd 启动的」（容器、WSL 之类）这一类失败，它们的原文在 stderr 里。
func noSystemd(err error) bool {
	text := err.Error()
	return strings.Contains(text, "not been booted with systemd") || strings.Contains(text, "Failed to connect to bus")
}

// showArgs 是 systemctl show 的参数。单元名放在 -- 后面，就算校验漏了也不会被当成选项。
func showArgs(name string) []string {
	return []string{"show", "--no-pager", "--property=" + strings.Join(properties, ","), "--", name}
}

// detect 找本进程所在的服务：先看 cgroup，找不到（不在 systemd 下、开发机上）再用
// .restart 用的 MIBOT_SERVICE，默认 mibot-lite.service。origin 说明名字是怎么来的。
func detect(a *app.App) (name, origin string) {
	if content, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		if name, ok := cgroupUnit(string(content)); ok {
			return name, "当前进程"
		}
	}
	name, ok := unitName(a.Env.Get("MIBOT_SERVICE", "mibot-lite.service"))
	if !ok {
		name = "mibot-lite.service"
	}
	return name, "默认"
}

// uptime 读主机开机至今的时长，测试里换成固定值。
var uptime = hostUptime

// hostUptime 是主机开机至今的时长，读不到为 0。/proc/uptime 按 CLOCK_BOOTTIME 计，
// systemd 的时间戳按 CLOCK_MONOTONIC 计，两者只差挂起的时间，服务器不挂起，可以直接相减。
func hostUptime() time.Duration {
	content, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(content))
	if len(fields) == 0 {
		return 0
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

func help(prefix string) string {
	p := command.Escape(prefix)
	return "⚙️ <b>服务状态</b>\n\n查看 systemd 服务的运行状态、运行时间、内存和 CPU。\n\n" +
		"• <code>" + p + "service</code> 查看本程序所在的服务\n" +
		"• <code>" + p + "service 服务名</code> 查看指定服务，如 " + command.Code(prefix+"service ssh") + "\n\n" +
		"不写后缀时按 .service 查，也可以写 .timer、.socket 等其他单元。需要主机上有 systemd。"
}

// Register 注册 .service。只读状态，不启停服务。
func Register(a *app.App) {
	a.Registry.Register(&command.Command{Name: "service", Group: command.GroupSystem, Description: "查看 systemd 服务状态",
		Usage: "[服务名]", Help: help, Timeout: time.Minute,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			if len(inv.Args) > 1 {
				return kit.Usage(inv.Prefix, "service [服务名]")
			}
			name, origin := "", ""
			if raw := inv.Arg(0); raw != "" {
				checked, ok := unitName(raw)
				if !ok {
					return kit.Fail("服务名只能有字母、数字和 - _ . @ :")
				}
				name = checked
			} else {
				name, origin = detect(a)
			}
			if err := inv.EditText(ctx, kit.Working("正在查询 "+name)); err != nil {
				return err
			}
			text, err := inspect(ctx, inv.Prefix, name, origin)
			if err != nil {
				return err
			}
			return inv.Edit(ctx, text)
		}})
}

// inspect 读一个单元的状态，返回状态卡片；失败时返回给用户看的错误。
// origin 不为空表示名字是自动检测的，找不到时提示怎么指定。
func inspect(ctx context.Context, prefix, name, origin string) (string, error) {
	output, err := systemctl(ctx, showArgs(name))
	switch {
	case errors.Is(err, errNoSystemctl):
		return "", kit.Fail("主机上没有 systemctl，查不了服务状态")
	case errors.Is(err, errTimeout):
		return "", kit.Fail("systemctl 没有应答，稍后再试")
	case err != nil && noSystemd(err):
		return "", kit.Fail("连不上 systemd，主机可能没有用 systemd 管理服务")
	case err != nil:
		return "", kit.FailWith("读取服务状态失败", err)
	}
	s := parseShow(output)
	switch s["LoadState"] {
	case "not-found":
		if origin != "" {
			return "", kit.Failf("没有叫 %s 的服务，用 %sservice 服务名 指定", name, prefix)
		}
		return "", kit.Failf("没有叫 %s 的服务", name)
	case "":
		return "", kit.Fail("systemctl 没有返回服务状态")
	}
	return report(name, origin, s, uptime()), nil
}
