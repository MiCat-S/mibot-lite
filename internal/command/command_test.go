package command

import (
	"log/slog"
	"strings"
	"testing"
)

func registry() *Registry {
	return New([]string{".", "。", "$", ".."}, slog.New(slog.DiscardHandler))
}

func TestParseRoutes(t *testing.T) {
	r := registry()
	cases := []struct {
		text    string
		ok      bool
		prefix  string
		command string
		args    []string
	}{
		{".ping", true, ".", "ping", nil},
		{"。ping", true, "。", "ping", nil},
		{"$rate BTC CNY", true, "$", "rate", []string{"BTC", "CNY"}},
		{"..help", true, "..", "help", nil},
		{".  ping  extra ", true, ".", "ping", []string{"extra"}},
		{"ping", false, "", "", nil},
		{".", false, "", "", nil},
		{".!bad", false, "", "", nil},
		{"hello .ping", false, "", "", nil},
	}
	for _, item := range cases {
		route, ok := r.Parse(item.text)
		if ok != item.ok {
			t.Errorf("%q: matched=%v, want %v", item.text, ok, item.ok)
			continue
		}
		if !ok {
			continue
		}
		if route.Prefix != item.prefix || route.Command != item.command {
			t.Errorf("%q: got %q+%q, want %q+%q", item.text, route.Prefix, route.Command, item.prefix, item.command)
		}
		if strings.Join(route.Args, ",") != strings.Join(item.args, ",") {
			t.Errorf("%q: args %v, want %v", item.text, route.Args, item.args)
		}
	}
}

// 最长的前缀优先，所以 ".." 按它自己的写法路由，而不会被读成 "."
// 加上一个以 "." 开头的命令。
func TestLongestPrefixWins(t *testing.T) {
	route, ok := registry().Parse("..ping")
	if !ok || route.Prefix != ".." {
		t.Fatalf("got %+v ok=%v", route, ok)
	}
}

func TestRegisterAndLookup(t *testing.T) {
	r := registry()
	r.Register(&Command{Name: "ping", Description: "p"}, &Command{Name: "ver", Description: "v", Hidden: true})
	if _, ok := r.Lookup("ping"); !ok {
		t.Error("ping should be registered")
	}
	if _, ok := r.Lookup("ver"); !ok {
		t.Error("a hidden command is still routable")
	}
	list := r.Commands()
	if len(list) != 1 || list[0].Name != "ping" {
		t.Fatalf("visible commands %v", list)
	}
}

func TestRegisterRejectsDuplicates(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a duplicate command name must panic at registration, not shadow silently")
		}
	}()
	r := registry()
	r.Register(&Command{Name: "ping"})
	r.Register(&Command{Name: "ping"})
}

func TestBriefPrefersRPCCode(t *testing.T) {
	if got := Brief(errTest("rpc error code 400: CHAT_ADMIN_REQUIRED (400)")); got != "CHAT_ADMIN_REQUIRED" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("x", 200)
	if got := Brief(errTest(long)); len(got) != 80 {
		t.Fatalf("long errors must be cut, got %d chars", len(got))
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

// withCommands 返回一个带几个真实命令和一张别名表的注册表，
// 和 .alias 修改之后的状态一样。
func withCommands(t *testing.T, aliases map[string]string) *Registry {
	t.Helper()
	built := registry()
	for _, name := range []string{"ping", "version", "speedtest", "gt"} {
		built.Register(&Command{Name: name})
	}
	built.SetAliases(aliases)
	return built
}

func TestAliasExpandsAndKeepsTheArguments(t *testing.T) {
	registry := withCommands(t, map[string]string{"测速": "speedtest 48463"})
	route, ok := registry.Parse(".测速 now")
	if !ok || route.Command != "speedtest" || strings.Join(route.Args, " ") != "48463 now" {
		t.Fatalf("got %+v, want speedtest with 48463 now", route)
	}
}

// 别名后面的词是命令的输入，有些命令从原始文本读取输入：
// .gt 就是逐行翻译的。
func TestAliasKeepsTheRestOfTheTextExactly(t *testing.T) {
	registry := withCommands(t, map[string]string{"译": "gt"})
	route, ok := registry.Parse(".译 hello\n  world\n")
	if !ok || route.Text != ".gt hello\n  world\n" {
		t.Fatalf("Text = %q, want the newlines and indentation kept", route.Text)
	}
}

func TestLongestAliasWins(t *testing.T) {
	registry := withCommands(t, map[string]string{"a": "version", "a b": "ping"})
	if route, _ := registry.Parse(".a b c"); route.Command != "ping" || strings.Join(route.Args, " ") != "c" {
		t.Errorf("got %+v, want the two-word alias", route)
	}
	if route, _ := registry.Parse(".a c"); route.Command != "version" || strings.Join(route.Args, " ") != "c" {
		t.Errorf("got %+v, want the one-word alias", route)
	}
}

// 和真实命令同名的单个词别名只会失效，不会劫持这个命令；
// 更长的别名仍然可以以命令名开头。
func TestRealCommandsOutrankOneWordAliases(t *testing.T) {
	registry := withCommands(t, map[string]string{"ping": "version", "ping now": "speedtest"})
	if route, _ := registry.Parse(".ping"); route.Command != "ping" {
		t.Errorf(".ping went to %q", route.Command)
	}
	if route, _ := registry.Parse(".ping now"); route.Command != "speedtest" {
		t.Errorf(".ping now went to %q, want the two-word alias", route.Command)
	}
}

func TestUnknownWordsStillDoNotParse(t *testing.T) {
	registry := withCommands(t, map[string]string{"测速": "speedtest"})
	for _, text := range []string{".你好", ".测", ". "} {
		if _, ok := registry.Parse(text); ok {
			t.Errorf("%q parsed as a command", text)
		}
	}
	if route, ok := registry.Parse(".ping hi"); !ok || route.Text != ".ping hi" {
		t.Errorf("a plain command lost its text: %+v", route)
	}
}

func TestWantsHelpAndHelpText(t *testing.T) {
	if !wantsHelp([]string{"--help"}) || wantsHelp([]string{"help"}) || wantsHelp([]string{"--help", "x"}) || wantsHelp(nil) {
		t.Error("只有单独一个 --help 才拦下来")
	}
	plain := &Command{Name: "restart", Usage: "[x]", Description: "重启 <服务>"}
	if got := plain.HelpText("."); got != "<b>.restart [x]</b>\n\n重启 &lt;服务&gt;" {
		t.Errorf("没有 Help 时的帮助：%q", got)
	}
	custom := &Command{Name: "a", Help: func(prefix string) string { return prefix + "自定义" }}
	if got := custom.HelpText("!"); got != "!自定义" {
		t.Errorf("有 Help 时应该用它：%q", got)
	}
}
