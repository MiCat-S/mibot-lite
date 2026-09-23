package ids

import (
	"strings"
	"testing"
	"time"
)

func TestEstimateCreation(t *testing.T) {
	now := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	if got := estimateCreation(215959394, now).Year(); got != 2016 {
		t.Errorf("id 215959394 estimated in %d, want 2016", got)
	}
	// 更大的 id 估出的时间不会更早。
	previous := time.Time{}
	for id := int64(0); id <= 8_000_000_000; id += 250_000_000 {
		got := estimateCreation(id, now)
		if got.Before(previous) {
			t.Fatalf("id %d went back in time", id)
		}
		previous = got
	}
	// 超出表的范围后，估计值继续往后推，到当前时间为止。
	if got := estimateCreation(8_600_000_000, now); !got.After(time.Unix(1767225600, 0)) && !got.Equal(now) {
		t.Errorf("an id past the table landed at %v", got)
	}
	if got := estimateCreation(99_000_000_000, now); !got.Equal(now) {
		t.Errorf("a far future id was not clamped to now: %v", got)
	}
}

func TestRenderIDs(t *testing.T) {
	now := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	user := renderIDs(&entityInfo{kind: "user", id: 215959394, name: "Cat", username: "cat", dc: 5, common: 3,
		flags: []string{"⭐ Premium"}}, time.Date(2024, 1, 2, 3, 4, 0, 0, time.Local), now)
	for _, wanted := range []string{"215959394", "@cat", "DC5（新加坡）", "2024-01-02 03:04", "tg://user?id=215959394", "t.me/cat", "⭐ Premium"} {
		if !strings.Contains(user, wanted) {
			t.Errorf("user card lost %q:\n%s", wanted, user)
		}
	}
	channel := renderIDs(&entityInfo{kind: "supergroup", id: 1771725356, name: "群", members: 42}, time.Time{}, now)
	for _, wanted := range []string{"超级群", "-1001771725356", "42", "看不出"} {
		if !strings.Contains(channel, wanted) {
			t.Errorf("channel card lost %q:\n%s", wanted, channel)
		}
	}
	if strings.Contains(channel, "注册时间") || strings.Contains(channel, "tg://user") {
		t.Errorf("a channel was described like a person:\n%s", channel)
	}
}
