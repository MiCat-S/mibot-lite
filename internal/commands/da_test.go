package commands

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

type daScenario struct {
	name     string
	peer     tg.PeerClass
	users    []tg.UserClass
	chats    []tg.ChatClass
	messages func() []*tg.Message
	setup    func(*fakeTelegram)
	deleted  []int
	kept     []int
}

// crowd 造一段群聊：编号能被 3 整除的是自己发的，其余是别人发的。
func crowd(peer tg.PeerClass, count int) func() []*tg.Message {
	return func() []*tg.Message {
		var messages []*tg.Message
		for id := 1; id <= count; id++ {
			from := tg.PeerClass(&tg.PeerUser{UserID: dmeOther})
			if id%3 == 0 {
				from = &tg.PeerUser{UserID: dmeSelf}
			}
			messages = append(messages, chatMessage(id, peer, from, id%3 == 0, "群消息", time.Minute))
		}
		return append(messages, chatMessage(dmeCommand, peer, &tg.PeerUser{UserID: dmeSelf}, true, ".da true", 0))
	}
}

func upTo(count int, keep func(int) bool) []int {
	var ids []int
	for id := 1; id <= count; id++ {
		if keep(id) {
			ids = append(ids, id)
		}
	}
	return ids
}

func daScenarios() []daScenario {
	basic := &tg.PeerChat{ChatID: 7000}
	all := func(int) bool { return true }
	own := func(id int) bool { return id%3 == 0 }
	others := func(id int) bool { return id%3 != 0 }
	return []daScenario{
		{
			name: "管理员清空整个超级群", peer: dmeSupergroup, chats: supergroup, messages: crowd(dmeSupergroup, 250),
			setup:   func(fake *fakeTelegram) { fake.admin = true },
			deleted: append(upTo(250, all), dmeCommand),
		},
		{
			name: "普通成员只删自己的", peer: dmeSupergroup, chats: supergroup, messages: crowd(dmeSupergroup, 120),
			deleted: append(upTo(120, own), dmeCommand), kept: upTo(120, others),
		},
		{
			name: "管理员一批删不掉就逐条重试", peer: dmeSupergroup, chats: supergroup, messages: crowd(dmeSupergroup, 150),
			setup: func(fake *fakeTelegram) {
				fake.admin = true
				fake.failDelete = func(ids []int) bool { return slices.Contains(ids, 42) }
			},
			deleted: upTo(150, func(id int) bool { return id != 42 }), kept: []int{42},
		},
		{
			name: "普通群按搜索删自己的", peer: basic, chats: []tg.ChatClass{&tg.Chat{ID: 7000, Title: "普通群"}},
			messages: crowd(basic, 30),
			deleted:  append(upTo(30, own), dmeCommand), kept: upTo(30, others),
		},
		{
			name: "没有自己的消息", peer: dmeSupergroup, chats: supergroup,
			messages: func() []*tg.Message {
				return []*tg.Message{chatMessage(1, dmeSupergroup, &tg.PeerUser{UserID: dmeOther}, false, "别人的", time.Minute),
					chatMessage(dmeCommand, dmeSupergroup, &tg.PeerUser{UserID: dmeSelf}, true, ".da true", 0)}
			},
			deleted: []int{dmeCommand}, kept: []int{1},
		},
	}
}

// daOutcome 是一次运行留下的全部痕迹。
type daOutcome struct {
	calls   []string
	alive   []int
	deleted int
	errors  string
	stored  int
}

func runDa(t *testing.T, scenario daScenario) daOutcome {
	t.Helper()
	fake := newFakeTelegram(t, dmeSelf, scenario.messages()...)
	if scenario.setup != nil {
		scenario.setup(fake)
	}
	client := fakeClient(fake, scenario.users, scenario.chats)
	service := &daService{store: store.New(filepath.Join(t.TempDir(), "da.json"), func() daDB { return daDB{Tasks: []daTask{}} }),
		active: map[string]*daSlot{}}
	id := bot.PeerID(scenario.peer)
	slot := &daSlot{cancel: func() {}}
	message := &bot.Message{ID: dmeCommand, Peer: scenario.peer, ChatID: id, Out: true}
	service.run(context.Background(), client, slot, id, message)
	db, err := service.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return daOutcome{calls: fake.calls, alive: fake.alive(), deleted: slot.task.DeletedMessages,
		errors: strings.Join(slot.task.Errors, "|"), stored: len(db.Tasks)}
}

// TestDaSnapshot 把每个场景发出的请求、删除计数和存档状态与快照比较。
func TestDaSnapshot(t *testing.T) {
	var out strings.Builder
	for _, scenario := range daScenarios() {
		outcome := runDa(t, scenario)
		out.WriteString(section(scenario.name, outcome.calls,
			fmt.Sprintf("删除 %d 条，错误 %q，存档里 %d 个任务", outcome.deleted, outcome.errors, outcome.stored)))
	}
	golden(t, "da", out.String())
}

func TestDaDeletesExactlyWhatItShould(t *testing.T) {
	for _, scenario := range daScenarios() {
		outcome := runDa(t, scenario)
		for _, id := range scenario.deleted {
			if slices.Contains(outcome.alive, id) {
				t.Errorf("%s：#%d 应该被删掉", scenario.name, id)
			}
		}
		for _, id := range scenario.kept {
			if !slices.Contains(outcome.alive, id) {
				t.Errorf("%s：#%d 不该被删", scenario.name, id)
			}
		}
		// 顺利完成的任务从存档里清掉；有失败的留着，下次还能看到。
		if wantStored := map[bool]int{true: 1, false: 0}[outcome.errors != ""]; outcome.stored != wantStored {
			t.Errorf("%s：存档里留了 %d 个任务，应为 %d（错误：%q）", scenario.name, outcome.stored, wantStored, outcome.errors)
		}
	}
}
