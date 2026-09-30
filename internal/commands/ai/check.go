package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

// 这个文件给 .checkapi 用：列出服务商、拉模型列表、逐模型测速、查余额，
// 以及把 MiBox checkapi 旧版存的 Key 并进统一配置。对应 MiBox ai 插件的
// selection、models、diagnostics、import_provider 四个服务。Key 和接口地址只在
// 本包里用，返回给 .checkapi 的结果里没有它们。

// ProviderSummary 是一个服务商的公开信息：标签、配置里写的类型（没写是 auto）
// 和各模式记下的模型，不带地址和 Key。
type ProviderSummary struct {
	Tag    string
	Type   string
	Models map[string]string
}

// Providers 按标签排序列出 ai 配置里的服务商。
func (s *Service) Providers() ([]ProviderSummary, error) {
	if s == nil {
		return nil, nil
	}
	cfg, err := s.read()
	if err != nil {
		return nil, err
	}
	tags := make([]string, 0, len(cfg.Configs))
	for tag := range cfg.Configs {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	list := make([]ProviderSummary, 0, len(tags))
	for _, tag := range tags {
		provider := cfg.Configs[tag]
		kind := provider.Type
		if kind == "" {
			kind = "auto"
		}
		models := make(map[string]string, len(provider.Models))
		for mode, model := range provider.Models {
			models[mode] = model
		}
		list = append(list, ProviderSummary{Tag: tag, Type: kind, Models: models})
	}
	return list, nil
}

// checkLimit 是模型列表、余额这类查询响应体的上限，与 MiBox 一样 2 MiB。
const checkLimit = 2 << 20

// checkTimeout 是诊断里单次请求（列模型、测一个模型）的时限。ai 的超时最长能设到 600 秒，
// 测速要逐个测三个模型，照那个算会超过命令的 15 分钟，一个都显示不出来；每次最多等
// 90 秒，慢的那个记为超时，其余的结果照样显示。测试里会调小。
var checkTimeout = 90 * time.Second

// errCheckRedirect 表示接口把请求跳转到了别的主机，或者从 HTTPS 降到了 HTTP。
var errCheckRedirect = errors.New("redirect to another host refused")

// checkClient 发诊断请求。和 httpx 分开，是因为诊断要读响应头（限流信息），
// httpx.Do 不返回响应头；代理设置照样从环境变量读，与 httpx 一致。
//
// 跳转只许留在同一个主机、不许降到 HTTP：Go 跨主机跳转时只去掉 Authorization 和 Cookie，
// x-api-key、anthropic-version 这些头会原样带到新主机，Key 就泄露了。
var checkClient = &http.Client{
	Transport: http.DefaultTransport.(*http.Transport).Clone(),
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		first := via[0].URL
		if len(via) >= 3 || !strings.EqualFold(req.URL.Host, first.Host) || (first.Scheme == "https" && req.URL.Scheme != "https") {
			return errCheckRedirect
		}
		return nil
	},
}

type checkResponse struct {
	status int
	header http.Header
	body   []byte
}

func (r checkResponse) ok() bool { return r.status >= 200 && r.status < 300 }

// checkDo 发一次请求，响应体超过 limit 算失败。body 不为 nil 时编码成 JSON。
func checkDo(ctx context.Context, method, target string, headers map[string]string, body any, timeout time.Duration, limit int64) (checkResponse, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return checkResponse{}, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return checkResponse{}, err
	}
	request.Header.Set("User-Agent", httpx.UserAgent)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := checkClient.Do(request)
	if err != nil {
		return checkResponse{}, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return checkResponse{status: response.StatusCode, header: response.Header}, err
	}
	if int64(len(data)) > limit {
		return checkResponse{status: response.StatusCode, header: response.Header}, httpx.ErrTooLarge
	}
	return checkResponse{status: response.StatusCode, header: response.Header, body: data}, nil
}

// checkFailure 把错误写成一句能发进聊天的话：给用户看的错误照原话，网络错误用 httpx.Reason
// 概括，不带接口地址。
func checkFailure(err error) string {
	if text, ok := kit.IsUserError(err); ok {
		return text
	}
	return transportFailure(err)
}

// transportFailure 概括请求本身的失败；被拒绝的跳转单独说明。
func transportFailure(err error) string {
	if errors.Is(err, errCheckRedirect) {
		return "接口跳转到了别的地址，为保护 Key 已拒绝"
	}
	return httpx.Reason(err)
}

// Models 列出服务商接口返回的模型，去重后按名字排序。与 MiBox 的 listProviderModels 一致：
// doubao 走 api/v3/models，gemini 走 v1beta/models，其余走规整后的 /v1/models。
func (s *Service) Models(ctx context.Context, tag string) ([]string, error) {
	if s == nil {
		return nil, kit.Fail("ai 命令没有启用")
	}
	cfg, err := s.read()
	if err != nil {
		return nil, err
	}
	return providerModels(ctx, cfg, tag)
}

func providerModels(ctx context.Context, cfg aiConfig, tag string) ([]string, error) {
	provider, ok := cfg.Configs[tag]
	if tag == "" || !ok {
		return nil, kit.Fail("未找到指定的 AI 提供商")
	}
	kind := resolveProviderType(provider)
	if kind == "codex" {
		return nil, kit.Fail("codex 类型的 API 不支持模型列表")
	}
	parsed, err := url.Parse(provider.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, kit.Fail("API 地址无效")
	}
	base, endpoint := normalizeOpenAIBaseURL(provider.URL), "models"
	switch kind {
	case "doubao":
		base, endpoint = parsed.Scheme+"://"+parsed.Host, "api/v3/models"
	case "gemini":
		root := *parsed
		root.Path, root.RawPath, root.RawQuery, root.Fragment = "/v1beta", "", "", ""
		base = root.String()
	}
	target := aiEndpoint(base, endpoint)
	headers := map[string]string{"User-Agent": CodexUserAgent}
	switch kind {
	case "gemini", "local-cliproxy":
		authenticated, _ := url.Parse(target)
		query := authenticated.Query()
		if !query.Has("key") {
			query.Set("key", provider.Key)
		}
		authenticated.RawQuery = query.Encode()
		target = authenticated.String()
	case "anthropic":
		headers["x-api-key"] = provider.Key
		headers["anthropic-version"] = "2023-06-01"
	default:
		headers["Authorization"] = "Bearer " + provider.Key
	}
	response, err := checkDo(ctx, http.MethodGet, target, headers, nil, min(time.Duration(cfg.Timeout)*time.Second, checkTimeout), checkLimit)
	if err != nil {
		return nil, kit.Fail("获取模型列表失败：" + transportFailure(err))
	}
	if !response.ok() {
		return nil, &httpStatusError{Status: response.status, err: kit.Fail(HTTPFailure(response.status, response.body, provider.Key))}
	}
	var payload map[string]any
	if err := json.Unmarshal(response.body, &payload); err != nil {
		return nil, kit.Fail("AI 返回无效 JSON")
	}
	if aiProviderFailed(payload) {
		return nil, providerFailure(payload, provider.Key)
	}
	rows := ListOf(payload["data"])
	if rows == nil {
		rows = ListOf(payload["models"])
	}
	seen := map[string]bool{}
	var names []string
	for _, row := range rows {
		item := ObjectOf(row)
		name := StringOf(item["id"])
		if name == "" {
			name = StringOf(item["name"])
		}
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, kit.Fail("接口没有返回任何模型")
	}
	sort.Strings(names)
	return names, nil
}

// TokenUsage 是一次对话消耗的 token。
type TokenUsage struct{ Prompt, Completion, Total int }

// RateLimit 是响应头里的一项限流信息。
type RateLimit struct{ Name, Value string }

// Probe 是对一个模型发一次「ok」的结果。OK 为假时 Error 是给用户看的原因。
type Probe struct {
	Model      string
	OK         bool
	Text       string
	Elapsed    time.Duration
	Usage      *TokenUsage
	RateLimits []RateLimit
	Error      string
}

// rateLimitHeaders 是要带回来的限流响应头，顺序与 MiBox 一致。
var rateLimitHeaders = []string{
	"retry-after", "ratelimit-limit", "ratelimit-remaining", "ratelimit-reset",
	"x-ratelimit-limit-requests", "x-ratelimit-remaining-requests", "x-ratelimit-reset-requests",
	"x-ratelimit-limit-tokens", "x-ratelimit-remaining-tokens", "x-ratelimit-reset-tokens",
}

func rateLimitsOf(header http.Header) []RateLimit {
	var limits []RateLimit
	for _, name := range rateLimitHeaders {
		if values := header.Values(name); len(values) > 0 {
			limits = append(limits, RateLimit{Name: name, Value: kit.TruncateRunes(values[0], 256)})
		}
	}
	return limits
}

// jsonInt 把 JSON 数字读成整数，不是数字时为 0。
func jsonInt(value any) int {
	number, _ := value.(float64)
	return int(number)
}

// usageOf 从响应里找 token 用量：OpenAI 的 usage、Gemini 的 usageMetadata，
// 流式 Responses 把它放在 response.usage 里。有多个负载时取最后一个带用量的。
func usageOf(payloads []map[string]any) (*TokenUsage, string) {
	var found *TokenUsage
	model := ""
	for _, payload := range payloads {
		if name := StringOf(payload["model"]); name != "" {
			model = name
		} else if name := StringOf(ObjectOf(payload["response"])["model"]); name != "" {
			model = name
		}
		usage := ObjectOf(payload["usage"])
		if len(usage) == 0 {
			usage = ObjectOf(payload["usageMetadata"])
		}
		if len(usage) == 0 {
			usage = ObjectOf(ObjectOf(payload["response"])["usage"])
		}
		if len(usage) == 0 {
			continue
		}
		pick := func(keys ...string) int {
			for _, key := range keys {
				if value, ok := usage[key]; ok && value != nil {
					return jsonInt(value)
				}
			}
			return 0
		}
		prompt := pick("prompt_tokens", "input_tokens", "promptTokenCount")
		completion := pick("completion_tokens", "output_tokens", "candidatesTokenCount")
		total := jsonInt(usage["total_tokens"])
		if total == 0 {
			total = prompt + completion
		}
		if total > 0 {
			found = &TokenUsage{Prompt: prompt, Completion: completion, Total: total}
		}
	}
	return found, model
}

// probeModel 用这个模型回答一次「ok」，最多 maxTokens 个 token，与 MiBox 的诊断一样：不走流式，
// 思考强度和服务等级都用 auto。MiBox 还带了 temperature=0，这里不带：推理模型
// （o 系列、gpt-5 等）不接受这个参数，带了测速就全是失败。
func probeModel(ctx context.Context, cfg aiConfig, tag, model string, maxTokens int) Probe {
	result := Probe{Model: model}
	if err := AssertAllowedModel(model); err != nil {
		result.Error = checkFailure(err)
		return result
	}
	provider := cfg.Configs[tag]
	provider.Stream = false
	configs := make(map[string]aiProvider, len(cfg.Configs))
	for key, value := range cfg.Configs {
		configs[key] = value
	}
	configs[tag] = provider
	cfg.Configs = configs
	request, err := buildChatRequest(cfg, aiSelection{Tag: tag, Model: model, Reasoning: "auto", Tier: "auto"}, "ok", "", chatOptions{maxOutputTokens: maxTokens})
	if err != nil {
		result.Error = checkFailure(err)
		return result
	}
	started := time.Now()
	response, err := checkDo(ctx, http.MethodPost, request.URL, request.Headers, request.Body, min(request.Timeout, checkTimeout), aiResponseLimit)
	result.Elapsed = time.Since(started)
	if response.header != nil {
		result.RateLimits = rateLimitsOf(response.header)
	}
	if err != nil {
		result.Error = transportFailure(err)
		return result
	}
	if !response.ok() {
		result.Error = HTTPFailure(response.status, response.body, provider.Key)
		return result
	}
	text, err := parseChatText(response.body, request.Format, provider.Key)
	if err != nil {
		result.Error = checkFailure(err)
		return result
	}
	result.OK, result.Text = true, text
	if payloads, err := Payloads(response.body); err == nil {
		usage, name := usageOf(payloads)
		result.Usage = usage
		if name != "" {
			result.Model = name
		}
	}
	return result
}

// ProviderIdentity 是诊断结果里的服务商：标签、识别出的类型和显示名。
type ProviderIdentity struct{ Tag, Type, DisplayName string }

// providerHosts 按主机名认出常见的 OpenAI 兼容服务商，顺序与 MiBox 一致。
var providerHosts = [][2]string{
	{"openrouter", "openrouter"}, {"deepseek", "deepseek"}, {"groq", "groq"}, {"together", "together"},
	{"fireworks", "fireworks"}, {"mistral", "mistral"}, {"perplexity", "perplexity"}, {"siliconflow", "siliconflow"},
	{"deepinfra", "deepinfra"}, {"vercel", "vercel"}, {"azure", "azure"}, {"x.ai", "xai"}, {"nvidia", "nvidia"},
	{"novita", "novita"}, {"cerebras", "cerebras"},
}

var providerNames = map[string]string{
	"openai": "OpenAI", "openrouter": "OpenRouter", "deepseek": "DeepSeek", "gemini": "Google Gemini",
	"anthropic": "Anthropic", "xai": "xAI (Grok)", "nvidia": "NVIDIA NIM", "novita": "Novita", "cerebras": "Cerebras",
}

func identifyProvider(tag string, provider aiProvider) ProviderIdentity {
	kind := resolveProviderType(provider)
	host := ""
	if parsed, err := url.Parse(provider.URL); err == nil {
		host = strings.ToLower(parsed.Hostname())
	}
	for _, pair := range providerHosts {
		if strings.Contains(host, pair[0]) {
			kind = pair[1]
			break
		}
	}
	name, ok := providerNames[kind]
	if !ok {
		name = "OpenAI 兼容 API"
	}
	return ProviderIdentity{Tag: tag, Type: kind, DisplayName: name}
}

// benchmarkModels 是各服务商测速时默认测的模型，与 MiBox 一致；不在表里的测它配置的聊天模型。
var benchmarkModels = map[string][]string{
	"openai":      {"gpt-4.1-mini", "gpt-4.1-nano", "gpt-4o-mini"},
	"deepseek":    {"deepseek-chat", "deepseek-reasoner"},
	"openrouter":  {"openai/gpt-4.1-mini", "anthropic/claude-3.5-haiku", "google/gemini-2.5-flash"},
	"groq":        {"llama-3.3-70b-versatile", "mixtral-8x7b-32768", "gemma2-9b-it"},
	"together":    {"meta-llama/Llama-4-Maverick-17B-128E-Instruct-FP8", "Qwen/Qwen3-235B-A22B"},
	"fireworks":   {"accounts/fireworks/models/llama-v3p3-70b-instruct", "accounts/fireworks/models/deepseek-v3"},
	"mistral":     {"mistral-small-2506", "codestral-2501"},
	"perplexity":  {"sonar-reasoning", "sonar-pro"},
	"siliconflow": {"Qwen/Qwen3-8B", "deepseek-ai/DeepSeek-V3"},
	"deepinfra":   {"meta-llama/Llama-4-Maverick-17B-128E-Instruct"},
	"vercel":      {"gpt-4o-mini"},
	"azure":       {"gpt-4o-mini"},
	"xai":         {"grok-3-mini", "grok-2-latest"},
	"nvidia":      {"nvidia/llama-3.1-nemotron-ultra-253b-v1"},
	"novita":      {"deepseek/deepseek-v3-0324", "meta-llama/Llama-3.3-70B-Instruct"},
	"cerebras":    {"llama3.1-8b", "llama3.3-70b"},
}

// configuredChatModel 是诊断用的聊天模型：先看服务商自己记下的 chat 模型，
// 它是当前聊天标签时再看当前模型，与 MiBox 的诊断顺序一致。
func configuredChatModel(cfg aiConfig, tag string) string {
	if model := cfg.Configs[tag].Models["chat"]; model != "" {
		return model
	}
	if cfg.CurrentChatTag == tag {
		return cfg.CurrentChatModel
	}
	return ""
}

// Benchmark 逐个模型测一次响应速度（每次最多 50 token）。测哪些模型见 benchmarkModels。
func (s *Service) Benchmark(ctx context.Context, tag string) (ProviderIdentity, []Probe, error) {
	if s == nil {
		return ProviderIdentity{}, nil, kit.Fail("ai 命令没有启用")
	}
	cfg, err := s.read()
	if err != nil {
		return ProviderIdentity{}, nil, err
	}
	provider, ok := cfg.Configs[tag]
	if tag == "" || !ok {
		return ProviderIdentity{}, nil, kit.Fail("未找到指定的 AI 提供商")
	}
	identity := identifyProvider(tag, provider)
	models := benchmarkModels[identity.Type]
	if models == nil {
		if model := configuredChatModel(cfg, tag); model != "" {
			models = []string{model}
		} else {
			listed, err := providerModels(ctx, cfg, tag)
			if err != nil {
				return identity, nil, err
			}
			models = listed[:1]
		}
	}
	probes := make([]Probe, 0, len(models))
	for _, model := range models {
		if err := ctx.Err(); err != nil {
			return identity, nil, err
		}
		probes = append(probes, probeModel(ctx, cfg, tag, model, 50))
	}
	return identity, probes, nil
}

// BalanceField 是余额查询结果里的一项。
type BalanceField struct{ Label, Value string }

// Balance 是余额查询的结果。Status 是 ok、unsupported（查不了，请去官网看）、
// invalid（Key 无效或没有权限）或 error。
type Balance struct {
	Status string
	Fields []BalanceField
}

// Diagnosis 是一个服务商的完整诊断：余额、模型列表和一次对话测试。
type Diagnosis struct {
	Provider ProviderIdentity
	Balance  Balance
	// Models 是接口返回的模型，ModelsError 不为空表示没拿到。
	Models      []string
	ModelsError string
	// Chat 为 nil 表示没有可以测试的模型。
	Chat *Probe
}

// Diagnose 同时查余额和模型列表，再用聊天模型（没有就用列表里第一个）试一次对话。
func (s *Service) Diagnose(ctx context.Context, tag string) (*Diagnosis, error) {
	if s == nil {
		return nil, kit.Fail("ai 命令没有启用")
	}
	cfg, err := s.read()
	if err != nil {
		return nil, err
	}
	provider, ok := cfg.Configs[tag]
	if tag == "" || !ok {
		return nil, kit.Fail("未找到指定的 AI 提供商")
	}
	result := &Diagnosis{Provider: identifyProvider(tag, provider)}
	selected := configuredChatModel(cfg, tag)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		result.Balance = providerBalance(ctx, provider, result.Provider.Type, selected)
	}()
	go func() {
		defer wait.Done()
		models, err := providerModels(ctx, cfg, tag)
		if err != nil {
			result.ModelsError = checkFailure(err)
			return
		}
		result.Models = models
	}()
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	model := selected
	if model == "" && len(result.Models) > 0 {
		model = result.Models[0]
	}
	if model != "" {
		chat := probeModel(ctx, cfg, tag, model, 50)
		result.Chat = &chat
	}
	return result, ctx.Err()
}

// balanceReply 是余额类查询的一次应答：ok 为真表示 2xx 且是合法 JSON。
type balanceReply struct {
	ok     bool
	status int
	data   map[string]any
}

func balanceGet(ctx context.Context, method, target string, headers map[string]string, body any, timeout time.Duration) balanceReply {
	response, err := checkDo(ctx, method, target, headers, body, timeout, checkLimit)
	if err != nil {
		return balanceReply{status: response.status}
	}
	reply := balanceReply{status: response.status}
	if strings.TrimSpace(string(response.body)) == "" {
		reply.data = map[string]any{}
	} else {
		var data any
		if json.Unmarshal(response.body, &data) != nil {
			return reply
		}
		reply.data = ObjectOf(data)
	}
	reply.ok = response.ok()
	return reply
}

func balanceAuth(provider aiProvider, kind string) map[string]string {
	if kind == "anthropic" {
		return map[string]string{"x-api-key": provider.Key, "anthropic-version": "2023-06-01"}
	}
	return map[string]string{"Authorization": "Bearer " + provider.Key}
}

// jsString 按 JavaScript 的 String() 写出一个 JSON 值：没有值时写「?」，与 MiBox 显示的一致。
func jsString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "?"
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// jsTruthy 是 JavaScript 的真值判断。
func jsTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case float64:
		return typed != 0
	case string:
		return typed != ""
	}
	return true
}

func yesNoText(value bool) string {
	if value {
		return "是"
	}
	return "否"
}

// providerBalance 按服务商类型查余额，接口与 MiBox 的诊断相同。失败不算诊断失败，只记在结果里。
func providerBalance(ctx context.Context, provider aiProvider, kind, model string) Balance {
	origin := provider.URL
	if parsed, err := url.Parse(provider.URL); err == nil && parsed.Host != "" {
		origin = parsed.Scheme + "://" + parsed.Host
	}
	switch kind {
	case "openai":
		return openAIBalance(ctx, provider)
	case "anthropic":
		return anthropicBalance(ctx, provider, origin, model)
	}
	var target string
	switch kind {
	case "openrouter":
		target = origin + "/api/v1/auth/key"
	case "deepseek":
		target = origin + "/user/balance"
	case "gemini":
		target = origin + "/v1beta/models?key=" + url.QueryEscape(provider.Key)
	default:
		target = strings.TrimSuffix(normalizeOpenAIBaseURL(provider.URL), "/") + "/models"
	}
	reply := balanceGet(ctx, http.MethodGet, target, balanceAuth(provider, kind), nil, 10*time.Second)
	if !reply.ok {
		if reply.status == 401 || reply.status == 403 {
			return Balance{Status: "invalid"}
		}
		return Balance{Status: "error"}
	}
	data := reply.data
	var fields []BalanceField
	switch kind {
	case "openrouter":
		inner, present := data["data"]
		root := data
		if present && inner != nil {
			root = ObjectOf(inner)
		}
		label := root["label"]
		if label == nil {
			label = root["name"]
		}
		fields = append(fields, BalanceField{"标签", jsString(label)}, BalanceField{"余额", jsString(root["credits"])},
			BalanceField{"已用", jsString(root["usage"])}, BalanceField{"限额", jsString(root["limit"])})
		if rate := ObjectOf(root["rate_limit"]); len(rate) > 0 {
			fields = append(fields, BalanceField{"速率", jsString(rate["requests"]) + " req / " + jsString(rate["interval"])})
		}
		if disabled := ListOf(root["disabled_providers"]); len(disabled) > 0 {
			fields = append(fields, BalanceField{"禁用", strconv.Itoa(len(disabled))})
		}
	case "deepseek":
		fields = append(fields, BalanceField{"可用", yesNoText(jsTruthy(data["is_available"]))})
		for _, row := range ListOf(data["balance_infos"]) {
			item := ObjectOf(row)
			label := "余额"
			if item["currency"] != nil {
				label = jsString(item["currency"])
			}
			fields = append(fields, BalanceField{label, jsString(item["total_balance"])})
		}
	default:
		rows := ListOf(data["data"])
		if rows == nil {
			rows = ListOf(data["models"])
		}
		fields = append(fields, BalanceField{"状态", "有效"}, BalanceField{"模型", strconv.Itoa(len(rows))})
	}
	return Balance{Status: "ok", Fields: fields}
}

// openAIBalance 同时查 OpenAI 的订阅、近 90 天用量和模型（看组织和等级）。
// 新版 platform Key 没有账单接口，查不到时如实说明，不算失败。
func openAIBalance(ctx context.Context, provider aiProvider) Balance {
	root := strings.TrimSuffix(normalizeOpenAIBaseURL(provider.URL), "/") + "/"
	headers := balanceAuth(provider, "openai")
	now := time.Now().Unix()
	targets := []string{
		aiEndpoint(root, "dashboard/billing/subscription"),
		aiEndpoint(root, fmt.Sprintf("dashboard/billing/usage?start_date=%d&end_date=%d", now-90*86400, now)),
		aiEndpoint(root, "models"),
	}
	replies := make([]balanceReply, len(targets))
	var wait sync.WaitGroup
	for index, target := range targets {
		wait.Add(1)
		go func() {
			defer wait.Done()
			replies[index] = balanceGet(ctx, http.MethodGet, target, headers, nil, 10*time.Second)
		}()
	}
	wait.Wait()
	subscription, usage, models := replies[0], replies[1], replies[2]
	if !subscription.ok && (subscription.status == 401 || subscription.status == 403) {
		return Balance{Status: "invalid"}
	}
	data := subscription.data
	var fields []BalanceField
	if subscription.ok {
		plan := ObjectOf(data["plan"])
		fields = append(fields, BalanceField{"套餐", jsString(plan["title"])}, BalanceField{"硬上限", jsString(data["hard_limit_usd"])},
			BalanceField{"软上限", jsString(data["soft_limit_usd"])}, BalanceField{"系统上限", jsString(data["system_hard_limit_usd"])})
	} else {
		fields = append(fields, BalanceField{"账单", "可能是 platform Key，看不到"})
	}
	if value, ok := data["access_until"]; ok {
		seconds, _ := value.(float64)
		fields = append(fields, BalanceField{"有效期", time.Unix(int64(seconds), 0).UTC().Format("2006-01-02")})
	}
	if value, ok := data["has_payment_method"]; ok {
		fields = append(fields, BalanceField{"支付方式", yesNoText(jsTruthy(value))})
	}
	if usage.ok {
		total, _ := usage.data["total_usage"].(float64)
		fields = append(fields, BalanceField{"近 90 天", fmt.Sprintf("$%.4f", total/100)})
	}
	if models.ok {
		rows := ListOf(models.data["data"])
		if len(rows) > 50 {
			rows = rows[:50]
		}
		var owners []string
		tier := ""
		for _, row := range rows {
			item := ObjectOf(row)
			if owner, ok := item["owned_by"].(string); ok && !slices.Contains(owners, owner) {
				owners = append(owners, owner)
			}
			if value, ok := item["max_tier"].(string); ok {
				tier = value
			}
		}
		if len(owners) > 3 {
			owners = owners[:3]
		}
		if len(owners) > 0 {
			fields = append(fields, BalanceField{"Org", strings.Join(owners, ", ")})
		}
		if tier != "" {
			fields = append(fields, BalanceField{"Tier", tier})
		}
	}
	return Balance{Status: "ok", Fields: fields}
}

// anthropicBalance 用聊天模型发一个 1 token 的请求看 Key 是否有效；Anthropic 没有余额接口。
func anthropicBalance(ctx context.Context, provider aiProvider, origin, model string) Balance {
	if model == "" {
		return Balance{Status: "unsupported", Fields: []BalanceField{{"余额", "请到官网查看"}}}
	}
	body := map[string]any{"model": model, "max_tokens": 1, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	reply := balanceGet(ctx, http.MethodPost, origin+"/v1/messages", balanceAuth(provider, "anthropic"), body, 15*time.Second)
	if reply.ok || reply.status == 429 {
		state := "正常"
		if reply.status == 429 {
			state = "限流"
		}
		return Balance{Status: "ok", Fields: []BalanceField{{"Key", "有效（" + state + "）"}, {"余额", "请到官网查看"}}}
	}
	if reply.status == 401 || reply.status == 403 {
		return Balance{Status: "invalid"}
	}
	return Balance{Status: "error"}
}

// ProviderImport 是要并进统一配置的一个服务商，对应 MiBox ai 插件的 import_provider 服务。
type ProviderImport struct {
	Tag, URL, Key, Type string
	Stream, Responses   bool
	// Models 是各模式的模型；Select 里的模式在还没有当前选择时选上这个服务商。
	Models map[string]string
	Select []string
}

var importTag = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

var importModes = []string{"chat", "search", "image", "video"}

// ImportProvider 把一个服务商并进统一配置，返回实际用的标签和是否新加了一项。
// 规则与 MiBox 一致：标签已被占用且内容不同，就依次试 标签-2、标签-3…；内容相同则只补上
// 缺的模型。已有当前选择的模式不改。
func (s *Service) ImportProvider(value ProviderImport) (string, bool, error) {
	if s == nil {
		return "", false, kit.Fail("ai 命令没有启用")
	}
	value.Tag, value.URL, value.Key = strings.TrimSpace(value.Tag), strings.TrimSpace(value.URL), strings.TrimSpace(value.Key)
	if !importTag.MatchString(value.Tag) || slices.Contains([]string{"__proto__", "constructor", "prototype"}, value.Tag) {
		return "", false, kit.Fail("导入标签无效")
	}
	if len(value.URL) > 2048 || value.Key == "" || len(value.Key) > 8192 {
		return "", false, kit.Fail("导入配置无效")
	}
	parsed, err := url.Parse(value.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil {
		return "", false, kit.Fail("导入地址无效")
	}
	if value.Type != "" && !slices.Contains(aiProviderTypes, value.Type) {
		return "", false, kit.Fail("导入类型无效")
	}
	models := map[string]string{}
	for _, mode := range importModes {
		if model := strings.TrimSpace(value.Models[mode]); model != "" {
			if err := AssertAllowedModel(model); err != nil {
				return "", false, err
			}
			models[mode] = model
		}
	}
	same := func(current aiProvider) bool {
		return current.URL == value.URL && current.Key == value.Key && current.Type == value.Type &&
			current.Stream == value.Stream && current.Responses == value.Responses
	}
	actual, imported := value.Tag, false
	err = s.update(func(cfg *aiConfig) error {
		existing, taken := cfg.Configs[value.Tag]
		if taken && !same(existing) {
			actual, taken = "", false
			for index := 2; index <= 999; index++ {
				suffix := "-" + strconv.Itoa(index)
				candidate := value.Tag[:min(len(value.Tag), 64-len(suffix))] + suffix
				current, used := cfg.Configs[candidate]
				if !used || same(current) {
					actual, existing, taken = candidate, current, used
					break
				}
			}
			if actual == "" {
				return kit.Fail("AI 导入标签已用尽")
			}
		}
		merged := map[string]string{}
		for mode, model := range existing.Models {
			merged[mode] = model
		}
		for mode, model := range models {
			if merged[mode] == "" {
				merged[mode] = model
			}
		}
		if !taken {
			cfg.Configs[actual] = aiProvider{Tag: actual, URL: value.URL, Key: value.Key, Type: value.Type,
				Stream: value.Stream, Responses: value.Responses, Models: models}
			imported = true
		} else if len(merged) != len(existing.Models) {
			existing.Models = merged
			cfg.Configs[actual] = existing
		}
		for _, mode := range value.Select {
			model := merged[mode]
			if model == "" {
				continue
			}
			var tag, current *string
			switch mode {
			case "chat":
				tag, current = &cfg.CurrentChatTag, &cfg.CurrentChatModel
			case "search":
				tag, current = &cfg.CurrentSearchTag, &cfg.CurrentSearchModel
			case "image":
				tag, current = &cfg.CurrentImageTag, &cfg.CurrentImageModel
			case "video":
				tag, current = &cfg.CurrentVideoTag, &cfg.CurrentVideoModel
			default:
				continue
			}
			if *tag == "" || *current == "" {
				*tag, *current = actual, model
			}
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return actual, imported, nil
}
