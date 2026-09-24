package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// TestComposeQuestion 检查回复上下文的拼法：输入和回复都有时带上下文，只回复时回复即问题。
func TestComposeQuestion(t *testing.T) {
	cases := []struct{ own, replied, question, text string }{
		{"翻译一下", "hello", "翻译一下", "上下文:\nhello\n\n问题:\n翻译一下"},
		{"", " hello ", "hello", "hello"},
		{"hello", "hello", "hello", "hello"},
		{"只有问题", "", "只有问题", "只有问题"},
		{"", "", "", ""},
	}
	for _, test := range cases {
		question, text := composeQuestion(test.own, test.replied)
		if question != test.question || text != test.text {
			t.Errorf("composeQuestion(%q, %q) = %q, %q", test.own, test.replied, question, text)
		}
	}
}

// TestErrorDetail 检查错误说明的提取、密钥打码和截断。
func TestErrorDetail(t *testing.T) {
	secret := "sk-secret-123"
	cases := []struct{ body, want string }{
		{`{"error":{"message":"Incorrect API key provided: sk-secret-123","type":"invalid_request_error"}}`, "Incorrect API key provided: ***"},
		{`[{"error":{"code":400,"message":"API key not valid","status":"INVALID_ARGUMENT"}}]`, "API key not valid"},
		{`{"error":"quota exceeded"}`, "quota exceeded"},
		{`{"detail":"Not Found"}`, "Not Found"},
		{`{"status":"failed"}`, `{"status":"failed"}`},
		{"upstream said key=sk-secret-123\n\tbad", "upstream said key=*** bad"},
		{"<html><body>502</body></html>", ""},
		{"", ""},
	}
	for _, test := range cases {
		if got := ErrorDetail([]byte(test.body), secret); got != test.want {
			t.Errorf("ErrorDetail(%q) = %q, want %q", test.body, got, test.want)
		}
	}
	long := ErrorDetail([]byte(`{"message":"`+strings.Repeat("长", 300)+`"}`), "")
	if len([]rune(long)) != errorDetailLimit+1 || !strings.HasSuffix(long, "…") {
		t.Fatalf("没有截断：%d", len([]rune(long)))
	}
	if got := HTTPFailure(401, []byte(`{"error":{"message":"bad key sk-secret-123"}}`), secret); got != "AI 接口返回 HTTP 401：bad key ***" {
		t.Fatalf("HTTPFailure = %q", got)
	}
	if got := HTTPFailure(429, nil, secret); got != "请求过于频繁，请稍后重试" {
		t.Fatalf("429 = %q", got)
	}
	if got := MaskSecret("a sk%2Bx b sk+x", "sk+x"); got != "a *** b ***" {
		t.Fatalf("URL 编码的密钥也要打码：%q", got)
	}
}

func testConfig(provider aiProvider) aiConfig {
	cfg := aiDefaults()
	cfg.Configs["p"] = provider
	cfg.CurrentChatTag, cfg.CurrentChatModel = "p", "m"
	return cfg
}

// TestBuildChatRequestImages 检查各家接口的多模态格式。
func TestBuildChatRequestImages(t *testing.T) {
	images := chatOptions{images: []aiImage{{Data: []byte("img"), MimeType: "image/png"}}, maxOutputTokens: 100}
	encoded := "aW1n"
	cases := []struct {
		name     string
		provider aiProvider
		url      string
		body     string
	}{
		{"chat", aiProvider{URL: "https://api.example.com/v1", Key: "k"}, "https://api.example.com/v1/chat/completions",
			`{"max_tokens":100,"messages":[{"content":"sys","role":"system"},{"content":[{"text":"看图","type":"text"},{"image_url":{"url":"data:image/png;base64,` + encoded + `"},"type":"image_url"}],"role":"user"}],"model":"m","stream":false}`},
		{"responses", aiProvider{URL: "https://api.example.com/v1", Key: "k", Responses: true}, "https://api.example.com/v1/responses",
			`{"input":[{"content":[{"text":"看图","type":"input_text"},{"image_url":"data:image/png;base64,` + encoded + `","type":"input_image"}],"role":"user"}],"instructions":"sys","max_output_tokens":100,"model":"m","stream":false}`},
		{"gemini", aiProvider{URL: "https://generativelanguage.googleapis.com/v1beta", Key: "k"}, "https://generativelanguage.googleapis.com/v1beta/models/m:generateContent?key=k",
			`{"contents":[{"parts":[{"text":"看图"},{"inlineData":{"data":"` + encoded + `","mimeType":"image/png"}}],"role":"user"}],"generationConfig":{"maxOutputTokens":100},"systemInstruction":{"parts":[{"text":"sys"}],"role":"system"}}`},
		{"anthropic", aiProvider{URL: "https://api.anthropic.com", Key: "k"}, "https://api.anthropic.com/v1/messages",
			`{"max_tokens":100,"messages":[{"content":[{"text":"看图","type":"text"},{"source":{"data":"` + encoded + `","media_type":"image/png","type":"base64"},"type":"image"}],"role":"user"}],"model":"m","system":"sys"}`},
	}
	for _, test := range cases {
		cfg := testConfig(test.provider)
		request, err := buildChatRequest(cfg, cfg.selection("chat"), "看图", "sys", images)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		body, _ := json.Marshal(request.Body)
		if request.URL != test.url || string(body) != test.body {
			t.Errorf("%s:\n url  %s\n body %s", test.name, request.URL, body)
		}
	}
	anthropic := testConfig(aiProvider{URL: "https://api.anthropic.com", Key: "k"})
	request, _ := buildChatRequest(anthropic, anthropic.selection("chat"), "hi", "", chatOptions{})
	if request.Headers["x-api-key"] != "k" || request.Headers["Authorization"] != "" || request.Body["max_tokens"] != 4096 {
		t.Fatalf("anthropic 请求头或默认 max_tokens 不对：%v %v", request.Headers, request.Body)
	}
	bare := testConfig(aiProvider{URL: "https://api.openai.com", Key: "k"})
	request, _ = buildChatRequest(bare, bare.selection("chat"), "hi", "", chatOptions{})
	if request.URL != "https://api.openai.com/v1/chat/completions" {
		t.Fatalf("只填域名时应补上 /v1：%s", request.URL)
	}
	tooMany := chatOptions{images: make([]aiImage, 5)}
	if _, err := buildChatRequest(bare, bare.selection("chat"), "hi", "", tooMany); err == nil {
		t.Fatal("超过 4 张图片应拒绝")
	}
}

// TestParseAnthropic 检查 Anthropic 响应和 2xx 里带错误时的说明。
func TestParseAnthropic(t *testing.T) {
	text, err := parseChatText([]byte(`{"content":[{"type":"text","text":"你好"},{"type":"tool_use"},{"type":"text","text":"世界"}]}`), formatAnthropic, "")
	if err != nil || text != "你好\n世界" {
		t.Fatalf("got %q %v", text, err)
	}
	_, err = parseChatText([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`), formatAnthropic, "")
	if message, _ := kit.IsUserError(err); message != "AI 提供商返回错误：Overloaded" {
		t.Fatalf("got %q", message)
	}
}

// TestSearchFallsBackToChat 检查没有选搜索模型时沿用聊天模型。
func TestSearchFallsBackToChat(t *testing.T) {
	cfg := testConfig(aiProvider{URL: "https://api.example.com/v1"})
	if sel := cfg.selection("search"); sel.Tag != "p" || sel.Model != "m" {
		t.Fatalf("got %+v", sel)
	}
	cfg.CurrentSearchTag, cfg.CurrentSearchModel = "q", "n"
	if sel := cfg.selection("search"); sel.Tag != "q" || sel.Model != "n" {
		t.Fatalf("got %+v", sel)
	}
}

func withShared(t *testing.T, cfg aiConfig) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ai.json")
	service := &aiService{store: store.New(path, aiDefaults)}
	if err := service.store.Update(func(value *aiConfig) error { *value = cfg; return nil }); err != nil {
		t.Fatal(err)
	}
	previous := shared
	shared = service
	t.Cleanup(func() { shared = previous })
}

// TestChatFallsBackToChatCompletions 检查 .sum 借 ai 调用时：用指定标签记下的 chat 模型，
// Responses 返回 404 时改走 Chat Completions。
func TestChatFallsBackToChatCompletions(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		paths, bodies = append(paths, r.URL.Path), append(bodies, body)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/responses") {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"message":"no such endpoint"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"摘要"}}]}`))
	}))
	defer server.Close()
	cfg := aiDefaults()
	cfg.Configs["main"] = aiProvider{URL: server.URL + "/v1", Key: "k"}
	cfg.Configs["sum-a"] = aiProvider{URL: server.URL + "/v1", Key: "k2", Responses: true, Models: map[string]string{"chat": "gpt-x"}}
	cfg.CurrentChatTag, cfg.CurrentChatModel = "main", "chat-model"
	withShared(t, cfg)

	text, err := Chat(context.Background(), ChatRequest{Tag: "sum-a", Text: "消息", SystemPrompt: "总结", MaxOutputTokens: 2000, FallbackToChat: true})
	if err != nil || text != "摘要" {
		t.Fatalf("got %q %v", text, err)
	}
	if len(paths) != 2 || paths[0] != "/v1/responses" || paths[1] != "/v1/chat/completions" {
		t.Fatalf("请求路径不对：%v", paths)
	}
	if bodies[1]["model"] != "gpt-x" || bodies[1]["max_tokens"] != float64(2000) {
		t.Fatalf("回退请求的模型或上限不对：%v", bodies[1])
	}
	if !HasProvider("sum-a") || HasProvider("missing") {
		t.Fatal("HasProvider 结果不对")
	}

	paths = nil
	text, err = Chat(context.Background(), ChatRequest{Text: "消息"})
	if err != nil || text != "摘要" || len(paths) != 1 || bodies[len(bodies)-1]["model"] != "chat-model" {
		t.Fatalf("不指定标签时应使用当前聊天模型：%q %v %v", text, err, paths)
	}

	_, err = Chat(context.Background(), ChatRequest{Tag: "sum-a", Text: "消息"})
	if message, _ := kit.IsUserError(err); message != "AI 接口返回 HTTP 404：no such endpoint" || statusOf(err) != 404 {
		t.Fatalf("不回退时应带上接口的错误说明：%q", message)
	}
}

// TestConvertMiBox 检查导入 MiBox 配置时补上的默认值，其余字段不变。
func TestConvertMiBox(t *testing.T) {
	raw := []byte(`{"configs":{"a":{"tag":"a","url":"https://api.example.com","key":"k"},"g":{"tag":"g","url":"https://generativelanguage.googleapis.com/v1beta","key":"k"},"x":{"tag":"x","url":"https://h","key":"k","responses":false,"extra":1}},"currentChatTag":"a","currentChatModel":"m","unknown":true}`)
	converted, err := ConvertMiBox(raw)
	if err != nil {
		t.Fatal(err)
	}
	var cfg aiConfig
	if err := json.Unmarshal(converted, &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Configs["a"].Responses || cfg.Configs["g"].Responses || cfg.Configs["x"].Responses {
		t.Fatalf("responses 默认值不对：%+v", cfg.Configs)
	}
	if cfg.CurrentSearchTag != "a" || cfg.CurrentSearchModel != "m" {
		t.Fatalf("搜索选择应沿用聊天：%+v", cfg)
	}
	if !bytes.Contains(converted, []byte(`"unknown": true`)) || !bytes.Contains(converted, []byte(`"extra": 1`)) {
		t.Fatalf("其他字段应原样保留：%s", converted)
	}
	cleared, _ := ConvertMiBox([]byte(`{"currentChatTag":"a","currentSearchTag":"","currentSearchModel":""}`))
	if !bytes.Contains(cleared, []byte(`"currentSearchTag": ""`)) {
		t.Fatalf("明确清空的搜索选择不能被覆盖：%s", cleared)
	}
	if _, err := ConvertMiBox([]byte("not json")); err == nil {
		t.Fatal("无效 JSON 应报错")
	}
}

// TestQuestionTextAndAnchor 检查问题取自原始文本（保留换行），以及回答回复哪条消息。
func TestQuestionTextAndAnchor(t *testing.T) {
	inv := &command.Invocation{Prefix: ".", Text: ".ai  search 第一行\n第二行 "}
	if got := questionText(inv, 1); got != "第一行\n第二行" {
		t.Errorf("got %q", got)
	}
	if got := questionText(inv, 0); got != "search 第一行\n第二行" {
		t.Errorf("got %q", got)
	}
	if got := questionText(&command.Invocation{Prefix: ".", Text: ".ai"}, 0); got != "" {
		t.Errorf("got %q", got)
	}
	own := &bot.Message{ID: 10, ChatID: "-1001"}
	if replyAnchor(own, &bot.Message{ID: 7, ChatID: "-1001"}) != 7 || replyAnchor(own, &bot.Message{ID: 7, ChatID: "-1002"}) != 0 {
		t.Error("只回复同一对话里的消息")
	}
	if replyAnchor(&bot.Message{ID: 10, ChatID: "-1001", TopicID: 3}, nil) != 3 {
		t.Error("论坛话题里应回复话题")
	}
}

// TestMergeImages 检查图片合计不超过 4 张、20 MiB。
func TestMergeImages(t *testing.T) {
	small := aiImage{Data: make([]byte, 10), MimeType: "image/png"}
	merged := mergeImages(imageSet{images: []aiImage{small, small, small}}, imageSet{images: []aiImage{small, small}})
	if len(merged.images) != 4 || !merged.dropped {
		t.Fatalf("got %d dropped=%v", len(merged.images), merged.dropped)
	}
	big := aiImage{Data: make([]byte, aiMaxImageBytes-5), MimeType: "image/png"}
	merged = mergeImages(imageSet{images: []aiImage{big, small}})
	if len(merged.images) != 1 || !merged.dropped {
		t.Fatalf("超过体积上限应丢弃：%d", len(merged.images))
	}
	if merged := mergeImages(imageSet{images: []aiImage{small}}); merged.dropped {
		t.Fatal("没超限不应标记丢弃")
	}
}

// TestImageHelpers 检查缩略图的选择和 GIF 首帧转 PNG。
func TestImageHelpers(t *testing.T) {
	kind, inline := thumbChoice([]tg.PhotoSizeClass{&tg.PhotoStrippedSize{Type: "i"}, &tg.PhotoSize{Type: "s", W: 90, H: 90}, &tg.PhotoSize{Type: "m", W: 320, H: 320}})
	if kind != "m" || inline != nil {
		t.Fatalf("got %q", kind)
	}
	kind, inline = thumbChoice([]tg.PhotoSizeClass{&tg.PhotoCachedSize{Type: "s", W: 90, H: 90, Bytes: []byte{1}}})
	if kind != "" || len(inline) != 1 {
		t.Fatalf("内嵌缩略图应直接返回字节")
	}
	frame := image.NewPaletted(image.Rect(0, 0, 4, 4), []color.Color{color.Black, color.White})
	var buffer bytes.Buffer
	if err := gif.EncodeAll(&buffer, &gif.GIF{Image: []*image.Paletted{frame, frame}, Delay: []int{1, 1}}); err != nil {
		t.Fatal(err)
	}
	png, err := firstFramePNG(buffer.Bytes())
	if err != nil || !bytes.HasPrefix(png, []byte("\x89PNG")) {
		t.Fatalf("GIF 首帧应转成 PNG：%v", err)
	}
	if _, err := firstFramePNG([]byte("not an image")); err == nil {
		t.Fatal("无效图片应报错")
	}
	animated := &tg.Document{MimeType: "video/mp4", Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeAnimated{}}}
	if !animatedDocument(animated) || animatedDocument(&tg.Document{MimeType: "image/webp"}) {
		t.Fatal("animatedDocument 判断不对")
	}
}
