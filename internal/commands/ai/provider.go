package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

// aiResponseLimit 是一次 AI 请求响应体的字节上限。流式响应把每个增量都包成一行
// JSON，长回答很容易超过原来的 2 MiB；16 MiB 足够宽裕，又不会无限制地吃内存。
const aiResponseLimit = 16 << 20

// ResponseLimit 是 AI 接口响应体的字节上限，.sum 自己的服务商也用它。
const ResponseLimit = aiResponseLimit

// aiOutputLimit 是解析出的回答长度上限（UTF-16 单位），与 MiBox 的 maxOutputChars 一致。
const aiOutputLimit = 1 << 20

// aiImage 是随问题一起发给模型的一张图片。
type aiImage struct {
	Data     []byte
	MimeType string
}

// chatOptions 是一次对话请求的附加内容。
type chatOptions struct {
	images          []aiImage
	maxOutputTokens int
}

// 请求格式：openai 兼容接口（含 Responses）、Gemini、Anthropic Messages。
const (
	formatOpenAI    = "openai"
	formatGemini    = "gemini"
	formatAnthropic = "anthropic"
)

type aiRequest struct {
	Model     string
	URL       string
	Headers   map[string]string
	Body      map[string]any
	Timeout   time.Duration
	Format    string
	Responses bool
	Provider  aiProvider
}

// EndpointProtocol 识别网关完整接口的协议。只有基址时交给服务商配置判断。
func EndpointProtocol(path string) string {
	path = strings.TrimRight(path, "/")
	switch {
	case strings.HasSuffix(path, "/chat/completions"), strings.HasSuffix(path, "/openai"):
		return shapeChat
	case strings.HasSuffix(path, "/responses"):
		return shapeResponses
	case strings.HasSuffix(path, "/messages"):
		return formatAnthropic
	case strings.HasSuffix(path, ":generateContent"), strings.HasSuffix(path, ":streamGenerateContent"):
		return formatGemini
	}
	return ""
}

func resolveProviderType(provider aiProvider) string {
	explicit := strings.ToLower(strings.TrimSpace(provider.Type))
	if slices.Contains(aiProviderTypes, explicit) {
		return explicit
	}
	if parsed, err := url.Parse(provider.URL); err == nil {
		kind := aiHostTypes[strings.ToLower(parsed.Hostname())]
		switch protocol := EndpointProtocol(parsed.Path); protocol {
		case formatAnthropic, formatGemini:
			return protocol
		case shapeChat, shapeResponses:
			if kind == formatGemini || kind == formatAnthropic {
				return "openai-compatible"
			}
		}
		if kind == formatGemini && strings.Contains(parsed.Path, "/openai/") {
			return "openai-compatible"
		}
		if kind != "" {
			return kind
		}
	}
	return "openai-compatible"
}

// ProviderEndpoint 从服务商给出的基址或完整接口地址派生同一 API 下的端点。
// 网关的路由前缀和查询参数属于配置；只有裸域名才补该协议的默认版本。
func ProviderEndpoint(base, kind, endpoint string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", kit.Fail("API 地址无效")
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	switch {
	case path == "":
		switch kind {
		case "gemini":
			path = "/v1beta"
		case "doubao":
			path = "/api/v3"
		default:
			path = "/v1"
		}
	case strings.HasSuffix(path, ":generateContent"), strings.HasSuffix(path, ":streamGenerateContent"):
		if i := strings.LastIndex(path, "/models/"); i >= 0 {
			path = path[:i]
		}
	default:
		for _, suffix := range []string{"/chat/completions", "/completions", "/responses", "/messages", "/images/generations", "/models"} {
			if strings.HasSuffix(path, suffix) {
				path = strings.TrimSuffix(path, suffix)
				break
			}
		}
	}
	u.RawPath = path + "/" + endpoint
	u.Path, err = url.PathUnescape(u.RawPath)
	if err != nil {
		return "", err
	}
	u.Fragment = ""
	return u.String(), nil
}

var modernChatModel = regexp.MustCompile(`^(?:openai/)?(?:gpt-(?:[5-9]|[1-9][0-9]+)|o[1-9])`)

// ChatTokenLimitField 为 GPT-5 及后续模型和 o 系列使用现行字段，老模型和兼容网关沿用 max_tokens。
func ChatTokenLimitField(model string) string {
	if modernChatModel.MatchString(strings.ToLower(model)) {
		return "max_completion_tokens"
	}
	return "max_tokens"
}

// AssertAllowedModel 拒绝项目策略禁止的模型。
func AssertAllowedModel(model string) error {
	if aiForbiddenModel.MatchString(strings.TrimSpace(model)) {
		return kit.Fail("该模型不可用于此项目")
	}
	return nil
}

func dataURL(image aiImage) string {
	return "data:" + image.MimeType + ";base64," + base64.StdEncoding.EncodeToString(image.Data)
}

// buildChatRequest 按服务商类型组装对话请求：先检查输入，再定接口地址和认证方式，
// 最后按请求体的形状（见 chatBodies）组装请求体。
func buildChatRequest(cfg aiConfig, sel aiSelection, text, systemPrompt string, options chatOptions) (*aiRequest, error) {
	provider, ok := cfg.Configs[sel.Tag]
	if sel.Tag == "" || sel.Model == "" || !ok {
		return nil, kit.Fail("请先用 ai config add 添加 API，并用 ai model chat 选择模型")
	}
	if err := checkChatInput(sel.Model, options); err != nil {
		return nil, err
	}
	kind := resolveProviderType(provider)
	format, endpoint := formatOpenAI, "chat/completions"
	switch kind {
	case "codex":
		return nil, kit.Fail("codex 类型的 API 不支持对话")
	case "gemini":
		format = formatGemini
		method := ":generateContent"
		if provider.Stream {
			method = ":streamGenerateContent"
		}
		endpoint = "models/" + url.PathEscape(strings.TrimPrefix(sel.Model, "models/")) + method
	case "anthropic":
		format, endpoint = formatAnthropic, "messages"
	default:
		if provider.Responses {
			endpoint = "responses"
		}
	}
	target, err := ProviderEndpoint(provider.URL, kind, endpoint)
	if err != nil {
		return nil, err
	}
	target, headers, secret := ProviderAuth(kind, provider.Key, target)
	provider.Key = secret
	if format == formatGemini && provider.Stream {
		u, _ := url.Parse(target)
		q := u.Query()
		q.Set("alt", "sse")
		u.RawQuery = q.Encode()
		target = u.String()
	}
	shape := format
	if format == formatOpenAI {
		shape = shapeChat
		if provider.Responses {
			shape = shapeResponses
		}
	}
	body := chatBodies[shape](chatInput{sel: sel, stream: provider.Stream, text: text, trimmed: strings.TrimSpace(text),
		system: strings.TrimSpace(systemPrompt), options: options})
	if format == formatOpenAI && sel.Tier != "" && sel.Tier != "auto" {
		body["service_tier"] = sel.Tier
	}
	provider.Tag = sel.Tag
	return &aiRequest{Model: sel.Model, URL: target, Headers: headers, Body: body, Timeout: time.Duration(cfg.Timeout) * time.Second,
		Format: format, Responses: format == formatOpenAI && provider.Responses, Provider: provider}, nil
}

// checkChatInput 检查模型是否允许使用，以及图片的张数、类型和总大小。
func checkChatInput(model string, options chatOptions) error {
	if err := AssertAllowedModel(model); err != nil {
		return err
	}
	if len(options.images) > aiMaxImages {
		return kit.Fail("图片输入无效")
	}
	total := 0
	for _, image := range options.images {
		total += len(image.Data)
		if !aiImageMime.MatchString(image.MimeType) {
			return kit.Fail("图片输入无效")
		}
	}
	if total > aiMaxImageBytes {
		return kit.Fail("图片输入过大")
	}
	return nil
}

// ProviderAuth 按服务商的认证方式放 Key：Gemini 和 local-cliproxy 放在查询参数 key 里
// （地址里已经带了就不覆盖），Anthropic 放在 x-api-key，其余用 Bearer。返回改过的地址、请求头，以及错误提示需要打码的实际 Key。
func ProviderAuth(kind, key, target string) (string, map[string]string, string) {
	headers := map[string]string{"Content-Type": "application/json"}
	if kind != formatGemini && kind != formatAnthropic {
		headers["User-Agent"] = CodexUserAgent
	}
	switch {
	case kind == formatGemini || kind == "local-cliproxy":
		authenticated, _ := url.Parse(target)
		query := authenticated.Query()
		if query.Has("key") {
			key = query.Get("key")
		} else {
			query.Set("key", key)
		}
		authenticated.RawQuery = query.Encode()
		target = authenticated.String()
	case kind == formatAnthropic:
		headers["x-api-key"] = key
		headers["anthropic-version"] = "2023-06-01"
	default:
		headers["Authorization"] = "Bearer " + key
	}
	return target, headers, key
}

// OpenAI 兼容接口的两种请求体形状，和 formatGemini、formatAnthropic 一起做 chatBodies 的键。
const (
	shapeChat      = "chat"
	shapeResponses = "responses"
)

// chatInput 是组装请求体要用的东西。text 是原文，trimmed 是去掉首尾空白后的：
// 用 trimmed 判断有没有文字，请求体里放哪一个沿用各家原来的写法。system 已经去掉首尾空白。
type chatInput struct {
	sel           aiSelection
	stream        bool
	text, trimmed string
	system        string
	options       chatOptions
}

// chatBodies 按请求体的形状组装。图片按各家的多模态格式放进用户消息：Gemini 用 inlineData，
// Anthropic 用 base64 source，Responses 用 input_image，Chat Completions 用 image_url，
// 后两者都是 data URL。
var chatBodies = map[string]func(chatInput) map[string]any{
	formatGemini:    geminiBody,
	formatAnthropic: anthropicBody,
	shapeResponses:  responsesBody,
	shapeChat:       chatCompletionsBody,
}

func geminiBody(in chatInput) map[string]any {
	body := map[string]any{}
	parts := []any{}
	if in.trimmed != "" {
		parts = append(parts, map[string]any{"text": in.text})
	}
	for _, image := range in.options.images {
		parts = append(parts, map[string]any{"inlineData": map[string]any{"data": base64.StdEncoding.EncodeToString(image.Data), "mimeType": image.MimeType}})
	}
	body["contents"] = []any{map[string]any{"role": "user", "parts": parts}}
	if in.system != "" {
		body["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": in.system}}}
	}
	if in.options.maxOutputTokens > 0 {
		body["generationConfig"] = map[string]any{"maxOutputTokens": in.options.maxOutputTokens}
	}
	return body
}

func anthropicBody(in chatInput) map[string]any {
	content := []any{}
	if in.trimmed != "" {
		content = append(content, map[string]any{"type": "text", "text": in.text})
	}
	for _, image := range in.options.images {
		content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": image.MimeType, "data": base64.StdEncoding.EncodeToString(image.Data)}})
	}
	maxTokens := in.options.maxOutputTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	body := map[string]any{"model": in.sel.Model, "max_tokens": maxTokens,
		"messages": []any{map[string]any{"role": "user", "content": content}}}
	if in.stream {
		body["stream"] = true
	}
	if in.system != "" {
		body["system"] = in.system
	}
	return body
}

func responsesBody(in chatInput) map[string]any {
	body := map[string]any{"model": in.sel.Model, "stream": in.stream}
	switch {
	case len(in.options.images) > 0:
		content := []any{}
		if in.trimmed != "" {
			content = append(content, map[string]any{"type": "input_text", "text": in.trimmed})
		}
		for _, image := range in.options.images {
			content = append(content, map[string]any{"type": "input_image", "image_url": dataURL(image)})
		}
		body["input"] = []any{map[string]any{"role": "user", "content": content}}
	case in.trimmed != "":
		body["input"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": in.trimmed}}}}
	default:
		body["input"] = in.text
	}
	if in.system != "" {
		body["instructions"] = in.system
	}
	if in.sel.Reasoning != "" && in.sel.Reasoning != "auto" {
		body["reasoning"] = map[string]any{"effort": in.sel.Reasoning}
	}
	if in.options.maxOutputTokens > 0 {
		body["max_output_tokens"] = in.options.maxOutputTokens
	}
	return body
}

func chatCompletionsBody(in chatInput) map[string]any {
	messages := []any{}
	if in.system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": in.system})
	}
	var content any = in.trimmed
	if in.trimmed == "" {
		content = in.text
	}
	if len(in.options.images) > 0 {
		parts := []any{}
		if in.trimmed != "" {
			parts = append(parts, map[string]any{"type": "text", "text": in.text})
		}
		for _, image := range in.options.images {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL(image)}})
		}
		content = parts
	}
	messages = append(messages, map[string]any{"role": "user", "content": content})
	body := map[string]any{"model": in.sel.Model, "messages": messages, "stream": in.stream}
	if in.sel.Reasoning != "" && in.sel.Reasoning != "auto" {
		body["reasoning_effort"] = in.sel.Reasoning
	}
	if in.options.maxOutputTokens > 0 {
		body[ChatTokenLimitField(in.sel.Model)] = in.options.maxOutputTokens
	}
	return body
}

// httpStatusError 是服务商返回非 2xx 时的错误：Error 是给用户看的说明，
// Status 留给需要按状态码回退的调用方。
type httpStatusError struct {
	Status int
	err    error
}

func (e *httpStatusError) Error() string { return e.err.Error() }

func (e *httpStatusError) Unwrap() error { return e.err }

// statusOf 取出服务商返回的 HTTP 状态码，不是这类错误时返回 0。
func statusOf(err error) int {
	var status *httpStatusError
	if errors.As(err, &status) {
		return status.Status
	}
	return 0
}

// HTTPFailure 把服务商的非 2xx 响应写成一句给用户看的话：带上接口返回的错误说明，
// 其中出现的 API Key 换成 ***。429 与 MiBox 一样只提示稍后重试。
func HTTPFailure(status int, body []byte, secret string) string {
	if status == 429 {
		return "请求过于频繁，稍后再试"
	}
	detail := ErrorDetail(body, secret)
	if detail == "" {
		return fmt.Sprintf("AI 接口返回 HTTP %d", status)
	}
	return fmt.Sprintf("AI 接口返回 HTTP %d：%s", status, detail)
}

func aiCall(ctx context.Context, request *aiRequest) (raw []byte, err error) {
	started := time.Now()
	info := request.info()
	info.Stage = "response"
	defer func() { err = info.Failure(err, started, request.Provider.Key) }()
	response, err := httpx.PostJSON(ctx, httpx.Request{URL: request.URL, Headers: request.Headers,
		Timeout: request.Timeout, MaxBytes: aiResponseLimit, WaitForHeaders: true}, request.Body)
	if err != nil {
		return nil, err
	}
	if !response.OK() {
		return nil, &httpStatusError{Status: response.Status, err: kit.Fail(HTTPFailure(response.Status, response.Body, request.Provider.Key))}
	}
	return response.Body, nil
}

// errorDetailLimit 是错误说明最多保留的字符数，免得一整页 HTML 或 JSON 被贴进聊天。
const errorDetailLimit = 200

var whitespaceRun = regexp.MustCompile(`\s+`)

// maskSecret 把 text 里出现的密钥（原文或 URL 编码后的形式）换成 ***。
func maskSecret(text, secret string) string {
	secret = strings.TrimSpace(secret)
	if len(secret) < 4 {
		return text
	}
	text = strings.ReplaceAll(text, secret, "***")
	if escaped := url.QueryEscape(secret); escaped != secret {
		text = strings.ReplaceAll(text, escaped, "***")
	}
	return text
}

// ErrorDetail 从服务商的错误响应里取出简短的说明：优先 error.message、error、
// message、detail 等字段，不是 JSON 时取正文文本（HTML 页面不取）。结果压成一行，
// 密钥换成 ***，最多 200 个字符。
func ErrorDetail(body []byte, secret string) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}
	var parsed any
	detail := ""
	if json.Unmarshal(body, &parsed) == nil {
		detail = errorMessageOf(parsed)
		if detail == "" {
			detail = text
		}
	} else if !strings.HasPrefix(text, "<") {
		detail = text
	}
	detail = strings.TrimSpace(whitespaceRun.ReplaceAllString(maskSecret(detail, secret), " "))
	if !utf8.ValidString(detail) {
		detail = strings.ToValidUTF8(detail, "")
	}
	if runes := []rune(detail); len(runes) > errorDetailLimit {
		detail = string(runes[:errorDetailLimit]) + "…"
	}
	return detail
}

// errorMessageOf 在错误 JSON 里找最像说明文字的字段。
func errorMessageOf(value any) string {
	if list, ok := value.([]any); ok {
		if len(list) == 0 {
			return ""
		}
		return errorMessageOf(list[0])
	}
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if inner, ok := object["error"].(map[string]any); ok {
		for _, key := range []string{"message", "msg", "detail", "code", "type"} {
			if text := strings.TrimSpace(StringOf(inner[key])); text != "" {
				return text
			}
		}
	}
	for _, key := range []string{"error", "message", "msg", "detail", "error_description"} {
		if text := strings.TrimSpace(StringOf(object[key])); text != "" {
			return text
		}
	}
	if response, ok := object["response"].(map[string]any); ok {
		return errorMessageOf(response)
	}
	return ""
}

func chatText(ctx context.Context, cfg aiConfig, sel aiSelection, text, systemPrompt string, options chatOptions) (string, error) {
	request, err := buildChatRequest(cfg, sel, text, systemPrompt, options)
	if err != nil {
		return "", err
	}
	started := time.Now()
	raw, err := aiCall(ctx, request)
	if err != nil {
		return "", err
	}
	answer, err := ParseText(raw, request.Format, request.Provider.Key)
	return answer, request.info().Failure(err, started, request.Provider.Key)
}
