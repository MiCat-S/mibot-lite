package bgp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestParseTarget(t *testing.T) {
	for input, want := range map[string]string{
		"1.1.1.1":                "1.1.1.1",
		" 8.8.8.8 ":              "8.8.8.8",
		"2001:4860:4860::8888":   "2001:4860:4860::8888",
		"1.1.1.0/24":             "1.1.1.0/24",
		"1.1.1.1/24":             "1.1.1.0/24", // 主机位清零
		"1.1.1.1/32":             "1.1.1.1",    // 单个地址按 IP 查
		"2606:4700::/32":         "2606:4700::/32",
		"AS13335":                "AS13335",
		"as13335":                "AS13335",
		"13335":                  "AS13335",
		"::ffff:1.1.1.1":         "1.1.1.1",
		"https://1.1.1.1:8443/x": "1.1.1.1",
		"[2606:4700::1111]:443":  "2606:4700::1111",
		"ASN4134":                "AS4134",
		"看看 AS4134 的上游":          "AS4134",
		"4294967295":             "AS4294967295",
		"2001:db8::1 和 9.9.9.9":  "2001:db8::1",
		"2606:4700::1111.":       "2606:4700::1111",
		"a 1.1.1.1 b 2.2.2.2":    "1.1.1.1",
		"AS13335 宣告了 1.1.1.0/24": "1.1.1.0/24", // IP 和前缀优先于 AS 号
		"fe80::1%eth0":           "fe80::1",    // 区域去掉，之后按保留地址拒绝
	} {
		got, ok := parseTarget(input)
		if !ok || got.String() != want {
			t.Errorf("parseTarget(%q) = %q, %v; want %q", input, got.String(), ok, want)
		}
	}
	for _, input := range []string{"", "example.com", "4294967296", "AS", "1.1.1.1/33", "hello"} {
		if got, ok := parseTarget(input); ok {
			t.Errorf("parseTarget(%q) = %q, want nothing", input, got.String())
		}
	}
}

// 被回复的消息是随便什么文字：时间、MAC 地址、代码里的 :: 都不能当成地址，纯数字也不当 AS 号。
func TestFindTargetInText(t *testing.T) {
	for text, want := range map[string]string{
		"连不上 1.1.1.1 了":                    "1.1.1.1",
		"地址是1.1.1.1吗":                      "1.1.1.1",
		"12:30:45 的时候 8.8.4.4 丢包":          "8.8.4.4",
		"网卡 aa:bb:cc:dd:ee:ff 拿到了 9.9.9.9": "9.9.9.9",
		"std::vector 和 AS174":              "AS174",
		"Cogent 是 as174。":                  "AS174",
	} {
		got, ok := findTarget(text)
		if !ok || got.String() != want {
			t.Errorf("findTarget(%q) = %q, %v; want %q", text, got.String(), ok, want)
		}
	}
	for _, text := range []string{
		"12:30:45", "std::vector<int>", "Foo::Bar", "deadbeef:cafe::1", "端口 13335", "1234.1.1.1", "::ffff:1.2.3.4x", "没有地址",
	} {
		if got, ok := findTarget(text); ok {
			t.Errorf("findTarget(%q) = %q, want nothing", text, got.String())
		}
	}
}

func TestReserved(t *testing.T) {
	for _, input := range []string{
		"10.0.0.1", "192.168.1.1", "172.16.5.4", "127.0.0.1", "100.64.1.1", "169.254.1.1", "0.1.2.3",
		"192.0.2.10", "198.51.100.1", "203.0.113.9", "198.18.0.1", "224.0.0.1", "255.255.255.255",
		"::1", "fd00::1", "fe80::1", "ff02::1", "2001:db8::1", "3fff::1", "::",
		"10.0.0.0/8", "192.168.0.0/24", "fc00::/7", "2001:db8::/48", "0.0.0.0/0", "1.0.0.0/7", "2000::/3",
		"AS0", "AS23456", "AS64512", "AS65535", "AS65551", "AS4200000000", "AS4294967295",
	} {
		target, ok := parseTarget(input)
		if !ok {
			t.Errorf("parseTarget(%q) failed", input)
			continue
		}
		if target.reserved() == "" {
			t.Errorf("%s should be rejected", input)
		}
	}
	for _, input := range []string{"1.1.1.1", "8.8.8.0/24", "192.0.0.0/8", "2606:4700::1111", "2001:4860::/32", "AS13335", "AS4134", "AS131072"} {
		target, _ := parseTarget(input)
		if reason := target.reserved(); reason != "" {
			t.Errorf("%s was rejected: %s", input, reason)
		}
	}
	target, _ := parseTarget("192.168.1.1")
	if reason := target.reserved(); reason != "192.168.1.1 是内网或保留地址，公网上没有它的路由" {
		t.Errorf("reason = %q", reason)
	}
}

// 下面是 RIPEstat 的真实应答（2026-09-30），去掉了用不到的字段，邻居列表只留几个。
const (
	overview1111 = `{"messages":[["warning","Given resource is not announced but result has been aligned to first-level less-specific (1.1.1.0/24)."]],"status":"ok","status_code":200,
		"data":{"is_less_specific":true,"announced":true,"asns":[{"asn":13335,"holder":"CLOUDFLARENET - Cloudflare, Inc."}],"related_prefixes":[],"resource":"1.1.1.0/24","type":"prefix",
		"block":{"resource":"1.0.0.0/8","desc":"APNIC (Status: ALLOCATED)","name":"IANA IPv4 Address Space Registry"},"actual_num_related":0,"num_filtered_out":0}}`
	overview8888 = `{"messages":[],"status":"ok","status_code":200,
		"data":{"is_less_specific":false,"announced":true,"asns":[{"asn":15169,"holder":"GOOGLE - Google LLC"}],"related_prefixes":["8.0.0.0/9","8.0.0.0/12"],"resource":"8.8.8.0/24",
		"block":{"resource":"8.0.0.0/8","desc":"Administered by ARIN"},"actual_num_related":2}}`
	overviewUnannounced = `{"messages":[],"status":"ok","status_code":200,
		"data":{"is_less_specific":false,"announced":false,"asns":[],"related_prefixes":[],"resource":"25.0.0.1","type":"prefix",
		"block":{"resource":"25.0.0.0/8","desc":"Administered by RIPE NCC"},"actual_num_related":0}}`
	neighbours13335 = `{"messages":[["info","Query time has been set to the latest available time (2026-09-30 00:00 UTC)"]],"status":"ok","status_code":200,
		"data":{"resource":"13335","neighbour_counts":{"left":7,"right":2,"unique":10,"uncertain":1},"neighbours":[
		{"asn":10026,"type":"left","power":1,"v4_peers":2,"v6_peers":0},
		{"asn":174,"type":"left","power":5899,"v4_peers":63889,"v6_peers":85901},
		{"asn":3356,"type":"left","power":5795,"v4_peers":59049,"v6_peers":90343},
		{"asn":9002,"type":"left","power":3161,"v4_peers":47083,"v6_peers":5018},
		{"asn":1299,"type":"left","power":7584,"v4_peers":102011,"v6_peers":121918},
		{"asn":3257,"type":"left","power":4865,"v4_peers":43003,"v6_peers":65045},
		{"asn":34549,"type":"left","power":3063,"v4_peers":29110,"v6_peers":2019},
		{"asn":64000,"type":"right","power":99999,"v4_peers":1,"v6_peers":0},
		{"asn":64001,"type":"uncertain","power":88888,"v4_peers":1,"v6_peers":0},
		{"asn":13336,"type":"right","power":2,"v4_peers":1,"v6_peers":0}]}}`
	names13335Upstreams = `{"messages":[],"status":"ok","status_code":200,"data":{"names":{
		"1299":"TWELVE99 Arelion, fka Telia Carrier","174":"COGENT-174 - Cogent Communications, LLC","3356":"LEVEL3 - Level 3 Parent, LLC",
		"3257":"GTT-BACKBONE GTT Communications Inc.","9002":"RETN-AS <RETN> & co"},"resources":["1299","174","3356","3257","9002"]}}`
	geo1111 = `{"messages":[],"status":"ok","status_code":200,"data":{"located_resources":[{"resource":"1.1.1.1/32","locations":[
		{"country":"?","city":"","resources":["1.1.1.0/24"],"latitude":0.0,"longitude":0.0,"covered_percentage":100.0}],"unknown_percentage":0}],"unknown_percentage":{"v4":0.0}}}`
	geo8888 = `{"messages":[],"status":"ok","status_code":200,"data":{"located_resources":[{"resource":"8.8.8.8/32","locations":[
		{"country":"US","city":"","resources":["8.8.8.0/24"],"covered_percentage":100.0}],"unknown_percentage":0}]}}`
	geoSlash8 = `{"messages":[],"status":"ok","status_code":200,"data":{"located_resources":[{"resource":"8.0.0.0/8","locations":[
		{"country":"CN","city":"","covered_percentage":15.26},{"country":"US","city":"","covered_percentage":40.0},{"country":"SG","city":"","covered_percentage":11.13},
		{"country":"US","city":"","covered_percentage":26.21},{"country":"HK","city":"","covered_percentage":1.87}],"unknown_percentage":0.06}]}}`
	geoGB   = `{"messages":[],"status":"ok","status_code":200,"data":{"located_resources":[{"resource":"25.0.0.1/32","locations":[{"country":"GB","city":"London","covered_percentage":100.0}]}]}}`
	ptr1111 = `{"messages":[],"status":"ok","status_code":200,"data":{"query_time":"2026-09-30T14:29:00","resource":"1.1.1.1","result":["one.one.one.one"],"error":""}}`
	ptr8888 = `{"messages":[],"status":"ok","status_code":200,"data":{"resource":"8.8.8.8","result":["dns.google"],"error":""}}`
	ptrNone = `{"messages":[],"status":"ok","status_code":200,"data":{"query_time":"2026-09-30T14:32:00","resource":"25.0.0.1","result":null,"error":"Domain name doesn't exist (NXDOMAIN): 1.0.0.25.in-addr.arpa. (PTR)"}}`
	as13335 = `{"messages":[],"status":"ok","status_code":200,"data":{"type":"as","resource":"13335",
		"block":{"resource":"13312-15359","desc":"Assigned by ARIN","name":"IANA 16-bit Autonomous System (AS) Numbers Registry"},"holder":"CLOUDFLARENET - Cloudflare, Inc.","announced":true}}`
	counts13335 = `{"messages":[["info","Query time has been set to the latest time (2026-09-30 08:00 UTC) data is available for."]],"status":"ok","status_code":200,
		"data":{"counts":{"v4":{"originating":2448},"v6":{"originating":3065}},"resource":"13335","list_prefixes":false,"af":["v4","v6"],"types":["o"],"noise":["filter"]}}`
	rirAPNIC    = `{"messages":[],"status":"ok","status_code":200,"data":{"rirs":[{"rir":"APNIC","first_time":"2026-09-29T00:00:00","last_time":"2026-09-29T00:00:00"}],"resource":"1.1.1.1"}}`
	rirARIN     = `{"messages":[],"status":"ok","status_code":200,"data":{"rirs":[{"rir":"ARIN","first_time":"2026-09-29T00:00:00","last_time":"2026-09-29T00:00:00"}]}}`
	rirRIPE     = `{"messages":[],"status":"ok","status_code":200,"data":{"rirs":[{"rir":"RIPE NCC","first_time":"2026-09-29T00:00:00","last_time":"2026-09-29T00:00:00"}]}}`
	rirTwo      = `{"messages":[],"status":"ok","status_code":200,"data":{"rirs":[{"rir":"APNIC"},{"rir":"ARIN"},{"rir":"APNIC"}]}}`
	rirNone     = `{"messages":[],"status":"ok","status_code":200,"data":{"rirs":[],"resource":"99999999"}}`
	badResource = `{"messages":[["error","1.1.1.0/24 is of unsupported type IP prefix. It should be one of: IP address."]],"status":"error","status_code":400,"data":{}}`
)

// answer 是假服务对一个「接口 resource」的应答：状态码和正文。
type answer struct {
	status int
	body   string
}

// fakeRIPEstat 冒充 RIPEstat：按「接口 resource」查表应答，没有登记的一律 404。
// 返回记下的请求（「接口 resource」），测试用它确认查了什么、没查什么。
func fakeRIPEstat(t *testing.T, answers map[string]answer) func() []string {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/data.json")
		key := name + " " + r.URL.Query().Get("resource")
		if r.URL.Query().Get("sourceapp") != sourceApp {
			t.Errorf("%s: sourceapp = %q", key, r.URL.Query().Get("sourceapp"))
		}
		if name == "ris-prefixes" && r.URL.Query().Get("list_prefixes") != "false" {
			t.Errorf("ris-prefixes should not list the prefixes")
		}
		mu.Lock()
		calls = append(calls, key)
		mu.Unlock()
		reply, ok := answers[key]
		if !ok {
			reply = answer{http.StatusNotFound, "not found"}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_, _ = io.WriteString(w, reply.body)
	}))
	old := apiBase
	apiBase = server.URL
	t.Cleanup(func() { server.Close(); apiBase = old })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func ok(body string) answer { return answer{http.StatusOK, body} }

func mustTarget(t *testing.T, input string) target {
	t.Helper()
	target, found := parseTarget(input)
	if !found {
		t.Fatalf("parseTarget(%q) failed", input)
	}
	return target
}

// checkHTML 确认消息能按 Telegram 的 HTML 解析，并且不超过一页。
func checkHTML(t *testing.T, text string) {
	t.Helper()
	if _, _, err := bot.ParseHTML(text); err != nil {
		t.Errorf("message does not parse: %v\n%s", err, text)
	}
	if kit.UTF16Len(text) > command.PageLimit {
		t.Errorf("message has %d units", kit.UTF16Len(text))
	}
}

func contains(t *testing.T, text string, wanted ...string) {
	t.Helper()
	for _, want := range wanted {
		if !strings.Contains(text, want) {
			t.Errorf("message lost %q:\n%s", want, text)
		}
	}
}

func TestLookupIP(t *testing.T) {
	calls := fakeRIPEstat(t, map[string]answer{
		"prefix-overview 1.1.1.1":                    ok(overview1111),
		"maxmind-geo-lite 1.1.1.1":                   ok(geo1111),
		"reverse-dns-ip 1.1.1.1":                     ok(ptr1111),
		"asn-neighbours AS13335":                     ok(neighbours13335),
		"as-names AS1299,AS174,AS3356,AS3257,AS9002": ok(names13335Upstreams),
		"rir 1.1.1.1":                                ok(rirAPNIC),
	})
	r, err := lookup(context.Background(), quiet, mustTarget(t, "1.1.1.1"))
	if err != nil {
		t.Fatal(err)
	}
	text := render(r)
	checkHTML(t, text)
	if !strings.HasPrefix(text, "🛰 <b>BGP 路由信息</b>\n\n查询：<code>1.1.1.1</code>\n前缀：<code>1.1.1.0/24</code>\n") {
		t.Errorf("head:\n%s", text)
	}
	contains(t, text,
		"起源 AS：<code>AS13335</code> CLOUDFLARENET - Cloudflare, Inc.",
		"地区：—", // 任播地址的国家是 "?"
		"RIR：APNIC",
		"反向解析：<code>one.one.one.one</code>",
		"<b>AS13335 的上游</b>（共 7 个，按可见度列前 5 个）\n"+
			"• <code>AS1299</code> TWELVE99 Arelion, fka Telia Carrier\n"+
			"• <code>AS174</code> COGENT-174 - Cogent Communications, LLC\n"+
			"• <code>AS3356</code> LEVEL3 - Level 3 Parent, LLC\n"+
			"• <code>AS3257</code> GTT-BACKBONE GTT Communications Inc.\n"+
			"• <code>AS9002</code> RETN-AS &lt;RETN&gt; &amp; co\n",
		`<a href="https://bgp.he.net/net/1.1.1.0/24">1.1.1.0/24</a> · <a href="https://bgp.he.net/AS13335">AS13335</a>`,
	)
	for _, unwanted := range []string{"AS64000", "AS64001", "AS34549", "⚠️", "相关前缀"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("message should not have %q:\n%s", unwanted, text)
		}
	}
	if got := len(calls()); got != 6 {
		t.Errorf("made %d calls: %q", got, calls())
	}
}

// 前缀查询不查反向解析；跨几个国家的前缀按覆盖比例列前三个，同一国家的合并。
func TestLookupPrefix(t *testing.T) {
	calls := fakeRIPEstat(t, map[string]answer{
		"prefix-overview 8.0.0.0/8":  ok(overview8888),
		"maxmind-geo-lite 8.0.0.0/8": ok(geoSlash8),
		"rir 8.0.0.0/8":              ok(rirTwo),
		"asn-neighbours AS15169":     ok(`{"status":"ok","data":{"neighbours":[]}}`),
	})
	r, err := lookup(context.Background(), quiet, mustTarget(t, "8.0.0.0/8"))
	if err != nil {
		t.Fatal(err)
	}
	text := render(r)
	checkHTML(t, text)
	contains(t, text, "相关前缀：<code>8.0.0.0/9</code>、<code>8.0.0.0/12</code>\n",
		"地区：🇺🇸 US 66% · 🇨🇳 CN 15% · 🇸🇬 SG 11%\nRIR：APNIC、ARIN\n", "<b>AS15169 的上游</b>\n无")
	for _, call := range calls() {
		if strings.HasPrefix(call, "reverse-dns-ip") || strings.HasPrefix(call, "as-names") {
			t.Errorf("should not call %s", call)
		}
	}
}

// 整段没有宣告、里面的小段有宣告的大前缀，不说成「未在公网宣告」。
func TestRenderPartlyAnnounced(t *testing.T) {
	r := report{target: mustTarget(t, "8.0.0.0/8"), prefix: &prefixOverview{Resource: "8.0.0.0/8", Related: []string{"8.0.0.0/9", "8.0.0.0/12", "8.16.0.0/12", "8.8.8.0/24"}, RelatedCount: 2220}}
	text := render(r)
	checkHTML(t, text)
	contains(t, text, "前缀：没有整段宣告\n相关前缀：<code>8.0.0.0/9</code>、<code>8.0.0.0/12</code>、<code>8.16.0.0/12</code> 等 2220 个\n",
		`<a href="https://bgp.he.net/net/8.0.0.0/8">8.0.0.0/8</a>`)
}

// 没有宣告的地址：不查上游，链接指向 IP 本身。
func TestLookupUnannounced(t *testing.T) {
	calls := fakeRIPEstat(t, map[string]answer{
		"prefix-overview 25.0.0.1":  ok(overviewUnannounced),
		"maxmind-geo-lite 25.0.0.1": ok(geoGB),
		"reverse-dns-ip 25.0.0.1":   ok(ptrNone),
		"rir 25.0.0.1":              ok(rirRIPE),
	})
	r, err := lookup(context.Background(), quiet, mustTarget(t, "25.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	text := render(r)
	checkHTML(t, text)
	contains(t, text, "前缀：未在公网宣告", "地区：🇬🇧 GB · London", "RIR：RIPE NCC", "反向解析：无",
		`<a href="https://bgp.he.net/ip/25.0.0.1">25.0.0.1</a>`)
	if strings.Contains(text, "上游") || strings.Contains(text, "起源") || strings.Contains(text, "NXDOMAIN") {
		t.Errorf("unexpected content:\n%s", text)
	}
	if len(calls()) != 4 {
		t.Errorf("calls = %q", calls())
	}
}

func TestLookupASN(t *testing.T) {
	fakeRIPEstat(t, map[string]answer{
		"as-overview AS13335":                        ok(as13335),
		"ris-prefixes AS13335":                       ok(counts13335),
		"asn-neighbours AS13335":                     ok(neighbours13335),
		"as-names AS1299,AS174,AS3356,AS3257,AS9002": ok(names13335Upstreams),
		"rir AS13335":                                ok(rirARIN),
	})
	r, err := lookup(context.Background(), quiet, mustTarget(t, "13335"))
	if err != nil {
		t.Fatal(err)
	}
	text := render(r)
	checkHTML(t, text)
	if !strings.HasPrefix(text, "🛰 <b>BGP 路由信息</b>\n\n查询：<code>AS13335</code>\n名称：CLOUDFLARENET - Cloudflare, Inc.\nRIR：ARIN\n宣告前缀：IPv4 2448 个 · IPv6 3065 个\n\n<b>AS13335 的上游</b>") {
		t.Errorf("head:\n%s", text)
	}
	contains(t, text, "• <code>AS1299</code> TWELVE99", `<a href="https://bgp.he.net/AS13335">AS13335</a>`)
	if strings.Contains(text, "地区") || strings.Contains(text, "反向解析") {
		t.Errorf("an AS lookup should not show places or PTR:\n%s", text)
	}
}

// 一项失败时照样显示其余各项，最后一行说哪几项没查到；上游的名字查不到只列号码，不算失败。
func TestLookupPartialFailure(t *testing.T) {
	fakeRIPEstat(t, map[string]answer{
		"prefix-overview 1.1.1.1":                    ok(overview1111),
		"maxmind-geo-lite 1.1.1.1":                   {http.StatusInternalServerError, "oops"},
		"reverse-dns-ip 1.1.1.1":                     ok(badResource),
		"asn-neighbours AS13335":                     ok(neighbours13335),
		"as-names AS1299,AS174,AS3356,AS3257,AS9002": {http.StatusTooManyRequests, ""},
	})
	r, err := lookup(context.Background(), quiet, mustTarget(t, "1.1.1.1"))
	if err != nil {
		t.Fatal(err)
	}
	text := render(r)
	checkHTML(t, text)
	contains(t, text, "前缀：<code>1.1.1.0/24</code>", "• <code>AS1299</code>\n• <code>AS174</code>\n", "\n\n⚠️ 部分数据没查到（地区、RIR、反向解析），稍后再试")
	if strings.Contains(text, "地区：") || strings.Contains(text, "RIR：") || strings.Contains(text, "反向解析：") || strings.Contains(text, "TWELVE99") {
		t.Errorf("failed parts should be left out:\n%s", text)
	}

	// 路由查不到时上游也无从查起，但地区和反向解析照样显示。
	fakeRIPEstat(t, map[string]answer{
		"prefix-overview 8.8.8.8":  {http.StatusBadGateway, "<html>"},
		"maxmind-geo-lite 8.8.8.8": ok(geo8888),
		"reverse-dns-ip 8.8.8.8":   ok(ptr8888),
		"rir 8.8.8.8":              ok(rirARIN),
	})
	r, err = lookup(context.Background(), quiet, mustTarget(t, "8.8.8.8"))
	if err != nil {
		t.Fatal(err)
	}
	text = render(r)
	checkHTML(t, text)
	contains(t, text, "地区：🇺🇸 US", "RIR：ARIN", "反向解析：<code>dns.google</code>", "⚠️ 部分数据没查到（路由），稍后再试", `https://bgp.he.net/ip/8.8.8.8`)
}

// 全部失败时返回错误；给用户看的那句话里没有地址和英文原话。
func TestLookupAllFailed(t *testing.T) {
	fakeRIPEstat(t, map[string]answer{
		"as-overview AS13335":    {http.StatusTooManyRequests, ""},
		"ris-prefixes AS13335":   {http.StatusTooManyRequests, ""},
		"asn-neighbours AS13335": {http.StatusTooManyRequests, ""},
		"rir AS13335":            {http.StatusTooManyRequests, ""},
	})
	_, err := lookup(context.Background(), quiet, mustTarget(t, "AS13335"))
	if err == nil {
		t.Fatal("every call failed but lookup succeeded")
	}
	text, _ := kit.IsUserError(kit.FailWith("BGP 数据查询失败", err))
	if text != "BGP 数据查询失败（请求过于频繁，稍后再试）" {
		t.Errorf("failure text = %q", text)
	}

	// 连不上：说法里不能有主机名和端口。
	apiBase = "http://127.0.0.1:1"
	_, err = lookup(context.Background(), quiet, mustTarget(t, "8.8.8.8"))
	if err == nil {
		t.Fatal("unreachable service but lookup succeeded")
	}
	text, _ = kit.IsUserError(kit.FailWith("BGP 数据查询失败", err))
	if !strings.HasPrefix(text, "BGP 数据查询失败") || strings.Contains(text, "127.0.0.1") || strings.Contains(text, "http") {
		t.Errorf("failure text = %q", text)
	}
}

// 未分配的 AS 号：名称写「—」，没有 RIR 这一行，前缀数和上游都是「无」，不算失败。
func TestLookupUnallocatedASN(t *testing.T) {
	fakeRIPEstat(t, map[string]answer{
		"as-overview AS99999999":    ok(`{"status":"ok","data":{"type":"as","resource":"99999999","block":{"desc":"Unallocated"},"holder":"","announced":false}}`),
		"ris-prefixes AS99999999":   ok(`{"status":"ok","data":{"counts":{"v4":{"originating":0},"v6":{"originating":0}}}}`),
		"rir AS99999999":            ok(rirNone),
		"asn-neighbours AS99999999": ok(`{"status":"ok","data":{"neighbour_counts":{"left":0,"right":0},"neighbours":[]}}`),
	})
	r, err := lookup(context.Background(), quiet, mustTarget(t, "AS99999999"))
	if err != nil {
		t.Fatal(err)
	}
	text := render(r)
	checkHTML(t, text)
	contains(t, text, "查询：<code>AS99999999</code>\n名称：—\n宣告前缀：无\n\n<b>AS99999999 的上游</b>\n无\n")
	if strings.Contains(text, "RIR") || strings.Contains(text, "⚠️") {
		t.Errorf("unexpected content:\n%s", text)
	}
}

func TestRenderPTR(t *testing.T) {
	for _, c := range []struct {
		ptr  reverseDNS
		want string
	}{
		{reverseDNS{Result: []string{"dns.google"}}, "PTR：<code>dns.google</code>"},
		{reverseDNS{Result: []string{"a.example", "b.example"}}, "PTR：<code>a.example</code>\nPTR：<code>b.example</code>"},
		{reverseDNS{Error: "Domain name doesn't exist (NXDOMAIN): 1.0.0.25.in-addr.arpa. (PTR)"}, "PTR：无"},
		{reverseDNS{}, "PTR：无"},
		{reverseDNS{Error: "The DNS operation timed out."}, "PTR：无法解析"},
		{reverseDNS{Error: "All nameservers failed to answer the query 4.3.2.1.in-addr.arpa. IN PTR: Server 1.1.1.1 UDP port 53 answered SERVFAIL"}, "PTR：无法解析"},
	} {
		text := renderPTR("8.8.8.8", &c.ptr)
		checkHTML(t, text)
		if want := "🛰 <b>反向解析</b>\n\n地址：<code>8.8.8.8</code>\n" + c.want; text != want {
			t.Errorf("renderPTR(%+v) =\n%s\nwant\n%s", c.ptr, text, want)
		}
	}
}

// .bgp dns 用的 fetchPTR：RIPEstat 的 400 应答是错误，不是「没有记录」。
func TestFetchPTR(t *testing.T) {
	fakeRIPEstat(t, map[string]answer{
		"reverse-dns-ip 8.8.8.8": ok(ptr8888),
		"reverse-dns-ip 9.9.9.9": {http.StatusBadRequest, badResource},
	})
	ptr, err := fetchPTR(context.Background(), "8.8.8.8")
	if err != nil || len(ptr.Result) != 1 || ptr.Result[0] != "dns.google" {
		t.Errorf("fetchPTR = %+v, %v", ptr, err)
	}
	if _, err := fetchPTR(context.Background(), "9.9.9.9"); err == nil {
		t.Error("a 400 answer should be an error")
	}
}

// 这些字段 TestCommandTextStyle 也会查，但要等命令注册进总表；这里先在包里查一遍。
func TestRegister(t *testing.T) {
	a := &app.App{Root: t.TempDir(), Registry: command.New([]string{"."}, quiet)}
	Register(a)
	cmd, found := a.Registry.Lookup("bgp")
	if !found {
		t.Fatal(".bgp was not registered")
	}
	if cmd.Group != command.GroupTools || cmd.Timeout <= lookupBudget {
		t.Errorf("group %q, timeout %v", cmd.Group, cmd.Timeout)
	}
	if n := len([]rune(cmd.Description)); n < 4 || n > 16 {
		t.Errorf("description %q has %d runes", cmd.Description, n)
	}
	helpText := cmd.Help(".")
	if !strings.HasPrefix(helpText, "🛰 <b>BGP 路由信息</b>") || strings.Contains(helpText, "...") {
		t.Errorf("help = %q", helpText)
	}
	checkHTML(t, helpText)
}
