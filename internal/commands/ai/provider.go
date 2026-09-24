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
	URL       string
	Headers   map[string]string
	Body      map[string]any
	Timeout   time.Duration
	Format    string
	Responses bool
	Provider  aiProvider
}

func resolveProviderType(provider aiProvider) string {
	explicit := strings.ToLower(strings.TrimSpace(provider.Type))
	if slices.Contains(aiProviderTypes, explicit) {
		return explicit
	}
	if parsed, err := url.Parse(provider.URL); err == nil {
		if kind, ok := aiHostTypes[parsed.Hostname()]; ok {
			return kind
		}
	}
	return "openai"
}

// providerURL 是实际使用的接口地址。与 MiBox 读取配置时的处理一致：openai、
// openai-compatible、moonshot 类型只填了域名（路径为空或 /）时补上 /v1。
func providerURL(provider aiProvider) string {
	parsed, err := url.Parse(provider.URL)
	if err != nil || parsed.Host == "" {
		return provider.URL
	}
	switch resolveProviderType(provider) {
	case "openai", "openai-compatible", "moonshot":
		if parsed.Path == "" || parsed.Path == "/" {
			return normalizeOpenAIBaseURL(provider.URL)
		}
	}
	return provider.URL
}

func normalizeOpenAIBaseURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if strings.Contains(parsed.Hostname(), "gateway.ai.cloudflare.com") {
		if index := strings.Index(parsed.Path, "/openai"); index >= 0 {
			parsed.Path = parsed.Path[:index+len("/openai")]
		}
	} else {
		for _, suffix := range []string{"/chat/completions", "/completions", "/responses", "/messages", "/images/generations"} {
			if strings.HasSuffix(parsed.Path, suffix) {
				parsed.Path = strings.TrimSuffix(parsed.Path, suffix)
				break
			}
		}
		prefix := "/v1"
		if strings.Contains(parsed.Path, "/api/v1") {
			prefix = "/api/v1"
		}
		if index := strings.Index(parsed.Path, prefix); index >= 0 {
			parsed.Path = parsed.Path[:index+len(prefix)]
		} else {
			parsed.Path = "/v1"
		}
	}
	parsed.RawQuery = ""
	return parsed.String()
}

func aiEndpoint(base, path string) string {
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	parsedBase, err := url.Parse(base)
	if err != nil {
		return base + path
	}
	reference, err := url.Parse(path)
	if err != nil {
		return base + path
	}
	return parsedBase.ResolveReference(reference).String()
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

// buildChatRequest 按服务商类型组装对话请求。图片按各家的多模态格式放进用户消息：
// Gemini 用 inlineData，Anthropic 用 base64 source，Responses 用 input_image，
// Chat Completions 用 image_url，后两者都是 data URL。
func buildChatRequest(cfg aiConfig, sel aiSelection, text, systemPrompt string, options chatOptions) (*aiRequest, error) {
	provider, ok := cfg.Configs[sel.Tag]
	if sel.Tag == "" || sel.Model == "" || !ok {
		return nil, kit.Fail("请先用 ai config add 添加 API，并用 ai model chat 选择模型")
	}
	if err := AssertAllowedModel(sel.Model); err != nil {
		return nil, err
	}
	if len(options.images) > aiMaxImages {
		return nil, kit.Fail("图片输入无效")
	}
	total := 0
	for _, image := range options.images {
		total += len(image.Data)
		if !aiImageMime.MatchString(image.MimeType) {
			return nil, kit.Fail("图片输入无效")
		}
	}
	if total > aiMaxImageBytes {
		return nil, kit.Fail("图片输入过大")
	}
	address := providerURL(provider)
	parsed, err := url.Parse(address)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, kit.Fail("API 地址无效")
	}
	kind := resolveProviderType(provider)
	if kind == "codex" {
		return nil, kit.Fail("codex 类型的 API 不支持对话")
	}
	format := formatOpenAI
	switch kind {
	case "gemini":
		format = formatGemini
	case "anthropic":
		format = formatAnthropic
	}
	base := address
	switch kind {
	case "doubao":
		base = parsed.Scheme + "://" + parsed.Host
	case "local-cliproxy":
		base = normalizeOpenAIBaseURL(address)
	}
	chatEndpoint := "chat/completions"
	if kind == "doubao" {
		chatEndpoint = "api/v3/chat/completions"
	}
	var target string
	switch {
	case format == formatGemini:
		target = aiEndpoint(base, "models/"+sel.Model+":generateContent")
	case format == formatAnthropic:
		target = aiEndpoint(normalizeOpenAIBaseURL(base), "messages")
	case provider.Responses:
		if kind == "doubao" {
			target = aiEndpoint(normalizeOpenAIBaseURL(aiEndpoint(base, chatEndpoint)), "responses")
		} else {
			target = aiEndpoint(normalizeOpenAIBaseURL(base), "responses")
		}
	default:
		target = aiEndpoint(base, chatEndpoint)
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if format == formatOpenAI {
		headers["User-Agent"] = CodexUserAgent
	}
	switch {
	case format == formatGemini || kind == "local-cliproxy":
		authenticated, _ := url.Parse(target)
		query := authenticated.Query()
		if !query.Has("key") {
			query.Set("key", provider.Key)
		}
		authenticated.RawQuery = query.Encode()
		target = authenticated.String()
	case format == formatAnthropic:
		headers["x-api-key"] = provider.Key
		headers["anthropic-version"] = "2023-06-01"
	default:
		headers["Authorization"] = "Bearer " + provider.Key
	}
	system := strings.TrimSpace(systemPrompt)
	trimmed := strings.TrimSpace(text)
	body := map[string]any{}
	switch format {
	case formatGemini:
		parts := []any{}
		if trimmed != "" {
			parts = append(parts, map[string]any{"text": text})
		}
		for _, image := range options.images {
			parts = append(parts, map[string]any{"inlineData": map[string]any{"data": base64.StdEncoding.EncodeToString(image.Data), "mimeType": image.MimeType}})
		}
		body["contents"] = []any{map[string]any{"role": "user", "parts": parts}}
		if system != "" {
			body["systemInstruction"] = map[string]any{"role": "system", "parts": []any{map[string]any{"text": system}}}
		}
		if options.maxOutputTokens > 0 {
			body["generationConfig"] = map[string]any{"maxOutputTokens": options.maxOutputTokens}
		}
	case formatAnthropic:
		content := []any{}
		if trimmed != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		for _, image := range options.images {
			content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": image.MimeType, "data": base64.StdEncoding.EncodeToString(image.Data)}})
		}
		maxTokens := options.maxOutputTokens
		if maxTokens <= 0 {
			maxTokens = 4096
		}
		body["model"], body["max_tokens"] = sel.Model, maxTokens
		body["messages"] = []any{map[string]any{"role": "user", "content": content}}
		if system != "" {
			body["system"] = system
		}
	default:
		if provider.Responses {
			switch {
			case len(options.images) > 0:
				content := []any{}
				if trimmed != "" {
					content = append(content, map[string]any{"type": "input_text", "text": trimmed})
				}
				for _, image := range options.images {
					content = append(content, map[string]any{"type": "input_image", "image_url": dataURL(image)})
				}
				body["input"] = []any{map[string]any{"role": "user", "content": content}}
			case trimmed != "":
				body["input"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": trimmed}}}}
			default:
				body["input"] = text
			}
			body["model"], body["stream"] = sel.Model, provider.Stream
			if system != "" {
				body["instructions"] = system
			}
			if sel.Reasoning != "" && sel.Reasoning != "auto" {
				body["reasoning"] = map[string]any{"effort": sel.Reasoning}
			}
			if options.maxOutputTokens > 0 {
				body["max_output_tokens"] = options.maxOutputTokens
			}
		} else {
			messages := []any{}
			if system != "" {
				messages = append(messages, map[string]any{"role": "system", "content": system})
			}
			var content any = trimmed
			if trimmed == "" {
				content = text
			}
			if len(options.images) > 0 {
				parts := []any{}
				if trimmed != "" {
					parts = append(parts, map[string]any{"type": "text", "text": text})
				}
				for _, image := range options.images {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL(image)}})
				}
				content = parts
			}
			messages = append(messages, map[string]any{"role": "user", "content": content})
			body["model"], body["messages"], body["stream"] = sel.Model, messages, provider.Stream
			if sel.Reasoning != "" && sel.Reasoning != "auto" {
				body["reasoning_effort"] = sel.Reasoning
			}
			if options.maxOutputTokens > 0 {
				body["max_tokens"] = options.maxOutputTokens
			}
		}
		if sel.Tier != "" && sel.Tier != "auto" {
			body["service_tier"] = sel.Tier
		}
	}
	return &aiRequest{URL: target, Headers: headers, Body: body, Timeout: time.Duration(cfg.Timeout) * time.Second,
		Format: format, Responses: format == formatOpenAI && provider.Responses, Provider: provider}, nil
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
		return "请求过于频繁，请稍后重试"
	}
	detail := ErrorDetail(body, secret)
	if detail == "" {
		return fmt.Sprintf("AI 接口返回 HTTP %d", status)
	}
	return fmt.Sprintf("AI 接口返回 HTTP %d：%s", status, detail)
}

func aiCall(ctx context.Context, request *aiRequest) ([]byte, error) {
	response, err := httpx.PostJSON(ctx, request.URL, request.Headers, request.Body, request.Timeout, aiResponseLimit)
	if err != nil {
		return nil, kit.Fail("AI 请求失败：" + httpx.Reason(err))
	}
	if !response.OK() {
		return nil, &httpStatusError{Status: response.Status, err: kit.Fail(HTTPFailure(response.Status, response.Body, request.Provider.Key))}
	}
	return response.Body, nil
}

// errorDetailLimit 是错误说明最多保留的字符数，免得一整页 HTML 或 JSON 被贴进聊天。
const errorDetailLimit = 200

var whitespaceRun = regexp.MustCompile(`\s+`)

// MaskSecret 把 text 里出现的密钥（原文或 URL 编码后的形式）换成 ***。
func MaskSecret(text, secret string) string {
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
	detail = strings.TrimSpace(whitespaceRun.ReplaceAllString(MaskSecret(detail, secret), " "))
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

// Payloads 把响应体拆成若干 JSON 负载：有 SSE 的 data 行就按行拆，
// 没有就把整个响应体当作一个。
func Payloads(raw []byte) ([]map[string]any, error) {
	var payloads []map[string]any
	text := string(raw)
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(line[5:])
			if line != "" && line != "[DONE]" {
				lines = append(lines, line)
			}
		}
	}
	if len(lines) == 0 {
		lines = []string{text}
	}
	for _, line := range lines {
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			return nil, kit.Fail("AI 返回无效 JSON")
		}
		payloads = append(payloads, payload)
	}
	return payloads, nil
}

// ObjectOf 把 JSON 值当作对象读取，不是对象时返回空对象。
func ObjectOf(value any) map[string]any {
	object, _ := value.(map[string]any)
	if object == nil {
		return map[string]any{}
	}
	return object
}

// ListOf 把 JSON 值当作数组读取。
func ListOf(value any) []any {
	list, _ := value.([]any)
	return list
}

// StringOf 把 JSON 值当作字符串读取。
func StringOf(value any) string {
	text, _ := value.(string)
	return text
}

// FirstOf 返回数组的第一个元素，空数组返回 nil。
func FirstOf(list []any) any {
	if len(list) == 0 {
		return nil
	}
	return list[0]
}

func aiProviderFailed(payload map[string]any) bool {
	response := ObjectOf(payload["response"])
	return payload["error"] != nil || response["error"] != nil || payload["type"] == "error" || payload["type"] == "response.failed" || payload["status"] == "failed" || response["status"] == "failed"
}

// providerFailure 是 2xx 响应里带着错误时给用户看的说明。
func providerFailure(payload map[string]any, secret string) error {
	detail := strings.TrimSpace(whitespaceRun.ReplaceAllString(MaskSecret(errorMessageOf(payload), secret), " "))
	if runes := []rune(detail); len(runes) > errorDetailLimit {
		detail = string(runes[:errorDetailLimit]) + "…"
	}
	if detail == "" {
		return kit.Fail("AI 提供商返回错误")
	}
	return kit.Fail("AI 提供商返回错误：" + detail)
}

func aiContentText(content any) string {
	if text, ok := content.(string); ok {
		if text == "AI 回复为空" {
			return ""
		}
		return text
	}
	var parts []string
	items := ListOf(content)
	if items == nil && content != nil {
		items = []any{content}
	}
	for _, item := range items {
		part := ObjectOf(item)
		if (part["type"] == "text" || part["type"] == "output_text") && StringOf(part["text"]) != "" {
			parts = append(parts, StringOf(part["text"]))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func geminiRoot(payload map[string]any) map[string]any {
	if response := ObjectOf(payload["response"]); len(response) > 0 {
		return response
	}
	if data := ObjectOf(payload["data"]); len(data) > 0 {
		return data
	}
	return payload
}

func parseChatText(raw []byte, format, secret string) (string, error) {
	if strings.TrimSpace(string(raw)) == "" {
		return "", kit.Fail("AI 返回内容为空")
	}
	payloads, err := Payloads(raw)
	if err != nil {
		return "", err
	}
	var text string
	switch format {
	case formatGemini:
		payload := payloads[0]
		if aiProviderFailed(payload) {
			return "", providerFailure(payload, secret)
		}
		candidate := ObjectOf(FirstOf(ListOf(geminiRoot(payload)["candidates"])))
		var parts []string
		for _, part := range ListOf(ObjectOf(candidate["content"])["parts"]) {
			parts = append(parts, StringOf(ObjectOf(part)["text"]))
		}
		text = strings.Join(parts, "")
	case formatAnthropic:
		payload := payloads[0]
		if aiProviderFailed(payload) {
			return "", providerFailure(payload, secret)
		}
		var parts []string
		for _, item := range ListOf(payload["content"]) {
			if part := ObjectOf(item); part["type"] == "text" {
				parts = append(parts, StringOf(part["text"]))
			}
		}
		text = strings.Join(parts, "\n")
	default:
		responses := false
		for _, payload := range payloads {
			if payload["object"] == "response" || ObjectOf(payload["response"])["object"] == "response" || strings.HasPrefix(StringOf(payload["type"]), "response.") {
				responses = true
			}
		}
		var deltas []string
		fallback := ""
		for _, payload := range payloads {
			if aiProviderFailed(payload) {
				return "", providerFailure(payload, secret)
			}
			if responses {
				if payload["type"] == "response.output_text.delta" && StringOf(payload["delta"]) != "" {
					deltas = append(deltas, StringOf(payload["delta"]))
				}
				if payload["type"] == "response.output_text.done" && StringOf(payload["text"]) != "" && len(deltas) == 0 {
					fallback = strings.TrimSpace(StringOf(payload["text"]))
				}
				response := map[string]any{}
				if ObjectOf(payload["response"])["object"] == "response" {
					response = ObjectOf(payload["response"])
				} else if payload["object"] == "response" {
					response = payload
				}
				items := append([]any{payload["item"]}, ListOf(response["output"])...)
				for _, item := range items {
					message := ObjectOf(item)
					if message["type"] != "message" {
						continue
					}
					var parts []string
					for _, part := range ListOf(message["content"]) {
						content := ObjectOf(part)
						if content["type"] == "output_text" && StringOf(content["text"]) != "" {
							parts = append(parts, StringOf(content["text"]))
						}
					}
					if value := strings.TrimSpace(strings.Join(parts, "\n")); value != "" {
						fallback = value
					}
				}
				continue
			}
			choice := ObjectOf(FirstOf(ListOf(payload["choices"])))
			if delta := aiContentText(ObjectOf(choice["delta"])["content"]); delta != "" {
				deltas = append(deltas, delta)
			}
			content := ObjectOf(choice["message"])["content"]
			if content == nil {
				content = choice["content"]
			}
			if content == nil {
				content = payload["content"]
			}
			switch {
			case content != nil:
				if value := aiContentText(content); value != "" {
					fallback = value
				}
			case strings.TrimSpace(StringOf(choice["text"])) != "":
				fallback = strings.TrimSpace(StringOf(choice["text"]))
			case strings.TrimSpace(StringOf(payload["text"])) != "":
				fallback = strings.TrimSpace(StringOf(payload["text"]))
			}
		}
		if len(deltas) > 0 {
			text = strings.Join(deltas, "")
		} else {
			text = fallback
		}
	}
	if kit.UTF16Len(text) > aiOutputLimit {
		return "", kit.Fail("AI 输出超出长度限制")
	}
	if strings.TrimSpace(text) == "" {
		return "", kit.Fail("AI 返回内容为空")
	}
	return strings.TrimSpace(text), nil
}

func chatText(ctx context.Context, cfg aiConfig, sel aiSelection, text, systemPrompt string, options chatOptions) (string, error) {
	request, err := buildChatRequest(cfg, sel, text, systemPrompt, options)
	if err != nil {
		return "", err
	}
	raw, err := aiCall(ctx, request)
	if err != nil {
		return "", err
	}
	return parseChatText(raw, request.Format, request.Provider.Key)
}
