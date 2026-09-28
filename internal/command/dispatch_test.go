package command

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
)

// editRecorder 冒充 Telegram，只记下命令消息被改成了什么。
type editRecorder struct {
	mu    sync.Mutex
	edits []string
}

func (r *editRecorder) Invoke(_ context.Context, input bin.Encoder, _ bin.Decoder) error {
	if edit, ok := input.(*tg.MessagesEditMessageRequest); ok {
		r.mu.Lock()
		r.edits = append(r.edits, edit.Message)
		r.mu.Unlock()
	}
	return nil
}

func (r *editRecorder) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.edits) == 0 {
		return ""
	}
	return r.edits[len(r.edits)-1]
}

// 处理函数返回的错误怎么显示：给用户看的错误照原话、带 ❌，不算故障；
// 内部错误只给概括，细节（这里是一个路径）只进日志。
func TestDispatchShowsErrors(t *testing.T) {
	var logs strings.Builder
	r := New([]string{"."}, slog.New(slog.NewTextHandler(&logs, nil)))
	r.Register(
		&Command{Name: "refuse", Handle: func(context.Context, *Invocation) error { return Fail("太多了 哒咩") }},
		&Command{Name: "crash", Handle: func(context.Context, *Invocation) error {
			return errors.New("open /root/mibot-lite/data/x.json: permission denied")
		}},
	)
	recorder := &editRecorder{}
	self := &tg.User{ID: 1, AccessHash: 1}
	peers := bot.NewPeerCache()
	peers.SetSelf(self.ID)
	client := bot.FromAPI(tg.NewClient(recorder), peers, self, slog.New(slog.DiscardHandler))
	message := func(text string) *bot.Message {
		return &bot.Message{ID: 10, Peer: &tg.PeerUser{UserID: self.ID}, ChatID: "1", Text: text, Out: true}
	}

	r.Dispatch(context.Background(), client, message(".refuse"))
	r.Wait(time.Second)
	if got := recorder.last(); got != "❌ 太多了 哒咩" {
		t.Errorf("user error shown as %q", got)
	}
	if !strings.Contains(logs.String(), "command.refused") || strings.Contains(logs.String(), "command.failed") {
		t.Errorf("a refusal was logged as a failure:\n%s", logs.String())
	}

	r.Dispatch(context.Background(), client, message(".crash"))
	r.Wait(time.Second)
	if got := recorder.last(); got != "❌ 命令执行失败：内部错误，详情见日志" {
		t.Errorf("internal error shown as %q", got)
	}
	if !strings.Contains(logs.String(), "permission denied") {
		t.Error("the detail of an internal error must still reach the log")
	}
}

// 「命令 help」由派发器显示帮助，处理函数不执行；收正文的命令照常执行。
func TestDispatchShowsHelp(t *testing.T) {
	r := New([]string{"."}, slog.New(slog.DiscardHandler))
	var ran []string
	var mu sync.Mutex
	handle := func(_ context.Context, inv *Invocation) error {
		mu.Lock()
		ran = append(ran, inv.Command)
		mu.Unlock()
		return nil
	}
	r.Register(
		&Command{Name: "restart", Help: func(string) string { return "重启说明" }, Handle: handle},
		&Command{Name: "tr", FreeText: true, Help: func(string) string { return "翻译说明" }, Handle: handle},
	)
	recorder := &editRecorder{}
	self := &tg.User{ID: 1, AccessHash: 1}
	peers := bot.NewPeerCache()
	peers.SetSelf(self.ID)
	client := bot.FromAPI(tg.NewClient(recorder), peers, self, slog.New(slog.DiscardHandler))
	dispatch := func(text string) {
		r.Dispatch(context.Background(), client, &bot.Message{ID: 10, Peer: &tg.PeerUser{UserID: self.ID}, ChatID: "1", Text: text, Out: true})
		r.Wait(time.Second)
	}

	dispatch(".restart h")
	if got := recorder.last(); got != "重启说明" || len(ran) != 0 {
		t.Errorf(".restart h：显示 %q，执行了 %v", got, ran)
	}
	dispatch(".tr help")
	if len(ran) != 1 || ran[0] != "tr" {
		t.Errorf(".tr help 应交给处理函数，执行了 %v", ran)
	}
	dispatch(".tr --help")
	if got := recorder.last(); got != "翻译说明" || len(ran) != 1 {
		t.Errorf(".tr --help：显示 %q，执行了 %v", got, ran)
	}
}
