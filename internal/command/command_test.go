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

// The longest prefix wins, so ".." routes its own spelling rather than
// being read as "." plus a command starting with ".".
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

// withCommands is a registry holding a few real commands and an alias
// table, the way .alias leaves it.
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

// The words after an alias are the command's input, and some commands
// read that input from the raw text: .gt translates it line by line.
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

// A one-word alias named like a real command is dead, not a hijack; a
// longer alias may still start with a command's name.
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
