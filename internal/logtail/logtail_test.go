package logtail

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// ring 构造一个写入新环形缓冲区的 handler，真正的输出直接丢弃，
// 那部分本来是 journal 完整保存的。
func ring(t *testing.T) (*slog.Logger, *Ring) {
	t.Helper()
	held := New()
	return slog.New(Wrap(slog.NewTextHandler(io.Discard, nil), held)), held
}

// 这个包存在的意义：进了环形缓冲区的内容可以放心转发。
// 真正要紧的就是这个测试，其余都是细节。
func TestNothingSensitiveReachesTheRing(t *testing.T) {
	logger, held := ring(t)
	logger.Info("dispatch.dropped",
		slog.Int64("account", 215959394),
		slog.String("chat", "-1001771725356"),
		slog.String("text", "我的银行卡密码是 123456"),
		slog.String("error", "dial 43.153.150.179:443: refused, see https://api.example.com/v1/x?key=sk-abcd1234efgh"),
	)
	whole := strings.Join(held.Tail(0, nil), "\n")
	for _, secret := range []string{
		"215959394", "-1001771725356", "银行卡", "123456",
		"43.153.150.179", "sk-abcd1234efgh", "/v1/x",
	} {
		if strings.Contains(whole, secret) {
			t.Errorf("ring leaked %q:\n%s", secret, whole)
		}
	}
	// 同时它还得留下值得看的内容。
	for _, wanted := range []string{"dispatch.dropped", "refused", "https://api.example.com"} {
		if !strings.Contains(whole, wanted) {
			t.Errorf("ring lost %q, leaving nothing to diagnose:\n%s", wanted, whole)
		}
	}
}

// 同一个聊天必须始终显示为同一个，否则一段对话的日志就没法读下去。
func TestDigestIsStableAndDistinct(t *testing.T) {
	if Digest("-100123") != Digest("-100123") {
		t.Error("the same id digested differently twice")
	}
	if Digest("-100123") == Digest("-100124") {
		t.Error("two ids collapsed to one digest")
	}
	if got := Digest(""); got != "#none" {
		t.Errorf("empty id = %q", got)
	}
}

// 用 WithAttrs 附加的属性，和记录上的属性走同样的脱敏。它们走的是
// 另一条代码路径，泄露恰恰最容易藏在这种地方不被发现。
func TestWithAttrsIsRedactedToo(t *testing.T) {
	logger, held := ring(t)
	logger.With(slog.String("chat", "-1001771725356")).Info("command.handled")
	line := strings.Join(held.Tail(0, nil), "\n")
	if strings.Contains(line, "-1001771725356") {
		t.Errorf("WithAttrs leaked the chat: %s", line)
	}
	if !strings.Contains(line, Digest("-1001771725356")) {
		t.Errorf("WithAttrs dropped the chat instead of digesting it: %s", line)
	}
}

func TestScrubKeepsTheDiagnosis(t *testing.T) {
	for input, want := range map[string]string{
		"ookla failed: signal: aborted":       "ookla failed: signal: aborted",
		"took 15.490826543s":                  "took 15.490826543s",
		"connect to 10.0.0.1 failed":          "connect to [ip] failed",
		"https://speedtest.net/result/c/abcd": "https://speedtest.net/…",
	} {
		if got := Scrub(input); got != want {
			t.Errorf("Scrub(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRingWrapsOldestFirst(t *testing.T) {
	held := New()
	for i := 0; i < Lines+5; i++ {
		held.add(strings.Repeat("x", 3) + string(rune('a'+i%26)))
	}
	lines := held.Tail(0, nil)
	if len(lines) != Lines {
		t.Fatalf("held %d lines, want %d", len(lines), Lines)
	}
	if held.Held() != Lines {
		t.Fatalf("Held = %d, want %d", held.Held(), Lines)
	}
	// 最后加进去的一行必须排在返回结果的最后。
	if lines[len(lines)-1] != "xxx"+string(rune('a'+(Lines+4)%26)) {
		t.Errorf("the newest line is not last: %q", lines[len(lines)-1])
	}
}

func TestTailCountsFromTheNewest(t *testing.T) {
	held := New()
	for _, line := range []string{"one", "two", "three"} {
		held.add(line)
	}
	got := held.Tail(2, nil)
	if len(got) != 2 || got[0] != "two" || got[1] != "three" {
		t.Errorf("Tail(2) = %v, want the last two in order", got)
	}
}

func TestAtLeastReadsTheLevel(t *testing.T) {
	logger, held := ring(t)
	logger.Info("quiet")
	logger.Error("loud")
	only := held.Tail(0, func(line string) bool { return AtLeast(line, slog.LevelError) })
	if len(only) != 1 || !strings.Contains(only[0], "loud") {
		t.Errorf("error filter returned %v", only)
	}
	if AtLeast("", slog.LevelError) || AtLeast("garbage line", slog.LevelError) {
		t.Error("an unparseable line should not pass a level filter")
	}
}

func TestLineIsCapped(t *testing.T) {
	held := New()
	held.add(strings.Repeat("字", LineLimit*3))
	line := held.Tail(0, nil)[0]
	if got := len([]rune(line)); got > LineLimit+1 {
		t.Errorf("line kept %d runes, want at most %d", got, LineLimit+1)
	}
}

func TestEnabledFollowsTheWrappedHandler(t *testing.T) {
	level := new(slog.LevelVar)
	level.Set(slog.LevelWarn)
	held := New()
	logger := slog.New(Wrap(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: level}), held))
	logger.Info("dropped below the level")
	if held.Held() != 0 {
		t.Errorf("a filtered record still reached the ring: %v", held.Tail(0, nil))
	}
	level.Set(slog.LevelDebug)
	logger.Debug("now it passes")
	if held.Held() != 1 {
		t.Errorf("raising the level did not reach the ring: %v", held.Tail(0, nil))
	}
}
