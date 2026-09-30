package checkapi

import (
	"encoding/json"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/MiCat-S/mibot-lite/internal/commands/ai"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// state 是 data/checkapi.json，沿用 MiBox checkapi v2 的 assets/checkapi/keys-v2.json，
// 可以原样复制过来。entries 是 v1 自己存的 Key（{name, key, baseUrl, provider, addedAt}），
// v2 启动时把它们并进 ai 的统一配置，之后清空并记下 aiMigrated。条目按原始 JSON 存，
// 其中哪一条格式不对只跳过那一条，不至于让整个文件读不出来。
type state struct {
	SchemaVersion  int               `json:"schemaVersion"`
	Entries        []json.RawMessage `json:"entries"`
	LegacyImported bool              `json:"legacyImported"`
	AIMigrated     bool              `json:"aiMigrated"`
}

func defaults() state { return state{SchemaVersion: 2, Entries: []json.RawMessage{}} }

// legacyEntry 是 checkapi v1 存的一个 API，已经补好地址、接口类型和聊天模型。
type legacyEntry struct {
	Name, Key, BaseURL, Type, Model string
}

// legacyDefault 是 checkapi v1 认出的一家服务商在 ai 里的样子。
type legacyDefault struct{ url, kind, model string }

// legacyProviders 按 checkapi v1 记下的 provider 补地址：v1 按 Key 前缀认出服务商时
// 不存地址（比如 sk-ant- 开头的就是 Anthropic），MiBox v2 迁移时要求有地址，把这些 Key 都丢了。
// 地址和模型取自 v1 自己探测时用的那些。不在表里的（cohere、replicate、azure 这类
// 不是 OpenAI 兼容接口、或者没有固定地址的）没存地址就没法迁。
var legacyProviders = map[string]legacyDefault{
	"openai":     {"https://api.openai.com/v1", "openai", "gpt-4o-mini"},
	"anthropic":  {"https://api.anthropic.com", "anthropic", "claude-3-5-haiku-20241022"},
	"gemini":     {"https://generativelanguage.googleapis.com/v1beta", "gemini", "gemini-2.5-flash"},
	"deepseek":   {"https://api.deepseek.com", "openai-compatible", "deepseek-chat"},
	"openrouter": {"https://openrouter.ai/api/v1", "openai-compatible", "openai/gpt-4o-mini"},
	"groq":       {"https://api.groq.com/openai/v1", "openai-compatible", "gpt-4o-mini"},
	"together":   {"https://api.together.xyz/v1", "openai-compatible", "gpt-4o-mini"},
	"fireworks":  {"https://api.fireworks.ai/inference/v1", "openai-compatible", "gpt-4o-mini"},
	"xai":        {"https://api.x.ai/v1", "openai-compatible", "gpt-4o-mini"},
	"nvidia":     {"https://integrate.api.nvidia.com/v1", "openai-compatible", "gpt-4o-mini"},
}

// legacyKind 决定导入时的接口类型：Anthropic 和 Gemini 的认证方式不同（x-api-key、
// 查询参数 key），按 OpenAI 兼容导入就用不了。先看 v1 记下的 provider，再看地址的主机名，
// 与 v1 识别服务商的规则一致。MiBox v2 一律按 openai-compatible 导入。
func legacyKind(provider, host string) string {
	host = strings.ToLower(host)
	switch {
	case provider == "anthropic" || strings.Contains(host, "anthropic"):
		return "anthropic"
	case provider == "gemini" || strings.Contains(host, "generativelanguage") || strings.Contains(host, "googleapis"):
		return "gemini"
	case host == "api.openai.com":
		return "openai"
	}
	return "openai-compatible"
}

// parseEntry 检查一条旧记录并补全，规则与 ai.ImportProvider 的检查一致，免得这里放过、
// 导入时再被拒：名字和 Key 是字符串，Key 去掉空白后不为空、不超过 8192 字符；地址是不带
// 用户名密码的 http(s)，不超过 2048 字符，没有地址时按 provider 补上。addedAt 用不上，不看。
func parseEntry(raw json.RawMessage) (legacyEntry, bool) {
	var item map[string]any
	if json.Unmarshal(raw, &item) != nil || item == nil {
		return legacyEntry{}, false
	}
	name, nameOK := item["name"].(string)
	key, keyOK := item["key"].(string)
	key = strings.TrimSpace(key)
	if !nameOK || !keyOK || key == "" || len(key) > 8192 {
		return legacyEntry{}, false
	}
	provider, _ := item["provider"].(string)
	provider = strings.ToLower(strings.TrimSpace(provider))
	base, _ := item["baseUrl"].(string)
	base = strings.TrimSpace(base)
	known, isKnown := legacyProviders[provider]
	if base == "" {
		if !isKnown {
			return legacyEntry{}, false
		}
		base = known.url
	}
	parsed, err := url.Parse(base)
	if err != nil || len(base) > 2048 || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil {
		return legacyEntry{}, false
	}
	kind := legacyKind(provider, parsed.Hostname())
	if kind == "gemini" && (parsed.Path == "" || parsed.Path == "/") {
		// ai 的 gemini 接口地址要带版本路径，v1 存的常常只有域名。
		base = parsed.Scheme + "://" + parsed.Host + "/v1beta"
	}
	model := "gpt-4o-mini"
	switch {
	case kind == "anthropic":
		model = legacyProviders["anthropic"].model
	case kind == "gemini":
		model = legacyProviders["gemini"].model
	case isKnown:
		model = known.model
	case strings.Contains(strings.ToLower(parsed.Hostname()), "deepseek"):
		model = legacyProviders["deepseek"].model
	}
	return legacyEntry{Name: name, Key: key, BaseURL: base, Type: kind, Model: model}, true
}

var unsafeTag = regexp.MustCompile(`[^A-Za-z0-9._-]`)
var dashRun = regexp.MustCompile(`-+`)

// legacyTag 把旧记录的名字变成 ai 的标签：非法字符换成 -，连续的 - 合并，去掉首尾的 -；
// 什么都不剩时用 checkapi-序号。与 MiBox 一致。
func legacyTag(name string, index int) string {
	tag := strings.Trim(dashRun.ReplaceAllString(unsafeTag.ReplaceAllString(name, "-"), "-"), "-")
	if tag == "" {
		tag = "checkapi-" + strconv.Itoa(index+1)
	}
	if len(tag) > 64 {
		tag = tag[:64]
	}
	return tag
}

// nameOf 取旧记录的名字，只用于日志（日志里不能有 Key）。
func nameOf(raw json.RawMessage) string {
	var item struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(raw, &item)
	return item.Name
}

// migrate 把还没并进 ai 的旧 Key 并过去，返回并了几条。做法与 MiBox 一致：按记录的服务商
// 补好地址、类型和聊天模型，第一条在还没有聊天选择时选作聊天模型。
//
// 格式不对、或者被 ai 拒绝（给用户看的错误）的记录跳过并记一条警告（不带 Key）；处理完就
// 清空 entries、记下 aiMigrated，Key 从此只留在 ai 的配置里。以前有一条被拒就整个返回、
// 不记 aiMigrated，之后每次 .checkapi 都把前面的重新导入一遍：已经用 .ai config del
// 删掉的服务商又回来了，内容变过的还会多出「标签-2」。只有读写配置文件本身出错时才中止，
// 下次再试。没有要并的就不写文件。
func (s *service) migrate(log *slog.Logger) (int, error) {
	current, err := s.store.Read()
	if err != nil || current.AIMigrated || len(current.Entries) == 0 {
		return 0, err
	}
	imported, valid := 0, 0
	for _, raw := range current.Entries {
		entry, ok := parseEntry(raw)
		if !ok {
			log.Warn("checkapi.legacy_skipped", "name", nameOf(raw), "reason", "记录不完整或地址无效")
			continue
		}
		var selected []string
		if valid == 0 {
			selected = []string{"chat"}
		}
		tag := legacyTag(entry.Name, valid)
		valid++
		_, _, err := s.models.ImportProvider(ai.ProviderImport{Tag: tag, URL: entry.BaseURL, Key: entry.Key,
			Type: entry.Type, Models: map[string]string{"chat": entry.Model}, Select: selected})
		if text, rejected := kit.IsUserError(err); rejected {
			log.Warn("checkapi.legacy_skipped", "name", entry.Name, "reason", text)
			continue
		}
		if err != nil {
			return imported, err
		}
		imported++
	}
	return imported, s.store.Update(func(value *state) error {
		value.SchemaVersion, value.Entries, value.LegacyImported, value.AIMigrated = 2, []json.RawMessage{}, true, true
		return nil
	})
}

// ConvertLegacy 把 MiBox checkapi v1 的 assets/checkapi/keys.json（一个数组）转成
// data/checkapi.json，下次用 .checkapi 时并进 ai。只在没有 v2 的 keys-v2.json 时用。
// 记录原样保留（包括 v1 的 provider 字段），并入时再按它补地址和类型。
func ConvertLegacy(raw []byte) ([]byte, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	entries := []json.RawMessage{}
	for _, item := range list {
		if _, ok := parseEntry(item); ok {
			entries = append(entries, item)
		}
	}
	return json.MarshalIndent(state{SchemaVersion: 2, Entries: entries, LegacyImported: true}, "", " ")
}
