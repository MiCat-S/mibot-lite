// Package checkapi 实现 .checkapi：检测 .ai 统一配置里的 API，看模型列表、发测试提问、
// 逐个模型测速、对比两个服务商。地址、Key、类型和模型都由 .ai 管理，这里只按标签检测，
// 结果里不出现地址和 Key。
package checkapi

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/ai"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// checker 是 .checkapi 用到的 ai 能力。*ai.Service 实现它；测试换成假的，不用真去请求接口。
type checker interface {
	Providers() ([]ai.ProviderSummary, error)
	Models(ctx context.Context, tag string) ([]string, error)
	Chat(ctx context.Context, request ai.ChatRequest) (string, error)
	Benchmark(ctx context.Context, tag string) (ai.ProviderIdentity, []ai.Probe, error)
	Diagnose(ctx context.Context, tag string) (*ai.Diagnosis, error)
	ImportProvider(value ai.ProviderImport) (string, bool, error)
}

type service struct {
	models checker
	store  *store.Store[state]
}

func help(prefix string) string {
	p := command.Escape(prefix)
	item := func(args, text string) string {
		return "• <code>" + p + "checkapi " + command.Escape(args) + "</code> " + text + "\n"
	}
	return "🩺 <b>API 检测</b>\n\n检测 ai 命令统一管理的 API。地址、Key、类型和模型都在 ai 里配置，这里只按标签检测。\n\n" +
		item("list", "查看 ai 配置里的标签") +
		item("check [标签]", "验证一个或全部标签的模型接口") +
		item("models 标签", "列出这个标签的全部模型") +
		item("ask 标签 [问题]", "发一次测试提问，省略问题时发 say hello，最多输出 100 token") +
		item("speed 标签", "逐个模型测响应速度，每次最多 50 token") +
		item("compare 标签1 标签2", "对比两个标签的余额、对话和模型") +
		"\n添加或删除 API 用 <code>" + p + "ai config add</code>、<code>" + p + "ai config del</code>，选聊天模型用 <code>" + p + "ai model chat</code>。"
}

// Register 注册 .checkapi。models 是 ai.Register 返回的统一配置；为 nil（.ai 没注册）时
// .checkapi 只显示帮助，其余子命令报错说 ai 没有启用。
func Register(a *app.App, models *ai.Service) {
	s := &service{store: kit.NewStore(a, "checkapi.json", defaults)}
	if models != nil {
		s.models = models
	}
	a.Registry.Register(&command.Command{Name: "checkapi", Group: command.GroupAI, Description: "检测 AI 接口与模型",
		Usage: "list|check|models|ask|speed|compare [标签]", Help: help,
		// 测速逐个模型等回答，列模型和每次测试各自最多等 90 秒（ai 包里的 checkTimeout），
		// 最慢是列模型加三个模型约 6 分钟；ask 按 ai 的超时设置，最长 600 秒。默认的 5 分钟不够。
		Timeout: 15 * time.Minute,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			return kit.FailWith("API 检测失败", s.handle(ctx, inv))
		}})
}

func (s *service) handle(ctx context.Context, inv *command.Invocation) error {
	sub := strings.ToLower(inv.Arg(0))
	if sub == "" || sub == "help" || sub == "h" {
		return inv.EditPages(ctx, command.HTMLPages(help(inv.Prefix), command.PageLimit))
	}
	if s.models == nil {
		return kit.Fail("ai 命令没有启用")
	}
	// MiBox checkapi 旧版自己存过 Key。每次先把还没并进 ai 的并过去，和 MiBox 一样；
	// 并不进去不影响这次检测，只记日志。
	if count, err := s.migrate(inv.Log); err != nil {
		inv.Log.Warn("checkapi.migrate_failed", "error", err.Error())
	} else if count > 0 {
		inv.Log.Info("checkapi.migrated", "count", count)
	}
	tag := inv.Arg(1)
	switch sub {
	case "save", "del", "delete":
		p := command.Escape(inv.Prefix)
		return inv.Edit(ctx, "API 由 ai 命令统一管理：<code>"+p+"ai config add</code> 添加、<code>"+p+"ai config del</code> 删除、<code>"+p+"ai model chat</code> 选聊天模型")
	case "list":
		providers, err := s.models.Providers()
		if err != nil {
			return err
		}
		return inv.EditPages(ctx, command.HTMLPages(renderList(providers, inv.Prefix), command.PageLimit))
	case "check":
		return s.check(ctx, inv, tag)
	case "models":
		if tag == "" {
			return kit.Usage(inv.Prefix, "checkapi models 标签")
		}
		if err := inv.EditText(ctx, kit.Working("正在获取模型列表")); err != nil {
			return err
		}
		list, err := s.models.Models(ctx, tag)
		if err != nil {
			return err
		}
		return inv.EditPages(ctx, command.HTMLPages(renderModels(tag, list), command.PageLimit))
	case "ask":
		return s.ask(ctx, inv, tag)
	case "speed":
		if tag == "" {
			return kit.Usage(inv.Prefix, "checkapi speed 标签")
		}
		if err := inv.EditText(ctx, kit.Working("正在测速")); err != nil {
			return err
		}
		identity, probes, err := s.models.Benchmark(ctx, tag)
		if err != nil {
			return err
		}
		return inv.EditPages(ctx, command.HTMLPages(renderSpeed(identity, probes), command.PageLimit))
	case "compare":
		return s.compare(ctx, inv, tag, inv.Arg(2))
	}
	return kit.Failf("没有 %s 这个子命令，%scheckapi help 查看用法", inv.Arg(0), inv.Prefix)
}

// check 验证模型接口：给了标签只查这一个，没给就逐个查全部。
func (s *service) check(ctx context.Context, inv *command.Invocation, tag string) error {
	if err := inv.EditText(ctx, kit.Working("正在检测")); err != nil {
		return err
	}
	if tag != "" {
		list, err := s.models.Models(ctx, tag)
		if err != nil {
			return err
		}
		return inv.Edit(ctx, kit.Feedback("success", fmt.Sprintf("已连通 %s，共 %d 个模型", tag, len(list)), ""))
	}
	providers, err := s.models.Providers()
	if err != nil {
		return err
	}
	rows := make([]checkRow, 0, len(providers))
	for _, provider := range providers {
		list, err := s.models.Models(ctx, provider.Tag)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		row := checkRow{tag: provider.Tag, count: len(list)}
		if err != nil {
			row.failure = kit.OrDefault(firstMessage(err), "检测失败")
		}
		rows = append(rows, row)
	}
	return inv.EditPages(ctx, command.HTMLPages(renderCheck(rows, inv.Prefix), command.PageLimit))
}

// firstMessage 取出给用户看的那句话；不是给用户看的错误（不该出现）只说检测失败。
func firstMessage(err error) string {
	if text, ok := kit.IsUserError(err); ok {
		return command.Truncate(text, 80)
	}
	return command.Brief(err)
}

// ask 用这个标签的聊天模型发一次测试提问，输出最多 100 token。
func (s *service) ask(ctx context.Context, inv *command.Invocation, tag string) error {
	if tag == "" {
		return kit.Usage(inv.Prefix, "checkapi ask 标签 [问题]")
	}
	question := strings.TrimSpace(inv.RawAfter(2))
	if question == "" {
		question = "say hello"
	}
	if err := inv.EditText(ctx, kit.Working("正在提问")); err != nil {
		return err
	}
	answer, err := s.models.Chat(ctx, ai.ChatRequest{Tag: tag, Text: question, MaxOutputTokens: 100})
	if err != nil {
		return err
	}
	pages := command.EscapedPages(answer, command.PageLimit)
	pages[0] = "🩺 <b>测试提问</b>（" + command.Code(tag) + "）\n\n" + pages[0]
	return inv.EditPages(ctx, pages)
}

// compare 同时诊断两个标签，结果上下排在一起。
func (s *service) compare(ctx context.Context, inv *command.Invocation, first, second string) error {
	if first == "" || second == "" {
		return kit.Usage(inv.Prefix, "checkapi compare 标签1 标签2")
	}
	if err := inv.EditText(ctx, kit.Working("正在检测两个 API")); err != nil {
		return err
	}
	var left, right *ai.Diagnosis
	var leftErr, rightErr error
	var wait sync.WaitGroup
	wait.Add(2)
	go func() { defer wait.Done(); left, leftErr = s.models.Diagnose(ctx, first) }()
	go func() { defer wait.Done(); right, rightErr = s.models.Diagnose(ctx, second) }()
	wait.Wait()
	if leftErr != nil {
		return leftErr
	}
	if rightErr != nil {
		return rightErr
	}
	return inv.EditPages(ctx, command.HTMLPages(renderCompare(left, right), command.PageLimit))
}

func renderList(providers []ai.ProviderSummary, prefix string) string {
	var rows []string
	for _, provider := range providers {
		row := "• " + command.Code(provider.Tag) + " · " + command.Escape(provider.Type)
		for _, mode := range []string{"chat", "search", "image", "video"} {
			if model := provider.Models[mode]; model != "" {
				row += " · " + command.Escape(mode+"="+model)
			}
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		rows = append(rows, "• 还没有配置，用 "+command.Code(prefix+"ai config add")+" 添加")
	}
	return "🩺 <b>AI 接口</b>\n\n" + strings.Join(rows, "\n")
}

// checkRow 是检测全部标签时的一行：failure 为空表示连通，count 是模型数。
type checkRow struct {
	tag, failure string
	count        int
}

func renderCheck(rows []checkRow, prefix string) string {
	if len(rows) == 0 {
		return "🩺 <b>API 检测</b>\n\n还没有配置 API，用 " + command.Code(prefix+"ai config add") + " 添加"
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.failure != "" {
			lines = append(lines, command.Code(row.tag)+" · ❌ "+command.Escape(row.failure))
			continue
		}
		lines = append(lines, command.Code(row.tag)+" · ✅ "+strconv.Itoa(row.count)+" 个模型")
	}
	return "🩺 <b>API 检测</b>\n\n" + strings.Join(lines, "\n")
}

func renderModels(tag string, models []string) string {
	lines := make([]string, 0, len(models))
	for _, model := range models {
		lines = append(lines, command.Code(model))
	}
	return "🩺 <b>模型列表</b>\n" + command.Code(tag) + " · " + strconv.Itoa(len(models)) + " 个\n\n" + strings.Join(lines, "\n")
}

// milliseconds 把耗时写成「812 ms」。
func milliseconds(elapsed time.Duration) string {
	return strconv.FormatInt(elapsed.Milliseconds(), 10) + " ms"
}

// tokensPerSecond 是这次回答的 token 总数除以耗时，没有用量或耗时为 0 时写「?」。
func tokensPerSecond(probe ai.Probe) string {
	if !probe.OK || probe.Usage == nil || probe.Elapsed <= 0 {
		return "?"
	}
	return strconv.FormatFloat(float64(probe.Usage.Total)/probe.Elapsed.Seconds(), 'f', 1, 64)
}

func renderSpeed(identity ai.ProviderIdentity, probes []ai.Probe) string {
	lines := []string{"🩺 <b>" + command.Escape(identity.DisplayName) + " 速度测试</b>", command.Code(identity.Tag) + " · 每次最多 50 token", ""}
	for _, probe := range probes {
		if probe.OK {
			lines = append(lines, "✅ "+command.Code(probe.Model)+"："+milliseconds(probe.Elapsed)+"（"+tokensPerSecond(probe)+" tok/s）")
			continue
		}
		lines = append(lines, "❌ "+command.Code(probe.Model)+"："+command.Escape(kit.OrDefault(probe.Error, "失败")))
	}
	return strings.Join(lines, "\n")
}

// diagnosisLines 是一个服务商的诊断：身份、余额、对话测试、模型列表，与 MiBox 的顺序一致。
func diagnosisLines(result *ai.Diagnosis) []string {
	lines := []string{
		"<b>" + command.Escape(result.Provider.DisplayName) + "</b>（" + command.Escape(result.Provider.Type) + "）· " + command.Code(result.Provider.Tag),
		"",
		"<b>账户余额</b>",
	}
	switch {
	case len(result.Balance.Fields) > 0:
		for _, field := range result.Balance.Fields {
			lines = append(lines, command.Escape(field.Label)+"："+command.Escape(field.Value))
		}
	case result.Balance.Status == "unsupported":
		lines = append(lines, "请到官网查看余额")
	case result.Balance.Status == "invalid":
		lines = append(lines, "⚠️ Key 无效或没有权限")
	default:
		lines = append(lines, "⚠️ 查询失败")
	}
	lines = append(lines, "", "<b>对话测试</b>")
	switch chat := result.Chat; {
	case chat == nil:
		lines = append(lines, "❌ 没有配置聊天模型")
	case chat.OK:
		lines = append(lines, "✅ 回复「"+command.Escape(chat.Text)+"」，"+milliseconds(chat.Elapsed)+" · "+command.Code(chat.Model))
		if chat.Usage != nil {
			lines = append(lines, fmt.Sprintf("Token：输入 %d · 输出 %d · 合计 %d", chat.Usage.Prompt, chat.Usage.Completion, chat.Usage.Total))
		}
		for _, limit := range chat.RateLimits {
			lines = append(lines, command.Escape(limit.Name)+"："+command.Escape(limit.Value))
		}
	default:
		lines = append(lines, "❌ "+command.Code(chat.Model)+"："+command.Escape(kit.OrDefault(chat.Error, "失败")))
	}
	lines = append(lines, "", "<b>可用模型</b>")
	if result.ModelsError != "" {
		lines = append(lines, "❌ 获取失败："+command.Escape(result.ModelsError))
		return lines
	}
	names := make([]string, 0, len(result.Models))
	for _, name := range result.Models {
		names = append(names, command.Code(name))
	}
	lines = append(lines, "共 "+strconv.Itoa(len(result.Models))+" 个")
	if len(names) > 0 {
		lines = append(lines, strings.Join(names, " · "))
	}
	return lines
}

func renderCompare(left, right *ai.Diagnosis) string {
	lines := append([]string{"🩺 <b>API 对比</b>", ""}, diagnosisLines(left)...)
	lines = append(lines, "", "━━━━━━━━━━━━━━━━", "")
	lines = append(lines, diagnosisLines(right)...)
	return strings.Join(lines, "\n")
}
