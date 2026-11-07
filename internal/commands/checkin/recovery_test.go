package checkin

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/store"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

func TestScheduledCheckinTracksAndResumesUntouchedTargets(t *testing.T) {
	r := command.New(nil, slog.New(slog.DiscardHandler))
	defer r.Abort()
	saved := store.New(filepath.Join(t.TempDir(), "checkin.json"), defaults)
	s := &service{a: &app.App{Registry: r}, store: saved}
	day := time.Date(2026, 11, 7, 12, 0, 0, 0, shanghai)
	if err := saved.Update(func(c *config) error {
		c.RunTime, c.RunTimeEnd = "10:00", ""
		c.Targets = []signTarget{{ID: "a", Name: "A", Target: "me", Command: "/first", Enabled: true}, {ID: "b", Name: "B", Target: "me", Command: "/second", Enabled: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	started, ended := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(r.Lifecycle())
	defer cancel()
	fake := telegram.InvokeFunc(func(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
		if req, ok := in.(*tg.MessagesSendMessageRequest); ok && req.Message == "/first" {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		t.Errorf("unexpected RPC %T", in)
		return errors.New("unexpected RPC")
	})
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 1}, slog.New(slog.DiscardHandler))
	go func() { s.tick(ctx, client, day); close(ended) }()
	<-started
	if r.Busy() != 1 {
		t.Errorf("active sign-in not tracked: %d", r.Busy())
	}
	if r.Quiesce(context.Background(), time.Millisecond) {
		t.Error("maintenance passed active sign-in")
	}
	cancel()
	<-ended
	cfg, _ := saved.Read()
	if cfg.LastRunDate != "" || len(cfg.Attempted) != 1 || cfg.Attempted[0] != "a" {
		t.Fatalf("interrupted progress: %+v", cfg)
	}
	// 新建服务模拟重启，只继续 B；A 可能已经送达，不能重发。
	oldGap, oldWait, oldPoll := gap, replyWait, pollEvery
	gap, replyWait, pollEvery = 0, time.Second, time.Millisecond
	defer func() { gap, replyWait, pollEvery = oldGap, oldWait, oldPoll }()
	f := &fakeBot{nextID: 10, reply: func(id int) *tg.Message { return botMessage(id+1, "OK", nil) }}
	// B 改用假机器人能解析的用户名。
	saved.Update(func(c *config) error { c.Targets[1].Target = "@sign_bot"; return nil })
	restarted := &service{a: s.a, store: saved}
	restarted.tick(r.Lifecycle(), fakeClient(t, f), day)
	if len(f.sent) != 2 || f.sent[0] != "/second" {
		t.Fatalf("replayed or missed target: %v", f.sent)
	}
	cfg, _ = saved.Read()
	if cfg.LastRunDate != dateOf(day) {
		t.Fatal("completed resumed run not saved")
	}
}

func TestCheckinOvernightPreservesPlanAcrossMidnight(t *testing.T) {
	oldWait, oldPoll := replyWait, pollEvery
	replyWait, pollEvery = time.Second, time.Millisecond
	defer func() { replyWait, pollEvery = oldWait, oldPoll }()
	day := time.Date(2026, 11, 7, 0, 0, 0, 0, shanghai)
	s := &service{store: store.New(filepath.Join(t.TempDir(), "checkin.json"), defaults)}
	s.store.Update(func(c *config) error {
		c.RunTime, c.RunTimeEnd = "22:00", "02:00"
		c.PlannedDate, c.CurrentRunTime = dateOf(day), "01:59"
		c.Targets = []signTarget{{ID: "a", Name: "A", Target: "@sign_bot", Command: "/sign", Enabled: true}}
		return nil
	})
	f := &fakeBot{nextID: 10, reply: func(id int) *tg.Message { return botMessage(id+1, "OK", nil) }}
	client := fakeClient(t, f)
	for _, at := range []time.Time{day.Add(22 * time.Hour), day.Add(24 * time.Hour), day.Add(25 * time.Hour)} {
		s.tick(context.Background(), client, at)
	}
	cfg, _ := s.store.Read()
	if len(f.sent) != 0 || cfg.PlannedDate != dateOf(day) || cfg.CurrentRunTime != "01:59" {
		t.Fatalf("future plan replaced or executed early: %+v %v", cfg, f.sent)
	}
	s.tick(context.Background(), client, day.Add(25*time.Hour+59*time.Minute))
	s.tick(context.Background(), client, day.Add(26*time.Hour))
	cfg, _ = s.store.Read()
	if len(f.sent) != 2 || cfg.LastRunDate != dateOf(day) {
		t.Fatalf("overnight did not run once: %+v %v", cfg, f.sent)
	}
}

func TestCanceledCheckinBeforeAttemptDoesNotConsumeTarget(t *testing.T) {
	s := &service{store: store.New(filepath.Join(t.TempDir(), "checkin.json"), defaults)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.runAll(ctx, nil, []signTarget{{ID: "a"}}, "手动", "2026/11/7")
	cfg, _ := s.store.Read()
	if !errors.Is(err, context.Canceled) || len(cfg.Attempted) != 0 || cfg.LastRunDate != "" {
		t.Fatalf("%v %+v", err, cfg)
	}
}

func TestCheckinAttemptMustPersistBeforeSend(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "file")
	s := &service{store: store.New(filepath.Join(parent, "checkin.json"), defaults)}
	// 先正常读入配置，再让后续写入失败。
	if _, err := s.store.Read(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	fake := telegram.InvokeFunc(func(context.Context, bin.Encoder, bin.Decoder) error { calls++; return errors.New("unexpected send") })
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 1}, slog.New(slog.DiscardHandler))
	if _, err := s.runAll(context.Background(), client, []signTarget{{ID: "a", Target: "me"}}, "手动", "2026/11/7"); err == nil {
		t.Fatal("ignored persistence failure")
	}
	if calls != 0 {
		t.Fatalf("sent without durable attempt: %d", calls)
	}
	cfg, _ := s.store.Read()
	if len(cfg.Attempted) != 0 || cfg.LastRunDate != "" {
		t.Fatalf("failed write polluted cached state: %+v", cfg)
	}
}

func TestCanceledTargetResolutionDoesNotConsumeAttempt(t *testing.T) {
	s := &service{store: store.New(filepath.Join(t.TempDir(), "checkin.json"), defaults)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := telegram.InvokeFunc(func(_ context.Context, in bin.Encoder, _ bin.Decoder) error {
		if _, ok := in.(*tg.ContactsResolveUsernameRequest); !ok {
			t.Errorf("sent during resolution: %T", in)
		}
		cancel()
		return context.Canceled
	})
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 1}, slog.New(slog.DiscardHandler))
	_, err := s.runAll(ctx, client, []signTarget{{ID: "a", Target: "@sign_bot"}}, "手动", "2026/11/7")
	cfg, _ := s.store.Read()
	if !errors.Is(err, context.Canceled) || len(cfg.Attempted) != 0 {
		t.Fatalf("read-only resolve consumed attempt: %v %+v", err, cfg)
	}
}
