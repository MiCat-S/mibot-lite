package commands

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
)

// delegateHarness 是一个只装了几个假命令和 sudo、sure 的应用，接在假 Telegram 上。
// 假命令只记下自己被执行过：参数是什么、是不是替别人执行的。
type delegateHarness struct {
	t      *testing.T
	app    *app.App
	fake   *fakeTelegram
	client *bot.Client
	mu     sync.Mutex
	ran    []string
}

const delegateGroup = "-1005000"

func newDelegateHarness(t *testing.T, messages ...*tg.Message) *delegateHarness {
	t.Helper()
	// 代发改成当场执行、触发消息立刻删，好让测试按顺序看到全部请求。
	background, delay := inBackground, sureDelay
	inBackground, sureDelay = func(work func()) { work() }, 0
	t.Cleanup(func() { inBackground, sureDelay = background, delay })

	h := &delegateHarness{t: t}
	h.app = &app.App{Root: t.TempDir(), Registry: command.New([]string{"."}, slog.New(slog.NewTextHandler(io.Discard, nil)))}
	for _, name := range []string{"ping", "ban", "da", "ai", "sb"} {
		h.app.Registry.Register(&command.Command{Name: name, Handle: func(_ context.Context, inv *command.Invocation) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			entry := strings.TrimSpace(name + " " + strings.Join(inv.Args, " "))
			if inv.Trigger != nil {
				entry += fmt.Sprintf("（替 #%d，代发的是 #%d）", inv.Trigger.ID, inv.Message.ID)
			}
			h.ran = append(h.ran, entry)
			return nil
		}})
	}
	Delegate(h.app)
	h.fake = newFakeTelegram(t, dmeSelf, messages...)
	h.client = fakeClient(h.fake, []tg.UserClass{withHash(&tg.User{ID: dmeOther, FirstName: "小明"})}, supergroup)
	return h
}

// manage 以本人身份执行一条 .sudo 或 .sure，返回命令消息最后被改成的样子。
func (h *delegateHarness) manage(text string, replyTo int) string {
	h.t.Helper()
	route, ok := h.app.Registry.Parse(text)
	if !ok {
		h.t.Fatalf("%q 不是命令", text)
	}
	handler, _ := h.app.Registry.Lookup(route.Command)
	message := &bot.Message{ID: 900, Peer: dmeSupergroup, ChatID: delegateGroup, Out: true, ReplyToID: replyTo,
		Sender: &tg.PeerUser{UserID: dmeSelf}}
	inv := fakeInvocation(h.client, message, route.Args...)
	inv.Text, inv.Prefix = route.Text, route.Prefix
	before := len(h.fake.calls)
	if err := handler.Handle(context.Background(), inv); err != nil {
		h.t.Fatalf("%s：%v", text, err)
	}
	if len(h.fake.calls) == before {
		h.t.Fatalf("%s 没有回应", text)
	}
	return h.fake.calls[len(h.fake.calls)-1]
}

// foreign 模拟群里别人发了一条消息，返回有没有被接手，以及因此发出的请求。
func (h *delegateHarness) foreign(id int, from int64, text string, replyTo int) (bool, []string) {
	h.t.Helper()
	before := len(h.fake.calls)
	message := &bot.Message{ID: id, Peer: dmeSupergroup, ChatID: delegateGroup, Sender: &tg.PeerUser{UserID: from},
		Text: text, ReplyToID: replyTo, ChatType: bot.ChatSupergroup}
	h.fake.messages[id] = chatMessage(id, dmeSupergroup, &tg.PeerUser{UserID: from}, false, text, 0)
	handled := h.app.OfferForeign(context.Background(), h.client, message)
	h.app.Registry.Wait(time.Second)
	return handled, slices.Clone(h.fake.calls[before:])
}

func (h *delegateHarness) takeRan() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ran := h.ran
	h.ran = nil
	return ran
}

func expectCalls(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s\n实际请求：\n  %s\n应为：\n  %s", label, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func expectRan(t *testing.T, label string, h *delegateHarness, want ...string) {
	t.Helper()
	if got := h.takeRan(); !slices.Equal(got, want) {
		t.Errorf("%s：执行了 %q，应为 %q", label, got, want)
	}
}

func TestSudoRelaysOnlyDelegableCommands(t *testing.T) {
	h := newDelegateHarness(t)
	if reply := h.manage(".sudo add 200", 0); !strings.Contains(reply, "已授权") || !strings.Contains(reply, "小明") {
		t.Fatalf("授权的回复不对：%s", reply)
	}

	handled, calls := h.foreign(10, dmeOther, ".ping", 5)
	if !handled {
		t.Fatal("名单里的人发 .ping 应该被接手")
	}
	expectCalls(t, ".ping 以账号身份发出、回复同一个目标，再执行", calls,
		`send ".ping" to channel5000 reply=5`, "get [2000]")
	expectRan(t, ".ping", h, "ping（替 #10，代发的是 #2000）")

	// 代发的那条万一又当作本人的消息收到，不能再执行一次。
	sent, _ := bot.Envelope(h.fake.messages[2000], dmeSelf, false, h.client.Peers())
	if h.app.Registry.Dispatch(context.Background(), h.client, sent) {
		t.Error("代发过的消息被当成本人的命令又执行了一次")
	}

	for _, refused := range []struct{ text, shown string }{
		{".da true", ".da"},
		{".sudo add 300", ".sudo"},
		{".sb 300", ".sb"},
		{".ai config key sk-xxx", ".ai"},
	} {
		handled, calls := h.foreign(11, dmeOther, refused.text, 5)
		if !handled {
			t.Errorf("%s 应该被接手（然后拒绝）", refused.text)
		}
		expectCalls(t, refused.text+" 只回一句没有权限，不以账号身份发出", calls,
			fmt.Sprintf(`send "⛔ %s 只有账号本人能用" to channel5000 reply=11`, refused.shown))
		expectRan(t, refused.text, h)
	}

	// 能借的命令，不改设置的子命令照常执行。前面四句拒绝占了 2001 到 2004。
	h.foreign(12, dmeOther, ".ai 你好", 0)
	expectRan(t, ".ai 你好", h, "ai 你好（替 #12，代发的是 #2005）")
}

// 别名展开之后再判断：把只限本人的命令起个别名，名单里的人照样用不了。
func TestSudoChecksAliasTargets(t *testing.T) {
	h := newDelegateHarness(t)
	h.manage(".sudo add 200", 0)
	h.app.Registry.SetAliases(map[string]string{"wipe": "da true", "p": "ping"})

	_, calls := h.foreign(10, dmeOther, ".wipe", 0)
	expectCalls(t, ".wipe 是 .da 的别名", calls, `send "⛔ .da 只有账号本人能用" to channel5000 reply=10`)
	expectRan(t, ".wipe", h)

	h.foreign(11, dmeOther, ".p", 0)
	expectRan(t, ".p 是 .ping 的别名", h, "ping（替 #11，代发的是 #2001）")
}

func TestSudoIgnoresWhatItShould(t *testing.T) {
	h := newDelegateHarness(t)
	h.manage(".sudo add 200", 0)
	for _, c := range []struct {
		from int64
		text string
		why  string
	}{
		{300, ".ping", "不在名单里的人"},
		{dmeOther, "你好", "不是命令"},
		{dmeOther, ".nosuch", "不认得的命令也不能照发，否则名单里的人能让账号说任何话"},
		{dmeOther, "/ping", "前缀不对"},
	} {
		if handled, calls := h.foreign(10, c.from, c.text, 0); handled || len(calls) > 0 {
			t.Errorf("%s（%q）应该被忽略，实际请求 %v", c.why, c.text, calls)
		}
	}

	// 设了对话名单，名单外的对话里不管用。
	if reply := h.manage(".sudo chat add -1009999", 0); !strings.Contains(reply, "已加入") {
		t.Fatalf("加对话名单的回复不对：%s", reply)
	}
	if handled, _ := h.foreign(11, dmeOther, ".ping", 0); handled {
		t.Error("对话名单外的群里不该生效")
	}
	h.manage(".sudo chat add", 0)
	if handled, _ := h.foreign(12, dmeOther, ".ping", 0); !handled {
		t.Error("把当前群加进对话名单之后应该生效")
	}
	expectRan(t, "对话名单", h, "ping（替 #12，代发的是 #2000）")

	h.manage(".sudo del 200", 0)
	if handled, _ := h.foreign(13, dmeOther, ".ping", 0); handled {
		t.Error("取消授权之后不该再生效")
	}
}

func TestSudoManagement(t *testing.T) {
	h := newDelegateHarness(t, chatMessage(5, dmeSupergroup, &tg.PeerUser{UserID: dmeOther}, false, "hi", 0))
	if reply := h.manage(".sudo ls", 0); !strings.Contains(reply, "没有任何用户") {
		t.Errorf("空名单：%s", reply)
	}
	if reply := h.manage(".sudo add", 5); !strings.Contains(reply, "已授权") || !strings.Contains(reply, "200") {
		t.Errorf("回复消息授权：%s", reply)
	}
	if reply := h.manage(".sudo add", 0); !strings.Contains(reply, "回复对方的消息") {
		t.Errorf("没回复也没参数应该给出用法：%s", reply)
	}
	if reply := h.manage(".sudo add 100", 0); !strings.Contains(reply, "账号自己") {
		t.Errorf("不该能把自己加进去：%s", reply)
	}
	if reply := h.manage(".sudo add -1005000", 0); !strings.Contains(reply, "不是用户") {
		t.Errorf("群 ID 不是用户：%s", reply)
	}
	if reply := h.manage(".sudo ls", 0); !strings.Contains(reply, "小明") || !strings.Contains(reply, "200") {
		t.Errorf("名单：%s", reply)
	}
	if reply := h.manage(".sudo chat ls", 0); !strings.Contains(reply, "所有对话里都能用") {
		t.Errorf("没设对话名单要提醒：%s", reply)
	}
	if reply := h.manage(".sudo del 200", 0); !strings.Contains(reply, "已取消授权") {
		t.Errorf("取消授权：%s", reply)
	}
	if reply := h.manage(".sudo del 200", 0); !strings.Contains(reply, "名单里没有") {
		t.Errorf("重复取消：%s", reply)
	}
	if reply := h.manage(".sudo", 0); !strings.Contains(reply, "能借出去的命令") {
		t.Errorf("帮助：%s", reply)
	}
}

func TestSureRules(t *testing.T) {
	h := newDelegateHarness(t)
	h.manage(".sure add 200", 0)
	if handled, _ := h.foreign(10, dmeOther, "/sb 123", 5); handled {
		t.Error("没有规则时 sure 不该生效")
	}

	if reply := h.manage(".sure msg add _command:/sb", 0); !strings.Contains(reply, "_command:/sb") {
		t.Fatalf("加规则：%s", reply)
	}
	if reply := h.manage(".sure msg redirect 1 .ban", 0); !strings.Contains(reply, "重定向到") {
		t.Fatalf("设重定向：%s", reply)
	}
	h.manage(".sure msg add 早  安", 0)
	h.manage(".sure msg add _command:/wipe", 0)
	h.manage(".sure msg redirect 3 .da true", 0)
	if reply := h.manage(".sure msg ls", 0); !strings.Contains(reply, "/sb") || !strings.Contains(reply, ".ban") || !strings.Contains(reply, "早  安") {
		t.Errorf("规则列表：%s", reply)
	}

	// 典型用法：群友回复某人发 /sb，账号回复同一个人发 .ban 并执行，触发的那条删掉。
	handled, calls := h.foreign(10, dmeOther, "/sb 123", 5)
	if !handled {
		t.Fatal("/sb 123 应该匹配 _command:/sb")
	}
	expectCalls(t, "/sb 123 重定向成 .ban 123", calls,
		`send ".ban 123" to channel5000 reply=5`, "get [2000]", "delete [10]")
	expectRan(t, "/sb 123", h, "ban 123（替 #10，代发的是 #2000）")

	if handled, _ := h.foreign(11, dmeOther, "/sbx", 5); handled {
		t.Error("/sbx 不该匹配 /sb")
	}

	// 原文规则：整条一致才算，照原样发出，不是命令就不执行。
	_, calls = h.foreign(12, dmeOther, "早  安", 0)
	expectCalls(t, "原文规则", calls, `send "早  安" to channel5000`, "delete [12]")
	expectRan(t, "原文规则", h)
	if handled, _ := h.foreign(13, dmeOther, "早安", 0); handled {
		t.Error("原文规则只认一字不差的")
	}

	// 重定向到只限本人的命令：不执行，只回没有权限。
	_, calls = h.foreign(14, dmeOther, "/wipe", 0)
	expectCalls(t, "重定向到 .da", calls, `send "⛔ .da 只有账号本人能用" to channel5000 reply=14`, "delete [14]")
	expectRan(t, "重定向到 .da", h)

	// 不在 sure 名单里的人不管用；sure 的名单和 sudo 的是两份。
	if handled, _ := h.foreign(15, 300, "/sb 1", 5); handled {
		t.Error("不在 sure 名单里的人不该生效")
	}
	if handled, _ := h.foreign(16, dmeOther, ".ping", 0); handled {
		t.Error("只在 sure 名单里的人不能像 sudo 那样发任意命令")
	}

	if reply := h.manage(".sure msg del 1", 0); !strings.Contains(reply, "已删除") {
		t.Errorf("删规则：%s", reply)
	}
	if reply := h.manage(".sure msg del 1", 0); !strings.Contains(reply, "没有编号为 1") {
		t.Errorf("重复删：%s", reply)
	}
	if handled, _ := h.foreign(17, dmeOther, "/sb 1", 5); handled {
		t.Error("删掉规则之后不该再生效")
	}
}

func TestSureRuleMatching(t *testing.T) {
	rules := delegateDocument{Messages: []sureRule{
		{ID: 1, Msg: "_command:/sb", Redirect: ".ban"},
		{ID: 2, Msg: "_command:/ping"},
		{ID: 3, Msg: "晚安", Redirect: ".ping"},
	}}
	for _, c := range []struct {
		text, want string
		ok         bool
	}{
		{"/sb", ".ban", true},
		{"/sb 1 2", ".ban 1 2", true},
		{"/sbx", "", false},
		{" /sb", "", false},
		{"/ping now", "/ping now", true},
		{"晚安", ".ping", true},
		{"晚安 ", "", false},
	} {
		got, ok := rules.match(c.text)
		if got != c.want || ok != c.ok {
			t.Errorf("%q → %q %v，应为 %q %v", c.text, got, ok, c.want, c.ok)
		}
	}
}

// 表里写的命令都得真的存在；拼错了就等于悄悄少借出去一个。
func TestDelegableNamesExist(t *testing.T) {
	a := &app.App{Root: t.TempDir(), Registry: command.New([]string{"."}, slog.New(slog.NewTextHandler(io.Discard, nil)))}
	RegisterAll(a)
	for name := range delegable {
		if _, ok := a.Registry.Lookup(name); !ok {
			t.Errorf("delegable 里的 %q 不是已注册的命令", name)
		}
	}
	for _, ownerOnly := range []string{"sudo", "sure", "dme", "da", "acn", "autochangename", "prefix", "alias", "bf", "log", "save", "restart", "update", "sb", "unsb", "sysinfo", "refresh", "aban"} {
		if _, ok := delegable[ownerOnly]; ok {
			t.Errorf("%q 不该能借出去", ownerOnly)
		}
	}
}
