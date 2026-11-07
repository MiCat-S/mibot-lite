package acn

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/store"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

func TestScheduledUpdateIgnoresForeignAccountConfig(t *testing.T) {
	saved := store.New(filepath.Join(t.TempDir(), "acn.json"), acnDefaults)
	saved.Update(func(s *acnState) error {
		u := newAcnUser("2")
		u.Enabled = true
		u.OriginalFirstName = "ForeignAccount"
		s.Users["2"] = u
		return nil
	})
	var names []string
	fake := telegram.InvokeFunc(func(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
		req := in.(*tg.AccountUpdateProfileRequest)
		names = append(names, req.FirstName)
		out.(*tg.UserBox).User = &tg.User{ID: 1}
		return nil
	})
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 1}, slog.New(slog.DiscardHandler))
	s := &acnService{store: saved}
	s.tick(context.Background(), client)
	if len(names) != 0 {
		t.Fatalf("foreign config renamed current account: %v", names)
	}
	saved.Update(func(st *acnState) error {
		u := newAcnUser("1")
		u.Enabled = true
		u.OriginalFirstName = "Self"
		st.Users["1"] = u
		return nil
	})
	s.tick(context.Background(), client)
	if len(names) != 1 || !strings.HasPrefix(names[0], "Self ") {
		t.Fatalf("self config not applied: %v", names)
	}
	state, _ := saved.Read()
	if state.Users["2"].OriginalFirstName != "ForeignAccount" {
		t.Fatal("foreign imported config changed")
	}
}
