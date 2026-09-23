package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// sumStep 是一条 config 命令，以及它是在收藏夹还是别的对话里发的：涉及 API Key 的只允许在收藏夹。
type sumStep struct {
	line  string
	saved bool
}

var sumSequences = [][]sumStep{
	{{"config", true}, {"config list", true}, {"config nonsense", true}},
	{{"config add a https://api.example.com sk-1 gpt-4o", false}, {"config list", true}},
	{{"config add a https://api.example.com sk-1 gpt-4o", true}, {"config add b https://b.example.com sk-2 gpt-4o-mini chat", true},
		{"config add c not-a-url sk-3 gpt-4o", true}, {"config add d https://d.example.com sk-4 gpt-4o weird", true},
		{"config add e https://e.example.com", true}, {"config list", true}},
	{{"config add a https://api.example.com sk-1 gpt-4o", true}, {"config add b https://b.example.com sk-2 gpt-4o", true},
		{"config set default b", true}, {"config set default nobody", true}, {"config list", true},
		{"config del a", true}, {"config del a", true}, {"config del", true}, {"config list", true}},
	{{"config set preview on", true}, {"config set spoiler off", true}, {"config set preview maybe", true},
		{"config set reasoning high", true}, {"config set reasoning extreme", true}, {"config set service flex", true},
		{"config list", true}},
	{{"config set prompt show", true}, {"config set prompt 用三句话总结", true}, {"config set prompt show", true},
		{"config set prompt reset", true}, {"config set prompt", true}, {"config set prompt show", true}},
	{{"config add a https://api.example.com sk-1 gpt-4o", true}, {"config set a model gpt-4o-mini", true},
		{"config set a url https://new.example.com", true}, {"config set a url nope", true}, {"config set a key sk-9", false},
		{"config set a key sk-9", true}, {"config set a type gemini", true}, {"config set a type weird", true},
		{"config set a colour red", true}, {"config set ghost model x", true}, {"config list", true}},
}

func runSum(t *testing.T, sequence []sumStep, configure func(*sumService, context.Context, *command.Invocation) error) ([]string, string) {
	t.Helper()
	fake := newFakeTelegram(t, dmeSelf)
	fake.fullSelf = true
	client := fakeClient(fake, otherUser, nil)
	path := filepath.Join(t.TempDir(), "sum.json")
	service := &sumService{store: store.New(path, sumDefaults)}
	for index, step := range sequence {
		var peer tg.PeerClass = dmePrivate
		if step.saved {
			peer = &tg.PeerUser{UserID: dmeSelf}
		}
		message := &bot.Message{ID: dmeCommand + index, Peer: peer, ChatID: bot.PeerID(peer), Saved: step.saved, Out: true}
		if err := configure(service, context.Background(), fakeInvocation(client, message, strings.Fields(step.line)...)); err != nil {
			fake.log("error %v", err)
		}
	}
	raw, _ := os.ReadFile(path)
	return fake.calls, string(raw)
}

// TestSumConfigSnapshot 把每一串 config 命令的回复和存下的配置与快照比较。
func TestSumConfigSnapshot(t *testing.T) {
	var out strings.Builder
	for _, sequence := range sumSequences {
		calls, stored := runSum(t, sequence, func(s *sumService, ctx context.Context, inv *command.Invocation) error { return s.config(ctx, inv) })
		var title []string
		for _, step := range sequence {
			title = append(title, step.line)
		}
		out.WriteString(section(strings.Join(title, " | "), calls, stored))
	}
	golden(t, "sum-config", out.String())
}
