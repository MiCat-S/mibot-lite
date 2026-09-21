package commands

import (
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/command"
)

func TestCalcEvaluates(t *testing.T) {
	cases := map[string]string{
		"2+2*5":       "12",
		"(10-3)*4":    "28",
		"-(2-5)/3":    "1",
		"3+7":         "10",
		"8/2+5":       "9",
		"1.5*2":       "3",
		"10/4":        "2.5",
		"  7  -  2  ": "5",
	}
	for expression, want := range cases {
		parser := &calcParser{text: []rune(expression)}
		value, err := parser.parse()
		if err != nil {
			t.Errorf("%s: %v", expression, err)
			continue
		}
		if got := formatCalc(value); got != want {
			t.Errorf("%s = %s, want %s", expression, got, want)
		}
	}
}

func TestCalcRejects(t *testing.T) {
	for _, expression := range []string{"1/0", "(1+2", "1++", "2**3", "abc", "", "1 2", "0x10", "1.2.3"} {
		parser := &calcParser{text: []rune(expression)}
		if _, err := parser.parse(); err == nil {
			t.Errorf("%q should not evaluate", expression)
		}
	}
}

func TestParseRateArgs(t *testing.T) {
	base, quote, amount, err := parseRateArgs([]string{"BTC", "CNY", "0.5"})
	if err != nil || base != "btc" || quote != "cny" || amount != 0.5 {
		t.Fatalf("got %s %s %v err=%v", base, quote, amount, err)
	}
	if base, quote, amount, _ := parseRateArgs(nil); base != "btc" || quote != "usd" || amount != 1 {
		t.Fatalf("defaults are %s %s %v", base, quote, amount)
	}
	// An alias resolves to its currency code.
	if base, _, _, _ := parseRateArgs([]string{"rmb"}); base != "cny" {
		t.Fatalf("rmb resolved to %s", base)
	}
	for _, args := range [][]string{{"../etc/passwd"}, {"<script>"}, {"nan"}, {strings.Repeat("a", 80)}} {
		if _, _, _, err := parseRateArgs(args); err == nil {
			t.Errorf("%v should be rejected", args)
		}
	}
}

func TestFormatPriceAndAmount(t *testing.T) {
	if got := formatAmount(1234567.891); got != "1,234,567.89" {
		t.Fatalf("formatAmount = %q", got)
	}
	if got := formatPrice(0.00001234); !strings.Contains(got, "e") {
		t.Fatalf("a tiny price should use exponent form, got %q", got)
	}
	if got := formatPrice(0.5); got != "0.5000" {
		t.Fatalf("formatPrice(0.5) = %q", got)
	}
}

func TestNormalizeDomain(t *testing.T) {
	for input, want := range map[string]string{
		"https://www.Example.com/path": "example.com",
		"github.com":                   "github.com",
		"a.b.co.uk":                    "a.b.co.uk",
	} {
		got, ok := normalizeDomain(input)
		if !ok || got != want {
			t.Errorf("%s -> %q (%v), want %q", input, got, ok, want)
		}
	}
	for _, input := range []string{"", "not a domain", "localhost", "-bad.com", strings.Repeat("a", 260) + ".com"} {
		if _, ok := normalizeDomain(input); ok {
			t.Errorf("%q should be rejected", input)
		}
	}
}

func TestWhoisReportExtractsFields(t *testing.T) {
	raw := "Domain Name: EXAMPLE.COM\nRegistrar: Example Registrar\nCreation Date: 1995-08-14T04:00:00Z\n" +
		"Registry Expiry Date: 2030-08-13T04:00:00Z\nName Server: A.IANA-SERVERS.NET\nName Server: A.IANA-SERVERS.NET\nName Server: B.IANA-SERVERS.NET\n"
	pages := whoisReport("example.com", raw)
	head := pages[0]
	for _, want := range []string{"Example Registrar", "1995-08-14", "A.IANA-SERVERS.NET", "B.IANA-SERVERS.NET"} {
		if !strings.Contains(head, want) {
			t.Errorf("summary is missing %q:\n%s", want, head)
		}
	}
	if strings.Count(head, "A.IANA-SERVERS.NET") != 1 {
		t.Error("duplicate name servers should be collapsed")
	}
	if !strings.HasPrefix(head, "<pre>") || !strings.HasSuffix(pages[len(pages)-1], "</blockquote>") {
		t.Error("summary and raw record should be wrapped")
	}
}

func TestParseDuration(t *testing.T) {
	for input, want := range map[string]time.Duration{
		"60s": time.Minute, "5m": 5 * time.Minute, "1h": time.Hour, "2d": 48 * time.Hour,
		"": 0, "forever": 0, "0m": 0, "-3h": 0,
	} {
		if got := parseDuration(input); got != want {
			t.Errorf("parseDuration(%q) = %v, want %v", input, got, want)
		}
	}
	if got := formatDuration(0); got != "永久" {
		t.Fatalf("formatDuration(0) = %q", got)
	}
	if got := formatDuration(90 * time.Minute); got != "1h" {
		t.Fatalf("formatDuration(90m) = %q", got)
	}
}

func TestApplyTextStyle(t *testing.T) {
	if got := applyTextStyle("ab12", "mono"); got != "𝚊𝚋𝟷𝟸" {
		t.Fatalf("mono style = %q", got)
	}
	if got := applyTextStyle("CZ", "double"); got != "ℂℤ" {
		t.Fatalf("double-struck capitals outside the block are wrong: %q", got)
	}
	if got := applyTextStyle("中文 ok", "sans"); !strings.HasPrefix(got, "中文 ") {
		t.Fatalf("non-latin text must pass through: %q", got)
	}
	if got := applyTextStyle("abc", "normal"); got != "abc" {
		t.Fatalf("normal style changed the text: %q", got)
	}
}

func TestZoneLabel(t *testing.T) {
	for format, want := range map[string]string{"GMT": "GMT+8", "UTC": "UTC+8", "OFFSET": "+08:00", "SIMP": "CST", "custom:北京时间": "北京时间"} {
		if got := zoneLabel("Asia/Shanghai", format); got != want {
			t.Errorf("zoneLabel(%q) = %q, want %q", format, got, want)
		}
	}
	if got := zoneLabel("UTC", "GMT"); got != "GMT" {
		t.Fatalf("UTC label = %q", got)
	}
	if got := zoneLabel("Not/AZone", "GMT"); got != "" {
		t.Fatalf("an unknown zone should render empty, got %q", got)
	}
}

// Re-saving the nickname must not bake in the clock the previous run
// appended, or the base name grows a timestamp every time.
func TestCleanNickname(t *testing.T) {
	if got := cleanNickname("我要一直陪着我 🕘 09:30"); got != "我要一直陪着我" {
		t.Fatalf("cleanNickname = %q", got)
	}
	if got := cleanNickname("  Alice   Smith  "); got != "Alice Smith" {
		t.Fatalf("cleanNickname = %q", got)
	}
}

func TestUTF16Helpers(t *testing.T) {
	if got := utf16Len("a中𝚊"); got != 4 {
		t.Fatalf("utf16Len = %d, want 4 (surrogate pair counts twice)", got)
	}
	if got := utf16Slice("hello 世界", 6, 2); got != "世界" {
		t.Fatalf("utf16Slice = %q", got)
	}
}

func TestNewerVersion(t *testing.T) {
	if !newer("", "v1.0.0") || !newer("dev", "v1.0.0") || !newer("0.1.0", "v0.2.0") {
		t.Error("a new release should be detected")
	}
	if newer("v1.2.3", "1.2.3") {
		t.Error("the same version with a different v prefix is not newer")
	}
}

// The .yvlu argument grammar is positional and irregular; these are the
// spellings its users already have in their fingers.
func TestParseYvlu(t *testing.T) {
	cases := []struct {
		text     string
		ok       bool
		count    int
		reply    bool
		format   string
		fakeText string
	}{
		{".yvlu", true, 1, false, "quote", ""},
		{".yvlu 3", true, 3, false, "quote", ""},
		{".yvlu r", true, 1, true, "quote", ""},
		{".yvlu r 4", true, 4, true, "quote", ""},
		{".yvlu r stories 2", true, 2, true, "stories", ""},
		{".yvlu png", true, 1, false, "image", ""},
		{".yvlu image 5", true, 5, false, "image", ""},
		{".yvlu stories", true, 1, false, "stories", ""},
		{".yvlu f 你好 世界", true, 1, false, "quote", "你好 世界"},
		{".yvlu fr 测试", true, 1, true, "quote", "测试"},
		{".yvlu u 12345 2", true, 2, false, "quote", ""},
		{".yvlu ur @name", true, 1, true, "quote", ""},
		{".yvlu nonsense", false, 0, false, "", ""},
	}
	for _, item := range cases {
		fields := strings.Fields(item.text)
		inv := &command.Invocation{Prefix: ".", Command: "yvlu", Args: fields[1:], Text: item.text}
		options, ok := parseYvlu(inv)
		if ok != item.ok {
			t.Errorf("%q: parsed=%v, want %v", item.text, ok, item.ok)
			continue
		}
		if !ok {
			continue
		}
		if options.Count != item.count || options.IncludeReply != item.reply || options.Format != item.format {
			t.Errorf("%q: count=%d reply=%v format=%q", item.text, options.Count, options.IncludeReply, options.Format)
		}
		if options.FakeText != item.fakeText {
			t.Errorf("%q: fake text %q, want %q", item.text, options.FakeText, item.fakeText)
		}
	}
}

func TestConvertEntities(t *testing.T) {
	entities := []tg.MessageEntityClass{
		&tg.MessageEntityBold{Offset: 0, Length: 4},
		&tg.MessageEntityTextURL{Offset: 5, Length: 3, URL: "https://example.com"},
		&tg.MessageEntityCustomEmoji{Offset: 9, Length: 2, DocumentID: 77},
		&tg.MessageEntityPre{Offset: 12, Length: 5, Language: "go"},
		&tg.MessageEntityUnknown{Offset: 20, Length: 1},
	}
	converted := convertEntities(entities, 0)
	if len(converted) != 4 {
		t.Fatalf("converted %d entities, want 4 (the unknown one is dropped)", len(converted))
	}
	if converted[0].Type != "bold" || converted[1].Type != "text_link" || converted[1].URL != "https://example.com" {
		t.Fatalf("unexpected conversion: %+v", converted)
	}
	if converted[2].CustomEmojiID != "77" || converted[3].Language != "go" {
		t.Fatalf("unexpected conversion: %+v", converted)
	}
}

// Faked text is cut out of the middle of the command, so the entities that
// survive have to move with it and the ones that straddle the cut clip.
func TestConvertEntitiesShift(t *testing.T) {
	converted := convertEntities([]tg.MessageEntityClass{
		&tg.MessageEntityBold{Offset: 10, Length: 4},
		&tg.MessageEntityItalic{Offset: 6, Length: 6},
		&tg.MessageEntityCode{Offset: 0, Length: 3},
	}, 8)
	if len(converted) != 2 {
		t.Fatalf("converted %+v, want the two that reach past the cut", converted)
	}
	if converted[0].Offset != 2 || converted[0].Length != 4 {
		t.Errorf("shifted entity is %+v", converted[0])
	}
	if converted[1].Offset != 0 || converted[1].Length != 4 {
		t.Errorf("straddling entity should clip to 0..4, got %+v", converted[1])
	}
}

// Every asset path comes out of a remote JSON document, so it is untrusted.
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

func TestNameHashIsStableAndPositive(t *testing.T) {
	first, second := nameHash("某个频道"), nameHash("某个频道")
	if first != second {
		t.Fatalf("the same name hashed to %d and %d", first, second)
	}
	if first < 0 {
		t.Fatalf("hash must be positive to serve as an id, got %d", first)
	}
	if nameHash("a") == nameHash("b") {
		t.Fatal("different names should not collide this easily")
	}
}
