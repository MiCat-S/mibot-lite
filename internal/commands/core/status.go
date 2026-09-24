package core

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/statuscard"
	"github.com/MiCat-S/mibot-lite/internal/sysinfo"
)

// drawing 让状态卡片同一时间只画一张：画一张要几 MB 画布，别人借用 .status 连发时不叠加。
var drawing sync.Mutex

// gotdVersion 是编进程序的 gotd 版本，读不到时为空。
func gotdVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dependency := range info.Deps {
			if dependency.Path == "github.com/gotd/td" {
				return dependency.Version
			}
		}
	}
	return ""
}

func gaugeOf(capacity sysinfo.Capacity) statuscard.Gauge {
	percent, ok := capacity.Percent()
	return statuscard.Gauge{Percent: percent, Known: ok}
}

func formatBytes(value uint64) string {
	return fmt.Sprintf("%.1f MB", sysinfo.Megabytes(value))
}

// statusCaption 是卡片下面的文字说明。不写主机名和网卡：.status 可以借给别人用，
// 那些归 .sysinfo（只限本人）。
func statusCaption(a *app.App, registry *command.Registry, resources sysinfo.Resources) string {
	process := sysinfo.Read()
	host := sysinfo.ReadHost()
	load := "不可用"
	if host.LoadAverage != [3]float64{} {
		load = fmt.Sprintf("%.2f / %.2f / %.2f", host.LoadAverage[0], host.LoadAverage[1], host.LoadAverage[2])
	}
	hostUptime := "不可用"
	if host.Uptime > 0 {
		hostUptime = formatUptime(host.Uptime.Seconds())
	}
	row := func(label, value string) string { return "• " + label + ": " + command.Code(value) }
	return strings.Join([]string{
		"<b>🧠 进程</b>",
		row("版本", kit.Version(a)),
		row("运行时间", formatUptime(time.Since(a.Started).Seconds())),
		row("PID", fmt.Sprintf("%d · %d 个 goroutine", os.Getpid(), process.Goroutines)),
		row("CPU", fmt.Sprintf("%.1f%%", resources.ProcessCPU)),
		row("RSS · Go 堆", formatBytes(process.RSS)+" · "+formatBytes(process.HeapAlloc)),
		row("命令 · 前缀", fmt.Sprintf("%d 个 · %s", len(registry.Commands()), strings.Join(registry.Prefixes(), " "))),
		"",
		"<b>🖥 主机</b>",
		row("系统", resources.OS+" · "+host.Platform),
		row("内核", kit.OrDefault(host.KernelRelease, "不可用")),
		row("主机在线", hostUptime),
		row("负载 1 / 5 / 15 分钟", load),
		row("状态采样", fmt.Sprintf("%dms", resources.Sampled.Milliseconds())),
	}, "\n")
}

// status 发状态卡片（附文字说明）并删掉命令消息，同 MiBox v2；卡片发不出去时退回纯文字。
func status(ctx context.Context, a *app.App, registry *command.Registry, inv *command.Invocation) error {
	if !drawing.TryLock() {
		return inv.EditText(ctx, "上一张状态卡片还在生成，请稍候")
	}
	defer drawing.Unlock()
	resources := sysinfo.SampleResources(ctx, a.Root, 160*time.Millisecond)
	caption := statusCaption(a, registry, resources)
	footer := "MiBot Lite " + kit.Version(a) + "  ·  Go " + runtime.Version()
	if version := gotdVersion(); version != "" {
		footer += "  ·  gotd " + version
	}
	var cpu statuscard.Gauge
	if resources.HasCPU {
		cpu = statuscard.Gauge{Percent: resources.SystemCPU, Known: true}
	}
	card, err := statuscard.Render(statuscard.Card{Name: "MiBot Lite", Uptime: time.Since(a.Started), CPU: cpu,
		Memory: gaugeOf(resources.Memory), Disk: gaugeOf(resources.Disk), Swap: gaugeOf(resources.Swap), Footer: footer})
	// 画布有几 MB，发完马上还给系统，不让一次 .status 抬高常驻内存。
	defer debug.FreeOSMemory()
	if err == nil {
		peer, sendErr := inv.Client.InputPeer(inv.Message.Peer)
		if sendErr == nil {
			sendErr = inv.Client.SendPhoto(ctx, peer, "status.png", card, caption, inv.Message.ReplyToID)
		}
		if sendErr == nil {
			return inv.Client.DeleteMessage(ctx, inv.Message)
		}
		inv.Log.Warn("status.card_send_failed", "error", sendErr.Error())
	} else {
		inv.Log.Warn("status.card_render_failed", "error", err.Error())
	}
	return inv.Edit(ctx, "<b>MiBot Lite 状态</b>\n\n"+caption)
}
