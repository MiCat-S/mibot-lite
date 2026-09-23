package eatgif

import (
	"testing"
)

// 每个素材路径都来自远程的 JSON 文档，因此不可信。
func TestSafeRelative(t *testing.T) {
	for _, good := range []string{"md/md1.png", "config.json", "a/b/c.png"} {
		if _, err := safeRelative(good); err != nil {
			t.Errorf("%q should be accepted: %v", good, err)
		}
	}
	for _, bad := range []string{"", "../secrets", "a/../../b", "a//b", "C:\\x", "https://evil/x", "./x"} {
		if _, err := safeRelative(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}
