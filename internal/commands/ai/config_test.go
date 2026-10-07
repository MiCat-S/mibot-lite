package ai

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// lastEdit 冒充 Telegram，记下命令消息最后被改成什么。
type lastEdit struct{ text string }

func (l *lastEdit) Invoke(_ context.Context, input bin.Encoder, _ bin.Decoder) error {
	if edit, ok := input.(*tg.MessagesEditMessageRequest); ok {
		l.text = edit.Message
	}
	return nil
}

// runConfig 在收藏夹（saved 为真）或者别的对话里执行一条 .ai 命令，返回错误和改成的文字。
func runConfig(t *testing.T, service *Service, saved bool, args ...string) (string, error) {
	t.Helper()
	edits := &lastEdit{}
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	client := bot.FromAPI(tg.NewClient(edits), peers, &tg.User{ID: 1}, slog.New(slog.DiscardHandler))
	message := &bot.Message{ID: 5, Peer: &tg.PeerUser{UserID: 1}, ChatID: "1", Saved: saved}
	inv := &command.Invocation{Prefix: ".", Command: "ai", Args: args, Message: message, Client: client, Log: slog.New(slog.DiscardHandler)}
	err := service.configure(context.Background(), inv)
	return edits.text, err
}

func TestConfigSet(t *testing.T) {
	service := &Service{store: store.New(filepath.Join(t.TempDir(), "ai.json"), aiDefaults)}
	if _, err := runConfig(t, service, true, "config", "add", "main", "https://api.example.com/v1", "sk-old"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "set", "main", "stream", "on"},
		{"config", "set", "main", "responses", "on"},
		{"config", "set", "main", "type", "Gemini"},
		{"config", "set", "main", "url", "https://new.example.com/v1"},
		{"config", "set", "main", "key", "sk-new"},
	} {
		if text, err := runConfig(t, service, true, args...); err != nil || !strings.HasPrefix(text, "✅ main ") {
			t.Fatalf("%v：%q %v", args, text, err)
		}
	}
	cfg, _ := service.read()
	got := cfg.Configs["main"]
	if got.URL != "https://new.example.com/v1" || got.Key != "sk-new" || got.Type != "gemini" || !got.Stream || !got.Responses {
		t.Fatalf("改完的配置不对：%+v", got)
	}

	// 旧写法照样能用。
	if _, err := runConfig(t, service, false, "config", "stream", "main", "off"); err != nil {
		t.Fatal(err)
	}
	// 同名再 add：换地址和 Key，流式、Responses、模型沿用。
	if err := service.update(func(cfg *aiConfig) error {
		provider := cfg.Configs["main"]
		provider.Models = map[string]string{"chat": "m1"}
		cfg.Configs["main"] = provider
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	text, err := runConfig(t, service, true, "config", "add", "main", "https://third.example.com", "sk-3")
	if err != nil || !strings.Contains(text, "已更新 main") {
		t.Fatalf("覆盖：%q %v", text, err)
	}
	cfg, _ = service.read()
	got = cfg.Configs["main"]
	if got.URL != "https://third.example.com" || got.Key != "sk-3" || got.Stream || !got.Responses || got.Models["chat"] != "m1" {
		t.Errorf("覆盖后应沿用 Responses 和模型：%+v", got)
	}

	// 列表只显示主机名，不显示路径和 Key。
	list, err := runConfig(t, service, true, "config", "list")
	if err != nil || !strings.Contains(list, "third.example.com") || strings.Contains(list, "sk-3") {
		t.Errorf("列表：%q %v", list, err)
	}
}

func TestConfigSetRejects(t *testing.T) {
	service := &Service{store: store.New(filepath.Join(t.TempDir(), "ai.json"), aiDefaults)}
	if _, err := runConfig(t, service, true, "config", "add", "main", "https://api.example.com", "sk"); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		saved bool
		args  []string
		want  string
	}{
		"群里改 Key": {false, []string{"config", "set", "main", "key", "sk-leak"}, "只能在收藏夹中修改"},
		"群里改地址":   {false, []string{"config", "set", "main", "url", "https://x.example.com"}, "只能在收藏夹中修改"},
		"标签不存在":   {true, []string{"config", "set", "nope", "stream", "on"}, "没有标签是 nope 的 API 配置"},
		"不认识的项":   {true, []string{"config", "set", "main", "model", "x"}, "能改的有 url、key"},
		"地址不完整":   {true, []string{"config", "set", "main", "url", "api.example.com"}, "API 地址无效"},
		"类型不对":    {true, []string{"config", "set", "main", "type", "foo"}, "无效 API 类型"},
		"开关不对":    {true, []string{"config", "set", "main", "stream", "maybe"}, ""},
		"缺参数":     {true, []string{"config", "set", "main", "url"}, "用法"},
	} {
		_, err := runConfig(t, service, c.saved, c.args...)
		message, ok := kit.IsUserError(err)
		if !ok || !strings.Contains(message, c.want) {
			t.Errorf("%s：%v，应包含 %q", name, err, c.want)
		}
	}
	cfg, _ := service.read()
	if got := cfg.Configs["main"]; got.Key != "sk" || got.URL != "https://api.example.com" {
		t.Errorf("被拒的修改不该生效：%+v", got)
	}
}
