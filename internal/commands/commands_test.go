package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"os"
	"path/filepath"
	"regexp"
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
	// 别名会解析成对应的货币代码。
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

// 重新保存昵称时，不能把上一次运行追加的时钟也存进去，否则基础昵称
// 每次都会多长出一个时间戳。
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

// .yvlu 的参数语法按位置解析，而且不规则；下面这些是用户早已用顺手的写法。
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

// 伪造的文字是从命令中间截出来的，所以保留下来的实体要跟着平移，
// 横跨截断点的实体要被裁掉一截。
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

// 这个接口的应答有两种结构，取决于请求里有没有要求它检测源语言。
func TestParseTranslation(t *testing.T) {
	detected, err := parseTranslation([]byte(`[["你好世界","en"]]`))
	if err != nil {
		t.Fatal(err)
	}
	if detected.Text != "你好世界" || detected.Source != "en" {
		t.Fatalf("got %+v", detected)
	}
	// 指明了源语言的请求，拿回来的是一个裸字符串。
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

// 第一个参数只有确实是某个语言名时，才算目标语言。如果按外形判断，
// "tr is this correct" 里的第一个词就会被吞掉。
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
	// "is" 和 "tr" 都是真实的语言代码，但被当成语言吞掉会让人意外，
	// 所以故意没放进列表。
	for _, ambiguous := range []string{"is", "no", "it"} {
		_, ok := namedLanguage(ambiguous)
		if ambiguous == "is" && ok {
			t.Error(`"is" should stay text: it is also an English word`)
		}
	}
}

// 翻译中文又没指定目标语言时，不能再要求译成中文。
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

// 服务器的公网地址会和测速结果一起发进聊天，所以要打码，不能原样公开。
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

// TestOoklaInstallLive 下载锁定版本的 CLI 并运行，用厂商实际提供的文件
// 核对源码里的摘要，而不是想当然地认为它对。只在 MIBOT_SPEEDTEST_LIVE=1
// 时运行，否则跳过。
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

	// 下一次还要能直接找到它，不用重新下载。
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

// 被篡改的压缩包绝不能落盘，更不能被执行。
func TestExtractOoklaRejectsJunk(t *testing.T) {
	if _, err := extractOokla([]byte("not a gzip stream"), t.TempDir()); err == nil {
		t.Fatal("garbage should not extract")
	}
}

// 结果图片只从 Speedtest 自己的结果页获取，而且拿回来的确实是 PNG 才用。
func TestResultImageRefusesOtherSources(t *testing.T) {
	for _, link := range []string{"", "http://evil.example/x.png", "https://example.com/result/c/1",
		"https://www.speedtest.net.evil.com/result/c/1"} {
		if image := resultImage(context.Background(), link); image != nil {
			t.Errorf("%q should not be fetched", link)
		}
	}
}

// 服务器列表是得知 ID 的唯一途径，所以它必须显示 ID、长度适合放进
// 聊天，还要标出当前固定的是哪一个。
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
	// 每个服务器占一行，否则列表又会变回一大片文字：表头、空行、
	// speedListLimit 个服务器、空行、页脚。
	if got := len(strings.Split(text, "\n")); got != speedListLimit+4 {
		t.Errorf("the list is %d lines, want %d", got, speedListLimit+4)
	}
	// 所有服务器共同的国家放在表头，只出现一次。
	if strings.Count(text, "Japan") != 1 {
		t.Errorf("the country is repeated per line:\n%s", text)
	}
	// 页脚必须是读者能直接复制使用的内容。
	if !strings.Contains(text, ".speedtest 1003") {
		t.Errorf("the footer has no usable example:\n%s", text)
	}
}

// 服务器分属不同国家时，列表要标明各自是哪个国家。
func TestRenderServersKeepsMixedCountries(t *testing.T) {
	text := renderServers([]speedServer{
		{ID: 1, Name: "A", Location: "Tokyo", Country: "Japan"},
		{ID: 2, Name: "B", Location: "Seoul", Country: "Korea"},
	}, 0, ".")
	if !strings.Contains(text, "Japan") || !strings.Contains(text, "Korea") {
		t.Errorf("a mixed list lost its countries:\n%s", text)
	}
}

// TestListServersLive 检查列表要满足的两件事：CLI 能在这台主机上列出
// 服务器；从列表里取出的 ID 确实能用来测速。只在
// MIBOT_SPEEDTEST_LIVE=1 时运行，否则跳过。
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

	// 列出来的服务器不一定连得上：东京的 56935 回应了枚举，随后却拒绝了
	// 套接字连接。这正是命令会做退回处理的情形，所以测试也同样沿着列表
	// 往下试，而不是硬要第一个能用。
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
		// 这本身不算缺陷，但原因必须看得懂：运维正是靠它才知道要换一个 ID。
		if lastErr == nil || lastErr.Error() == "" {
			t.Fatal("every server failed and none said why")
		}
		t.Skip("这台机器到列表里每个服务器都不通，指定测速无法验证")
	}
	if measured.Download <= 0 || measured.Server == "" {
		t.Error("pinning a server produced an empty measurement")
	}

	// 自动挑选必须一直可用，因为固定的服务器测不通时就会落到这里。
	auto, err := runExternal(context.Background(), path, "ookla", home, 0)
	if err != nil {
		t.Fatalf("auto selection failed after a pinned run: %v", err)
	}
	t.Logf("自动挑选：%s，下载 %s", auto.Server, formatSpeed(auto.Download))
}

// CLI 给出的失败原因是一条 JSON 日志记录；聊天里需要的是其中那句话，
// 而不是外面那层包装。
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

// CLI 的措辞准确，但对用户没什么用。这两条是在部署环境里真实遇到过的，
// 所以把它们的翻译固定下来。
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

// 每个子命令都必须出现在帮助里，否则跟不存在没什么两样：.speedtest list
// 当初就是这样漏掉的。
func TestSpeedtestHelpDocumentsEverySubcommand(t *testing.T) {
	text := speedtestHelp(t.TempDir(), ".", 0)
	for _, wanted := range []string{".speedtest list", ".speedtest set", ".speedtest clear", ".speedtest config", ".st"} {
		if !strings.Contains(text, wanted) {
			t.Errorf("the help never mentions %q", wanted)
		}
	}
	if !strings.Contains(text, "自动挑选") {
		t.Error("the help does not say which server is in use")
	}
	if pinned := speedtestHelp(t.TempDir(), ".", 48463); !strings.Contains(pinned, "48463") {
		t.Error("the help does not show the pinned server")
	}
}

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

// 说明文字跟着备份文件本身一起发，不管有多少个命令配置文件，都得控制在
// Telegram 的 1024 字符以内。
func TestBackupCaptionFits(t *testing.T) {
	names := []string{"config.json", "gotd-session.json", ".env"}
	for i := 0; i < 40; i++ {
		names = append(names, fmt.Sprintf("data/command%02d.json", i))
	}
	caption := backupCaption("0.1.12", names, 48000)
	plain := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(caption, "")
	if n := len([]rune(plain)); n > 1024 {
		t.Errorf("caption is %d characters, over Telegram's 1024", n)
	}
	for _, wanted := range []string{"--restore", "不要转发", "43 个文件"} {
		if !strings.Contains(caption, wanted) {
			t.Errorf("caption is missing %q:\n%s", wanted, plain)
		}
	}
}

func TestBackupHelpSaysWhereItGoesAndHowToRestore(t *testing.T) {
	text := backupHelp(".")
	for _, wanted := range []string{"收藏夹", "--restore", "--force", ".log"} {
		if !strings.Contains(text, wanted) {
			t.Errorf("the help never mentions %q", wanted)
		}
	}
}

// Telegram 给出的四种链接形式，以及一些只是看起来像链接的东西。
func TestParseLink(t *testing.T) {
	cases := map[string]messageLink{
		"https://t.me/c/1234567890/42":        {ChatID: "-1001234567890", ID: 42},
		"t.me/c/1234567890/5/42":              {ChatID: "-1001234567890", ID: 42},
		"https://t.me/durov/123":              {Username: "durov", ID: 123},
		"https://t.me/some_group/7/88?single": {Username: "some_group", ID: 88},
		"telegram.me/durov/9":                 {Username: "durov", ID: 9},
	}
	for text, want := range cases {
		got, ok := parseLink(text)
		if !ok || got.ChatID != want.ChatID || got.Username != want.Username || got.ID != want.ID {
			t.Errorf("parseLink(%q) = %+v %v, want %+v", text, got, ok, want)
		}
	}
	for _, text := range []string{"https://t.me/durov", "https://example.com/c/1/2", "t.me/c/abc/1", "hello", "t.me/ab/1"} {
		if _, ok := parseLink(text); ok {
			t.Errorf("%q parsed as a message link", text)
		}
	}
}

func TestParseSaveArgs(t *testing.T) {
	request, err := parseSaveArgs([]string{"https://t.me/durov/1", "https://t.me/durov/2", "@friend"})
	if err != nil || len(request.Links) != 2 || request.Target != "@friend" {
		t.Errorf("links and a target: %+v %v", request, err)
	}
	request, err = parseSaveArgs([]string{"t.me/c/123/100|t.me/c/123/5"})
	if err != nil || request.Range == nil || request.Range[0].ID != 5 || request.Range[1].ID != 100 {
		t.Errorf("a reversed range should be put in order: %+v %v", request, err)
	}
	for label, args := range map[string][]string{
		"two chats":      {"t.me/c/1/1|t.me/c/2/9"},
		"half a range":   {"t.me/c/1/1|nope"},
		"two targets":    {"t.me/durov/1", "@a", "@b"},
		"range and link": {"t.me/c/1/1|t.me/c/1/9", "t.me/durov/1"},
	} {
		if _, err := parseSaveArgs(args); err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
}

// 和人的私聊没有 t.me 地址；硬编一个出来，打印的就是一条打不开的链接。
func TestLinkURL(t *testing.T) {
	if got := (messageLink{ChatID: "-1001234", ID: 5}).url(); got != "https://t.me/c/1234/5" {
		t.Errorf("channel url = %q", got)
	}
	if got := (messageLink{Username: "durov", ID: 5}).url(); got != "https://t.me/durov/5" {
		t.Errorf("public url = %q", got)
	}
	if got := (messageLink{ChatID: "777", ID: 5}).url(); got != "" {
		t.Errorf("a private chat got a url: %q", got)
	}
}

// 本地保存按发送者起的名字给文件命名，这个名字不能让文件跑出目录。
func TestSanitizeSegment(t *testing.T) {
	for input, want := range map[string]string{
		"../../etc/passwd": "etc_passwd",
		"视频 2024.mp4":      "视频_2024.mp4",
		"...":              "file",
		"a/b\\c":           "a_b_c",
	} {
		if got := sanitizeSegment(input); got != want {
			t.Errorf("sanitizeSegment(%q) = %q, want %q", input, got, want)
		}
	}
	if got := localExtension(&bot.MediaSource{MimeType: "video/mp4"}); got != ".mp4" {
		t.Errorf("video/mp4 saved as %q", got)
	}
}

func TestFindAddress(t *testing.T) {
	for text, want := range map[string]string{
		"连不上 8.8.8.8 了":                 "8.8.8.8",
		"看这个 https://example.com/a?b=1": "example.com",
		"example.org 挂了吗":               "example.org",
		"2001:4860:4860::8888":          "2001:4860:4860::8888",
		"没有地址":                          "",
	} {
		if got := findAddress(text); got != want {
			t.Errorf("findAddress(%q) = %q, want %q", text, got, want)
		}
	}
}

// 只查发卡行前缀；粘贴进来的卡号其余部分绝不能进入请求。
func TestBINDigitsNeverSendsMoreThanEight(t *testing.T) {
	if got, ok := binDigits("4150 4212 3456 7890"); !ok || got != "41504212" {
		t.Errorf("a full card number became %q", got)
	}
	if got, ok := binDigits("415042"); !ok || got != "415042" {
		t.Errorf("six digits became %q", got)
	}
	if _, ok := binDigits("41504"); ok {
		t.Error("five digits were accepted")
	}
}

// 下面的应答是在部署环境上查 415042 时 binlist.net 返回的内容，
// 留着它是为了按真实结构检查渲染结果。
func TestRenderBINFromARealResponse(t *testing.T) {
	var result binResult
	raw := `{"number":{},"scheme":"visa","type":"credit","brand":"Visa Rewards","country":{"numeric":"643","alpha2":"RU","name":"Russian Federation (the)","emoji":"🇷🇺","currency":"RUB"},"bank":{"name":"(Ofac Sanctioned) Vtb Bank Pjsc"}}`
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	text := renderBIN("415042", result)
	for _, wanted := range []string{"Visa", "贷记卡", "REWARDS", "Russian Federation", "卢布（RUB）", "Vtb Bank", "预付卡：未知"} {
		if !strings.Contains(text, wanted) {
			t.Errorf("rendering lost %q:\n%s", wanted, text)
		}
	}
	if strings.Contains(text, "(the)") {
		t.Errorf("the country kept binlist's article:\n%s", text)
	}
}

// 同样，这是 ip-api 对 8.8.8.8 的真实应答。
func TestRenderIPFromARealResponse(t *testing.T) {
	var result ipResult
	raw := `{"status":"success","country":"美国","regionName":"弗吉尼亚州","city":"Ashburn","timezone":"America/New_York","isp":"Google LLC","org":"Google Public DNS","as":"AS15169 Google LLC","proxy":false,"hosting":true,"query":"8.8.8.8"}`
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	text := renderIP(result)
	for _, wanted := range []string{"8.8.8.8", "美国 · 弗吉尼亚州 · Ashburn", "Google LLC", "数据中心", "bgp.he.net/AS15169"} {
		if !strings.Contains(text, wanted) {
			t.Errorf("rendering lost %q:\n%s", wanted, text)
		}
	}
	if strings.Contains(text, "代理") {
		t.Errorf("a non-proxy was flagged as one:\n%s", text)
	}
}

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
