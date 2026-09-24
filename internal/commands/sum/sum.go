// Package sum 实现 .sum：群消息的即时摘要和定时摘要。
package sum

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/robfig/cron/v3"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/ai"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// sum 命令沿用 MiBox 的 sum 插件写的 JSON 结构（assets/sum/database.json），
// 所以 data/sum.json 可以直接复制过来。MiBox 新版把服务商迁进了 ai 插件
// （aiConfig.aiMigrated，任务的 aiProvider 改成 ai 的标签），这种数据走 ai 的
// 统一配置；sum 自己仍有服务商时优先用自己的。

const sumDefaultPrompt = "你是 Telegram 群聊摘要助手。根据以下聊天记录，只输出 Telegram HTML 格式的中文总结。\n\n允许使用 <b>、<code>、<a href=\"...\">、<blockquote expandable>；禁止使用 Markdown、#、**、```、[文字](链接)、裸 URL、<https://...>。聊天记录中每条消息末尾都有“来源”链接。每条摘要、资源、结论、互动、零散信息或时间线条目都必须附带最对应的 Telegram 原消息链接，格式为 <a href=\"Telegram消息链接\">来源</a>；不要编造链接。\n\n只记录聊天中明确出现的事实、反馈、决定和计划。只有存在明确完成反馈、验证结果或维护者确认时，才可使用“已确认”“已解决”“已完成”等表达；个人测试、成员讨论或推测使用“有人反馈”“初步判断”“可能”“尚待复测”“未见最终确认”等表述。不要把“计划支持”“准备测试”“正在修改”写成已经实现或可用。合并重复消息，忽略纯寒暄、表情、广告、机器人状态和无结论闲聊。\n\n总长度控制在 900-1600 个中文字符；重要讨论较多时可接近上限。信息应完整、可回溯，但不要逐条复述聊天记录。\n\n固定输出：\n<b>📌 本次摘要</b>\n用 2-3 句话概括本次聊天背景、关键结果和当前状态；末尾附 1-2 个 <a href=\"Telegram消息链接\">来源</a>。\n\n随后按实际内容选择下列栏目，不相关的栏目完全不要输出：\n<b>💬 主要话题</b>：日常交流、综合讨论、一般观点或群内共识。\n<b>🧩 技术与项目</b>：技术方案、配置、开发、排障、版本更新、命令和实现细节。\n<b>📰 资源分享</b>：重要外部链接、文件、工具、新闻或可复用资源。\n<b>👥 重要互动</b>：明确的求助、答复、邀请、提醒、分工、争议或值得关注的人际互动。\n<b>🗂 零散信息</b>：无法归入其他栏目但值得保留的版本、环境、数据、状态、背景或简短结论。\n<b>🕒 时间线梳理</b>：仅在同一轮聊天出现多个明确时间点，且时间顺序有助于理解事件进展时输出。\n\n不要输出“待处理事项”“行动项”“下一步”这类面向管理者的栏目；群成员未必负责跟进。若聊天中存在未解决问题、风险或后续计划，将其放入最相关的上述栏目，并使用“仍待确认”“尚待复测”“计划继续”等中性表述。\n\n每个栏目使用以下格式：\n<b>栏目标题</b>\n<blockquote expandable>• 要点：说明结论、必要背景、明确分歧、风险或计划 <a href=\"Telegram消息链接\">来源</a>\n• 要点：说明结论、必要背景、明确分歧、风险或计划 <a href=\"Telegram消息链接\">来源</a></blockquote>\n\n规则：\n1. 每个栏目 1-3 条；每条建议 35-90 个中文字符。内容多时优先压缩重复过程，保留结论、关键依据、数据、风险和计划。\n2. 技术内容较多时，可在 <b>🧩 技术与项目</b> 内使用 <b>1. 小标题</b> 分组；最多 3 个小标题，每个小标题只保留 1-2 条。\n3. 时间线每条使用“<code>HH:MM</code>：事件概述 <a href=\"Telegram消息链接\">来源</a>”；最多 4 条，只保留转折、决定、故障、修复或重要更新。\n4. 命令、模型名、插件名、配置名、版本号、错误码使用 <code>...</code>。\n5. 外部链接仅在确实影响后续操作时保留，格式为 <a href=\"完整URL\">名称</a>，并在同一条末尾保留 Telegram <a href=\"Telegram消息链接\">来源</a>。\n6. 不输出空栏目、“无”“暂无”“未发现”或处理过程。每个栏目之间空一行，只输出最终总结。"

type sumProvider struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
	Type    string `json:"type,omitempty"`
}

type sumAIConfig struct {
	Providers              map[string]sumProvider `json:"providers"`
	DefaultProvider        string                 `json:"default_provider,omitempty"`
	DefaultPrompt          string                 `json:"default_prompt,omitempty"`
	DefaultSpoiler         bool                   `json:"default_spoiler"`
	DefaultTimeout         int                    `json:"default_timeout,omitempty"`
	DefaultReasoningEffort string                 `json:"default_reasoning_effort,omitempty"`
	DefaultServiceTier     string                 `json:"default_service_tier,omitempty"`
	ReplyMode              bool                   `json:"reply_mode"`
	MaxOutputLength        int                    `json:"max_output_length"`
	LinkPreview            bool                   `json:"link_preview"`
	AIMigrated             bool                   `json:"aiMigrated,omitempty"`
}

type sumTask struct {
	ID           string `json:"id"`
	Cron         string `json:"cron"`
	ChatID       string `json:"chatId"`
	ChatDisplay  string `json:"chatDisplay,omitempty"`
	Interval     string `json:"interval"`
	MessageCount int    `json:"messageCount"`
	// TimeRange 大于 0 时只总结最近这么多小时内的消息（仍受 MessageCount 限制）。
	TimeRange  int    `json:"timeRange,omitempty"`
	PushTarget string `json:"pushTarget,omitempty"`
	AIProvider string `json:"aiProvider,omitempty"`
	AIPrompt   string `json:"aiPrompt,omitempty"`
	UseSpoiler bool   `json:"useSpoiler"`
	CreatedAt  string `json:"createdAt"`
	LastRunAt  string `json:"lastRunAt,omitempty"`
	LastResult string `json:"lastResult,omitempty"`
	LastError  string `json:"lastError,omitempty"`
	Disabled   bool   `json:"disabled,omitempty"`
	Remark     string `json:"remark,omitempty"`
}

type sumDB struct {
	Seq               json.Number `json:"seq"`
	Tasks             []sumTask   `json:"tasks"`
	AIConfig          sumAIConfig `json:"aiConfig"`
	DefaultPushTarget string      `json:"defaultPushTarget,omitempty"`
}

func sumDefaults() sumDB {
	return sumDB{Seq: "0", Tasks: []sumTask{}, AIConfig: sumAIConfig{Providers: map[string]sumProvider{}, DefaultPrompt: sumDefaultPrompt,
		DefaultTimeout: 60000, DefaultReasoningEffort: "auto", DefaultServiceTier: "auto"}}
}

func (db *sumDB) normalize() {
	if db.AIConfig.Providers == nil {
		db.AIConfig.Providers = map[string]sumProvider{}
	}
	if db.Tasks == nil {
		db.Tasks = []sumTask{}
	}
	if db.Seq == "" {
		db.Seq = "0"
	}
}

// sumTimeout 是一次摘要请求的超时。MiBox 存的是毫秒，但早期版本存过 10-300 的秒数，
// 读取时按秒换算，与 MiBox 的 normalizeStoredTimeout 一致。
func sumTimeout(db sumDB) time.Duration {
	value := db.AIConfig.DefaultTimeout
	if value >= 10 && value <= 300 {
		value *= 1000
	}
	if value <= 0 {
		value = 60000
	}
	return time.Duration(value) * time.Millisecond
}

type sumMessage struct {
	Text     string
	Content  string
	Link     string
	URLs     []string
	FileName string
}

func sumMessageLink(chatID string, messageID int, username string) string {
	if username != "" {
		return fmt.Sprintf("https://t.me/%s/%d", username, messageID)
	}
	return fmt.Sprintf("https://t.me/c/%s/%d", strings.TrimPrefix(chatID, "-100"), messageID)
}

func sumFileName(media tg.MessageMediaClass) string {
	switch value := media.(type) {
	case *tg.MessageMediaDocument:
		document, ok := value.Document.(*tg.Document)
		if !ok {
			return ""
		}
		if document.MimeType == "application/x-tgsticker" || document.MimeType == "video/webm" {
			return ""
		}
		name := ""
		for _, attribute := range document.Attributes {
			switch attr := attribute.(type) {
			case *tg.DocumentAttributeSticker, *tg.DocumentAttributeCustomEmoji:
				return ""
			case *tg.DocumentAttributeFilename:
				name = attr.FileName
			}
		}
		if name != "" {
			return name
		}
		if document.MimeType != "" {
			return "[" + document.MimeType + "]"
		}
	case *tg.MessageMediaPhoto:
		return "[图片]"
	}
	return ""
}

func sumEntityURLs(message *tg.Message) []string {
	var urls []string
	for _, entity := range message.Entities {
		switch value := entity.(type) {
		case *tg.MessageEntityTextURL:
			urls = append(urls, value.URL)
		case *tg.MessageEntityURL:
			if link := kit.UTF16Slice(message.Message, value.Offset, value.Length); link != "" {
				urls = append(urls, link)
			}
		}
	}
	return urls
}

// sumMessageText 是发给 AI 的一行：[时间] 发送者: 正文，带附件时在后面注明文件名。
// 时间按本机时区显示，与 MiBox 一致，内置提示词里的 HH:MM 时间线就靠它。
func sumMessageText(date int, sender, content, fileName string) string {
	text := content
	if fileName != "" {
		if text != "" {
			text += " [文件: " + fileName + "]"
		} else {
			text = "[文件: " + fileName + "]"
		}
	}
	return "[" + time.Unix(int64(date), 0).In(time.Local).Format("2006-01-02 15:04") + "] " + sender + ": " + text
}

var sumURLPattern = regexp.MustCompile(`https?://[^\s\]）】>]+`)

func sumFormatMessages(rows []sumMessage) string {
	var lines []string
	for _, row := range rows {
		lines = append(lines, row.Text+" [来源]("+row.Link+")")
	}
	type mapping struct{ url, link string }
	var urls []mapping
	seen := map[string]bool{}
	var files []mapping
	for _, row := range rows {
		for _, link := range append(append([]string{}, row.URLs...), sumURLPattern.FindAllString(row.Content, -1)...) {
			if !seen[link] {
				seen[link] = true
				urls = append(urls, mapping{link, row.Link})
			}
		}
		if row.FileName != "" {
			files = append(files, mapping{row.FileName, row.Link})
		}
	}
	result := strings.Join(lines, "\n")
	if len(urls) > 0 {
		result += "\n\n--- 消息中包含的外部链接（资源URL - 来源消息链接）---\n"
		for _, item := range urls {
			result += item.url + " - [查看原消息](" + item.link + ")\n"
		}
	}
	if len(files) > 0 {
		result += "\n\n--- 消息中包含的附件（文件名 - 来源消息链接）---\n"
		for _, item := range files {
			result += item.url + " - [查看原消息](" + item.link + ")\n"
		}
	}
	return result
}

func sumNormalizedBase(raw string) string {
	trimmed := strings.TrimRight(raw, "/")
	return regexp.MustCompile(`(?i)/v1(?:beta)?$`).ReplaceAllString(trimmed, "")
}

func sumDetectProtocol(provider sumProvider) string {
	if provider.Type != "" && provider.Type != "auto" && provider.Type != "openai" {
		return provider.Type
	}
	model := strings.ToLower(provider.Model)
	switch {
	case strings.HasPrefix(model, "gemini"):
		return "gemini"
	case strings.HasPrefix(model, "claude"):
		return "anthropic"
	case regexp.MustCompile(`^(gpt-[5-9]|o[1-9])`).MatchString(model):
		return "responses"
	}
	return "chat"
}

func sumParseText(raw []byte, gemini bool) (string, error) {
	payloads, err := ai.Payloads(raw)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, payload := range payloads {
		if gemini {
			root := payload
			if response := ai.ObjectOf(payload["response"]); len(response) > 0 {
				root = response
			} else if data := ai.ObjectOf(payload["data"]); len(data) > 0 {
				root = data
			}
			for _, part := range ai.ListOf(ai.ObjectOf(ai.ObjectOf(ai.FirstOf(ai.ListOf(root["candidates"])))["content"])["parts"]) {
				parts = append(parts, ai.StringOf(ai.ObjectOf(part)["text"]))
			}
			continue
		}
		choice := ai.ObjectOf(ai.FirstOf(ai.ListOf(payload["choices"])))
		text := ai.StringOf(ai.ObjectOf(choice["delta"])["content"])
		if text == "" {
			text = ai.StringOf(ai.ObjectOf(choice["message"])["content"])
		}
		if text == "" {
			text = ai.StringOf(choice["text"])
		}
		if text == "" {
			text = ai.StringOf(payload["text"])
		}
		parts = append(parts, text)
	}
	text := strings.TrimSpace(strings.Join(parts, ""))
	if text == "" {
		return "", kit.Fail("AI 返回内容为空")
	}
	return text, nil
}

// sumHTTPFailure 是 sum 自己的服务商返回非 2xx 时的说明：带上接口的错误说明，密钥打码。
func sumHTTPFailure(status int, body []byte, key string) error {
	return kit.Fail("AI 调用失败：" + ai.HTTPFailure(status, body, key))
}

func sumCallAI(ctx context.Context, provider sumProvider, messages, prompt, reasoning, tier string, timeout time.Duration) (string, error) {
	if err := ai.AssertAllowedModel(provider.Model); err != nil {
		return "", kit.Fail("配置模型被项目策略禁止，请更换模型")
	}
	input := prompt + "\n\n" + messages
	base := sumNormalizedBase(provider.BaseURL)
	request := func(protocol string) (httpx.Response, error) {
		headers := map[string]string{}
		var target string
		var data map[string]any
		switch protocol {
		case "gemini":
			target = base + "/v1beta/models/" + url.PathEscape(provider.Model) + ":generateContent?key=" + url.QueryEscape(provider.APIKey)
			data = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": input}}}}}
		case "anthropic":
			target = base + "/v1/messages"
			headers["x-api-key"], headers["anthropic-version"] = provider.APIKey, "2023-06-01"
			data = map[string]any{"model": provider.Model, "max_tokens": 2000, "messages": []any{map[string]any{"role": "user", "content": input}}}
		default:
			headers["Authorization"], headers["User-Agent"] = "Bearer "+provider.APIKey, ai.CodexUserAgent
			if protocol == "responses" {
				target = base + "/v1/responses"
				data = map[string]any{"model": provider.Model, "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": input}}}}, "max_output_tokens": 2000, "store": false}
				if reasoning != "auto" {
					data["reasoning"] = map[string]any{"effort": reasoning}
				}
			} else {
				target = base + "/v1/chat/completions"
				data = map[string]any{"model": provider.Model, "messages": []any{map[string]any{"role": "user", "content": input}}, "max_tokens": 2000}
				if reasoning != "auto" {
					data["reasoning_effort"] = reasoning
				}
			}
			if tier != "auto" {
				data["service_tier"] = tier
			}
		}
		return httpx.PostJSON(ctx, target, headers, data, timeout, ai.ResponseLimit)
	}
	protocol := sumDetectProtocol(provider)
	result, err := request(protocol)
	if err != nil {
		return "", kit.Fail("AI 请求失败：" + httpx.Reason(err))
	}
	auto := provider.Type == "" || provider.Type == "auto" || provider.Type == "openai"
	if !result.OK() && auto && protocol != "chat" && (result.Status == 404 || result.Status == 400 && regexp.MustCompile(`(?i)unsupported_upstream|endpoint|not supported`).Match(result.Body)) {
		protocol = "chat"
		result, err = request(protocol)
		if err != nil {
			return "", kit.Fail("AI 请求失败：" + httpx.Reason(err))
		}
	}
	if !result.OK() {
		return "", sumHTTPFailure(result.Status, result.Body, provider.APIKey)
	}
	if protocol == "anthropic" || protocol == "responses" {
		var data map[string]any
		if err := json.Unmarshal(result.Body, &data); err != nil {
			return "", kit.Fail("AI 返回无效 JSON")
		}
		if data["error"] != nil || data["status"] == "failed" {
			if detail := ai.ErrorDetail(result.Body, provider.APIKey); detail != "" {
				return "", kit.Fail("AI 提供商返回错误：" + detail)
			}
			return "", kit.Fail("AI 提供商返回错误")
		}
		var parts []string
		if protocol == "anthropic" {
			for _, item := range ai.ListOf(data["content"]) {
				if ai.ObjectOf(item)["type"] == "text" {
					parts = append(parts, ai.StringOf(ai.ObjectOf(item)["text"]))
				}
			}
		} else if text := ai.StringOf(data["output_text"]); text != "" {
			parts = append(parts, text)
		} else {
			for _, item := range ai.ListOf(data["output"]) {
				for _, part := range ai.ListOf(ai.ObjectOf(item)["content"]) {
					if ai.ObjectOf(part)["type"] == "output_text" {
						parts = append(parts, ai.StringOf(ai.ObjectOf(part)["text"]))
					}
				}
			}
		}
		content := strings.TrimSpace(strings.Join(parts, "\n"))
		if content == "" {
			return "", kit.Fail("AI 返回内容为空")
		}
		return content, nil
	}
	return sumParseText(result.Body, protocol == "gemini")
}

// sumBackend 是一次摘要实际用的模型：sum 自己的服务商，或 ai 统一配置里的某个标签
// （aiTag 为空表示 ai 当前的聊天选择）。
type sumBackend struct {
	own   *sumProvider
	aiTag string
	name  string
}

// sumResolveBackend 决定用哪个模型。requested 是任务或 --provider 指定的名称，
// 为空时用 sum 的默认服务商。sum 自己有这个服务商且配了 Key 就用它；否则交给
// ai 的统一配置：名称是 ai 的标签就用那个标签，没指定名称时用 ai 当前的聊天选择。
// 这样 MiBox 迁移过的数据（服务商已挪进 ai、任务存的是 ai 标签）照样能用。
func sumResolveBackend(db sumDB, requested string, aiAvailable bool, aiHas func(string) bool) (sumBackend, error) {
	name := requested
	if name == "" {
		name = db.AIConfig.DefaultProvider
	}
	provider, own := db.AIConfig.Providers[name]
	if name != "" && own && strings.TrimSpace(provider.APIKey) != "" {
		return sumBackend{own: &provider, name: name}, nil
	}
	if aiAvailable {
		if name != "" && aiHas(name) {
			return sumBackend{aiTag: name, name: name}, nil
		}
		if requested == "" {
			return sumBackend{name: ""}, nil
		}
		return sumBackend{}, kit.Fail("未找到 AI 配置：" + requested)
	}
	if name != "" && own {
		return sumBackend{}, kit.Fail("AI 配置 " + name + " 缺少 API Key")
	}
	if requested != "" {
		return sumBackend{}, kit.Fail("未找到 AI 配置：" + requested)
	}
	return sumBackend{}, kit.Fail("请先用 sum config add 添加 AI 配置，或用 ai config add 和 ai model chat 配置聊天模型")
}

func (s *sumService) backend(db sumDB, requested string) (sumBackend, error) {
	return sumResolveBackend(db, requested, ai.Available(), ai.HasProvider)
}

// providerKnown 判断名称是 sum 自己的服务商或 ai 的标签。
func providerKnown(db sumDB, name string) bool {
	if _, ok := db.AIConfig.Providers[name]; ok {
		return true
	}
	return ai.Available() && ai.HasProvider(name)
}

func (b sumBackend) call(ctx context.Context, db sumDB, messages, prompt string) (string, error) {
	timeout := sumTimeout(db)
	if b.own != nil {
		reasoning, tier := db.AIConfig.DefaultReasoningEffort, db.AIConfig.DefaultServiceTier
		if !slices.Contains(ai.ReasoningValues, reasoning) {
			reasoning = "auto"
		}
		if !slices.Contains(ai.TierValues, tier) {
			tier = "auto"
		}
		return sumCallAI(ctx, *b.own, messages, prompt, reasoning, tier, timeout)
	}
	return ai.Chat(ctx, ai.ChatRequest{Tag: b.aiTag, Text: messages, SystemPrompt: prompt, MaxOutputTokens: 2000, FallbackToChat: true, MinTimeout: timeout})
}

type sumService struct {
	a       *app.App
	store   *store.Store[sumDB]
	cron    *cron.Cron
	mu      sync.Mutex
	entries map[string]cron.EntryID
	running map[string]bool
	client  *bot.Client
}

func (s *sumService) read() (sumDB, error) {
	db, err := s.store.Read()
	db.normalize()
	return db, err
}

func (s *sumService) update(mutate func(db *sumDB) error) error {
	return s.store.Update(func(db *sumDB) error { db.normalize(); return mutate(db) })
}

// peerOfInput 把 input peer 转回 peer，InputPeerSelf 换成本账号。
func peerOfInput(input tg.InputPeerClass, selfID int64) tg.PeerClass {
	switch value := input.(type) {
	case *tg.InputPeerChannel:
		return &tg.PeerChannel{ChannelID: value.ChannelID}
	case *tg.InputPeerChat:
		return &tg.PeerChat{ChatID: value.ChatID}
	case *tg.InputPeerUser:
		return &tg.PeerUser{UserID: value.UserID}
	case *tg.InputPeerSelf:
		return &tg.PeerUser{UserID: selfID}
	}
	return nil
}

// chatTitle 返回对话的标题和公开用户名（频道和超级群才有）。
func chatTitle(client *bot.Client, peer tg.PeerClass) (string, string) {
	if channel, ok := peer.(*tg.PeerChannel); ok {
		if info, known := client.Peers().Channel(channel.ChannelID); known {
			return info.Title, info.Username
		}
	}
	return client.Peers().Title(peer), ""
}

// readMessages 从 beforeID 之前往回读，最多 count 条有内容的消息；timeRange 大于 0 时
// 读到那么多小时以前就停。返回按时间正序排列的消息和对话标题。
func (s *sumService) readMessages(ctx context.Context, client *bot.Client, chatID string, beforeID, count, timeRange int) ([]sumMessage, string, error) {
	input, err := client.ResolveTarget(ctx, chatID)
	if err != nil {
		return nil, "", kit.Fail("无法定位群组，请先在该群发过消息或用 here")
	}
	peer := peerOfInput(input, client.SelfID())
	title, username := chatTitle(client, peer)
	if title == "" {
		title = chatID
	}
	linkID := bot.PeerID(peer)
	var cutoff int
	if timeRange > 0 {
		cutoff = int(time.Now().Unix()) - timeRange*3600
	}
	var rows []sumMessage
	offset := beforeID
	scanned, limit := 0, min(800, count*3)
	done := false
	for !done && len(rows) < count && scanned < limit {
		batch := min(100, limit-scanned)
		request := &tg.MessagesGetHistoryRequest{Peer: input, OffsetID: offset, Limit: batch}
		result, err := client.API().MessagesGetHistory(ctx, request)
		if err != nil {
			return nil, "", err
		}
		messages, _ := client.Unpack(result)
		if len(messages) == 0 {
			break
		}
		for _, item := range messages {
			scanned++
			message, ok := item.(*tg.Message)
			if !ok {
				continue
			}
			if message.ID < offset || offset == 0 {
				offset = message.ID
			}
			if beforeID > 0 && message.ID >= beforeID {
				continue
			}
			if cutoff > 0 && message.Date < cutoff {
				done = true
				break
			}
			content := strings.TrimSpace(message.Message)
			fileName := sumFileName(message.Media)
			if content == "" && fileName == "" {
				continue
			}
			sender := "未知发送者"
			if from, ok := message.GetFromID(); ok {
				sender = client.Peers().Title(from)
			} else if message.Post {
				sender = title
			}
			rows = append(rows, sumMessage{Text: sumMessageText(message.Date, sender, content, fileName), Content: content,
				Link: sumMessageLink(linkID, message.ID, username), URLs: sumEntityURLs(message), FileName: fileName})
			if len(rows) >= count {
				break
			}
		}
		if len(messages) < batch {
			break
		}
	}
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		rows[left], rows[right] = rows[right], rows[left]
	}
	return rows, title, nil
}

var sumThinkPattern = regexp.MustCompile(`(?is)<thinking>.*?</thinking>|<think>.*?</think>`)

// sumJob 是一次摘要的参数。
type sumJob struct {
	chatID    string
	count     int
	timeRange int
	provider  string
	prompt    string
	spoiler   bool
	beforeID  int
}

func (s *sumService) summarize(ctx context.Context, client *bot.Client, job sumJob) (string, error) {
	db, err := s.read()
	if err != nil {
		return "", err
	}
	backend, err := s.backend(db, job.provider)
	if err != nil {
		return "", err
	}
	rows, title, err := s.readMessages(ctx, client, job.chatID, job.beforeID, job.count, job.timeRange)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", kit.Fail("未找到可总结的消息")
	}
	prompt := job.prompt
	if prompt == "" {
		prompt = db.AIConfig.DefaultPrompt
	}
	if prompt == "" {
		prompt = sumDefaultPrompt
	}
	output, err := backend.call(ctx, db, sumFormatMessages(rows), prompt)
	if err != nil {
		return "", err
	}
	output = strings.TrimSpace(sumThinkPattern.ReplaceAllString(output, ""))
	if limit := db.AIConfig.MaxOutputLength; limit > 0 && kit.UTF16Len(output) > limit {
		output = kit.TruncateRunes(output, limit) + "\n\n内容已按配置长度截断。"
	}
	if job.spoiler && !strings.Contains(output, "<blockquote expandable>") {
		output = "<blockquote expandable>" + output + "</blockquote>"
	}
	return "📊 <b>群组总结</b>\n" + command.Escape(title) + " · " + time.Now().Format("2006-01-02 15:04") + "\n\n" + output, nil
}

// pushTarget 是任务的推送目标：任务自己的，否则默认推送目标，都没有就是收藏夹。
func pushTarget(db sumDB, task sumTask) string {
	switch {
	case task.PushTarget != "":
		return task.PushTarget
	case db.DefaultPushTarget != "":
		return db.DefaultPushTarget
	}
	return "me"
}

func (s *sumService) push(ctx context.Context, client *bot.Client, task sumTask) error {
	html, err := s.summarize(ctx, client, sumJob{chatID: task.ChatID, count: task.MessageCount, timeRange: task.TimeRange,
		provider: task.AIProvider, prompt: task.AIPrompt, spoiler: task.UseSpoiler})
	if err != nil {
		return err
	}
	db, err := s.read()
	if err != nil {
		return err
	}
	peer, err := client.ResolveTarget(ctx, pushTarget(db, task))
	if err != nil {
		return kit.Fail("无法定位推送目标：" + pushTarget(db, task))
	}
	for _, page := range command.HTMLPages(html, 3800) {
		if _, err := client.SendHTML(ctx, peer, page, bot.SendOptions{LinkPreview: db.AIConfig.LinkPreview}); err != nil {
			return err
		}
	}
	return nil
}

// sumErrorText 是记进任务 lastError 的错误说明：给用户看的错误原样记，其余只记简短描述。
func sumErrorText(err error) string {
	if text, ok := kit.IsUserError(err); ok {
		return text
	}
	return command.Brief(err)
}

func sumHelp(prefix string) string {
	p := command.Escape(prefix)
	code := func(args string) string { return "<code>" + p + "sum" + command.Escape(args) + "</code>" }
	return "<b>群消息总结</b>\n\n• " + code("") + " - 总结当前群最近 100 条消息\n• " + code(" 200") + " - 指定消息数量，范围 10-500\n• " + code(" 100 --provider 名称") + " - 临时选择 AI 配置（sum 的配置名或 ai 的标签）\n\n" +
		"<b>定时总结</b>\n• " + code(" add here 2h 100") + " - 定时总结当前群，推送到默认目标（默认收藏夹）\n• " + code(" add @群组 \"0 9,21 * * *\" --time 12 --provider 名称 --spoiler 备注") + "\n" +
		"  群组可以是 here、数字 ID、@用户名、t.me 链接或邀请链接；间隔为 30m、2h、1d，或五、六字段 Cron（六字段第一位是秒）\n" +
		"• " + code(" list") + " / " + code(" run ID") + " / " + code(" del ID") + " / " + code(" disable ID") + " / " + code(" enable ID") + "\n" +
		"• " + code(" edit ID spoiler on|off") + " / " + code(" edit ID provider [名称]") + " / " + code(" edit ID prompt [内容]") + "\n" +
		"• " + code(" reorder") + " - 按当前顺序重新编号\n• " + code(" debug [数量]") + " - 预览发给 AI 的文本\n\n" +
		"<b>AI 配置</b>\n• " + code(" config list") + "\n• " + code(" config add 名称 BaseURL API_KEY 模型 [auto|chat|responses|gemini|anthropic]") + "\n• " + code(" config set default 名称") + "\n• " +
		code(" config set 名称 model|url|key|type 值") + "\n• " + code(" config set preview|spoiler|reply on|off") + "\n• " + code(" config set push 目标") + "\n• " + code(" config set timeout 秒数") + " / " + code(" config set maxoutput 字符数") + "\n• " +
		code(" config set reasoning|service 值") + "\n• " + code(" config set prompt 内容|reset|show") + "\n• " + code(" config del 名称") + "\n\n没有 sum 自己的 AI 配置时，使用 ai 命令的聊天模型。"
}

// Register 注册 .sum 及其定时任务。
func Register(a *app.App) {
	service := &sumService{a: a, store: kit.NewStore(a, "sum.json", sumDefaults), cron: cron.New(cron.WithParser(sumCronParser)),
		entries: map[string]cron.EntryID{}, running: map[string]bool{}}
	a.Registry.AddJob(func(ctx context.Context, client *bot.Client) {
		service.mu.Lock()
		service.client = client
		service.mu.Unlock()
		db, err := service.read()
		if err != nil {
			a.Logger.Error("sum.load_failed", slog.String("error", err.Error()))
			return
		}
		for _, task := range db.Tasks {
			if err := service.register(task); err != nil {
				a.Logger.Error("sum.register_failed", slog.String("task", task.ID), slog.String("cron", task.Cron), slog.String("error", err.Error()))
			}
		}
		service.cron.Start()
		<-ctx.Done()
		service.cron.Stop()
	})
	a.Registry.Register(&command.Command{Name: "sum", Description: "群消息即时与定时摘要", Usage: "[数量] | add | list | run | edit | config ...", Help: sumHelp, Timeout: 10 * time.Minute,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			err := service.handle(ctx, inv)
			if err == nil || ctx.Err() != nil {
				return err
			}
			detail := kit.ChatHTMLError(err)
			if detail == "" {
				inv.Log.Error("sum.failed", "error", err.Error())
				detail = "摘要操作失败，请检查配置、权限和网络后重试"
			}
			return inv.Edit(ctx, "❌ "+detail)
		}})
}

func (s *sumService) handle(ctx context.Context, inv *command.Invocation) error {
	sub := strings.ToLower(inv.Arg(0))
	switch sub {
	case "help", "h", "?":
		return inv.Edit(ctx, sumHelp(inv.Prefix))
	case "config":
		return s.config(ctx, inv)
	case "list", "ls":
		return s.list(ctx, inv)
	case "run", "now":
		return s.runNow(ctx, inv)
	case "del", "rm", "disable", "enable":
		return s.toggle(ctx, inv, sub)
	case "add":
		return s.add(ctx, inv)
	case "edit":
		return s.edit(ctx, inv)
	case "reorder", "sort":
		return s.reorder(ctx, inv)
	case "debug":
		return s.debug(ctx, inv)
	}
	return s.instant(ctx, inv, sub)
}

// instant 立即总结当前群：.sum [数量] [--provider 名称]。
func (s *sumService) instant(ctx context.Context, inv *command.Invocation, sub string) error {
	count := 100
	if sub != "" && regexp.MustCompile(`^\d+$`).MatchString(sub) {
		count, _ = strconv.Atoi(sub)
	}
	if count < 10 || count > 500 {
		return kit.Fail("消息数量范围为 10-500")
	}
	provider := ""
	for index, arg := range inv.Args {
		if (arg == "--provider" || arg == "-p") && index+1 < len(inv.Args) {
			provider = inv.Args[index+1]
		}
	}
	if err := inv.EditText(ctx, "📝 正在生成群聊摘要..."); err != nil {
		return err
	}
	db, err := s.read()
	if err != nil {
		return err
	}
	html, err := s.summarize(ctx, inv.Client, sumJob{chatID: inv.Message.ChatID, count: count, provider: provider,
		spoiler: db.AIConfig.DefaultSpoiler, beforeID: inv.Message.ID})
	if err != nil {
		return err
	}
	pages := command.HTMLPages(html, 3800)
	if !db.AIConfig.ReplyMode {
		return kit.SendPages(ctx, inv, pages)
	}
	// 回复模式：摘要作为新消息发出（回复命令所回复的那条），再删掉命令消息，
	// 这样耗时较长的摘要不会被新消息顶到上面去。
	peer, err := inv.Client.InputPeer(inv.Message.Peer)
	if err != nil {
		return err
	}
	for _, page := range pages {
		if _, err := inv.Client.SendHTML(ctx, peer, page, bot.SendOptions{ReplyTo: inv.Message.ReplyToID, LinkPreview: db.AIConfig.LinkPreview}); err != nil {
			return err
		}
	}
	if err := inv.Client.DeleteMessage(ctx, inv.Message); err != nil {
		inv.Log.Warn("sum.delete_command_failed", "error", err.Error())
	}
	return nil
}

// debug 预览发给 AI 的文本（最后 2000 个字符）。
func (s *sumService) debug(ctx context.Context, inv *command.Invocation) error {
	count := 50
	if value, err := strconv.Atoi(inv.Arg(1)); err == nil && value > 0 {
		count = kit.Clamp(value, 1, 500)
	}
	if err := inv.EditText(ctx, "⏳ 正在获取消息..."); err != nil {
		return err
	}
	rows, _, err := s.readMessages(ctx, inv.Client, inv.Message.ChatID, inv.Message.ID, count, 0)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return kit.Fail("未找到消息")
	}
	preview := sumFormatMessages(rows)
	if runes := []rune(preview); len(runes) > 2000 {
		preview = "...(前面省略)...\n\n" + string(runes[len(runes)-2000:])
	}
	return kit.SendPages(ctx, inv, command.HTMLPages("📋 发送给 AI 的文本预览（最后2000字符）：\n\n"+command.Code(preview), 3800))
}
