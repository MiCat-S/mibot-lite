package checkapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/ai"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// fakeAI 冒充 ai.Service：记下调用，按标签返回写好的结果。
type fakeAI struct {
	mu        sync.Mutex
	providers []ai.ProviderSummary
	models    map[string][]string
	imported  []ai.ProviderImport
	// reject 不为空时决定 ImportProvider 要不要拒绝这一条。
	reject func(ai.ProviderImport) error
	chats  []ai.ChatRequest
}

func (f *fakeAI) Providers() ([]ai.ProviderSummary, error) { return f.providers, nil }

func (f *fakeAI) Models(_ context.Context, tag string) ([]string, error) {
	if list, ok := f.models[tag]; ok {
		return list, nil
	}
	return nil, kit.Fail("AI 接口返回 HTTP 401：Incorrect API key provided: ***")
}

func (f *fakeAI) Chat(_ context.Context, request ai.ChatRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chats = append(f.chats, request)
	return "hello <world>", nil
}

func (f *fakeAI) Benchmark(_ context.Context, tag string) (ai.ProviderIdentity, []ai.Probe, error) {
	return ai.ProviderIdentity{Tag: tag, Type: "deepseek", DisplayName: "DeepSeek"}, []ai.Probe{
		{Model: "deepseek-chat", OK: true, Elapsed: 800 * time.Millisecond, Usage: &ai.TokenUsage{Total: 20}},
		{Model: "deepseek-reasoner", Error: "请求超时，稍后再试"},
	}, nil
}

func (f *fakeAI) Diagnose(_ context.Context, tag string) (*ai.Diagnosis, error) {
	if tag == "missing" {
		return nil, kit.Fail("未找到指定的 AI 提供商")
	}
	return &ai.Diagnosis{Provider: ai.ProviderIdentity{Tag: tag, Type: "openai", DisplayName: "OpenAI"},
		Balance: ai.Balance{Status: "ok", Fields: []ai.BalanceField{{Label: "套餐", Value: "Plus"}}},
		Models:  []string{"gpt-4o", "gpt-4o-mini"},
		Chat:    &ai.Probe{Model: "gpt-4o-mini", OK: true, Text: "ok", Elapsed: 321 * time.Millisecond, Usage: &ai.TokenUsage{Prompt: 5, Completion: 1, Total: 6}}}, nil
}

func (f *fakeAI) ImportProvider(value ai.ProviderImport) (string, bool, error) {
	if f.reject != nil {
		if err := f.reject(value); err != nil {
			return "", false, err
		}
	}
	f.imported = append(f.imported, value)
	return value.Tag, true, nil
}

// fakeTelegram 只接编辑和发送，记下最后显示的文字。
type fakeTelegram struct {
	mu    sync.Mutex
	edits []string
}

func respond(output bin.Decoder, value bin.Encoder) error {
	var buffer bin.Buffer
	if err := value.Encode(&buffer); err != nil {
		return err
	}
	return output.Decode(&buffer)
}

func (f *fakeTelegram) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch request := input.(type) {
	case *tg.MessagesEditMessageRequest:
		f.edits = append(f.edits, request.Message)
		return respond(output, &tg.Updates{})
	case *tg.MessagesSendMessageRequest:
		f.edits = append(f.edits, request.Message)
		return respond(output, &tg.UpdateShortSentMessage{ID: 100})
	}
	return tgerr.New(400, "UNEXPECTED_REQUEST")
}

func (f *fakeTelegram) last() string { return f.edits[len(f.edits)-1] }

func run(t *testing.T, s *service, args ...string) (*fakeTelegram, error) {
	t.Helper()
	fake := &fakeTelegram{}
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	text := ".checkapi " + strings.Join(args, " ")
	inv := &command.Invocation{Prefix: ".", Command: "checkapi", Args: args, Text: text, Client: client,
		Message: &bot.Message{ID: 7, Peer: &tg.PeerUser{UserID: 1}, ChatID: "1", Text: text, Out: true, Saved: true},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil))}
	return fake, s.handle(context.Background(), inv)
}

func newService(t *testing.T, models *fakeAI) *service {
	return &service{models: models, store: store.New(filepath.Join(t.TempDir(), "checkapi.json"), defaults)}
}

func TestSubcommands(t *testing.T) {
	models := &fakeAI{
		providers: []ai.ProviderSummary{{Tag: "main", Type: "auto", Models: map[string]string{"chat": "gpt-4o"}}, {Tag: "dead", Type: "gemini"}},
		models:    map[string][]string{"main": {"a", "b", "c"}},
	}
	s := newService(t, models)

	fake, err := run(t, s, "list")
	if err != nil || fake.last() != "🩺 AI 接口\n\n• main · auto · chat=gpt-4o\n• dead · gemini" {
		t.Errorf("list：%q %v", fake.last(), err)
	}
	fake, err = run(t, s, "check")
	if err != nil || !strings.Contains(fake.last(), "main · ✅ 3 个模型") || !strings.Contains(fake.last(), "dead · ❌ AI 接口返回 HTTP 401") {
		t.Errorf("check：%q %v", fake.last(), err)
	}
	fake, err = run(t, s, "check", "main")
	if err != nil || fake.last() != "✅ 已连通 main，共 3 个模型" {
		t.Errorf("check main：%q %v", fake.last(), err)
	}
	fake, err = run(t, s, "models", "main")
	if err != nil || fake.last() != "🩺 模型列表\nmain · 3 个\n\na\nb\nc" {
		t.Errorf("models：%q %v", fake.last(), err)
	}
	fake, err = run(t, s, "ask", "main")
	if err != nil || fake.last() != "🩺 测试提问（main）\n\nhello <world>" || models.chats[0] != (ai.ChatRequest{Tag: "main", Text: "say hello", MaxOutputTokens: 100}) {
		t.Errorf("ask：%q %v %+v", fake.last(), err, models.chats)
	}
	fake, err = run(t, s, "speed", "main")
	if err != nil || !strings.Contains(fake.last(), "✅ deepseek-chat：800 ms（25.0 tok/s）") || !strings.Contains(fake.last(), "❌ deepseek-reasoner：请求超时，稍后再试") {
		t.Errorf("speed：%q %v", fake.last(), err)
	}
	fake, err = run(t, s, "compare", "main", "other")
	if err != nil || strings.Count(fake.last(), "OpenAI（openai）") != 2 || !strings.Contains(fake.last(), "✅ 回复「ok」，321 ms · gpt-4o-mini") ||
		!strings.Contains(fake.last(), "Token：输入 5 · 输出 1 · 合计 6") || !strings.Contains(fake.last(), "共 2 个\ngpt-4o · gpt-4o-mini") {
		t.Errorf("compare：%q %v", fake.last(), err)
	}
	if _, err := run(t, s, "compare", "main", "missing"); err == nil {
		t.Error("比较不存在的标签应该报错")
	}
	for _, args := range [][]string{{"models"}, {"speed"}, {"ask"}, {"compare", "main"}} {
		_, err := run(t, s, args...)
		if text, ok := kit.IsUserError(err); !ok || !strings.HasPrefix(text, "用法：.checkapi ") {
			t.Errorf("%v 缺参数：%v", args, err)
		}
	}
	if _, err := run(t, s, "nope"); err == nil {
		t.Error("未知子命令应该报错")
	}
	fake, err = run(t, s)
	if err != nil || !strings.HasPrefix(fake.last(), "🩺 API 检测") {
		t.Errorf("不带参数应显示帮助：%q %v", fake.last(), err)
	}
}

func TestRenderDiagnosisFailures(t *testing.T) {
	result := &ai.Diagnosis{Provider: ai.ProviderIdentity{Tag: "x", Type: "anthropic", DisplayName: "Anthropic"},
		Balance: ai.Balance{Status: "invalid"}, ModelsError: "请求超时，稍后再试",
		Chat: &ai.Probe{Model: "claude", Error: "AI 接口返回 HTTP 401：invalid x-api-key"}}
	text := strings.Join(diagnosisLines(result), "\n")
	for _, want := range []string{"⚠️ Key 无效或没有权限", "❌ <code>claude</code>：AI 接口返回 HTTP 401：invalid x-api-key", "❌ 获取失败：请求超时，稍后再试"} {
		if !strings.Contains(text, want) {
			t.Errorf("缺少 %q：\n%s", want, text)
		}
	}
	if text := strings.Join(diagnosisLines(&ai.Diagnosis{Balance: ai.Balance{Status: "unsupported"}}), "\n"); !strings.Contains(text, "请到官网查看余额") || !strings.Contains(text, "❌ 没有配置聊天模型") {
		t.Errorf("没有模型时：\n%s", text)
	}
}

// 旧 Key：并进 ai 后清空；格式不对的跳过；并过一次就不再并。
func TestMigrateLegacyKeys(t *testing.T) {
	models := &fakeAI{}
	s := newService(t, models)
	raw := `{"schemaVersion":2,"entries":[{"name":"My Relay!","key":"sk-1","baseUrl":"https://relay.example/v1","addedAt":1},
		{"name":"bad","key":"","baseUrl":"https://x"},{"name":"ftp","key":"k","baseUrl":"ftp://x"},"junk",
		{"name":"---","key":"sk-2","baseUrl":"http://second.example"}],"legacyImported":true,"aiMigrated":false}`
	if err := writeState(s, raw); err != nil {
		t.Fatal(err)
	}
	count, err := s.migrate(quiet)
	if err != nil || count != 2 {
		t.Fatalf("并入：%d %v", count, err)
	}
	if first := models.imported[0]; first.Tag != "My-Relay" || first.Key != "sk-1" || first.Type != "openai-compatible" ||
		first.Models["chat"] != "gpt-4o-mini" || len(first.Select) != 1 || first.Select[0] != "chat" {
		t.Errorf("第一条：%+v", first)
	}
	if second := models.imported[1]; second.Tag != "checkapi-2" || len(second.Select) != 0 {
		t.Errorf("第二条：%+v", second)
	}
	current, _ := s.store.Read()
	if !current.AIMigrated || len(current.Entries) != 0 {
		t.Errorf("并完没有清空：%+v", current)
	}
	if count, _ := s.migrate(quiet); count != 0 || len(models.imported) != 2 {
		t.Error("并过一次不该再并")
	}
}

// 文件不存在时什么也不写。
func TestMigrateWithoutFile(t *testing.T) {
	s := newService(t, &fakeAI{})
	if count, err := s.migrate(quiet); count != 0 || err != nil {
		t.Fatal(count, err)
	}
	if matches, _ := filepath.Glob(s.store.Path()); len(matches) != 0 {
		t.Error("没有要并的也写了文件")
	}
}

func TestConvertLegacy(t *testing.T) {
	// v1 按 Key 前缀认出的记录没有 baseUrl，也要留下，并入时按 provider 补地址。
	converted, err := ConvertLegacy([]byte(`[{"name":"a","key":"k","baseUrl":"https://a.example/v1","addedAt":5},{"name":"b"},
		{"name":"c","key":"sk-ant-x","provider":"anthropic"},{"name":"d","key":"x","provider":"cohere"}]`))
	if err != nil {
		t.Fatal(err)
	}
	var value state
	if err := json.Unmarshal(converted, &value); err != nil {
		t.Fatal(err)
	}
	if value.SchemaVersion != 2 || !value.LegacyImported || value.AIMigrated || len(value.Entries) != 2 {
		t.Errorf("转换结果：%s", converted)
	}
	if _, err := ConvertLegacy([]byte(`{}`)); err == nil {
		t.Error("不是数组应该报错")
	}
}

func writeState(s *service, raw string) error {
	var value state
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return err
	}
	return s.store.Update(func(current *state) error { *current = value; return nil })
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// v1 按 Key 前缀认出服务商时不存地址：按 provider 补上地址、类型和模型；Gemini 只有域名时补上
// /v1beta；认不出又没有地址的跳过。
func TestMigrateFillsProviders(t *testing.T) {
	models := &fakeAI{}
	s := newService(t, models)
	raw := `{"schemaVersion":2,"entries":[{"name":"claude","key":"sk-ant-1","provider":"anthropic","addedAt":1},
		{"name":"g","key":"AIza1","baseUrl":"https://generativelanguage.googleapis.com","provider":"gemini"},
		{"name":"co","key":"co-1","provider":"cohere"},
		{"name":"relay","key":"sk-2","baseUrl":"https://my-anthropic-proxy.example/v1"},
		{"name":"official","key":"sk-3","provider":"openai"}],"legacyImported":true,"aiMigrated":false}`
	if err := writeState(s, raw); err != nil {
		t.Fatal(err)
	}
	if count, err := s.migrate(quiet); err != nil || count != 4 {
		t.Fatalf("%d %v", count, err)
	}
	want := []ai.ProviderImport{
		{Tag: "claude", URL: "https://api.anthropic.com", Type: "anthropic", Models: map[string]string{"chat": "claude-3-5-haiku-20241022"}},
		{Tag: "g", URL: "https://generativelanguage.googleapis.com/v1beta", Type: "gemini", Models: map[string]string{"chat": "gemini-2.5-flash"}},
		{Tag: "relay", URL: "https://my-anthropic-proxy.example/v1", Type: "anthropic", Models: map[string]string{"chat": "claude-3-5-haiku-20241022"}},
		{Tag: "official", URL: "https://api.openai.com/v1", Type: "openai", Models: map[string]string{"chat": "gpt-4o-mini"}},
	}
	for index, item := range want {
		got := models.imported[index]
		if got.Tag != item.Tag || got.URL != item.URL || got.Type != item.Type || got.Models["chat"] != item.Models["chat"] {
			t.Errorf("第 %d 条：%+v", index+1, got)
		}
	}
}

// 被 ai 拒绝的记录跳过，照样记下已迁移，下次不再导入；配置文件出错时不记，下次再试。
func TestMigrateSkipsRejected(t *testing.T) {
	models := &fakeAI{reject: func(value ai.ProviderImport) error {
		if value.Tag == "bad" {
			return kit.Fail("导入地址无效")
		}
		return nil
	}}
	s := newService(t, models)
	raw := `{"schemaVersion":2,"entries":[{"name":"bad","key":"k1","baseUrl":"https://a.example"},{"name":"good","key":"k2","baseUrl":"https://b.example"}],"aiMigrated":false}`
	if err := writeState(s, raw); err != nil {
		t.Fatal(err)
	}
	if count, err := s.migrate(quiet); err != nil || count != 1 || len(models.imported) != 1 || models.imported[0].Tag != "good" {
		t.Fatalf("%d %v %+v", count, err, models.imported)
	}
	if current, _ := s.store.Read(); !current.AIMigrated || len(current.Entries) != 0 {
		t.Errorf("有一条被拒也应记下已迁移：%+v", current)
	}

	broken := newService(t, &fakeAI{reject: func(ai.ProviderImport) error { return errors.New("disk full") }})
	if err := writeState(broken, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := broken.migrate(quiet); err == nil {
		t.Fatal("配置文件出错应该中止")
	}
	if current, _ := broken.store.Read(); current.AIMigrated || len(current.Entries) != 2 {
		t.Errorf("中止时不该记下已迁移：%+v", current)
	}
}
