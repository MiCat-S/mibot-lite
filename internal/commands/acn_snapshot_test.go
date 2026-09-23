package commands

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// 每一串命令在一个全新的环境里依次执行，覆盖 .acn 的每个子命令和它们的出错分支。
var acnSequences = [][]string{
	{"", "help", "status"},
	{"on"},
	{"save", "config", "mode", "mode", "mode", "mode", "config"},
	{"save", "tz list", "tz on", "tz format utc", "tz format custom:东八区", "tz format bad", "tz Asia/Tokyo",
		"tz set Europe/London", "tz Nowhere/Nope", "tz off", "config"},
	{"save", "emoji on", "time off", "emoji maybe", "style mono", "style weird", "style normal",
		"order", "order time,name,time", "order bogus", "config"},
	{"save", "on", "update", "now", "off", "reset", "config", "nonsense"},
	{"save", "text list", "text add 你好", "text add 世界", "text list", "config"},
}

var digits = regexp.MustCompile(`\d+`)

// runAcn 把一串命令喂给指定的处理函数，返回发出的请求和最后存下的配置。
// 昵称和更新时间里带着当前时刻，数字统一换成 # 再比。
func runAcn(t *testing.T, sequence []string, handle func(context.Context, *acnService, *command.Invocation) error) ([]string, string) {
	t.Helper()
	fake := newFakeTelegram(t, dmeSelf)
	client := fakeClient(fake, otherUser, nil)
	path := filepath.Join(t.TempDir(), "acn.json")
	service := &acnService{store: store.New(path, acnDefaults)}
	for index, line := range sequence {
		message := &bot.Message{ID: dmeCommand + index, Peer: dmePrivate, ChatID: bot.PeerID(dmePrivate), Out: true}
		if err := handle(context.Background(), service, fakeInvocation(client, message, strings.Fields(line)...)); err != nil {
			fake.log("error %v", err)
		}
	}
	raw, _ := os.ReadFile(path)
	calls := make([]string, len(fake.calls))
	for index, call := range fake.calls {
		calls[index] = digits.ReplaceAllString(call, "#")
	}
	return calls, digits.ReplaceAllString(string(raw), "#")
}

// TestAcnSnapshot 把每一串命令发出的请求和存下的配置与快照比较。
func TestAcnSnapshot(t *testing.T) {
	var out strings.Builder
	for _, sequence := range acnSequences {
		calls, stored := runAcn(t, sequence, func(ctx context.Context, service *acnService, inv *command.Invocation) error {
			return acnHandle(ctx, inv, service)
		})
		out.WriteString(section(strings.Join(sequence, " | "), calls, stored))
	}
	golden(t, "acn", out.String())
}
