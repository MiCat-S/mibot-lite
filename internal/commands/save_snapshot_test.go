package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// saveStep 是一条 .save 命令，reply 非零表示它回复了那条消息。
type saveStep struct {
	line  string
	reply int
}

type saveScenario struct {
	name  string
	steps []saveStep
	setup func(*fakeTelegram)
}

// groupHistory 是超级群里的几条消息：5 和 7 号不存在，6 号自带禁止转发标记，2 号带格式。
func groupHistory() []*tg.Message {
	var messages []*tg.Message
	for _, id := range []int{1, 2, 3, 4, 6, 8} {
		message := chatMessage(id, dmeSupergroup, &tg.PeerUser{UserID: dmeOther}, false, "第"+string(rune('0'+id))+"条", time.Minute)
		if id == 2 {
			message.SetEntities([]tg.MessageEntityClass{&tg.MessageEntityBold{Offset: 0, Length: 1}})
		}
		if id == 6 {
			message.Noforwards = true
		}
		messages = append(messages, message)
	}
	return messages
}

func saveScenarios() []saveScenario {
	restricted := func(fake *fakeTelegram) { fake.restrictForward = true }
	return []saveScenario{
		{name: "回复一条消息保存", steps: []saveStep{{".save", 3}}},
		{name: "按链接批量", steps: []saveStep{{"t.me/c/5000/2 t.me/c/5000/4", 0}}},
		{name: "按范围，缺号跳过，带禁止转发标记的改成复制", steps: []saveStep{{"t.me/c/5000/1|t.me/c/5000/8", 0}}},
		{name: "范围超过上限", steps: []saveStep{{"t.me/c/5000/1|t.me/c/5000/600", 0}}},
		{name: "对话禁止转发就复制，格式保留", steps: []saveStep{{"t.me/c/5000/2 t.me/c/5000/3", 0}}, setup: restricted},
		{name: "链接指向不存在的消息", steps: []saveStep{{"t.me/c/5000/7", 0}}},
		{name: "指定目标", steps: []saveStep{{"t.me/c/5000/2 -1005000", 0}}},
		{name: "本地模式跳过纯文本", steps: []saveStep{{"t.me/c/5000/2 local", 0}}},
		{name: "打开来源行", steps: []saveStep{{"source on", 0}, {"t.me/c/5000/2", 0}, {"t.me/c/5000/1|t.me/c/5000/3", 0}}},
		{name: "设置和出错", steps: []saveStep{
			{"help", 0}, {"target", 0}, {"source off", 0}, {"to", 0}, {"to -1005000", 0}, {"target", 0},
			{"to local", 0}, {"to me", 0}, {"", 0}, {"t.me/c/5000/2 @a @b", 0}, {"t.me/c/5000/1|nope", 0},
			{"t.me/c/5000/1|t.me/c/6000/2", 0}, {"t.me/c/5000/1|t.me/c/5000/2 t.me/c/5000/3", 0},
		}},
	}
}

func runSave(t *testing.T, scenario saveScenario, handle func(context.Context, *command.Invocation, *store.Store[saveDocument], string) error) ([]string, string) {
	t.Helper()
	fake := newFakeTelegram(t, dmeSelf, groupHistory()...)
	if scenario.setup != nil {
		scenario.setup(fake)
	}
	client := fakeClient(fake, otherUser, supergroup)
	root := t.TempDir()
	path := filepath.Join(root, "save.json")
	settings := store.New(path, func() saveDocument { return saveDocument{} })
	for index, step := range scenario.steps {
		message := &bot.Message{ID: dmeCommand + index, Peer: dmeSupergroup, ChatID: bot.PeerID(dmeSupergroup), Out: true, ReplyToID: step.reply}
		args := strings.Fields(strings.TrimPrefix(step.line, ".save"))
		if err := handle(context.Background(), fakeInvocation(client, message, args...), settings, root); err != nil {
			fake.log("error %v", err)
		}
	}
	raw, _ := os.ReadFile(path)
	return fake.calls, string(raw)
}

// TestSaveSnapshot 把每个场景发出的请求和存下的设置与快照比较。
func TestSaveSnapshot(t *testing.T) {
	var out strings.Builder
	for _, scenario := range saveScenarios() {
		calls, stored := runSave(t, scenario, saveHandle)
		out.WriteString(section(scenario.name, calls, stored))
	}
	golden(t, "save", out.String())
}
