package ai

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// payloads 解码普通 JSON 或 SSE。SSE 的同一个事件可以含多行 data，
// 部分兼容网关也会连续发多条 JSON 而省略事件之间的空行。
func payloads(raw []byte) ([]map[string]any, bool, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, false, kit.Fail("AI 返回内容为空")
	}
	invalid := kit.Fail("AI 返回无效 JSON")
	if raw[0] == '{' {
		var object map[string]any
		if json.Unmarshal(raw, &object) != nil {
			return nil, false, invalid
		}
		return []map[string]any{object}, true, nil
	}
	var events []map[string]any
	var data strings.Builder
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		decoder := json.NewDecoder(strings.NewReader(data.String()))
		for {
			var event map[string]any
			err := decoder.Decode(&event)
			if err == io.EOF {
				data.Reset()
				return nil
			}
			if err != nil || event == nil {
				return invalid
			}
			events = append(events, event)
		}
	}
	done := false
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "data: [DONE]" || line == "data:[DONE]" {
			if err := flush(); err != nil {
				return nil, false, err
			}
			if line != "" {
				done = true
				break
			}
		} else if value, ok := strings.CutPrefix(line, "data:"); ok {
			data.WriteString(value)
			data.WriteByte('\n')
		}
	}
	if err := flush(); err != nil {
		return nil, false, err
	}
	if len(events) == 0 {
		return nil, false, invalid
	}
	return events, done, nil
}

// ObjectOf 把 JSON 值当作对象读取，不是对象时返回 nil。
func ObjectOf(value any) map[string]any {
	object, _ := value.(map[string]any)
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
	detail := strings.TrimSpace(whitespaceRun.ReplaceAllString(maskSecret(errorMessageOf(payload), secret), " "))
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
		return text
	}
	var parts []string
	items := ListOf(content)
	if items == nil && content != nil {
		items = []any{content}
	}
	for _, item := range items {
		part := ObjectOf(item)
		switch part["type"] {
		case "text", "output_text":
			parts = append(parts, StringOf(part["text"]))
		case "refusal":
			parts = append(parts, StringOf(part["refusal"]))
		}
	}
	return strings.Join(parts, "\n")
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

// ParseText 解码服务商的完整回复，供 .ai 和 .sum 共用。
func ParseText(raw []byte, protocol, secret string) (string, error) {
	text, _, err := decodeReply(raw, protocol, secret)
	return text, err
}

// decodeReply 同时保留原始事件，搜索来源和诊断用量无需再次解码。
func decodeReply(raw []byte, protocol, secret string) (string, []map[string]any, error) {
	events, complete, err := payloads(raw)
	if err != nil {
		return "", nil, err
	}
	for _, event := range events {
		if aiProviderFailed(event) {
			return "", nil, providerFailure(event, secret)
		}
	}
	var answer string
	switch protocol {
	case formatGemini:
		answer, complete, err = geminiText(events, complete)
	case formatAnthropic:
		answer, complete, err = anthropicText(events, complete)
	default:
		answer, complete, err = openAIText(events, complete)
	}
	if err != nil {
		return "", nil, err
	}
	if !complete {
		return "", nil, kit.Fail("AI 流式回复在结束前中断")
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return "", nil, kit.Fail("AI 返回内容为空")
	}
	if kit.UTF16Len(answer) > aiOutputLimit {
		return "", nil, kit.Fail("AI 输出超出长度限制")
	}
	return answer, events, nil
}

func geminiText(events []map[string]any, complete bool) (string, bool, error) {
	var text strings.Builder
	for _, event := range events {
		root := geminiRoot(event)
		candidate := ObjectOf(FirstOf(ListOf(root["candidates"])))
		for _, value := range ListOf(ObjectOf(candidate["content"])["parts"]) {
			part := ObjectOf(value)
			if part["thought"] != true {
				text.WriteString(StringOf(part["text"]))
			}
		}
		if reason := StringOf(candidate["finishReason"]); reason != "" {
			complete = true
			if reason != "STOP" {
				return "", false, kit.Failf("AI 回答未完成（%s）", reason)
			}
		}
		if reason := StringOf(ObjectOf(root["promptFeedback"])["blockReason"]); reason != "" {
			return "", false, kit.Failf("AI 拒绝了输入（%s）", reason)
		}
	}
	return text.String(), complete, nil
}

func anthropicText(events []map[string]any, complete bool) (string, bool, error) {
	var text strings.Builder
	for _, event := range events {
		switch event["type"] {
		case "content_block_start":
			block := ObjectOf(event["content_block"])
			if block["type"] == "text" {
				if text.Len() > 0 {
					text.WriteByte('\n')
				}
				text.WriteString(StringOf(block["text"]))
			}
		case "content_block_delta":
			if delta := ObjectOf(event["delta"]); delta["type"] == "text_delta" {
				text.WriteString(StringOf(delta["text"]))
			}
		case "message_stop":
			complete = true
		default:
			if content := aiContentText(event["content"]); content != "" {
				text.WriteString(content)
			}
		}
		reason := StringOf(event["stop_reason"])
		if event["type"] == "message_delta" {
			reason = StringOf(ObjectOf(event["delta"])["stop_reason"])
		}
		if reason == "pause_turn" || reason == "max_tokens" || reason == "model_context_window_exceeded" {
			return "", false, kit.Failf("AI 回答未完成（%s）", reason)
		}
	}
	return text.String(), complete, nil
}

func openAIText(events []map[string]any, complete bool) (string, bool, error) {
	var text strings.Builder
	var final, items, parts []string
	for _, event := range events {
		response := event
		if nested := ObjectOf(event["response"]); len(nested) > 0 {
			response = nested
		}
		if event["type"] == "response.incomplete" || response["status"] == "incomplete" {
			reason := StringOf(ObjectOf(response["incomplete_details"])["reason"])
			if reason == "" {
				return "", false, kit.Fail("AI 回答未完成")
			}
			return "", false, kit.Failf("AI 回答未完成（%s）", reason)
		}
		switch event["type"] {
		case "response.output_text.delta", "response.refusal.delta":
			text.WriteString(StringOf(event["delta"]))
		case "response.output_text.done":
			parts = append(parts, StringOf(event["text"]))
		case "response.refusal.done":
			parts = append(parts, StringOf(event["refusal"]))
		case "response.output_item.done":
			if item := ObjectOf(event["item"]); item["type"] == "message" {
				items = append(items, aiContentText(item["content"]))
			}
		case "response.completed":
			complete = true
		}
		if summary := StringOf(response["output_text"]); summary != "" {
			final = []string{summary}
		} else if output := ListOf(response["output"]); len(output) > 0 {
			var messages []string
			for _, value := range output {
				item := ObjectOf(value)
				if item["type"] == "message" || item["type"] == nil {
					if reply := aiContentText(item["content"]); reply != "" {
						messages = append(messages, reply)
					}
				}
			}
			if len(messages) > 0 {
				final = messages
			}
		}
		if strings.HasPrefix(StringOf(event["type"]), "response.") {
			continue
		}
		choice := ObjectOf(FirstOf(ListOf(event["choices"])))
		if reason := StringOf(choice["finish_reason"]); reason != "" {
			complete = true
			if reason == "length" || reason == "content_filter" {
				return "", false, kit.Failf("AI 回答未完成（%s）", reason)
			}
		}
		delta := ObjectOf(choice["delta"])
		text.WriteString(aiContentText(delta["content"]))
		text.WriteString(StringOf(delta["refusal"]))
		message := ObjectOf(choice["message"])
		for _, value := range []any{message["content"], message["refusal"], choice["content"], choice["text"], event["content"], event["text"]} {
			if reply := aiContentText(value); reply != "" {
				final = []string{reply}
				break
			}
		}
	}
	// Responses 的最终快照、完成项和文字增量可能包含同一段回答。
	if answer := strings.Join(final, "\n"); answer != "" {
		return answer, complete, nil
	}
	if text.Len() > 0 {
		return text.String(), complete, nil
	}
	for _, snapshot := range [][]string{items, parts} {
		if answer := strings.Join(snapshot, "\n"); answer != "" {
			return answer, complete, nil
		}
	}
	return "", complete, nil
}
