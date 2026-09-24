package sudo

import (
	"io"
	"log/slog"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/command"
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
	for _, name := range []string{"ping", "da", "ai", "sudo", "sb", "sum", "speedtest", "yvlu", "whois"} {
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
