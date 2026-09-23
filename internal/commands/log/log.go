// Package log 实现 .log：导出脱敏后的运行日志。
package log

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/logtail"
)

// 日志以文件而不是消息的形式发出。它本来就是要转给帮忙排查的人的，
// 文件可以阅读、搜索、附到问题报告里；聊天里的一大段文字只能来回翻。
const (
	logDefaultLines = 200
	logMimeType     = "text/plain"
)

// logFilter 把参数解析成行数和一个过滤条件。
func logFilter(inv *command.Invocation) (int, func(string) bool, string) {
	count := logDefaultLines
	var terms []string
	for index := 0; ; index++ {
		argument := inv.Arg(index)
		if argument == "" {
			break
		}
		if value, err := strconv.Atoi(argument); err == nil && value > 0 {
			count = value
			continue
		}
		terms = append(terms, argument)
	}
	if len(terms) == 0 {
		return count, nil, ""
	}
	switch strings.ToLower(terms[0]) {
	case "error", "err", "错误":
		return count, func(line string) bool { return logtail.AtLeast(line, slog.LevelError) }, "仅错误"
	case "warn", "warning", "警告":
		return count, func(line string) bool { return logtail.AtLeast(line, slog.LevelWarn) }, "警告及以上"
	}
	needle := strings.ToLower(strings.Join(terms, " "))
	return count, func(line string) bool {
		return strings.Contains(strings.ToLower(line), needle)
	}, "包含 " + needle
}

// logHeader 向最终读到这个文件的人说明它是什么，并写明删掉了哪些内容，
// 免得有人把缺的部分当成故障。
func logHeader(a *app.App, shown, held int, filter string) string {
	level := "INFO"
	if a.Level != nil {
		level = a.Level.Level().String()
	}
	lines := []string{
		"mibot-lite " + a.Version,
		"运行时长 " + shortDuration(time.Since(a.Started)),
		"命令 " + strconv.Itoa(len(a.Registry.Commands())) + " 个",
		"日志级别 " + level,
		fmt.Sprintf("导出 %d 行，进程内共保留 %d 行", shown, held),
	}
	if filter != "" {
		lines = append(lines, "过滤 "+filter)
	}
	lines = append(lines,
		"",
		"已脱敏：聊天与账号 ID 换成 #xxxx（同一个仍是同一个），消息内容、密钥、",
		"URL 路径与 IP 地址已移除。进程重启后这里就空了，那种情况要看服务器日志。",
		strings.Repeat("-", 60),
		"")
	return strings.Join(lines, "\n")
}

func shortDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%.1f 天", d.Hours()/24)
	case d >= time.Hour:
		return fmt.Sprintf("%.1f 小时", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0f 分钟", d.Minutes())
	}
	return fmt.Sprintf("%.0f 秒", d.Seconds())
}

func logHelp(prefix string) string {
	p := command.Escape(prefix)
	return "📄 <b>运行日志</b>\n\n把进程内保留的日志导出成文件发出来，方便转给别人看。\n\n• <code>" +
		p + "log</code> 最近 " + strconv.Itoa(logDefaultLines) + " 行\n• <code>" + p +
		"log 50</code> 指定行数\n• <code>" + p + "log error</code> 只要错误\n• <code>" + p +
		"log warn</code> 警告及以上\n• <code>" + p + "log speedtest</code> 含该词的行\n• <code>" +
		p + "log debug on</code> 临时调到 debug 级别，<code>off</code> 调回来\n\n<b>脱敏</b>\n" +
		"聊天与账号 ID 在写进内存时就换成了 #xxxx，同一个仍然是同一个，但看不出是谁；" +
		"消息内容、密钥、URL 路径和 IP 地址直接丢弃。文件可以放心转发。\n\n" +
		"日志只留在进程里，重启就没了。要看重启之前的，得上服务器翻 journalctl。"
}

// setLogLevel 在运行中调高或调低日志级别。
//
// 以前要调高级别，得改服务单元再重启账号，而一重启，正在追查的那个
// 现场也就没了。
func setLogLevel(ctx context.Context, a *app.App, inv *command.Invocation) error {
	if a.Level == nil {
		return inv.EditText(ctx, "这个构建不支持在线改日志级别")
	}
	switch strings.ToLower(inv.Arg(1)) {
	case "on", "开":
		a.Level.Set(slog.LevelDebug)
	case "off", "关":
		a.Level.Set(slog.LevelInfo)
	case "":
		return inv.Edit(ctx, "当前日志级别 "+command.Code(a.Level.Level().String())+
			"\n用 "+command.Code(inv.Prefix+"log debug on")+" 或 "+command.Code(inv.Prefix+"log debug off"))
	default:
		return inv.EditText(ctx, "只能是 on 或 off")
	}
	// debug 级别会记下服务器推送的每条更新和经过的每条消息，所以只适合
	// 临时开一会儿。
	note := ""
	if a.Level.Level() == slog.LevelDebug {
		note = "\n\n<i>debug 很吵，查完记得关掉。</i>"
	}
	return inv.Edit(ctx, "日志级别已设为 "+command.Code(a.Level.Level().String())+note)
}

// Register 注册 .log。
func Register(a *app.App) {
	handle := func(ctx context.Context, inv *command.Invocation) error {
		switch strings.ToLower(inv.Arg(0)) {
		case "help", "h":
			return inv.Edit(ctx, logHelp(inv.Prefix))
		case "debug", "level":
			return setLogLevel(ctx, a, inv)
		}
		if a.Logs == nil {
			return inv.EditText(ctx, "这个构建没有保留日志")
		}
		count, keep, filter := logFilter(inv)
		lines := a.Logs.Tail(count, keep)
		if len(lines) == 0 {
			return inv.EditText(ctx, "没有匹配的日志")
		}
		body := logHeader(a, len(lines), a.Logs.Held(), filter) + strings.Join(lines, "\n") + "\n"
		peer, err := inv.Client.InputPeer(inv.Message.Peer)
		if err != nil {
			return err
		}
		name := "mibot-lite-" + time.Now().Format("20060102-1504") + ".log"
		caption := "📄 运行日志 " + command.Code(a.Version) + " · " + strconv.Itoa(len(lines)) + " 行\n<i>已脱敏，可直接转发</i>"
		if err := inv.Client.SendDocument(ctx, peer, name, logMimeType, []byte(body), caption, 0); err != nil {
			return err
		}
		return inv.Client.DeleteMessage(ctx, inv.Message)
	}
	a.Registry.Register(&command.Command{
		Name: "log", Description: "导出运行日志（已脱敏）", Usage: "[行数|error|关键词|debug on]",
		Help: logHelp, Handle: handle,
	})
}
