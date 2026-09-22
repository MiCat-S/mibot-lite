package commands

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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

// The endpoint answers with two different shapes depending on whether the
// request asked it to detect the source language.
func TestParseTranslation(t *testing.T) {
	detected, err := parseTranslation([]byte(`[["你好世界","en"]]`))
	if err != nil {
		t.Fatal(err)
	}
	if detected.Text != "你好世界" || detected.Source != "en" {
		t.Fatalf("got %+v", detected)
	}
	// A request that named its source gets a bare string back.
	named, err := parseTranslation([]byte(`["早上好"]`))
	if err != nil {
		t.Fatal(err)
	}
	if named.Text != "早上好" || named.Source != "" {
		t.Fatalf("got %+v", named)
	}
	for _, raw := range []string{``, `{}`, `[]`, `[[]]`, `[[""]]`, `[""]`, `not json`} {
		if _, err := parseTranslation([]byte(raw)); err == nil {
			t.Errorf("%q should not parse into a translation", raw)
		}
	}
}

// The first argument is a target language only when it names one. Deciding
// by shape would eat the first word of "tr is this correct".
func TestNamedLanguage(t *testing.T) {
	for input, want := range map[string]string{
		"en": "en", "EN": "en", "english": "en", "英文": "en",
		"zh": "zh-CN", "cn": "zh-CN", "中文": "zh-CN", "tw": "zh-TW",
		"jp": "ja", "ja": "ja", "ko": "ko", "ru": "ru",
	} {
		got, ok := namedLanguage(input)
		if !ok || got != want {
			t.Errorf("namedLanguage(%q) = %q %v, want %q", input, got, ok, want)
		}
	}
	for _, input := range []string{"", "hello", "this", "翻译一下", "zzz", "xyzzy"} {
		if code, ok := namedLanguage(input); ok {
			t.Errorf("%q should be text, not the language %q", input, code)
		}
	}
	// "is" and "tr" are real codes and would be surprising to swallow, so
	// they are deliberately absent from the list.
	for _, ambiguous := range []string{"is", "no", "it"} {
		_, ok := namedLanguage(ambiguous)
		if ambiguous == "is" && ok {
			t.Error(`"is" should stay text: it is also an English word`)
		}
	}
}

// An unqualified translation of Chinese must not ask for Chinese back.
func TestHasHan(t *testing.T) {
	if !hasHan("今天天气不错") || !hasHan("mixed 中文 text") {
		t.Error("Chinese text should be detected")
	}
	if hasHan("hello world") || hasHan("こんにちは") || hasHan("") {
		t.Error("non-Han text should not be detected as Chinese")
	}
}

func TestLanguageName(t *testing.T) {
	if got := languageName("en"); got != "英语" {
		t.Fatalf("languageName(en) = %q", got)
	}
	if got := languageName("qqq"); got != "qqq" {
		t.Fatalf("an unknown code should render as itself, got %q", got)
	}
}

// A server's public address goes into a chat with the reading, so it is
// masked rather than published.
func TestMaskAddress(t *testing.T) {
	if got := maskAddress("43.153.150.179"); got != "43.153.x.x" {
		t.Fatalf("maskAddress = %q", got)
	}
	if got := maskAddress("2400:cb00:1234:5678::1"); !strings.HasSuffix(got, "…") || strings.Contains(got, "5678") {
		t.Fatalf("IPv6 was not masked: %q", got)
	}
	if got := maskAddress(""); got != "未知" {
		t.Fatalf("empty address = %q", got)
	}
	if got := maskAddress("nonsense"); got != "…" {
		t.Fatalf("unparseable address should not be echoed, got %q", got)
	}
}

func TestFormatSpeed(t *testing.T) {
	for bits, want := range map[float64]string{
		2.5e9: "2.50 Gbps", 118.4e6: "118.4 Mbps", 5400: "5.4 Kbps", 12: "12 bps",
	} {
		if got := formatSpeed(bits); got != want {
			t.Errorf("formatSpeed(%v) = %q, want %q", bits, got, want)
		}
	}
}

// TestOoklaInstallLive downloads the pinned CLI and runs it, so the digest
// in the source is checked against what the vendor actually serves rather
// than assumed. Skipped unless MIBOT_SPEEDTEST_LIVE=1.
func TestOoklaInstallLive(t *testing.T) {
	if os.Getenv("MIBOT_SPEEDTEST_LIVE") != "1" {
		t.Skip("set MIBOT_SPEEDTEST_LIVE=1 to download the real CLI")
	}
	dir := t.TempDir()
	path, err := installOokla(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("installed file is not executable: %v %v", info, err)
	}
	t.Logf("装到 %s（%.1f MB）", path, float64(info.Size())/(1<<20))

	// It must also be found the next time without downloading again.
	found, kind := externalTool(dir)
	if found != path || kind != "ookla" {
		t.Fatalf("externalTool found %q (%s), want the installed copy", found, kind)
	}

	result, err := runExternal(context.Background(), path, "ookla", filepath.Join(dir, "speedtest"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s：延迟 %s 抖动 %s，下载 %s，上传 %s",
		result.Source, formatLatency(result.Latency), formatLatency(result.Jitter),
		formatSpeed(result.Download), formatSpeed(result.Upload))
	t.Logf("服务器 %s，ISP %s，出口 %s", result.Server, result.ISP, maskAddress(result.ExternalIP))
	if result.ExternalIP == "" {
		t.Error("the CLI reports the external address and the reading should carry it")
	}
	if result.Link == "" {
		t.Error("without a result link there is no picture to send")
	}
	if result.Download <= 0 || result.Upload <= 0 || result.Latency <= 0 {
		t.Error("the CLI reported an empty measurement")
	}
	if result.Server == "" || result.ISP == "" {
		t.Error("the CLI should report its server and the ISP")
	}
}

// A tampered archive must never reach the disk, let alone be executed.
func TestExtractOoklaRejectsJunk(t *testing.T) {
	if _, err := extractOokla([]byte("not a gzip stream"), t.TempDir()); err == nil {
		t.Fatal("garbage should not extract")
	}
}

// The result picture is fetched only from Speedtest's own result pages,
// and only when what comes back is really a PNG.
func TestResultImageRefusesOtherSources(t *testing.T) {
	for _, link := range []string{"", "http://evil.example/x.png", "https://example.com/result/c/1",
		"https://www.speedtest.net.evil.com/result/c/1"} {
		if image := resultImage(context.Background(), link); image != nil {
			t.Errorf("%q should not be fetched", link)
		}
	}
}

// The server list is the only way to learn an ID, so it has to show the
// ID, stay short enough for a chat, and say which one is currently pinned.
func TestRenderServersMarksThePinnedOne(t *testing.T) {
	servers := make([]speedServer, 0, speedListLimit+5)
	for i := 0; i < speedListLimit+5; i++ {
		servers = append(servers, speedServer{ID: 1000 + i, Name: "Server", Location: "Tokyo", Country: "Japan"})
	}
	text := renderServers(servers, 1003, ".")
	if !strings.Contains(text, "1003") || !strings.Contains(text, "✅") {
		t.Errorf("the pinned server is not marked:\n%s", text)
	}
	if strings.Contains(text, strconv.Itoa(1000+speedListLimit)) {
		t.Errorf("the list ran past its limit:\n%s", text)
	}
	if !strings.Contains(text, "1000") {
		t.Errorf("the nearest server is missing:\n%s", text)
	}
}

// TestListServersLive checks the two things the list is for: that the CLI
// will enumerate servers from this host, and that an ID taken from that
// list can actually be measured against. Skipped unless
// MIBOT_SPEEDTEST_LIVE=1.
func TestListServersLive(t *testing.T) {
	if os.Getenv("MIBOT_SPEEDTEST_LIVE") != "1" {
		t.Skip("set MIBOT_SPEEDTEST_LIVE=1 to reach the real servers")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "speedtest")
	path, err := installOokla(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	servers, err := listServers(context.Background(), path, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("取到 %d 个服务器", len(servers))
	for index, server := range servers {
		if index >= 3 {
			break
		}
		t.Logf("  %d  %s  %s %s", server.ID, server.Name, server.Location, server.Country)
	}
	if servers[0].ID <= 0 || servers[0].Name == "" {
		t.Fatalf("the first entry is unusable: %+v", servers[0])
	}

	// A listed server is not necessarily reachable: 56935 in Tokyo
	// answered the enumeration and then refused the socket. That is the
	// case the command falls back on, so the test walks the list the same
	// way rather than insisting the first entry works.
	var measured *reading
	var lastErr error
	for _, server := range servers {
		result, err := runExternal(context.Background(), path, "ookla", home, server.ID)
		if err == nil {
			measured = result
			t.Logf("指定 %d 测得：%s，下载 %s，上传 %s", server.ID, result.Server,
				formatSpeed(result.Download), formatSpeed(result.Upload))
			break
		}
		lastErr = err
		t.Logf("服务器 %d（%s）测不通", server.ID, server.Name)
	}
	if measured == nil {
		// Not a defect here, but the reason has to be readable — that is
		// what tells the operator to pick another ID.
		if lastErr == nil || lastErr.Error() == "" {
			t.Fatal("every server failed and none said why")
		}
		t.Skip("这台机器到列表里每个服务器都不通，指定测速无法验证")
	}
	if measured.Download <= 0 || measured.Server == "" {
		t.Error("pinning a server produced an empty measurement")
	}

	// Auto selection has to keep working, since that is where a failed
	// pin lands.
	auto, err := runExternal(context.Background(), path, "ookla", home, 0)
	if err != nil {
		t.Fatalf("auto selection failed after a pinned run: %v", err)
	}
	t.Logf("自动挑选：%s，下载 %s", auto.Server, formatSpeed(auto.Download))
}

// The CLI's failure reason is a JSON log record; a chat needs the
// sentence inside it, not the envelope.
func TestLastLineUnwrapsTheCLIRecord(t *testing.T) {
	raw := `{"type":"log","timestamp":"2026-09-22T10:53:22Z","message":"Could not retrieve or read configuration","level":"error"}`
	if got := lastLine(raw); got != "Could not retrieve or read configuration" {
		t.Errorf("lastLine = %q", got)
	}
	if got := lastLine("ookla failed: signal: aborted"); got != "ookla failed: signal: aborted" {
		t.Errorf("a plain line should survive unchanged, got %q", got)
	}
	if got := lastLine("  \n\n plain \n\n"); got != "plain" {
		t.Errorf("lastLine = %q", got)
	}
}

// The CLI's wording is accurate and useless. These two were met for real
// on the deployment, so the translation is pinned.
func TestExplainCLI(t *testing.T) {
	if got := explainCLI("Configuration - Could not retrieve or read configuration (ConfigurationError)"); !strings.Contains(got, "过一阵再试") {
		t.Errorf("throttling was not explained: %q", got)
	}
	if got := explainCLI("Error: [0] Cannot read from socket: "); !strings.Contains(got, "换一个 ID") {
		t.Errorf("an unreachable server was not explained: %q", got)
	}
	if got := explainCLI("signal: aborted"); got != "signal: aborted" {
		t.Errorf("an unknown reason should pass through, got %q", got)
	}
}
