package tts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

func TestCleanText(t *testing.T) {
	for input, want := range map[string]string{
		"你好😀世界":           "你好世界",
		"Hello, world!!!": "Hello, world!",
		"好好好。。。真的？？":      "好好好。真的？",
		"价格 $100 #标签 @某人": "价格 100 标签 某人",
		"  ☀️晴天✨  ":       "晴天",
		"🎉🎉":              "",
	} {
		if got := cleanText(input); got != want {
			t.Errorf("cleanText(%q) = %q，应为 %q", input, got, want)
		}
	}
}

func TestRolesAndVoice(t *testing.T) {
	config := document{Roles: map[string]string{"自己的": strings.Repeat("a", 32), "雷军": strings.Repeat("b", 32)}}
	roles := config.roles()
	if roles["雷军"] != strings.Repeat("b", 32) {
		t.Error("文件里的同名角色应该盖过自带的")
	}
	if roles["周杰伦"] == "" || len(roles) != len(builtinRoles)+1 {
		t.Errorf("角色数 %d，应为自带的 %d 个加 1", len(roles), len(builtinRoles))
	}
	names := config.roleNames()
	if names[len(names)-1] != "自己的" {
		t.Errorf("自己加的应该排在最后，实际 %v", names[len(names)-3:])
	}
	if name, id := (document{}).voice("1"); name != defaultRole || id != builtinRoles[defaultRole] {
		t.Errorf("没选过角色时应为 %s，实际 %s %s", defaultRole, name, id)
	}
	chosen := document{Users: map[string]userConfig{"1": {DefaultRole: "可莉", DefaultRoleID: "x"}}}
	if name, id := chosen.voice("1"); name != "可莉" || id != "x" {
		t.Errorf("应该用选过的角色，实际 %s %s", name, id)
	}
	if name, _ := chosen.voice("2"); name != defaultRole {
		t.Error("别的账号的设置不该用在本账号上")
	}
	if (document{}).cover("薯薯") == "" || (document{Covers: map[string]string{"可莉": "https://x"}}).cover("可莉") != "https://x" {
		t.Error("封面没取对")
	}
}

// MiBox 的 tts_data.json 原样读得进来。
func TestReadsMiBoxData(t *testing.T) {
	raw := `{"users":{"42":{"apiKey":"k","defaultRole":"丁真","defaultRoleId":"54a5170264694bfc8e9ad98df7bd89c3"}},"roles":{"丁真":"54a5170264694bfc8e9ad98df7bd89c3"},"covers":{"薯薯":"https://raw.githubusercontent.com/Yu9191/-/main/image.png"}}`
	var config document
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	if name, _ := config.voice("42"); name != "丁真" || config.Users["42"].APIKey != "k" {
		t.Errorf("读 MiBox 数据出错：%+v", config)
	}
}

func TestSynthesize(t *testing.T) {
	var got struct {
		auth, model string
		body        map[string]string
	}
	status, reply := http.StatusOK, "ID3fake-mp3"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.auth, got.model = r.Header.Get("Authorization"), r.Header.Get("model")
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &got.body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	defer server.Close()
	t.Setenv("MIBOT_FISH_ENDPOINT", server.URL)

	audio, err := synthesize(context.Background(), document{Model: "s1"}, "secret", "voice-id", "你好")
	if err != nil || string(audio) != reply {
		t.Fatalf("合成返回 %q %v", audio, err)
	}
	if got.auth != "Bearer secret" || got.model != "s1" || got.body["text"] != "你好" || got.body["reference_id"] != "voice-id" || got.body["format"] != "mp3" {
		t.Errorf("请求不对：%+v", got)
	}
	if _, err := synthesize(context.Background(), document{}, "k", "v", "x"); err != nil || got.model != "" {
		t.Errorf("没设模型时不该带 model 头：%q %v", got.model, err)
	}

	for _, c := range []struct {
		status int
		body   string
		want   string
	}{
		{401, `{"message":"Unauthorized"}`, "API Key 无效"},
		{402, `{"message":"Insufficient balance"}`, "余额不足"},
		{400, `{"message":"reference not found"}`, "reference not found"},
		{500, ``, "HTTP 500"},
	} {
		status, reply = c.status, c.body
		_, err := synthesize(context.Background(), document{}, "k", "v", "x")
		text, ok := kit.IsUserError(err)
		if !ok || !strings.Contains(text, c.want) {
			t.Errorf("HTTP %d：%v，应含 %q", c.status, err, c.want)
		}
	}
}

func TestHelpMentionsEverySubcommand(t *testing.T) {
	text := help(".")
	for _, part := range []string{".t 文本", ".t song", ".t fm", ".ts", ".tk", ".t model"} {
		if !strings.Contains(text, part) {
			t.Errorf("帮助里没有 %s", part)
		}
	}
}
