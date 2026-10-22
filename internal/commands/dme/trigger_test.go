package dme

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// 消息里包含触发词就算，不区分大小写；别的对话的触发词不算。
func TestTriggerIndexMatch(t *testing.T) {
	index := buildIndex([]dmeTrigger{
		{ChatID: "-100", Keyword: "清理", Count: 10},
		{ChatID: "-100", Keyword: ".Sum", Count: 20},
		{ChatID: "42", Keyword: "bye", Count: 30},
	})
	for _, c := range []struct {
		chat, text string
		want       int
	}{
		{"-100", "麻烦清理一下", 10},
		{"-100", "跑一下 .SUM 看看", 20},
		{"-100", "bye", 0},
		{"42", "OK BYE", 30},
		{"7", "清理", 0},
	} {
		hit, ok := index.match(c.chat, c.text)
		if got := map[bool]int{true: hit.Count}[ok]; got != c.want {
			t.Errorf("match(%s, %q) 命中条数 %d，应为 %d", c.chat, c.text, got, c.want)
		}
	}
}

func keywordInvocation(text string) *command.Invocation {
	fields := strings.Fields(strings.TrimPrefix(text, "."))
	return &command.Invocation{Prefix: ".", Command: fields[0], Args: fields[1:], Text: text, Message: &bot.Message{ChatID: "-100"}}
}

func TestParseTrigger(t *testing.T) {
	for text, want := range map[string]dmeTrigger{
		".dme kw 100 暗号":             {ChatID: "-100", Keyword: "暗号", Count: 100},
		".dme kw -f 50 hello  world": {ChatID: "-100", Keyword: "hello  world", Count: 50, Anti: true},
		".dme kw 5000 x":             {ChatID: "-100", Keyword: "x", Count: 2000},
		".dme kw 999999 .sum":        {ChatID: "-100", Keyword: ".sum", Count: allMessages},
	} {
		got, err := parseTrigger(keywordInvocation(text))
		if err != nil || got != want {
			t.Errorf("%q 解析成 %+v（%v），应为 %+v", text, got, err, want)
		}
	}
	for _, text := range []string{".dme kw 0 x", ".dme kw 100", ".dme kw abc x", ".dme kw -f x", ".dme kw 10 " + strings.Repeat("长", maxKeywordRunes+1)} {
		if got, err := parseTrigger(keywordInvocation(text)); err == nil {
			t.Errorf("%q 不该解析成功，得到 %+v", text, got)
		}
	}
}

// triggerHarness 是一个不真删的 service：后台任务先排着，由测试决定什么时候跑；
// 跑的时候只记下触发了哪个触发词。
type triggerHarness struct {
	*service
	queued  []func()
	swept   []dmeTrigger
	allowed bool
}

func newTriggerHarness(t *testing.T) *triggerHarness {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &triggerHarness{allowed: true}
	h.service = newService(store.New(filepath.Join(t.TempDir(), "dme.json"), dmeDefaults), logger, func() (func(), bool) {
		return func() {}, h.allowed
	})
	h.background = func(work func()) { h.queued = append(h.queued, work) }
	h.sweep = func(_ context.Context, _ *bot.Client, _ *bot.Message, hit dmeTrigger) { h.swept = append(h.swept, hit) }
	return h
}

func (h *triggerHarness) add(t *testing.T, triggers ...dmeTrigger) {
	t.Helper()
	if err := h.update(func(cfg *dmeConfig) error { cfg.Triggers = append(cfg.Triggers, triggers...); return nil }); err != nil {
		t.Fatal(err)
	}
}

func (h *triggerHarness) say(chat, text string, out bool) {
	h.offer(context.Background(), nil, &bot.Message{ID: 9, ChatID: chat, Text: text, Out: out})
}

func (h *triggerHarness) runQueued() {
	work := h.queued
	h.queued = nil
	for _, run := range work {
		run()
	}
}

// 同一个对话同时只删一次；自己发的不触发；同时在跑的触发删除有上限；删完了才能再触发。
func TestOfferStartsOneRunPerChat(t *testing.T) {
	h := newTriggerHarness(t)
	h.add(t, dmeTrigger{ChatID: "-100", Keyword: "清理", Count: 10},
		dmeTrigger{ChatID: "42", Keyword: "bye", Count: allMessages, Anti: true},
		dmeTrigger{ChatID: "7", Keyword: "走", Count: 5})

	h.say("-100", "请清理一下", false)
	h.say("-100", "清理清理", false) // 这个对话还在删
	h.say("42", "bye", true)     // 自己发的
	h.say("42", "BYE now", false)
	h.say("7", "走了", false) // 同时在跑的已经满了
	h.say("8", "清理", false) // 这个对话没有触发词
	if len(h.queued) != 2 {
		t.Fatalf("排了 %d 个删除，应为 2 个", len(h.queued))
	}
	h.runQueued()
	if len(h.swept) != 2 || h.swept[0].Keyword != "清理" || h.swept[1].Keyword != "bye" || !h.swept[1].Anti {
		t.Fatalf("触发了 %+v", h.swept)
	}
	// 删完之后名额和对话都放开了。
	h.say("-100", "清理", false)
	h.say("7", "走", false)
	if len(h.queued) != 2 {
		t.Errorf("删完后排了 %d 个删除，应为 2 个", len(h.queued))
	}
}

// 维护或退出期间不删，也不能把对话和名额占住不放。
func TestOfferSkipsDuringMaintenance(t *testing.T) {
	h := newTriggerHarness(t)
	h.add(t, dmeTrigger{ChatID: "-100", Keyword: "清理", Count: 10})
	h.allowed = false
	h.say("-100", "清理", false)
	if len(h.queued) != 0 {
		t.Fatalf("维护期间排了 %d 个删除", len(h.queued))
	}
	h.allowed = true
	h.say("-100", "清理", false)
	h.say("-100", "清理", false)
	if len(h.queued) != 1 {
		t.Errorf("维护结束后排了 %d 个删除，应为 1 个", len(h.queued))
	}
}

// 触发词存进文件，重启后照样生效。
func TestTriggersSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dme.json")
	first := newService(store.New(path, dmeDefaults), nil, nil)
	if err := first.update(func(cfg *dmeConfig) error {
		cfg.Triggers = append(cfg.Triggers, dmeTrigger{ChatID: "-100", Keyword: "清理", Count: 10})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	again := newService(store.New(path, dmeDefaults), nil, nil)
	if hit, ok := again.current().match("-100", "清理"); !ok || hit.Count != 10 {
		t.Errorf("重启后读到 %+v %v", hit, ok)
	}
	if cfg, _ := again.config(); cfg.BatchSize != 50 {
		t.Errorf("加触发词把删除设置弄丢了：%+v", cfg)
	}
}
