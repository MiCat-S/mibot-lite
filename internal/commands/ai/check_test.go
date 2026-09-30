package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// checkService 造一个只有一个服务商的 Service，接口地址指向测试服务器。
func checkService(t *testing.T, providers map[string]aiProvider, chatTag, chatModel string) *Service {
	t.Helper()
	s := &Service{store: store.New(filepath.Join(t.TempDir(), "ai.json"), aiDefaults)}
	if err := s.update(func(cfg *aiConfig) error {
		for tag, provider := range providers {
			provider.Tag = tag
			cfg.Configs[tag] = provider
		}
		cfg.CurrentChatTag, cfg.CurrentChatModel = chatTag, chatModel
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// 模型列表：去重、排序；Key 放在 Bearer 里；出错时聊天里看不到 Key。
func TestModelsListing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path != "/v1/models":
			w.WriteHeader(404)
		case r.Header.Get("Authorization") != "Bearer sk-good":
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided: sk-bad"}}`))
		default:
			_, _ = w.Write([]byte(`{"data":[{"id":"b"},{"id":"a"},{"name":"c"},{"id":"a"},{}]}`))
		}
	}))
	defer server.Close()
	s := checkService(t, map[string]aiProvider{
		"good": {URL: server.URL, Key: "sk-good", Type: "openai-compatible"},
		"bad":  {URL: server.URL + "/v1/chat/completions", Key: "sk-bad", Type: "openai-compatible"},
	}, "", "")
	models, err := s.Models(context.Background(), "good")
	if err != nil || !slices.Equal(models, []string{"a", "b", "c"}) {
		t.Fatalf("模型列表：%v %v", models, err)
	}
	_, err = s.Models(context.Background(), "bad")
	text, ok := kit.IsUserError(err)
	if !ok || strings.Contains(text, "sk-bad") || !strings.Contains(text, "HTTP 401") {
		t.Errorf("Key 无效时的错误：%q", text)
	}
	if _, err := s.Models(context.Background(), "missing"); err == nil {
		t.Error("不存在的标签应该报错")
	}
}

// 测速：不走流式、不带 temperature；读出用量、模型名和限流头。
func TestBenchmarkProbe(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		if body["model"] == "broken" {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"message":"model broken not found"}}`))
			return
		}
		w.Header().Set("x-ratelimit-remaining-requests", "499")
		_, _ = w.Write([]byte(`{"model":"` + body["model"].(string) + `-2026","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`))
	}))
	defer server.Close()
	s := checkService(t, map[string]aiProvider{"relay": {URL: server.URL, Key: "k", Type: "openai-compatible", Stream: true, Models: map[string]string{"chat": "m1"}}}, "", "")
	identity, probes, err := s.Benchmark(context.Background(), "relay")
	if err != nil {
		t.Fatal(err)
	}
	if identity.DisplayName != "OpenAI 兼容 API" || identity.Type != "openai-compatible" {
		t.Errorf("身份：%+v", identity)
	}
	if len(probes) != 1 || !probes[0].OK || probes[0].Text != "ok" || probes[0].Model != "m1-2026" {
		t.Fatalf("测速结果：%+v", probes)
	}
	if usage := probes[0].Usage; usage == nil || usage.Total != 6 || usage.Prompt != 5 {
		t.Errorf("用量：%+v", usage)
	}
	if len(probes[0].RateLimits) != 1 || probes[0].RateLimits[0] != (RateLimit{"x-ratelimit-remaining-requests", "499"}) {
		t.Errorf("限流头：%+v", probes[0].RateLimits)
	}
	if bodies[0]["stream"] != false || bodies[0]["temperature"] != nil || bodies[0]["max_tokens"] != float64(50) {
		t.Errorf("请求体：%v", bodies[0])
	}
	cfg, _ := s.read()
	failed := probeModel(context.Background(), cfg, "relay", "broken", 50)
	if failed.OK || !strings.Contains(failed.Error, "model broken not found") {
		t.Errorf("失败的模型：%+v", failed)
	}
}

// 识别服务商：按主机名认出常见的几家，其余按配置的类型。
func TestIdentify(t *testing.T) {
	cases := map[string]ProviderIdentity{
		"https://openrouter.ai/api/v1":               {Type: "openrouter", DisplayName: "OpenRouter"},
		"https://api.deepseek.com":                   {Type: "deepseek", DisplayName: "DeepSeek"},
		"https://api.x.ai/v1":                        {Type: "xai", DisplayName: "xAI (Grok)"},
		"https://generativelanguage.googleapis.com/": {Type: "gemini", DisplayName: "Google Gemini"},
		"https://relay.example.com/v1":               {Type: "openai", DisplayName: "OpenAI"},
	}
	for address, want := range cases {
		got := identifyProvider("t", aiProvider{URL: address})
		if got.Type != want.Type || got.DisplayName != want.DisplayName {
			t.Errorf("%s：%+v", address, got)
		}
	}
	if got := identifyProvider("t", aiProvider{URL: "https://relay.example.com", Type: "openai-compatible"}); got.DisplayName != "OpenAI 兼容 API" {
		t.Errorf("兼容接口：%+v", got)
	}
}

// 完整诊断：DeepSeek 查余额、列模型、用配置的聊天模型对话一次。
func TestDiagnoseDeepSeek(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/balance":
			_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"12.50"}]}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-chat"},{"id":"deepseek-reasoner"}]}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	provider := aiProvider{URL: server.URL, Key: "k", Type: "openai-compatible"}
	s := checkService(t, map[string]aiProvider{"ds": provider}, "ds", "deepseek-chat")
	cfg, _ := s.read()
	// 测试服务器的主机名认不出 DeepSeek，直接按 deepseek 查余额。
	got := providerBalance(context.Background(), cfg.Configs["ds"], "deepseek", "")
	want := []BalanceField{{"可用", "是"}, {"CNY", "12.50"}}
	if got.Status != "ok" || !slices.Equal(got.Fields, want) {
		t.Errorf("余额：%+v", got)
	}
	result, err := s.Diagnose(context.Background(), "ds")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Models, []string{"deepseek-chat", "deepseek-reasoner"}) || result.Chat == nil || !result.Chat.OK || result.Chat.Model != "deepseek-chat" {
		t.Errorf("诊断：%+v %+v", result, result.Chat)
	}
	if result.Balance.Status != "ok" || result.Balance.Fields[0] != (BalanceField{"状态", "有效"}) {
		t.Errorf("兼容接口的余额：%+v", result.Balance)
	}
}

// OpenRouter 的余额在 data 里；Key 无效时记成 invalid。
func TestBalanceOpenRouter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"label":"main","usage":1.5,"limit":null,"rate_limit":{"requests":20,"interval":"10s"}}}`))
	}))
	defer server.Close()
	got := providerBalance(context.Background(), aiProvider{URL: server.URL + "/api/v1", Key: "good"}, "openrouter", "")
	want := []BalanceField{{"标签", "main"}, {"余额", "?"}, {"已用", "1.5"}, {"限额", "?"}, {"速率", "20 req / 10s"}}
	if got.Status != "ok" || !slices.Equal(got.Fields, want) {
		t.Errorf("OpenRouter：%+v", got)
	}
	if got := providerBalance(context.Background(), aiProvider{URL: server.URL, Key: "bad"}, "openrouter", ""); got.Status != "invalid" {
		t.Errorf("Key 无效：%+v", got)
	}
}

// 导入：新标签直接加；同名但内容不同的改用 标签-2；内容相同只补模型；已有的聊天选择不改。
func TestImportProvider(t *testing.T) {
	s := checkService(t, map[string]aiProvider{"old": {URL: "https://a.example/v1", Key: "k1", Type: "openai-compatible"}}, "", "")
	tag, imported, err := s.ImportProvider(ProviderImport{Tag: "old", URL: "https://b.example/v1", Key: "k2", Type: "openai-compatible",
		Models: map[string]string{"chat": "gpt-4o-mini"}, Select: []string{"chat"}})
	if err != nil || tag != "old-2" || !imported {
		t.Fatalf("同名不同内容：%s %v %v", tag, imported, err)
	}
	tag, imported, err = s.ImportProvider(ProviderImport{Tag: "old", URL: "https://a.example/v1", Key: "k1", Type: "openai-compatible",
		Models: map[string]string{"chat": "m"}, Select: []string{"chat"}})
	if err != nil || tag != "old" || imported {
		t.Fatalf("同名同内容：%s %v %v", tag, imported, err)
	}
	cfg, _ := s.read()
	if cfg.CurrentChatTag != "old-2" || cfg.CurrentChatModel != "gpt-4o-mini" {
		t.Errorf("聊天选择应是第一次导入的：%s %s", cfg.CurrentChatTag, cfg.CurrentChatModel)
	}
	if cfg.Configs["old"].Models["chat"] != "m" || cfg.Configs["old-2"].Key != "k2" {
		t.Errorf("配置：%+v", cfg.Configs)
	}
	for _, bad := range []ProviderImport{{Tag: "a b", URL: "https://x", Key: "k"}, {Tag: "a", URL: "ftp://x", Key: "k"}, {Tag: "a", URL: "https://u:p@x", Key: "k"}, {Tag: "a", URL: "https://x", Key: ""}} {
		if _, _, err := s.ImportProvider(bad); err == nil {
			t.Errorf("应该拒绝：%+v", bad)
		}
	}
}

// Providers 不带地址和 Key。
func TestProvidersHideSecrets(t *testing.T) {
	s := checkService(t, map[string]aiProvider{"b": {URL: "https://b", Key: "sk", Models: map[string]string{"chat": "m"}}, "a": {URL: "https://a", Key: "sk2", Type: "gemini"}}, "", "")
	list, err := s.Providers()
	if err != nil || len(list) != 2 || list[0].Tag != "a" || list[0].Type != "gemini" || list[1].Type != "auto" || list[1].Models["chat"] != "m" {
		t.Fatalf("%+v %v", list, err)
	}
	encoded, _ := json.Marshal(list)
	if strings.Contains(string(encoded), "sk") || strings.Contains(string(encoded), "https") {
		t.Errorf("服务商列表带了敏感信息：%s", encoded)
	}
}

// 接口跳转到别的主机时拒绝：x-api-key 不能带过去。
func TestRedirectToOtherHostRefused(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = true
		_, _ = w.Write([]byte(`{"data":[{"id":"x"}]}`))
	}))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	s := checkService(t, map[string]aiProvider{"claude": {URL: server.URL, Key: "sk-ant-secret", Type: "anthropic", Models: map[string]string{"chat": "claude"}}}, "", "")
	_, err := s.Models(context.Background(), "claude")
	if text, _ := kit.IsUserError(err); !strings.Contains(text, "为保护 Key 已拒绝") || leaked {
		t.Errorf("跳转：%v，泄露：%v", err, leaked)
	}
	_, probes, err := s.Benchmark(context.Background(), "claude")
	if err != nil || len(probes) != 1 || !strings.Contains(probes[0].Error, "为保护 Key 已拒绝") || leaked {
		t.Errorf("测速跳转：%+v %v，泄露：%v", probes, err, leaked)
	}
}

// 每次测试最多等 checkTimeout，慢的模型记为超时，不拖到 ai 设置的超时。
func TestProbeTimeoutCapped(t *testing.T) {
	previous := checkTimeout
	checkTimeout = 50 * time.Millisecond
	defer func() { checkTimeout = previous }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(400 * time.Millisecond):
		}
	}))
	defer server.Close()
	s := checkService(t, map[string]aiProvider{"slow": {URL: server.URL, Key: "k", Type: "openai-compatible", Models: map[string]string{"chat": "m"}}}, "", "")
	started := time.Now()
	_, probes, err := s.Benchmark(context.Background(), "slow")
	if err != nil || len(probes) != 1 || probes[0].OK || !strings.Contains(probes[0].Error, "超时") || time.Since(started) > 300*time.Millisecond {
		t.Errorf("%+v %v %v", probes, err, time.Since(started))
	}
}
