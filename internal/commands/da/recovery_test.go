package da

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/store"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

func TestStatusDoesNotOverwriteProgress(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(fmt.Sprintf("removed=%v", remove), func(t *testing.T) {
			testDelayedStatus(t, remove)
		})
	}
}

func testDelayedStatus(t *testing.T, remove bool) {
	started, release, ended := make(chan struct{}), make(chan struct{}), make(chan struct{})
	fake := telegram.InvokeFunc(func(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
		switch in.(type) {
		case *tg.MessagesEditMessageRequest:
			close(started)
			<-release
			out.(*tg.UpdatesBox).Updates = &tg.Updates{}
			return nil
		case *tg.MessagesDeleteMessagesRequest:
			v := out.(*tg.MessagesAffectedMessages)
			*v = tg.MessagesAffectedMessages{}
			return nil
		default:
			t.Fatalf("unexpected %T", in)
			return nil
		}
	})
	log := slog.New(slog.DiscardHandler)
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 1}, log)
	s := &daService{a: &app.App{Logger: log}, store: store.New(filepath.Join(t.TempDir(), "da.json"), func() daDB { return daDB{} })}
	live := &daTask{ChatID: "-1009", IsRunning: true, SavedMessageID: 90}
	s.save(live)
	snapshot := s.current(live.ChatID)
	go func() { s.progress(context.Background(), client, snapshot, "状态查询"); close(ended) }()
	<-started
	r := &daRun{ctx: context.Background(), client: client, peer: &tg.InputPeerSelf{}, task: live, save: s.save, logger: log}
	r.deleteIDs([]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
	before, _ := s.store.Read()
	if remove {
		if err := s.store.Update(func(db *daDB) error { db.Tasks = nil; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	<-ended
	after, _ := s.store.Read()
	if before.Tasks[0].DeletedMessages != 10 {
		t.Fatal("delete progress not saved")
	}
	if remove {
		if len(after.Tasks) != 0 {
			t.Fatal("status resurrected removed task")
		}
	} else if after.Tasks[0].DeletedMessages != 10 {
		t.Fatal("status overwrote live delete progress")
	}
}
