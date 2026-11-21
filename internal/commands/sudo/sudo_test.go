package sudo

import (
	"context"
	"io"
	"log/slog"
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

// 能不能借按别名展开之后的真实命令判断；改设置的子命令只限本人，大小写不影响。
func TestDelegationAllowed(t *testing.T) {
	registry := command.New([]string{"."}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, name := range []string{"ping", "da", "ai", "sudo", "sb", "sum", "speedtest", "yvlu", "whois", "tr"} {
		registry.Register(&command.Command{Name: name})
	}
	registry.SetAliases(map[string]string{"wipe": "da true", "p": "ping", "key": "ai config key"})
	for text, want := range map[string]bool{
		".ping": true, ".p": true, ".ai 你好": true,
		".da true": false, ".wipe": false, ".sudo add 1": false, ".sb 1": false,
		".ai config key x": false, ".ai CONFIG": false, ".key x": false,
		// sum、speedtest 只借得出平常的用法，管理类子命令（包括以后新加的）都不行。
		".sum": true, ".sum 200": true, ".sum now": false, ".sum edit 1 prompt x": false, ".sum ls": false, ".sum nosuch": false,
		".speedtest": true, ".speedtest 12345": true, ".speedtest list": true,
		".speedtest fix": false, ".speedtest set 1": false, ".speedtest diagnose": false,
		".yvlu": true, ".yvlu 3": true, ".yvlu s": false, ".whois a.com": true, ".whois history": false,
		// .speedtest config 和 help 一样只是看说明。
		".speedtest config": true,
		// .tr set 改的是你的默认目标语言，不能借；翻译本身可以。
		".tr hello": true, ".tr en hello": true, ".tr set en": false, ".tr SET en": false,
	} {
		route, ok := registry.Parse(text)
		if !ok {
			t.Fatalf("%q 没有解析成命令", text)
		}
		if got := delegationAllowed(route); got != want {
			t.Errorf("%q（展开成 %s %v）能否借出：%v，应为 %v", text, route.Command, route.Args, got, want)
		}
	}
}

func TestQueuedDelegationHonorsRevocation(t *testing.T) {
	for _, name := range []string{"sudo", "sure"} {
		t.Run(name, func(t *testing.T) {
			log := slog.New(slog.DiscardHandler)
			a := &app.App{Root: t.TempDir(), Registry: command.New([]string{"."}, log), Logger: log}
			defer a.Registry.Abort()
			saved := store.New(filepath.Join(a.DataDir(), name+".json"), func() delegateDocument { return delegateDocument{} })
			if err := saved.Update(func(d *delegateDocument) error {
				d.Users = []delegateEntry{{ID: "42"}}
				d.Messages = []sureRule{{ID: 1, Msg: ".ping"}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			Register(a)
			a.Registry.Register(&command.Command{Name: "ping"})

			pending := make(chan func(), 1)
			oldBackground := inBackground
			inBackground = func(work func()) { pending <- work }
			defer func() { inBackground = oldBackground }()
			fake := telegram.InvokeFunc(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				switch in.(type) {
				case *tg.MessagesEditMessageRequest:
					out.(*tg.UpdatesBox).Updates = &tg.Updates{}
				default:
					t.Fatalf("revoked delegation sent RPC %T", in)
				}
				return nil
			})
			peers := bot.NewPeerCache()
			peers.SetSelf(1)
			client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 1}, log)
			peer := &tg.PeerUser{UserID: 1}
			ctx := a.Registry.Lifecycle()
			trigger := &bot.Message{ID: 7, Peer: peer, ChatID: "1", Sender: &tg.PeerUser{UserID: 42}, Text: ".ping"}
			if !a.OfferForeign(ctx, client, trigger) {
				t.Fatal("delegation was not queued")
			}
			var work func()
			select {
			case work = <-pending:
			case <-time.After(time.Second):
				t.Fatal("queued delegation did not start")
			}
			handler, _ := a.Registry.Lookup(name)
			inv := &command.Invocation{Client: client, Prefix: ".", Args: []string{"del", "42"}, Log: log,
				Message: &bot.Message{ID: 8, Peer: peer, ChatID: "1", Out: true}}
			if err := handler.Handle(ctx, inv); err != nil {
				t.Fatal(err)
			}
			work()
		})
	}
}
