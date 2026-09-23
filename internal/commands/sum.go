package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/robfig/cron/v3"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// sum 命令沿用 MiBox 的 sum 插件写的 JSON 结构（assets/sum/database.json），
// 所以 data/sum.json 可以直接复制过来。

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
	PushTarget   string `json:"pushTarget,omitempty"`
	AIProvider   string `json:"aiProvider,omitempty"`
	AIPrompt     string `json:"aiPrompt,omitempty"`
	UseSpoiler   bool   `json:"useSpoiler"`
	CreatedAt    string `json:"createdAt"`
	LastRunAt    string `json:"lastRunAt,omitempty"`
	LastResult   string `json:"lastResult,omitempty"`
	LastError    string `json:"lastError,omitempty"`
	Disabled     bool   `json:"disabled,omitempty"`
	Remark       string `json:"remark,omitempty"`
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

var sumIntervalPattern = regexp.MustCompile(`^(\d+)(m|h|d)$`)

func sumIntervalCron(interval string) (string, error) {
	match := sumIntervalPattern.FindStringSubmatch(strings.ToLower(interval))
	if match == nil {
		return "", fail("间隔格式示例：30m、2h、1d")
	}
	value, err := strconv.Atoi(match[1])
	if err != nil || value <= 0 {
		return "", fail("间隔必须为正整数")
	}
	switch match[2] {
	case "m":
		if value > 59 || 60%value != 0 {
			return "", fail("分钟间隔须为 1-59 且能整除 60")
		}
		return fmt.Sprintf("*/%d * * * *", value), nil
	case "h":
		if value > 23 || 24%value != 0 {
			return "", fail("小时间隔须为 1-23 且能整除 24")
		}
		return fmt.Sprintf("0 */%d * * *", value), nil
	}
	if value != 1 {
		return "", fail("当前按天间隔仅支持 1d")
	}
	return "0 0 * * *", nil
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
			if link := utf16Slice(message.Message, value.Offset, value.Length); link != "" {
				urls = append(urls, link)
			}
		}
	}
	return urls
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
	payloads, err := aiPayloads(raw)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, payload := range payloads {
		if gemini {
			root := payload
			if response := objectOf(payload["response"]); len(response) > 0 {
				root = response
			} else if data := objectOf(payload["data"]); len(data) > 0 {
				root = data
			}
			for _, part := range listOf(objectOf(objectOf(firstOf(listOf(root["candidates"])))["content"])["parts"]) {
				parts = append(parts, stringOf(objectOf(part)["text"]))
			}
			continue
		}
		choice := objectOf(firstOf(listOf(payload["choices"])))
		text := stringOf(objectOf(choice["delta"])["content"])
		if text == "" {
			text = stringOf(objectOf(choice["message"])["content"])
		}
		if text == "" {
			text = stringOf(choice["text"])
		}
		if text == "" {
			text = stringOf(payload["text"])
		}
		parts = append(parts, text)
	}
	text := strings.TrimSpace(strings.Join(parts, ""))
	if text == "" {
		return "", fail("AI 返回内容为空")
	}
	return text, nil
}

func sumCallAI(ctx context.Context, provider sumProvider, messages, prompt, reasoning, tier string, timeout time.Duration) (string, error) {
	if err := assertAllowedModel(provider.Model); err != nil {
		return "", fail("配置模型被项目策略禁止，请更换模型")
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
			headers["Authorization"], headers["User-Agent"] = "Bearer "+provider.APIKey, codexUserAgent
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
		return httpx.PostJSON(ctx, target, headers, data, timeout, 2<<20)
	}
	protocol := sumDetectProtocol(provider)
	result, err := request(protocol)
	if err != nil {
		return "", fail("AI 请求失败：" + httpx.Reason(err))
	}
	auto := provider.Type == "" || provider.Type == "auto" || provider.Type == "openai"
	if !result.OK() && auto && protocol != "chat" && (result.Status == 404 || result.Status == 400 && regexp.MustCompile(`(?i)unsupported_upstream|endpoint|not supported`).Match(result.Body)) {
		protocol = "chat"
		result, err = request(protocol)
		if err != nil {
			return "", fail("AI 请求失败：" + httpx.Reason(err))
		}
	}
	if !result.OK() {
		return "", failf("AI HTTP %d", result.Status)
	}
	if protocol == "anthropic" || protocol == "responses" {
		var data map[string]any
		if err := json.Unmarshal(result.Body, &data); err != nil {
			return "", fail("AI 返回无效 JSON")
		}
		if data["error"] != nil || data["status"] == "failed" {
			return "", fail("AI 提供商返回错误")
		}
		var parts []string
		if protocol == "anthropic" {
			for _, item := range listOf(data["content"]) {
				if objectOf(item)["type"] == "text" {
					parts = append(parts, stringOf(objectOf(item)["text"]))
				}
			}
		} else if text := stringOf(data["output_text"]); text != "" {
			parts = append(parts, text)
		} else {
			for _, item := range listOf(data["output"]) {
				for _, part := range listOf(objectOf(item)["content"]) {
					if objectOf(part)["type"] == "output_text" {
						parts = append(parts, stringOf(objectOf(part)["text"]))
					}
				}
			}
		}
		content := strings.TrimSpace(strings.Join(parts, "\n"))
		if content == "" {
			return "", fail("AI 返回内容为空")
		}
		return content, nil
	}
	return sumParseText(result.Body, protocol == "gemini")
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

func (s *sumService) readMessages(ctx context.Context, client *bot.Client, chatID string, beforeID, count int) ([]sumMessage, string, error) {
	peer, err := client.ResolveTarget(ctx, chatID)
	if err != nil {
		return nil, "", fail("无法定位群组，请先在该群发过消息或用 here")
	}
	target, _ := bot.PeerFromID(chatID)
	title, username := chatID, ""
	if channel, ok := target.(*tg.PeerChannel); ok {
		if info, known := client.Peers().Channel(channel.ChannelID); known {
			title, username = info.Title, info.Username
		}
	} else if target != nil {
		title = client.Peers().Title(target)
	}
	var rows []sumMessage
	offset := beforeID
	scanned, limit := 0, min(800, count*3)
	for len(rows) < count && scanned < limit {
		batch := min(100, limit-scanned)
		request := &tg.MessagesGetHistoryRequest{Peer: peer, OffsetID: offset, Limit: batch}
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
			body := content
			if body == "" {
				body = fileName
			}
			rows = append(rows, sumMessage{Text: "[" + sender + "] " + body, Content: content, Link: sumMessageLink(chatID, message.ID, username), URLs: sumEntityURLs(message), FileName: fileName})
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

func (s *sumService) summarize(ctx context.Context, client *bot.Client, chatID string, count int, providerName, prompt string, spoiler bool, beforeID int) (string, error) {
	db, err := s.read()
	if err != nil {
		return "", err
	}
	if providerName == "" {
		providerName = db.AIConfig.DefaultProvider
	}
	provider, ok := db.AIConfig.Providers[providerName]
	if providerName == "" || !ok {
		return "", fail("请先用 sum config add 添加 AI 配置并设置默认提供商")
	}
	if provider.APIKey == "" {
		return "", fail("默认 AI 配置缺少 API Key")
	}
	rows, title, err := s.readMessages(ctx, client, chatID, beforeID, count)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fail("未找到可总结的消息")
	}
	if prompt == "" {
		prompt = db.AIConfig.DefaultPrompt
	}
	if prompt == "" {
		prompt = sumDefaultPrompt
	}
	timeout := time.Duration(db.AIConfig.DefaultTimeout) * time.Millisecond
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	reasoning, tier := db.AIConfig.DefaultReasoningEffort, db.AIConfig.DefaultServiceTier
	if !containsString(aiReasoningValues, reasoning) {
		reasoning = "auto"
	}
	if !containsString(aiTierValues, tier) {
		tier = "auto"
	}
	output, err := sumCallAI(ctx, provider, sumFormatMessages(rows), prompt, reasoning, tier, timeout)
	if err != nil {
		return "", err
	}
	output = strings.TrimSpace(sumThinkPattern.ReplaceAllString(output, ""))
	if limit := db.AIConfig.MaxOutputLength; limit > 0 && utf16Len(output) > limit {
		output = truncateRunes(output, limit) + "\n\n内容已按配置长度截断。"
	}
	if spoiler && !strings.Contains(output, "<blockquote expandable>") {
		output = "<blockquote expandable>" + output + "</blockquote>"
	}
	return "📊 <b>群组总结</b>\n" + command.Escape(title) + " · " + time.Now().Format("2006-01-02 15:04") + "\n\n" + output, nil
}

func (s *sumService) push(ctx context.Context, client *bot.Client, task sumTask) error {
	html, err := s.summarize(ctx, client, task.ChatID, task.MessageCount, task.AIProvider, task.AIPrompt, task.UseSpoiler, 0)
	if err != nil {
		return err
	}
	db, err := s.read()
	if err != nil {
		return err
	}
	target := task.PushTarget
	if target == "" {
		target = db.DefaultPushTarget
	}
	peer, err := client.ResolveTarget(ctx, target)
	if err != nil {
		return err
	}
	for _, page := range command.HTMLPages(html, 3800) {
		if _, err := client.SendHTML(ctx, peer, page, bot.SendOptions{LinkPreview: db.AIConfig.LinkPreview}); err != nil {
			return err
		}
	}
	return nil
}

func (s *sumService) register(task sumTask) error {
	if task.Disabled {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[task.ID]; exists {
		return nil
	}
	id, err := s.cron.AddFunc(task.Cron, func() {
		s.mu.Lock()
		client := s.client
		if s.running[task.ID] || client == nil {
			s.mu.Unlock()
			return
		}
		s.running[task.ID] = true
		s.mu.Unlock()
		defer func() { s.mu.Lock(); delete(s.running, task.ID); s.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		err := s.push(ctx, client, task)
		_ = s.update(func(db *sumDB) error {
			for index := range db.Tasks {
				if db.Tasks[index].ID == task.ID {
					db.Tasks[index].LastRunAt = time.Now().UTC().Format(time.RFC3339)
					if err != nil {
						db.Tasks[index].LastError = "总结执行失败"
					} else {
						db.Tasks[index].LastResult, db.Tasks[index].LastError = "总结已发送", ""
					}
				}
			}
			return nil
		})
		if err != nil {
			s.a.Logger.Error("sum.scheduled_failed", slog.String("task", task.ID), slog.String("error", err.Error()))
		}
	})
	if err != nil {
		return fail("定时表达式无效")
	}
	s.entries[task.ID] = id
	return nil
}

func (s *sumService) unregister(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.entries[id]; ok {
		s.cron.Remove(entry)
		delete(s.entries, id)
	}
}

func sumHelp(prefix string) string {
	p := command.Escape(prefix)
	return "<b>群消息总结</b>\n\n• <code>" + p + "sum</code> - 总结当前群最近 100 条消息\n• <code>" + p + "sum 200</code> - 指定消息数量，范围 10-500\n• <code>" + p + "sum 100 --provider 名称</code> - 临时选择 AI 配置\n• <code>" + p +
		"sum add here 2h 100</code> - 定时总结当前群并推送到收藏夹\n• <code>" + p + "sum list</code> / <code>" + p + "sum run ID</code> / <code>" + p + "sum del ID</code>\n• <code>" + p + "sum disable ID</code> / <code>" + p +
		"sum enable ID</code>\n\n<b>AI 配置</b>\n• <code>" + p + "sum config list</code>\n• <code>" + p + "sum config add 名称 BaseURL API_KEY 模型 [auto|chat|responses|gemini|anthropic]</code>\n• <code>" + p + "sum config set default 名称</code>\n• <code>" + p +
		"sum config set 名称 model|url|key|type 值</code>\n• <code>" + p + "sum config set preview|spoiler on|off</code>\n• <code>" + p + "sum config set reasoning|service 值</code>\n• <code>" + p + "sum config set prompt 内容|reset|show</code>\n• <code>" + p + "sum config del 名称</code>"
}

// Sum 注册 .sum 及其定时任务。
func Sum(a *app.App) {
	service := &sumService{a: a, store: newStore(a, "sum.json", sumDefaults), cron: cron.New(), entries: map[string]cron.EntryID{}, running: map[string]bool{}}
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
				a.Logger.Error("sum.register_failed", slog.String("task", task.ID))
			}
		}
		service.cron.Start()
		<-ctx.Done()
		service.cron.Stop()
	})
	a.Registry.Register(&command.Command{Name: "sum", Description: "群消息即时与定时摘要", Usage: "[数量] | add | list | run | config ...", Help: sumHelp, Timeout: 10 * time.Minute,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			err := service.handle(ctx, inv)
			if err == nil || ctx.Err() != nil {
				return err
			}
			detail := chatHTMLError(err)
			if detail == "" {
				inv.Log.Error("sum.failed", "error", err.Error())
				detail = "摘要操作失败，请检查配置、权限和网络后重试"
			}
			return inv.Edit(ctx, "❌ "+detail)
		}})
}

func sumProviderView(provider sumProvider) string {
	kind := provider.Type
	if kind == "" {
		kind = "auto"
	}
	return command.Escape(provider.Name) + " · " + command.Code(provider.Model) + " · " + command.Escape(kind)
}

func (s *sumService) handle(ctx context.Context, inv *command.Invocation) error {
	sub := strings.ToLower(inv.Arg(0))
	switch sub {
	case "help", "h", "?":
		return inv.Edit(ctx, sumHelp(inv.Prefix))
	case "config":
		return s.config(ctx, inv)
	case "list":
		db, err := s.read()
		if err != nil {
			return err
		}
		var rows []string
		for _, task := range db.Tasks {
			state := "启用"
			if task.Disabled {
				state = "停用"
			}
			rows = append(rows, fmt.Sprintf("• %s · %s · %d 条 · %s", command.Code(task.ID), command.Escape(task.Interval), task.MessageCount, state))
		}
		body := strings.Join(rows, "\n")
		if body == "" {
			body = "暂无定时任务"
		}
		return inv.Edit(ctx, "<b>摘要任务</b>\n"+body)
	case "run", "del", "disable", "enable":
		id := inv.Arg(1)
		db, err := s.read()
		if err != nil {
			return err
		}
		var task *sumTask
		for index := range db.Tasks {
			if db.Tasks[index].ID == id {
				task = &db.Tasks[index]
			}
		}
		if task == nil {
			return fail("摘要任务不存在")
		}
		if sub == "run" {
			if err := inv.EditText(ctx, "📝 正在生成摘要..."); err != nil {
				return err
			}
			if err := s.push(ctx, inv.Client, *task); err != nil {
				return err
			}
			return inv.EditText(ctx, "✅ 摘要已推送")
		}
		if sub == "del" || sub == "disable" {
			s.unregister(id)
		}
		if err := s.update(func(db *sumDB) error {
			for index := range db.Tasks {
				if db.Tasks[index].ID != id {
					continue
				}
				if sub == "del" {
					db.Tasks = append(db.Tasks[:index], db.Tasks[index+1:]...)
				} else {
					db.Tasks[index].Disabled = sub == "disable"
				}
				break
			}
			return nil
		}); err != nil {
			return err
		}
		if sub == "enable" {
			enabled := *task
			enabled.Disabled = false
			if err := s.register(enabled); err != nil {
				return err
			}
		}
		return inv.EditText(ctx, "✅ 摘要任务已更新")
	case "add":
		target, interval := inv.Arg(1), inv.Arg(2)
		count := 100
		if inv.Arg(3) != "" {
			parsed, err := strconv.Atoi(inv.Arg(3))
			if err != nil {
				return fail("用法：sum add here|群组 2h 100")
			}
			count = parsed
		}
		if target == "" || count < 10 || count > 500 {
			return fail("用法：sum add here|群组 2h 100")
		}
		chatID := target
		if target == "here" {
			chatID = inv.Message.ChatID
		}
		spec, err := sumIntervalCron(interval)
		if err != nil {
			return err
		}
		db, err := s.read()
		if err != nil {
			return err
		}
		previous, _ := db.Seq.Int64()
		id := strconv.FormatInt(previous+1, 10)
		task := sumTask{ID: id, Cron: spec, ChatID: chatID, Interval: interval, MessageCount: count, PushTarget: "me", CreatedAt: time.Now().UTC().Format(time.RFC3339),
			AIProvider: db.AIConfig.DefaultProvider, UseSpoiler: db.AIConfig.DefaultSpoiler}
		if err := s.update(func(db *sumDB) error { db.Seq = json.Number(id); db.Tasks = append(db.Tasks, task); return nil }); err != nil {
			return err
		}
		if err := s.register(task); err != nil {
			_ = s.update(func(db *sumDB) error {
				for index := range db.Tasks {
					if db.Tasks[index].ID == id {
						db.Tasks = append(db.Tasks[:index], db.Tasks[index+1:]...)
						break
					}
				}
				db.Seq = json.Number(strconv.FormatInt(previous, 10))
				return nil
			})
			return err
		}
		return inv.EditText(ctx, "✅ 已创建摘要任务 "+id)
	}
	count := 100
	if sub != "" && regexp.MustCompile(`^\d+$`).MatchString(sub) {
		count, _ = strconv.Atoi(sub)
	}
	if count < 10 || count > 500 {
		return fail("消息数量范围为 10-500")
	}
	provider := ""
	for index, arg := range inv.Args {
		if arg == "--provider" && index+1 < len(inv.Args) {
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
	html, err := s.summarize(ctx, inv.Client, inv.Message.ChatID, count, provider, "", db.AIConfig.DefaultSpoiler, inv.Message.ID)
	if err != nil {
		return err
	}
	return sendPages(ctx, inv, command.HTMLPages(html, 3800))
}

// sumTypes 是服务商接口类型的可选值；auto 表示按 BaseURL 自动判断。
var sumTypes = []string{"auto", "chat", "responses", "gemini", "anthropic"}

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
	case "list":
		return s.configList(ctx, inv, db)
	case "add":
		err = s.configAdd(inv, name, property, rest)
	case "del":
		err = s.configDelete(db, name)
	case "set":
		replied, err = s.configSet(ctx, inv, db, name, property, rest)
	default:
		return fail("未知 config 子命令")
	}
	if err != nil || replied {
		return err
	}
	return inv.EditText(ctx, "✅ 摘要配置已更新")
}

// configList 列出所有服务商、默认用哪个，以及提示词和链接预览的状态。
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
	promptState := "自定义"
	if db.AIConfig.DefaultPrompt == "" || db.AIConfig.DefaultPrompt == sumDefaultPrompt {
		promptState = "内置"
	}
	return inv.Edit(ctx, "<b>摘要 AI 配置</b>\n"+body+"\n\n提示词："+promptState+"\n链接预览："+onOffText(db.AIConfig.LinkPreview))
}

// configAdd 添加一个服务商：名称 BaseURL API_KEY 模型 [类型]。第一个添加的自动成为默认。
// 命令里带着 API Key，只允许在收藏夹里发，免得密钥留在别的对话里。
func (s *sumService) configAdd(inv *command.Invocation, name, base string, rest []string) error {
	if !inv.Message.Saved {
		return fail("涉及 API Key 的配置命令只能在收藏夹使用")
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
		return fail("用法：sum config add 名称 BaseURL API_KEY 模型 [type]")
	}
	if parsed, err := url.Parse(base); err != nil || parsed.Host == "" {
		return fail("BaseURL 无效")
	}
	if err := assertAllowedModel(model); err != nil {
		return err
	}
	if !containsString(sumTypes, kind) {
		return fail("无效接口类型")
	}
	return s.update(func(db *sumDB) error {
		db.AIConfig.Providers[name] = sumProvider{Name: name, BaseURL: base, APIKey: key, Model: model, Type: kind}
		if db.AIConfig.DefaultProvider == "" {
			db.AIConfig.DefaultProvider = name
		}
		return nil
	})
}

// configDelete 删掉一个服务商；删的正好是默认的话，默认清空。
func (s *sumService) configDelete(db sumDB, name string) error {
	if _, ok := db.AIConfig.Providers[name]; name == "" || !ok {
		return fail("AI 配置不存在")
	}
	return s.update(func(db *sumDB) error {
		delete(db.AIConfig.Providers, name)
		if db.AIConfig.DefaultProvider == name {
			db.AIConfig.DefaultProvider = ""
		}
		return nil
	})
}

// configSet 改一项设置。第一个返回值表示已经自己回复过了（只有 prompt show 这样），
// 调用方就不要再回「已更新」。
func (s *sumService) configSet(ctx context.Context, inv *command.Invocation, db sumDB, name, property string, rest []string) (bool, error) {
	switch name {
	case "default":
		if _, ok := db.AIConfig.Providers[property]; !ok {
			return false, fail("AI 配置不存在")
		}
		return false, s.update(func(db *sumDB) error { db.AIConfig.DefaultProvider = property; return nil })
	case "preview", "spoiler":
		enabled, err := onOff(property)
		if err != nil {
			return false, err
		}
		return false, s.update(func(db *sumDB) error {
			if name == "preview" {
				db.AIConfig.LinkPreview = enabled
			} else {
				db.AIConfig.DefaultSpoiler = enabled
			}
			return nil
		})
	case "reasoning", "service":
		values := aiReasoningValues
		if name == "service" {
			values = aiTierValues
		}
		if !containsString(values, property) {
			return false, fail("无效选项")
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
		return false, fail("提示词不能为空")
	}
	return false, s.update(func(db *sumDB) error { db.AIConfig.DefaultPrompt = prompt; return nil })
}

// configProvider 改某个服务商的一个字段：model、url、key、type。改 key 同样只允许在收藏夹里。
func (s *sumService) configProvider(inv *command.Invocation, db sumDB, name, property, value string) error {
	provider, ok := db.AIConfig.Providers[name]
	if !ok || !containsString([]string{"model", "url", "key", "type"}, property) || value == "" {
		return fail("用法：sum config set 名称 model|url|key|type 值")
	}
	if property == "key" && !inv.Message.Saved {
		return fail("涉及 API Key 的配置命令只能在收藏夹使用")
	}
	switch property {
	case "model":
		if err := assertAllowedModel(value); err != nil {
			return err
		}
		provider.Model = value
	case "url":
		if parsed, err := url.Parse(value); err != nil || parsed.Host == "" {
			return fail("BaseURL 无效")
		}
		provider.BaseURL = value
	case "key":
		provider.APIKey = value
	default:
		if !containsString(sumTypes, value) {
			return fail("无效接口类型")
		}
		provider.Type = value
	}
	return s.update(func(db *sumDB) error { db.AIConfig.Providers[name] = provider; return nil })
}
