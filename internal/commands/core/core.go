// Package core 是基础命令：.ping .version .memory .status .sysinfo .help。
package core

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/sysinfo"
)

// Register 注册 ping、version、memory、status、sysinfo 和 help。
func Register(a *app.App) {
	registry := a.Registry
	registry.Register(
		&command.Command{Name: "ping", Description: "测试 Telegram 延迟", Handle: func(ctx context.Context, inv *command.Invocation) error {
			elapsed, err := inv.Client.Ping(ctx)
			if err != nil {
				return inv.EditText(ctx, "Telegram 延迟测试失败")
			}
			editing := time.Now()
			if err := inv.EditText(ctx, "Pong!"); err != nil {
				return err
			}
			return inv.EditText(ctx, fmt.Sprintf("Pong!\nTelegram API: %d ms\n消息编辑: %d ms", elapsed.Milliseconds(), time.Since(editing).Milliseconds()))
		}},
		&command.Command{Name: "version", Description: "查看版本信息", Handle: func(ctx context.Context, inv *command.Invocation) error {
			return inv.Edit(ctx, versionText(a))
		}},
		&command.Command{Name: "ver", Description: "version 的简写", Hidden: true, Handle: func(ctx context.Context, inv *command.Invocation) error {
			return inv.Edit(ctx, versionText(a))
		}},
		&command.Command{Name: "memory", Description: "查看内存状态", Handle: func(ctx context.Context, inv *command.Invocation) error {
			return inv.Edit(ctx, memoryReport())
		}},
		&command.Command{Name: "status", Description: "查看运行状态", Handle: func(ctx context.Context, inv *command.Invocation) error {
			lines := []string{
				"<b>MiBot Lite 状态</b>", "",
				"版本: " + command.Code(kit.Version(a)),
				"运行时间: " + command.Code(formatUptime(time.Since(a.Started).Seconds())),
				"命令数: " + command.Code(fmt.Sprint(len(registry.Commands()))),
				"前缀: " + command.Code(strings.Join(registry.Prefixes(), " ")),
				"PID: " + command.Code(fmt.Sprint(os.Getpid())),
				"", memoryReport(),
			}
			return inv.Edit(ctx, strings.Join(lines, "\n"))
		}},
		&command.Command{Name: "sysinfo", Description: "查看详细系统信息", Handle: func(ctx context.Context, inv *command.Invocation) error {
			machine := sysinfo.ReadHost()
			process := sysinfo.Read()
			lines := []string{
				"<b>系统信息</b>", "",
				"主机: " + command.Code(machine.Hostname),
				"系统: " + command.Code(machine.Platform),
				"内核: " + command.Code(machine.KernelRelease),
				"运行时间: " + command.Code(formatUptime(machine.Uptime.Seconds())),
				"负载: " + command.Code(fmt.Sprintf("%.2f / %.2f / %.2f", machine.LoadAverage[0], machine.LoadAverage[1], machine.LoadAverage[2])),
				"CPU: " + command.Code(fmt.Sprintf("%d 核", machine.CPUs)),
				"系统内存: " + command.Code(fmt.Sprintf("%.2f / %.2f MB", sysinfo.Megabytes(machine.TotalMemory-machine.FreeMemory), sysinfo.Megabytes(machine.TotalMemory))),
				"",
				"<b>MiBot Lite 进程</b>",
				"Go: " + command.Code(runtime.Version()),
				"PID: " + command.Code(fmt.Sprint(os.Getpid())),
				"RSS: " + command.Code(fmt.Sprintf("%.2f MB", sysinfo.Megabytes(process.RSS))),
				"Heap: " + command.Code(fmt.Sprintf("%.2f / %.2f MB", sysinfo.Megabytes(process.HeapAlloc), sysinfo.Megabytes(process.HeapSys))),
				"Sys: " + command.Code(fmt.Sprintf("%.2f MB", sysinfo.Megabytes(process.Sys))),
			}
			return inv.Edit(ctx, strings.Join(lines, "\n"))
		}},
		&command.Command{Name: "help", Description: "查看命令列表或单条命令说明", Usage: "[命令]", Handle: func(ctx context.Context, inv *command.Invocation) error {
			if name := inv.Arg(0); name != "" {
				return inv.Edit(ctx, renderCommandHelp(registry, inv.Prefix, name))
			}
			return inv.Edit(ctx, renderHelpList(registry, inv.Prefix))
		}},
		&command.Command{Name: "h", Description: "help 的简写", Hidden: true, Handle: func(ctx context.Context, inv *command.Invocation) error {
			if name := inv.Arg(0); name != "" {
				return inv.Edit(ctx, renderCommandHelp(registry, inv.Prefix, name))
			}
			return inv.Edit(ctx, renderHelpList(registry, inv.Prefix))
		}},
	)
}

func versionText(a *app.App) string {
	return strings.Join([]string{
		"<b>MiBot Lite 版本</b>", "",
		"MiBot Lite: " + command.Code(kit.Version(a)),
		"Go: " + command.Code(runtime.Version()),
		"平台: " + command.Code(runtime.GOOS+" "+runtime.GOARCH),
		"PID: " + command.Code(fmt.Sprint(os.Getpid())),
	}, "\n")
}

func memoryReport() string {
	current := sysinfo.Read()
	lines := []string{"<b>内存状态</b>", ""}
	if current.RSS > 0 {
		lines = append(lines, "RSS（进程总占用）: "+command.Code(fmt.Sprintf("%.2f MB", sysinfo.Megabytes(current.RSS))))
	} else {
		lines = append(lines, "RSS（进程总占用）: "+command.Code("不可用"))
	}
	lines = append(lines,
		"Heap（Go 已用 / 预留）: "+command.Code(fmt.Sprintf("%.2f / %.2f MB", sysinfo.Megabytes(current.HeapAlloc), sysinfo.Megabytes(current.HeapSys))),
		"Sys（运行时预留）: "+command.Code(fmt.Sprintf("%.2f MB", sysinfo.Megabytes(current.Sys))),
		"Goroutine: "+command.Code(fmt.Sprint(current.Goroutines)),
	)
	return strings.Join(lines, "\n")
}

func formatUptime(seconds float64) string {
	total := int64(seconds)
	return fmt.Sprintf("%d天 %d小时 %d分钟", total/86400, (total/3600)%24, (total/60)%60)
}

func renderHelpList(registry *command.Registry, prefix string) string {
	lines := []string{"<b>命令列表</b>", ""}
	for _, cmd := range registry.Commands() {
		usage := prefix + cmd.Name
		if cmd.Usage != "" {
			usage += " " + cmd.Usage
		}
		lines = append(lines, command.Code(usage)+" — "+command.Escape(cmd.Description))
	}
	quoted := make([]string, 0, len(registry.Prefixes()))
	for _, value := range registry.Prefixes() {
		quoted = append(quoted, command.Code(value))
	}
	lines = append(lines, "", "前缀: "+strings.Join(quoted, " "), "用 "+command.Code(prefix+"help 命令")+" 查看单条说明。")
	return strings.Join(lines, "\n")
}

func renderCommandHelp(registry *command.Registry, prefix, name string) string {
	cmd, ok := registry.Lookup(name)
	if !ok {
		return "未知命令: " + command.Code(name)
	}
	if cmd.Help != nil {
		return cmd.Help(prefix)
	}
	usage := prefix + cmd.Name
	if cmd.Usage != "" {
		usage += " " + cmd.Usage
	}
	return "<b>" + command.Escape(usage) + "</b>\n\n" + command.Escape(cmd.Description)
}
