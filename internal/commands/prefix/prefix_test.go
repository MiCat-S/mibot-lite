package prefix

import (
	"slices"
	"testing"
)

func TestCheckPrefix(t *testing.T) {
	for _, good := range []string{".", "。", "！", "~", "$", "!!", "喵"} {
		if problem := checkPrefix(good); problem != "" {
			t.Errorf("%q 被拒了：%s", good, problem)
		}
	}
	for _, bad := range []string{"a", "1", "_", ".a", `"`, "'", "~~~~~~~~~", "\t"} {
		if checkPrefix(bad) == "" {
			t.Errorf("%q 应该被拒", bad)
		}
	}
}

func TestNextPrefixes(t *testing.T) {
	current := []string{".", "。", "$"}
	for _, c := range []struct {
		action string
		tokens []string
		want   []string
	}{
		{"set", []string{"!", "！", "!"}, []string{"!", "！"}},
		{"add", []string{"~", "."}, []string{".", "。", "$", "~"}},
		{"del", []string{"$", "。"}, []string{"."}},
		{"del", []string{".", "。", "$"}, nil},
	} {
		if got := nextPrefixes(c.action, current, c.tokens); !slices.Equal(got, c.want) {
			t.Errorf("%s %v = %v，应为 %v", c.action, c.tokens, got, c.want)
		}
	}
}
