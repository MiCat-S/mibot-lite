package restart

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// systemctl 被这次重启本身打断（被信号结束、命令的 ctx 被取消）时回执要留着，重启回来才能
// 改那条消息；真的失败（非零退出、超时）才撤掉回执，并且允许再试。
func TestScheduleKeepsReceiptWhenInterrupted(t *testing.T) {
	previous := triggerRestart
	t.Cleanup(func() { triggerRestart = previous })
	newRestarter := func(t *testing.T) *Restarter {
		return &Restarter{a: &app.App{BootID: "old"}, service: "mibot-lite.service",
			store: store.New(filepath.Join(t.TempDir(), "restart-receipt.json"), func() receiptDocument { return receiptDocument{} })}
	}
	killed := func(context.Context, string) error { return exec.Command("sh", "-c", "kill -TERM $$").Run() }
	failed := func(context.Context, string) error { return exec.Command("sh", "-c", "exit 1").Run() }

	for name, c := range map[string]struct {
		trigger  func(context.Context, string) error
		canceled bool
		keep     bool
	}{
		"提交成功":            {trigger: func(context.Context, string) error { return nil }, keep: true},
		"systemctl 被信号结束": {trigger: killed, keep: true},
		"命令的 ctx 被取消":     {trigger: func(ctx context.Context, _ string) error { return ctx.Err() }, canceled: true, keep: true},
		"systemctl 非零退出":  {trigger: failed},
		"systemctl 超时": {trigger: func(context.Context, string) error {
			return fmt.Errorf("%w: %w", errTriggerTimeout, exec.Command("sh", "-c", "kill -KILL $$").Run())
		}},
	} {
		triggerRestart = c.trigger
		r := newRestarter(t)
		ctx, cancel := context.WithCancel(context.Background())
		if c.canceled {
			cancel()
		}
		err := r.Schedule(ctx, nil, "812473405", 309529, 0, "update")
		cancel()
		doc, _ := r.store.Read()
		var failure triggerError
		switch {
		case c.keep && (err != nil || doc.Pending == nil || doc.Pending.MessageID != 309529 || doc.Pending.Kind != "update"):
			t.Errorf("%s：回执应留着，得到 %v %+v", name, err, doc.Pending)
		case !c.keep && (!errors.As(err, &failure) || doc.Pending != nil):
			t.Errorf("%s：应报失败并撤掉回执，得到 %v %+v", name, err, doc.Pending)
		case !c.keep && errors.Is(r.Schedule(context.Background(), nil, "1", 1, 0, "restart"), ErrPending):
			t.Errorf("%s：失败之后应该可以再试", name)
		case c.keep && !errors.Is(r.Schedule(context.Background(), nil, "1", 1, 0, "restart"), ErrPending):
			t.Errorf("%s：重启已经提交，再来一次应回 ErrPending", name)
		}
	}
}

// newReceiptRestarter 造一个只带回执存储的 Restarter。
func newReceiptRestarter(t *testing.T, bootID string) *Restarter {
	t.Helper()
	return &Restarter{a: &app.App{BootID: bootID}, service: "mibot-lite.service",
		store: store.New(filepath.Join(t.TempDir(), "restart-receipt.json"), func() receiptDocument { return receiptDocument{} })}
}

// 回执要连同 access hash 一起存，重启回来的进程才能直接寻址。普通群和 self 没有 hash，
// 认不出的 peer（含 nil）只存 chatID。论坛话题照样存下。
func TestScheduleStoresPeer(t *testing.T) {
	previous := triggerRestart
	t.Cleanup(func() { triggerRestart = previous })
	triggerRestart = func(context.Context, string) error { return nil }
	for name, c := range map[string]struct {
		peer     tg.InputPeerClass
		wantType string
		wantHash int64
	}{
		"self":    {peer: &tg.InputPeerSelf{}, wantType: "self"},
		"user":    {peer: &tg.InputPeerUser{UserID: 812473405, AccessHash: 7}, wantType: "user", wantHash: 7},
		"chat":    {peer: &tg.InputPeerChat{ChatID: 99}, wantType: "chat"},
		"channel": {peer: &tg.InputPeerChannel{ChannelID: 55, AccessHash: 11}, wantType: "channel", wantHash: 11},
		"nil":     {peer: nil},
	} {
		r := newReceiptRestarter(t, "old")
		if err := r.Schedule(context.Background(), c.peer, "812473405", 309529, 42, "restart"); err != nil {
			t.Fatalf("%s：%v", name, err)
		}
		doc, _ := r.store.Read()
		if doc.Pending == nil {
			t.Fatalf("%s：回执没写下来", name)
		}
		if doc.Pending.PeerType != c.wantType || doc.Pending.AccessHash != c.wantHash {
			t.Errorf("%s：存了 %q/%d，应为 %q/%d", name, doc.Pending.PeerType, doc.Pending.AccessHash, c.wantType, c.wantHash)
		}
		if doc.Pending.TopicID != 42 {
			t.Errorf("%s：存的话题是 %d，应为 42", name, doc.Pending.TopicID)
		}
	}
}

// editRecorder 冒充 Telegram，记下 restart 回执改的那条消息和另发的消息。
type editRecorder struct {
	mu    sync.Mutex
	edits []*tg.MessagesEditMessageRequest
	sends []*tg.MessagesSendMessageRequest
	// refuse 不为空时，编辑一律回这个错误。
	refuse error
}

func (r *editRecorder) Invoke(_ context.Context, input bin.Encoder, _ bin.Decoder) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch request := input.(type) {
	case *tg.MessagesEditMessageRequest:
		r.edits = append(r.edits, request)
		return r.refuse
	case *tg.MessagesSendMessageRequest:
		r.sends = append(r.sends, request)
	}
	return nil
}

func (r *editRecorder) first() *tg.MessagesEditMessageRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.edits) == 0 {
		return nil
	}
	return r.edits[0]
}

func receiptClient(recorder *editRecorder) *bot.Client {
	return bot.FromAPI(tg.NewClient(recorder), bot.NewPeerCache(), &tg.User{ID: 1},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// 重启回来时缓存是空的：回执里存了 access hash 就直接寻址，不必先靠 updates.json。
func TestNotifyReadyUsesReceiptPeer(t *testing.T) {
	recorder := &editRecorder{}
	r := newReceiptRestarter(t, "new")
	if err := r.store.Update(func(doc *receiptDocument) error {
		doc.Pending = &receipt{ChatID: "812473405", MessageID: 309529, RequestedAt: time.Now().UnixMilli(),
			BootID: "old", Kind: "update", PeerType: "user", AccessHash: 7}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.notifyReady(context.Background(), receiptClient(recorder))
	edit := recorder.first()
	if edit == nil {
		t.Fatal("带 hash 的回执应该直接改那条消息")
	}
	peer, ok := edit.Peer.(*tg.InputPeerUser)
	if !ok || peer.UserID != 812473405 || peer.AccessHash != 7 {
		t.Errorf("改消息用的 peer：%+v", edit.Peer)
	}
	if edit.ID != 309529 {
		t.Errorf("改的消息 id：%d", edit.ID)
	}
	if doc, _ := r.store.Read(); doc.Pending != nil {
		t.Errorf("回执应该清掉：%+v", doc.Pending)
	}
}

// pendingUpdateReceipt 写下一张带 access hash、等着重启回来处理的回执。
func pendingUpdateReceipt(t *testing.T, r *Restarter) {
	t.Helper()
	if err := r.store.Update(func(doc *receiptDocument) error {
		doc.Pending = &receipt{ChatID: "812473405", MessageID: 309529, RequestedAt: time.Now().UnixMilli(),
			BootID: "old", Kind: "update", PeerType: "user", AccessHash: 7}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// 那条消息被删了、改不了时另发一条，同一个对话、同样的内容；回执照样清掉。
func TestNotifyReadySendsWhenEditRefused(t *testing.T) {
	recorder := &editRecorder{refuse: tgerr.New(400, "MESSAGE_ID_INVALID")}
	r := newReceiptRestarter(t, "new")
	pendingUpdateReceipt(t, r)
	r.notifyReady(context.Background(), receiptClient(recorder))
	if len(recorder.sends) != 1 {
		t.Fatalf("另发了 %d 条，应为 1 条", len(recorder.sends))
	}
	sent := recorder.sends[0]
	if peer, ok := sent.Peer.(*tg.InputPeerUser); !ok || peer.UserID != 812473405 || peer.AccessHash != 7 {
		t.Errorf("另发用的 peer：%+v", sent.Peer)
	}
	if !strings.Contains(sent.Message, "更新完成") {
		t.Errorf("另发的内容：%q", sent.Message)
	}
	if doc, _ := r.store.Read(); doc.Pending != nil {
		t.Errorf("回执应该清掉：%+v", doc.Pending)
	}
	if sent.ReplyTo != nil {
		t.Errorf("私聊不在话题里，另发不该带回复目标：%+v", sent.ReplyTo)
	}
}

// 在论坛话题里发的命令，另发的那条要回到同一个话题，不能落到 General。
func TestNotifyReadySendsIntoTopic(t *testing.T) {
	recorder := &editRecorder{refuse: tgerr.New(400, "MESSAGE_ID_INVALID")}
	r := newReceiptRestarter(t, "new")
	if err := r.store.Update(func(doc *receiptDocument) error {
		doc.Pending = &receipt{ChatID: "-1001234", MessageID: 500, RequestedAt: time.Now().UnixMilli(),
			BootID: "old", Kind: "restart", PeerType: "channel", AccessHash: 11, TopicID: 42}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.notifyReady(context.Background(), receiptClient(recorder))
	if len(recorder.sends) != 1 {
		t.Fatalf("另发了 %d 条，应为 1 条", len(recorder.sends))
	}
	sent := recorder.sends[0]
	if peer, ok := sent.Peer.(*tg.InputPeerChannel); !ok || peer.ChannelID != 1234 || peer.AccessHash != 11 {
		t.Errorf("另发用的 peer：%+v", sent.Peer)
	}
	reply, ok := sent.ReplyTo.(*tg.InputReplyToMessage)
	if !ok {
		t.Fatalf("另发的回复目标是 %T，应为发回话题的 InputReplyToMessage", sent.ReplyTo)
	}
	if top, _ := reply.GetTopMsgID(); reply.ReplyToMsgID != 42 || top != 42 {
		t.Errorf("另发进了话题 %d（top %d），应为 42", reply.ReplyToMsgID, top)
	}
}

// 编辑成功、或者连接出错（编辑可能已经生效）时都不另发。
func TestNotifyReadyDoesNotSendTwice(t *testing.T) {
	for name, refuse := range map[string]error{"edited": nil, "connection lost": errors.New("connection reset")} {
		recorder := &editRecorder{refuse: refuse}
		r := newReceiptRestarter(t, "new")
		pendingUpdateReceipt(t, r)
		r.notifyReady(context.Background(), receiptClient(recorder))
		if len(recorder.edits) != 1 || len(recorder.sends) != 0 {
			t.Errorf("%s：编辑 %d 次、另发 %d 条，应为 1、0", name, len(recorder.edits), len(recorder.sends))
		}
	}
}

// 旧格式的回执没有 peer 信息，缓存里又没有这个用户时，不发编辑，只清掉回执。
func TestNotifyReadyOldReceiptWithoutCache(t *testing.T) {
	recorder := &editRecorder{}
	r := newReceiptRestarter(t, "new")
	if err := r.store.Update(func(doc *receiptDocument) error {
		doc.Pending = &receipt{ChatID: "812473405", MessageID: 309529, RequestedAt: time.Now().UnixMilli(),
			BootID: "old", Kind: "update"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.notifyReady(context.Background(), receiptClient(recorder))
	if recorder.first() != nil {
		t.Error("缓存里没有这个用户，不该发编辑")
	}
	if doc, _ := r.store.Read(); doc.Pending != nil {
		t.Errorf("回执应该清掉：%+v", doc.Pending)
	}
}

// self 类型的回执只认本账号自己的对话；chatId 对不上就不用它，退回按 chatId 解析。
func TestReceiptSelfMustBeOwnChat(t *testing.T) {
	if peer, ok := receiptInputPeer(receipt{ChatID: "1", PeerType: "self"}, 1); !ok {
		t.Errorf("自己的收藏夹应该认：%v", peer)
	}
	if _, ok := receiptInputPeer(receipt{ChatID: "812473405", PeerType: "self"}, 1); ok {
		t.Error("别人的对话标成 self 不该认")
	}
}
