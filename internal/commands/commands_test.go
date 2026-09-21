package commands

import (
	"strings"
	"testing"
	"time"
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

func TestIPPrefixAndMask(t *testing.T) {
	if got := ipPrefix("1.2.3.4", 24); got != "1.2.3.0/24" {
		t.Fatalf("/24 prefix = %q", got)
	}
	if got := ipPrefix("1.2.3.4", 23); got != "1.2.2.0/23" {
		t.Fatalf("/23 prefix = %q", got)
	}
	if got := maskIP("peer 203.0.113.9 and 198.51.100.7"); strings.Contains(got, "113.9") || strings.Contains(got, "100.7") {
		t.Fatalf("addresses were not masked: %q", got)
	}
	if !validIPv4("255.255.255.255") || validIPv4("256.1.1.1") || validIPv4("1.2.3") || validIPv4("01.2.3.4") {
		t.Fatal("IPv4 validation is wrong")
	}
	if got := extractIPv4("see 8.8.8.8/24 please"); got != "8.8.8.8" {
		t.Fatalf("extract = %q", got)
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
