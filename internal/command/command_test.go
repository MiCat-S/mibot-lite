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
