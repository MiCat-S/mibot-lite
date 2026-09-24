package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

// answerPageLimit 是每页回答可见字符的上限，给续页标签和署名留出余量，
// 总长仍在 Telegram 的 4096 以内。
const answerPageLimit = 3500

type aiSource struct{ URL, Title string }

// sourcesHTML 列出搜索来源，最多 8 条。
func sourcesHTML(sources []aiSource) string {
	if len(sources) == 0 {
		return ""
	}
	var rows []string
	for index, item := range sources {
		if index >= 8 {
			break
		}
		title := item.Title
		if title == "" {
			title = item.URL
		}
		rows = append(rows, fmt.Sprintf("%d. <a href=\"%s\">%s</a>", index+1, command.Escape(item.URL), command.Escape(title)))
	}
	return "\n\n<b>🔗 Sources</b>\n" + strings.Join(rows, "\n")
}

// telegraphLinkHTML 是回答太长、改发 Telegraph 时放在 A: 下面的内容。
func telegraphLinkHTML(link string) string {
	return "📰内容比较长，Telegraph 观感更好喔:\n🔗 <a href=\"" + command.Escape(link) + "\">点我阅读内容</a>"
}

// answerPages 按 MiBox 的版式排出回答：Q: 问题、A: 回答，开启折叠时两段各自包进
// 可展开的引用；超长时分页，续页带「续 (i/n)」标签，最后一页署上服务商标签。
func answerPages(question, answerHTML, tag string, collapse bool) []string {
	wrap := func(html string) string {
		if collapse && strings.TrimSpace(html) != "" {
			return "<blockquote expandable>" + html + "</blockquote>"
		}
		return html
	}
	separator := "\n\n"
	if collapse {
		separator = "\n"
	}
	source := "Q:\n" + wrap(command.Escape(question)) + separator + "A:\n" + wrap(answerHTML)
	pages := htmlPages(source, answerPageLimit)
	for index := range pages {
		if index > 0 {
			pages[index] = fmt.Sprintf("📋 <b>续 (%d/%d):</b>\n\n", index, len(pages)-1) + pages[index]
		}
	}
	if tag != "" {
		pages[len(pages)-1] += "\n<i>🍀Powered by " + command.Escape(tag) + "</i>"
	}
	return pages
}

// replyAnchor 是回答要回复的消息：命令回复了同一对话里的某条消息就回复那条，
// 在论坛话题里就回复话题，否则不回复。
func replyAnchor(command, reply *bot.Message) int {
	switch {
	case reply != nil && reply.ChatID == command.ChatID:
		return reply.ID
	case command.TopicID != 0:
		return command.TopicID
	}
	return 0
}

// deliverAnswer 和 MiBox 一样把回答作为新消息发出：第一页回复 anchor，续页回复第一页，
// 发完删掉命令消息（属于相册时连同整组）。不编辑命令消息，是因为命令可能是带图片的
// 说明文字，说明文字的长度上限远小于普通消息。
func deliverAnswer(ctx context.Context, inv *command.Invocation, pages []string, anchor int) error {
	peer, err := inv.Client.InputPeer(inv.Message.Peer)
	if err != nil {
		return err
	}
	first := 0
	for index, page := range pages {
		if err := ctx.Err(); err != nil {
			return err
		}
		target := anchor
		if index > 0 && first != 0 {
			target = first
		}
		id, err := inv.Client.SendHTML(ctx, peer, page, bot.SendOptions{ReplyTo: target})
		if err != nil {
			return err
		}
		if index == 0 {
			first = id
		}
	}
	ids := []int{inv.Message.ID}
	if inv.Message.Raw != nil {
		if grouped, ok := inv.Message.Raw.GetGroupedID(); ok && grouped != 0 {
			ids = ids[:0]
			for _, member := range albumMembers(ctx, inv.Client, inv.Message, grouped) {
				ids = append(ids, member.ID)
			}
		}
	}
	if err := inv.Client.Delete(ctx, peer, ids); err != nil {
		inv.Log.Warn("ai.delete_command_failed", "error", err.Error())
	}
	return nil
}

func searchText(ctx context.Context, cfg aiConfig, text string, options chatOptions) (string, []aiSource, string, error) {
	sel := cfg.selection("search")
	provider, ok := cfg.Configs[sel.Tag]
	if !ok {
		return "", nil, "", kit.Fail("请先用 ai model search 选择搜索模型")
	}
	kind := resolveProviderType(provider)
	if kind == "doubao" || kind == "moonshot" {
		return "", nil, "", kit.Failf("当前 %s 提供商不支持 search 模式", kind)
	}
	if kind == "local-cliproxy" && strings.Contains(sel.Model, "gemini") {
		if parsed, err := url.Parse(provider.URL); err == nil {
			if !strings.HasPrefix(parsed.Path, "/v1beta") {
				parsed.Path = "/v1beta"
			}
			parsed.RawQuery, parsed.Fragment = "", ""
			provider.Type, provider.URL = "gemini", parsed.String()
			configs := make(map[string]aiProvider, len(cfg.Configs))
			for tag, other := range cfg.Configs {
				configs[tag] = other
			}
			configs[sel.Tag] = provider
			cfg.Configs = configs
		}
	}
	request, err := buildChatRequest(cfg, sel, text, cfg.Prompt, options)
	if err != nil {
		return "", nil, "", err
	}
	switch {
	case request.Format == formatGemini:
		request.Body["tools"] = []any{map[string]any{"googleSearch": map[string]any{}}}
	case request.Responses:
		request.Body["tools"] = []any{map[string]any{"type": "web_search"}}
		request.Body["include"] = []any{"web_search_call.action.sources"}
	default:
		request.Body["tools"] = []any{map[string]any{"type": "web_search", "web_search": map[string]any{"searchContextSize": "high"}}}
		request.Body["web_search_options"] = map[string]any{"search_context_size": "high"}
	}
	raw, err := aiCall(ctx, request)
	if err != nil {
		return "", nil, "", err
	}
	output, err := parseChatText(raw, request.Format, provider.Key)
	if err != nil {
		return "", nil, "", err
	}
	var sources []aiSource
	seen := map[string]bool{}
	add := func(link, title any) {
		text := StringOf(link)
		if !strings.HasPrefix(strings.ToLower(text), "http://") && !strings.HasPrefix(strings.ToLower(text), "https://") || seen[text] {
			return
		}
		seen[text] = true
		sources = append(sources, aiSource{URL: text, Title: StringOf(title)})
	}
	payloads, err := Payloads(raw)
	if err != nil {
		return output, nil, sel.Tag, nil
	}
	for _, payload := range payloads {
		if request.Format == formatGemini {
			candidate := ObjectOf(FirstOf(ListOf(geminiRoot(payload)["candidates"])))
			metadata := ObjectOf(candidate["groundingMetadata"])
			if len(metadata) == 0 {
				metadata = ObjectOf(candidate["grounding_metadata"])
			}
			chunks := ListOf(metadata["groundingChunks"])
			if chunks == nil {
				chunks = ListOf(metadata["grounding_chunks"])
			}
			for _, chunk := range chunks {
				web := ObjectOf(ObjectOf(chunk)["web"])
				if len(web) == 0 {
					web = ObjectOf(ObjectOf(chunk)["web_chunk"])
				}
				add(web["uri"], web["title"])
			}
			continue
		}
		choice := ObjectOf(FirstOf(ListOf(payload["choices"])))
		for _, object := range []map[string]any{payload, choice, ObjectOf(choice["message"]), ObjectOf(choice["delta"])} {
			for _, entry := range append(ListOf(object["citations"]), ListOf(object["annotations"])...) {
				citation := ObjectOf(ObjectOf(entry)["url_citation"])
				if len(citation) == 0 {
					citation = ObjectOf(entry)
				}
				add(citation["url"], citation["title"])
			}
		}
		response := ObjectOf(payload["response"])
		if len(response) == 0 {
			response = payload
		}
		for _, item := range append([]any{payload["item"]}, ListOf(response["output"])...) {
			entry := ObjectOf(item)
			for _, part := range ListOf(entry["content"]) {
				for _, annotation := range ListOf(ObjectOf(part)["annotations"]) {
					add(ObjectOf(annotation)["url"], ObjectOf(annotation)["title"])
				}
			}
			actions := ListOf(entry["action"])
			if actions == nil && entry["action"] != nil {
				actions = []any{entry["action"]}
			}
			for _, action := range actions {
				for _, source := range ListOf(ObjectOf(action)["sources"]) {
					add(ObjectOf(source)["url"], ObjectOf(source)["title"])
				}
			}
		}
		for _, annotation := range ListOf(ObjectOf(payload["part"])["annotations"]) {
			add(ObjectOf(annotation)["url"], ObjectOf(annotation)["title"])
		}
	}
	return output, sources, sel.Tag, nil
}

func (s *aiService) telegraphPost(ctx context.Context, cfg aiConfig, method string, body any) (map[string]any, error) {
	response, err := httpx.PostJSON(ctx, "https://api.telegra.ph/"+method, nil, body, time.Duration(cfg.Timeout)*time.Second, 1<<20)
	if err != nil {
		return nil, kit.Fail("Telegraph 请求失败：" + httpx.Reason(err))
	}
	if !response.OK() {
		return nil, kit.Failf("Telegraph 返回 HTTP %d", response.Status)
	}
	var parsed map[string]any
	if err := json.Unmarshal(response.Body, &parsed); err != nil || parsed["ok"] != true {
		if detail := StringOf(parsed["error"]); detail != "" {
			return nil, kit.Fail("Telegraph 返回错误：" + detail)
		}
		return nil, kit.Fail("Telegraph 返回错误")
	}
	return ObjectOf(parsed["result"]), nil
}

// telegraphMarkdown 是发到 Telegraph 的全文：问题、回答和最多 20 条来源。
func telegraphMarkdown(question, answer string, sources []aiSource) string {
	markdown := "**Q:**\n" + question + "\n\n**A:**\n" + answer + "\n"
	if len(sources) > 0 {
		var rows []string
		for index, item := range sources {
			if index >= 20 {
				break
			}
			title := item.Title
			if title == "" {
				title = item.URL
			}
			rows = append(rows, fmt.Sprintf("%d. %s\n%s", index+1, title, item.URL))
		}
		markdown += "\n**Sources:**\n" + strings.Join(rows, "\n") + "\n"
	}
	return markdown
}

func (s *aiService) publish(ctx context.Context, cfg aiConfig, question, answer string, sources []aiSource) (string, error) {
	token := cfg.TelegraphToken
	if token == "" {
		account, err := s.telegraphPost(ctx, cfg, "createAccount", map[string]any{"short_name": "MiBotAI", "author_name": "MiBot"})
		if err != nil {
			return "", err
		}
		token = StringOf(account["access_token"])
		if token == "" {
			return "", kit.Fail("Telegraph 未返回 token")
		}
		if err := s.update(func(cfg *aiConfig) error { cfg.TelegraphToken = token; return nil }); err != nil {
			return "", err
		}
	}
	compact := strings.Join(strings.Fields(question), " ")
	title := compact
	if runes := []rune(compact); len(runes) > 24 {
		title = string(runes[:24]) + "…"
	}
	if title == "" {
		title = "Telegraph - " + time.Now().UTC().Format(time.RFC3339)
	}
	content := telegraphNodes(telegraphMarkdown(question, answer, sources))
	page, err := s.telegraphPost(ctx, cfg, "createPage", map[string]any{"access_token": token, "title": title, "content": content, "return_content": false})
	if err != nil {
		return "", err
	}
	link := StringOf(page["url"])
	if !strings.HasPrefix(link, "https://telegra.ph/") {
		return "", kit.Fail("Telegraph 返回无效链接")
	}
	item := aiTelegraphItem{URL: link, Title: title, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	_ = s.update(func(cfg *aiConfig) error {
		cfg.Telegraph.List = append(cfg.Telegraph.List, item)
		if len(cfg.Telegraph.List) > cfg.Telegraph.Limit {
			cfg.Telegraph.List = cfg.Telegraph.List[len(cfg.Telegraph.List)-cfg.Telegraph.Limit:]
		}
		return nil
	})
	return link, nil
}
