package alias

import (
	"strings"
	"testing"
)

// 别名在第一个真正的命令名处结束，这样别名才能由几个词组成，目标命令
// 也能带上自己的参数。
func TestSplitAlias(t *testing.T) {
	known := map[string]bool{"speedtest": true, "ping": true, "version": true}
	isCommand := func(name string) bool { return known[name] }
	cases := []struct {
		tokens        []string
		alias, target string
		ok            bool
	}{
		{[]string{"测速", "speedtest", "48463"}, "测速", "speedtest 48463", true},
		{[]string{"ping", "now", "version"}, "ping now", "version", true},
		{[]string{"a", "b", "c"}, "", "", false},
		{[]string{"测速", ".speedtest"}, "", "", false},
	}
	for _, c := range cases {
		alias, target, ok := splitAlias(c.tokens, isCommand)
		if alias != c.alias || target != c.target || ok != c.ok {
			t.Errorf("splitAlias(%v) = %q %q %v", c.tokens, alias, target, ok)
		}
	}
}

func TestRenderAliases(t *testing.T) {
	empty := renderAliases(nil, ".")
	if !strings.Contains(empty, ".alias set 测速 speedtest") {
		t.Errorf("an empty list should show how to add one:\n%s", empty)
	}
	listed := renderAliases(map[string]string{"译": "gt", "测速": "speedtest 48463"}, ".")
	if !strings.Contains(listed, ".测速") || !strings.Contains(listed, ".speedtest 48463") {
		t.Errorf("the list lost an alias:\n%s", listed)
	}
}
