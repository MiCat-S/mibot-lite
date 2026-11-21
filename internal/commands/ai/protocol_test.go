package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// 通过搜索入口检查真实发出的工具字段、认证和流式回包里的来源。
func TestSearchProtocols(t *testing.T) {
	tests := []struct {
		name, kind, path, tool, events string
		responses                      bool
	}{
		{name: "chat", kind: "openai-compatible", path: "/chat/completions", events: `data: {"choices":[{"delta":{"content":"答"}}]}
data: {"choices":[{"delta":{"content":"案","annotations":[{"type":"url_citation","url_citation":{"url":"https://example.org","title":"来源"}}]},"finish_reason":"stop"}]}
data: [DONE]`},
		{name: "responses", kind: "openai-compatible", responses: true, path: "/responses", tool: "web_search", events: `data: {"type":"response.output_text.delta","delta":"答"}

data: {"type":"response.output_text.annotation.added","annotation":{"type":"url_citation","url":"https://example.org","title":"来源"}}

data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"答案"}]}]}}`},
		{name: "anthropic", kind: "anthropic", path: "/messages", tool: "web_search_20250305", events: `event: content_block_start
data: {"type":"content_block_start","content_block":{"type":"text","text":""}}

data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"答"}}

data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"案"}}

data: {"type":"content_block_delta","delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"https://example.org","title":"来源"}}}

data: {"type":"message_stop"}`},
		{name: "gemini", kind: "gemini", path: "/models/m:streamGenerateContent", tool: "googleSearch", events: `data: {"candidates":[{"content":{"parts":[{"thought":true,"text":"推理"},{"text":"答"}]}}]}

data: {"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"案"}]},"groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://example.org","title":"来源"}}]}}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/tenant/v1"+tc.path || r.URL.Query().Get("api-version") != "1" {
					t.Errorf("请求路径：%s %s", r.Method, r.URL)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				tool := ObjectOf(FirstOf(ListOf(body["tools"])))
				switch tc.name {
				case "chat":
					if body["tools"] != nil || ObjectOf(body["web_search_options"])["search_context_size"] != "high" {
						t.Errorf("Chat 搜索参数：%v", body)
					}
				case "responses":
					if tool["type"] != tc.tool || FirstOf(ListOf(body["include"])) != "web_search_call.action.sources" {
						t.Errorf("Responses 搜索参数：%v", body)
					}
				case "anthropic":
					if tool["type"] != tc.tool || tool["name"] != "web_search" || r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") == "" {
						t.Errorf("Messages 搜索参数：%v", body)
					}
				case "gemini":
					if tool[tc.tool] == nil || r.URL.Query().Get("alt") != "sse" || r.URL.Query().Get("key") != "test-key" {
						t.Errorf("Gemini 搜索参数：%v %s", body, r.URL)
					}
				}
				if tc.kind == "openai-compatible" && r.Header.Get("Authorization") != "Bearer test-key" || tc.kind != "gemini" && body["stream"] != true {
					t.Errorf("认证或流式字段错误：%v %v", r.Header, body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.events)
			}))
			defer server.Close()
			cfg := testConfig(aiProvider{URL: server.URL + "/tenant/v1?api-version=1", Key: "test-key", Type: tc.kind, Responses: tc.responses, Stream: true})
			if tc.kind == "gemini" {
				cfg.CurrentChatModel = "models/m"
			}
			answer, sources, _, err := searchText(context.Background(), cfg, "问题", chatOptions{})
			if err != nil || answer != "答案" || !slices.Equal(sources, []aiSource{{URL: "https://example.org", Title: "来源"}}) {
				t.Fatalf("搜索返回 %q，%v，%v", answer, sources, err)
			}
		})
	}
}

func TestReplyTextAndTerminalEvents(t *testing.T) {
	for _, tc := range []struct{ name, format, raw, want, failure string }{
		{"多条消息", "openai", `{"output":[{"type":"reasoning"},{"type":"message","content":[{"type":"output_text","text":"第一段"}]},{"type":"message","content":[{"type":"output_text","text":"第二段"}]}]}`, "第一段\n第二段", ""},
		{"拒绝回答", "openai", `{"choices":[{"message":{"content":null,"refusal":"无法回答"}}]}`, "无法回答", ""},
		{"Responses 拒绝", "openai", `{"output":[{"type":"message","content":[{"type":"refusal","refusal":"无法回答"}]}]}`, "无法回答", ""},
		{"多行 SSE", "openai", "event: chunk\r\ndata: {\"choices\":\r\ndata: [{\"delta\":{\"content\":\"答\"}}]}\r\n\r\ndata: [DONE]\r\n", "答", ""},
		{"流被截断", "openai", `data: {"type":"response.output_text.delta","delta":"部分"}`, "", "中断"},
		{"明确未完成", "openai", `data: {"type":"response.output_text.delta","delta":"部分"}

data: {"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`, "", "max_output_tokens"},
		{"错误替代部分回答", "anthropic", `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"部分"}}

data: {"type":"error","error":{"message":"test-key failed"}}`, "", "*** failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer, err := ParseText([]byte(tc.raw), tc.format, "test-key")
			message, _ := kit.IsUserError(err)
			if answer != tc.want || (err == nil) != (tc.failure == "") || !strings.Contains(message, tc.failure) {
				t.Errorf("返回 %q，%v", answer, err)
			}
		})
	}
}

func TestProviderURLAndMode(t *testing.T) {
	s := serviceWith(t, aiDefaults())
	if _, err := runConfig(t, s, true, "config", "add", "p", "https://example.org/tenant%2Fone/responses?api-version=1", "key"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := s.read()
	req, err := buildChatRequest(cfg, aiSelection{Tag: "p", Model: "m"}, "问题", "", chatOptions{})
	if err != nil || !req.Responses || req.URL != "https://example.org/tenant%2Fone/responses?api-version=1" {
		t.Fatalf("完整 Responses 地址：%+v，%v", req, err)
	}
	cfg = testConfig(aiProvider{URL: "https://generativelanguage.googleapis.com/v1beta/openai/", Key: "test"})
	req, err = buildChatRequest(cfg, cfg.selection("chat"), "问题", "", chatOptions{})
	if err != nil || req.Format != formatOpenAI || req.Headers["Authorization"] != "Bearer test" || req.URL != "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions" {
		t.Fatalf("Gemini 兼容端点：%+v，%v", req, err)
	}
	for path, want := range map[string]string{
		"/relay/v1/messages": formatAnthropic, "/relay/v1beta/models/m:generateContent": formatGemini,
	} {
		cfg := testConfig(aiProvider{URL: "https://example.org" + path, Key: "key"})
		request, err := buildChatRequest(cfg, cfg.selection("chat"), "问题", "", chatOptions{})
		if err != nil || request.Format != want {
			t.Fatalf("%s: %+v, %v", path, request, err)
		}
	}
	raw, err := ConvertMiBox([]byte(`{"configs":{"g":{"url":"https://generativelanguage.googleapis.com/v1beta/openai/"},"c":{"url":"https://example.org/v1/chat/completions"}}}`))
	if err != nil || json.Unmarshal(raw, &cfg) != nil || cfg.Configs["g"].Responses || cfg.Configs["c"].Responses {
		t.Fatalf("导入的 Chat Completions 地址不应改用 Responses：%s，%v", raw, err)
	}
}

func TestNativeProviderDoesNotRepeatFailedSummary(t *testing.T) {
	for _, kind := range []string{"gemini", "anthropic"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(http.StatusBadRequest)
			}))
			defer server.Close()
			s := serviceWith(t, testConfig(aiProvider{Type: kind, URL: server.URL, Key: "test", Responses: true}))
			if _, err := s.Chat(context.Background(), ChatRequest{Text: "摘要", FallbackToChat: true}); err == nil || calls != 1 {
				t.Fatalf("原生接口发送了 %d 次，%v", calls, err)
			}
		})
	}
}

func TestNativeModelListPages(t *testing.T) {
	for _, kind := range []string{"gemini", "anthropic"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if r.Method != http.MethodGet || r.URL.Path != "/tenant/api/models" || q.Get("project") != "one" {
					t.Errorf("模型列表请求：%s %s", r.Method, r.URL)
				}
				switch kind {
				case "gemini":
					if q.Get("key") != "test" {
						t.Error("Gemini 缺少认证")
					}
					if q.Get("pageToken") == "next" {
						fmt.Fprint(w, `{"models":[{"name":"models/b"}]}`)
					} else {
						fmt.Fprint(w, `{"models":[{"name":"models/a"}],"nextPageToken":"next"}`)
					}
				case "anthropic":
					if r.Header.Get("x-api-key") != "test" {
						t.Error("Anthropic 缺少认证")
					}
					if q.Get("after_id") == "models/a" {
						fmt.Fprint(w, `{"data":[{"id":"models/b"}],"has_more":false}`)
					} else {
						fmt.Fprint(w, `{"data":[{"id":"models/a"}],"has_more":true,"last_id":"models/a"}`)
					}
				}
			}))
			defer server.Close()
			s := serviceWith(t, testConfig(aiProvider{URL: server.URL + "/tenant/api?project=one", Key: "test", Type: kind}))
			names, err := s.Models(context.Background(), "p")
			if err != nil || !slices.Equal(names, []string{"models/a", "models/b"}) {
				t.Fatalf("模型列表：%v，%v", names, err)
			}
		})
	}
}

func TestQueryKeyRedactedOnFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "gateway-key" {
			t.Error("未使用地址内已有的 Key")
		}
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"invalid gateway-key"}}`)
	}))
	defer server.Close()
	cfg := testConfig(aiProvider{URL: server.URL + "?key=gateway-key", Key: "stored-key", Type: "gemini"})
	s := serviceWith(t, cfg)
	_, chatErr := s.Chat(context.Background(), ChatRequest{Text: "问题"})
	_, listErr := s.Models(context.Background(), "p")
	for _, err := range []error{chatErr, listErr} {
		message, _ := kit.IsUserError(err)
		if err == nil || strings.Contains(err.Error(), "gateway-key") || !strings.Contains(message, "invalid ***") {
			t.Fatalf("实际发送的 Key 应在错误说明中打码：%q，%v", message, err)
		}
	}
}
