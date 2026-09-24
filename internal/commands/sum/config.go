package sum

import (
	"context"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/ai"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// sumTypes 是服务商接口类型的可选值；auto 表示按模型名自动判断。
var sumTypes = []string{"auto", "chat", "responses", "gemini", "anthropic"}

func sumProviderView(provider sumProvider) string {
	kind := provider.Type
	if kind == "" {
		kind = "auto"
	}
	return command.Escape(provider.Name) + " · " + command.Code(provider.Model) + " · " + command.Escape(kind)
}

// config 处理 .sum config：list、add、del、set。改动成功统一回一句「已更新」；
// 出错时返回 fail，由调用方原样显示。
func (s *sumService) config(ctx context.Context, inv *command.Invocation) error {
	action, name, property := strings.ToLower(inv.Arg(1)), inv.Arg(2), inv.Arg(3)
	if action == "" {
		action = "list"
	}
	rest := []string{}
	if len(inv.Args) > 4 {
		rest = inv.Args[4:]
	}
	db, err := s.read()
	if err != nil {
		return err
	}
	replied := false
	switch action {
	case "list", "ls":
		return s.configList(ctx, inv, db)
	case "add":
		err = s.configAdd(inv, name, property, rest)
	case "del", "rm":
		replied, err = s.configDelete(ctx, inv, db, name)
	case "set":
		replied, err = s.configSet(ctx, inv, db, name, property, rest)
	default:
		return kit.Fail("未知 config 子命令")
	}
	if err != nil || replied {
		return err
	}
	return inv.EditText(ctx, "✅ 摘要配置已更新")
}

// configList 列出所有服务商、默认用哪个，以及提示词、推送目标和输出设置。
// 没有自己的服务商（或数据已迁到 ai 统一配置）时，显示 ai 当前的聊天选择。
func (s *sumService) configList(ctx context.Context, inv *command.Invocation, db sumDB) error {
	names := make([]string, 0, len(db.AIConfig.Providers))
	for key := range db.AIConfig.Providers {
		names = append(names, key)
	}
	sort.Strings(names)
	var rows []string
	for _, key := range names {
		marker := ""
		if db.AIConfig.DefaultProvider == key {
			marker = "（默认）"
		}
		rows = append(rows, "• "+command.Code(key)+marker+" · "+sumProviderView(db.AIConfig.Providers[key]))
	}
	body := strings.Join(rows, "\n")
	if body == "" {
		body = "• 尚未配置 AI"
	}
	if ai.Available() && (len(names) == 0 || db.AIConfig.AIMigrated) {
		tag, model := ai.ChatSelection()
		body += "\n• ai 聊天模型：" + command.Code(kit.OrDash(tag)+" / "+kit.OrDash(model))
	}
	promptState := "自定义"
	if db.AIConfig.DefaultPrompt == "" || db.AIConfig.DefaultPrompt == sumDefaultPrompt {
		promptState = "内置"
	}
	limit := "不限"
	if db.AIConfig.MaxOutputLength > 0 {
		limit = strconv.Itoa(db.AIConfig.MaxOutputLength) + " 字符"
	}
	settings := "\n默认推送：" + command.Code(kit.OrDefault(db.DefaultPushTarget, "me")) +
		"\n超时：" + strconv.Itoa(int(sumTimeout(db).Seconds())) + " 秒 · 输出上限：" + limit + " · 回复模式：" + kit.OnOffText(db.AIConfig.ReplyMode)
	return inv.Edit(ctx, "<b>摘要 AI 配置</b>\n"+body+"\n\n提示词："+promptState+"\n链接预览："+kit.OnOffText(db.AIConfig.LinkPreview)+settings)
}

// configAdd 添加一个服务商：名称 BaseURL API_KEY 模型 [类型]。第一个添加的自动成为默认。
// 命令里带着 API Key，只允许在收藏夹里发，免得密钥留在别的对话里。
func (s *sumService) configAdd(inv *command.Invocation, name, base string, rest []string) error {
	if !inv.Message.Saved {
		return kit.Fail("涉及 API Key 的配置命令只能在收藏夹使用")
	}
	key, model, kind := "", "", "auto"
	if len(rest) > 0 {
		key = rest[0]
	}
	if len(rest) > 1 {
		model = rest[1]
	}
	if len(rest) > 2 {
		kind = strings.ToLower(rest[2])
	}
	if name == "" || base == "" || key == "" || model == "" {
		return kit.Fail("用法：sum config add 名称 BaseURL API_KEY 模型 [type]")
	}
	if parsed, err := url.Parse(base); err != nil || parsed.Host == "" {
		return kit.Fail("BaseURL 无效")
	}
	if err := ai.AssertAllowedModel(model); err != nil {
		return err
	}
	if !slices.Contains(sumTypes, kind) {
		return kit.Fail("无效接口类型")
	}
	return s.update(func(db *sumDB) error {
		db.AIConfig.Providers[name] = sumProvider{Name: name, BaseURL: base, APIKey: key, Model: model, Type: kind}
		if db.AIConfig.DefaultProvider == "" {
			db.AIConfig.DefaultProvider = name
		}
		return nil
	})
}

// configDelete 删掉一个服务商；删的正好是默认的话，默认清空；用它的任务改回全局默认，
// 免得这些任务以后每次都因为找不到配置而失败。
func (s *sumService) configDelete(ctx context.Context, inv *command.Invocation, db sumDB, name string) (bool, error) {
	if _, ok := db.AIConfig.Providers[name]; name == "" || !ok {
		return false, kit.Fail("AI 配置不存在")
	}
	var reset []string
	if err := s.update(func(db *sumDB) error { reset = removeProvider(db, name); return nil }); err != nil {
		return false, err
	}
	if len(reset) == 0 {
		return false, nil
	}
	return true, inv.EditText(ctx, "✅ 摘要配置已更新，任务 "+strings.Join(reset, "、")+" 改用全局默认配置")
}

// removeProvider 删掉服务商，清空指向它的默认设置，并把用它的任务改回全局默认，
// 返回被改动的任务 ID。
func removeProvider(db *sumDB, name string) []string {
	delete(db.AIConfig.Providers, name)
	if db.AIConfig.DefaultProvider == name {
		db.AIConfig.DefaultProvider = ""
	}
	var reset []string
	for index := range db.Tasks {
		if db.Tasks[index].AIProvider == name {
			db.Tasks[index].AIProvider = ""
			reset = append(reset, db.Tasks[index].ID)
		}
	}
	return reset
}

// configSet 改一项设置。第一个返回值表示已经自己回复过了（只有 prompt show 这样），
// 调用方就不要再回「已更新」。
func (s *sumService) configSet(ctx context.Context, inv *command.Invocation, db sumDB, name, property string, rest []string) (bool, error) {
	switch name {
	case "default":
		if _, ok := db.AIConfig.Providers[property]; !ok {
			return false, kit.Fail("AI 配置不存在")
		}
		return false, s.update(func(db *sumDB) error { db.AIConfig.DefaultProvider = property; return nil })
	case "preview", "spoiler", "reply":
		enabled, err := kit.OnOff(property)
		if err != nil {
			return false, err
		}
		return false, s.update(func(db *sumDB) error {
			switch name {
			case "preview":
				db.AIConfig.LinkPreview = enabled
			case "spoiler":
				db.AIConfig.DefaultSpoiler = enabled
			default:
				db.AIConfig.ReplyMode = enabled
			}
			return nil
		})
	case "push":
		target := strings.TrimSpace(strings.Join(append([]string{property}, rest...), " "))
		if target == "" {
			return false, kit.Fail("推送目标不能为空")
		}
		return false, s.update(func(db *sumDB) error { db.DefaultPushTarget = target; return nil })
	case "timeout":
		seconds, err := strconv.Atoi(property)
		if err != nil || seconds < 10 || seconds > 600 {
			return false, kit.Fail("超时时间须为 10-600 秒")
		}
		return false, s.update(func(db *sumDB) error { db.AIConfig.DefaultTimeout = seconds * 1000; return nil })
	case "maxoutput":
		length, err := strconv.Atoi(property)
		if err != nil || length < 0 {
			return false, kit.Fail("请输入有效的字符数（0 表示不限制）")
		}
		return false, s.update(func(db *sumDB) error { db.AIConfig.MaxOutputLength = length; return nil })
	case "reasoning", "service":
		values := ai.ReasoningValues
		if name == "service" {
			values = ai.TierValues
		}
		if !slices.Contains(values, property) {
			return false, kit.Fail("无效选项")
		}
		return false, s.update(func(db *sumDB) error {
			if name == "reasoning" {
				db.AIConfig.DefaultReasoningEffort = property
			} else {
				db.AIConfig.DefaultServiceTier = property
			}
			return nil
		})
	case "prompt":
		return s.configPrompt(ctx, inv, db, property, rest)
	}
	return false, s.configProvider(inv, db, name, property, strings.TrimSpace(strings.Join(rest, " ")))
}

// configPrompt 查看、修改或还原摘要提示词。
func (s *sumService) configPrompt(ctx context.Context, inv *command.Invocation, db sumDB, property string, rest []string) (bool, error) {
	if property == "show" {
		prompt := db.AIConfig.DefaultPrompt
		if prompt == "" {
			prompt = sumDefaultPrompt
		}
		return true, inv.Edit(ctx, "<b>当前摘要提示词</b>\n\n"+command.Code(prompt))
	}
	prompt := strings.TrimSpace(strings.Join(append([]string{property}, rest...), " "))
	if property == "reset" {
		prompt = sumDefaultPrompt
	}
	if prompt == "" {
		return false, kit.Fail("提示词不能为空")
	}
	return false, s.update(func(db *sumDB) error { db.AIConfig.DefaultPrompt = prompt; return nil })
}

// configProvider 改某个服务商的一个字段：model、url、key、type。改 key 同样只允许在收藏夹里。
func (s *sumService) configProvider(inv *command.Invocation, db sumDB, name, property, value string) error {
	provider, ok := db.AIConfig.Providers[name]
	if !ok || !slices.Contains([]string{"model", "url", "key", "type"}, property) || value == "" {
		return kit.Fail("用法：sum config set 名称 model|url|key|type 值")
	}
	if property == "key" && !inv.Message.Saved {
		return kit.Fail("涉及 API Key 的配置命令只能在收藏夹使用")
	}
	switch property {
	case "model":
		if err := ai.AssertAllowedModel(value); err != nil {
			return err
		}
		provider.Model = value
	case "url":
		if parsed, err := url.Parse(value); err != nil || parsed.Host == "" {
			return kit.Fail("BaseURL 无效")
		}
		provider.BaseURL = value
	case "key":
		provider.APIKey = value
	default:
		if !slices.Contains(sumTypes, value) {
			return kit.Fail("无效接口类型")
		}
		provider.Type = value
	}
	return s.update(func(db *sumDB) error { db.AIConfig.Providers[name] = provider; return nil })
}
