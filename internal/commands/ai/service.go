package ai

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// ChatRequest 是其他命令借 ai 的统一配置生成文字的一次请求，对应 MiBox ai 插件的
// chat 服务：.sum 迁移到统一配置之后，就是这样调用模型的。
type ChatRequest struct {
	// Tag 是 ai 配置里的服务商标签，为空时用当前的聊天选择。
	Tag string
	// Text 是用户消息，SystemPrompt 是系统提示词（为空时用 ai 的提示词）。
	Text         string
	SystemPrompt string
	// MaxOutputTokens 大于 0 时限制输出长度。
	MaxOutputTokens int
	// FallbackToChat 为真时，Responses 接口返回 400 或 404 就改走 Chat Completions 再试一次。
	FallbackToChat bool
	// MinTimeout 大于 ai 的超时设置时，用它作为这次请求的超时。
	MinTimeout time.Duration
}

// HasProvider 判断 ai 配置里有没有这个服务商标签。
func HasProvider(tag string) bool {
	if shared == nil || tag == "" {
		return false
	}
	cfg, err := shared.read()
	if err != nil {
		return false
	}
	_, ok := cfg.Configs[tag]
	return ok
}

// ChatSelection 返回 ai 当前的聊天服务商标签和模型。
func ChatSelection() (tag, model string) {
	if shared == nil {
		return "", ""
	}
	cfg, err := shared.read()
	if err != nil {
		return "", ""
	}
	return cfg.CurrentChatTag, cfg.CurrentChatModel
}

// chatModelFor 按 MiBox 的规则找出某个标签的聊天模型：是当前聊天标签就用当前模型，
// 否则用这个服务商记下的 chat 模型。
func chatModelFor(cfg aiConfig, tag string) (string, error) {
	provider, ok := cfg.Configs[tag]
	if tag == "" || !ok {
		return "", kit.Fail("未找到指定的 AI 提供商")
	}
	model := provider.Models["chat"]
	if tag == cfg.CurrentChatTag && cfg.CurrentChatModel != "" {
		model = cfg.CurrentChatModel
	}
	if model == "" {
		return "", kit.Fail("请先配置 ai chat 模型")
	}
	return model, AssertAllowedModel(model)
}

// Chat 用 ai 的统一配置生成文字。调用前先用 Available 确认。
func Chat(ctx context.Context, request ChatRequest) (string, error) {
	if shared == nil {
		return "", kit.Fail("请先配置 ai 聊天模型")
	}
	cfg, err := shared.read()
	if err != nil {
		return "", err
	}
	tag := request.Tag
	if tag == "" {
		tag = cfg.CurrentChatTag
	}
	if tag == "" {
		return "", kit.Fail("请先用 ai config add 添加 API，并用 ai model chat 选择模型")
	}
	model, err := chatModelFor(cfg, tag)
	if err != nil {
		return "", err
	}
	if seconds := int(request.MinTimeout / time.Second); seconds > cfg.Timeout {
		cfg.Timeout = seconds
	}
	sel := aiSelection{Tag: tag, Model: model, Reasoning: cfg.CurrentChatReasoningEffort, Tier: cfg.CurrentChatServiceTier}
	system := request.SystemPrompt
	if system == "" {
		system = cfg.Prompt
	}
	options := chatOptions{maxOutputTokens: request.MaxOutputTokens}
	text, err := chatText(ctx, cfg, sel, request.Text, system, options)
	if err == nil || !request.FallbackToChat || !cfg.Configs[tag].Responses {
		return text, err
	}
	if status := statusOf(err); status != 400 && status != 404 {
		return "", err
	}
	provider := cfg.Configs[tag]
	provider.Responses = false
	configs := make(map[string]aiProvider, len(cfg.Configs))
	for key, value := range cfg.Configs {
		configs[key] = value
	}
	configs[tag] = provider
	cfg.Configs = configs
	return chatText(ctx, cfg, sel, request.Text, system, options)
}

// ConvertMiBox 把 MiBox 的 assets/ai/config.json 转成 data/ai.json。
//
// MiBox 读取配置时会补上文件里没写的两类值，这里把它们写进文件，Lite 读到的就和
// MiBox 用的一样：服务商没写 responses 时，openai、openai-compatible、local-cliproxy
// 类型按开启处理；文件里没有搜索选择时沿用聊天选择。其余字段原样保留。
func ConvertMiBox(raw []byte) ([]byte, error) {
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	if document == nil {
		document = map[string]any{}
	}
	if configs, ok := document["configs"].(map[string]any); ok {
		for tag, value := range configs {
			provider, ok := value.(map[string]any)
			if !ok {
				continue
			}
			if _, set := provider["responses"].(bool); !set {
				kind := resolveProviderType(aiProvider{URL: StringOf(provider["url"]), Type: StringOf(provider["type"])})
				provider["responses"] = kind == "openai" || kind == "openai-compatible" || kind == "local-cliproxy"
			}
			if _, set := provider["stream"].(bool); !set {
				provider["stream"] = false
			}
			configs[tag] = provider
		}
	}
	for _, suffix := range []string{"Tag", "Model"} {
		if _, present := document["currentSearch"+suffix]; !present {
			document["currentSearch"+suffix] = strings.TrimSpace(StringOf(document["currentChat"+suffix]))
		}
	}
	return json.MarshalIndent(document, "", " ")
}
