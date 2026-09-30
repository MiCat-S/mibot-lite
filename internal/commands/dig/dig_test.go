package dig

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// 合法的参数变成的 dig 命令行。dig 自己的等待时间总是压短，没写输出选项时用 +short。
func TestParseArgsBuildsDigCommandLine(t *testing.T) {
	tail := []string{"+time=3", "+tries=2"}
	for _, c := range []struct {
		in   []string
		want []string
	}{
		{[]string{"example.com"}, []string{"example.com", "A", "+short"}},
		{[]string{"example.com", "mx"}, []string{"example.com", "MX", "+short"}},
		{[]string{"example.com", "MX", "@1.1.1.1"}, []string{"@1.1.1.1", "example.com", "MX", "+short"}},
		{[]string{"@1.1.1.1", "example.com"}, []string{"@1.1.1.1", "example.com", "A", "+short"}},
		{[]string{"example.com", "MX", "dns.google"}, []string{"@dns.google", "example.com", "MX", "+short"}},
		{[]string{"example.com", "MX", "+noall", "+ANSWER", "+noall"}, []string{"example.com", "MX", "+noall", "+answer"}},
		{[]string{"_dmarc.example.com.", "TXT"}, []string{"_dmarc.example.com.", "TXT", "+short"}},
		{[]string{"_sip._tcp.example.com", "srv"}, []string{"_sip._tcp.example.com", "SRV", "+short"}},
		{[]string{"example.com", "AAAA", "@2606:4700:4700::1111"}, []string{"@2606:4700:4700::1111", "example.com", "AAAA", "+short"}},
		{[]string{"例子.中国"}, []string{"xn--fsqu00a.xn--fiqs8s", "A", "+short"}},
		{[]string{"8.8.8.8"}, []string{"-x", "8.8.8.8", "+short"}},
		{[]string{"2001:4860:4860::8888", "PTR", "@1.1.1.1"}, []string{"@1.1.1.1", "-x", "2001:4860:4860::8888", "+short"}},
	} {
		q, err := parseArgs(c.in)
		if err != nil {
			t.Errorf("parseArgs(%q): %v", c.in, err)
			continue
		}
		if got, want := buildArgs(q), append(c.want, tail...); !reflect.DeepEqual(got, want) {
			t.Errorf("parseArgs(%q) → %q, want %q", c.in, got, want)
		}
	}
}

// 不合法的参数在启动 dig 之前就被拒绝，错误是给用户看的中文。
func TestParseArgsRejects(t *testing.T) {
	for _, in := range [][]string{
		{"-x"}, {"-f/etc/passwd"}, {"--help"}, {"example.com;id"}, {"$(id).com"}, {"example"}, {"exa mple.com"},
		{"example.com", "ANY"}, {"example.com", "AXFR"}, {"example.com", "-t"},
		{"example.com", "+trace"}, {"example.com", "+time=1"}, {"example.com", "+short;id"},
		{"example.com", "A", "@1.1.1.1", "@8.8.8.8"}, {"example.com", "A", "1.1.1.1", "extra"}, {"example.com", "A", "@1.1.1.1", "x"},
		{"example.com", "A", "@"}, {"example.com", "A", "@-p5353"}, {"example.com", "A", "-p5353"}, {"example.com", "A", "@dns_bad.example"},
		{"8.8.8.8", "MX"}, {"fe80::1%eth0"},
		{"@1.1.1.1"}, {strings.Repeat("a", 64) + ".com"},
	} {
		q, err := parseArgs(in)
		if err == nil {
			t.Errorf("parseArgs(%q) = %+v, want an error", in, q)
			continue
		}
		if _, ok := kit.IsUserError(err); !ok {
			t.Errorf("parseArgs(%q): %v is not a user-facing error", in, err)
		}
	}
}

// 除了自己加的 -x，交给 dig 的参数没有一个以 - 开头，不会被当成选项。
func TestBuiltArgsNeverStartWithDash(t *testing.T) {
	for _, in := range [][]string{{"example.com", "A", "@1.1.1.1", "+tcp"}, {"8.8.8.8"}, {"a-b.example.com", "CAA", "ns1.example.com"}} {
		q, err := parseArgs(in)
		if err != nil {
			t.Fatal(err)
		}
		args := buildArgs(q)
		for index, arg := range args {
			if strings.HasPrefix(arg, "-") && !(arg == "-x" && index+1 < len(args)) {
				t.Errorf("%q: argument %q looks like an option", in, arg)
			}
		}
	}
}

// publicIP 既挡借用时指定的 DNS 服务器，也决定哪些地址会发给归属地服务，所以非公网的地址段要挡全。
func TestPublicIP(t *testing.T) {
	for value, want := range map[string]bool{
		// 公网
		"1.1.1.1": true, "8.8.8.8": true, "93.184.215.14": true, "100.63.255.255": true, "100.128.0.0": true,
		"198.17.255.255": true, "198.20.0.0": true, "223.255.255.255": true, "192.0.1.1": true,
		"2606:4700:4700::1111": true, "2001:4860:4860::8888": true, "2400:cb00::1": true, "64:ff9c::1": true,
		// netip 自带的判断
		"10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false, "127.0.0.1": false, "127.255.255.254": false,
		"169.254.169.254": false, "224.0.0.1": false, "239.255.255.250": false, "0.0.0.0": false,
		"::": false, "::1": false, "fe80::1": false, "fd00::1": false, "fc00::1": false, "ff02::1": false, "ff05::2": false,
		// 显式列出的地址段
		"0.1.2.3": false, "0.255.255.255": false,
		"100.64.0.1": false, "100.127.255.255": false,
		"192.0.0.1": false, "192.0.0.255": false, "192.0.2.1": false, "192.88.99.1": false,
		"198.18.0.1": false, "198.19.255.255": false, "198.51.100.7": false, "203.0.113.9": false,
		"240.0.0.1": false, "250.1.2.3": false, "255.255.255.255": false,
		"::10.0.0.1": false, "::ffff:10.0.0.1": false, "::ffff:127.0.0.1": false, "::ffff:198.18.0.1": false,
		"64:ff9b::a00:1": false, "64:ff9b::7f00:1": false, "64:ff9b::808:808": false, "64:ff9b:1::1": false,
		"100::1": false, "2001::1": false, "2001:0:4136:e378::1": false, "2001:db8::1": false, "2002:a00:1::1": false,
		"3fff::1": false, "5f00::1": false, "fec0::1": false,
		// 不是地址、带区域的
		"": false, "dns.google": false, "1.1.1": false, "fe80::1%eth0": false,
	} {
		if got := publicIP(value); got != want {
			t.Errorf("publicIP(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestBorrowedServerAllowed(t *testing.T) {
	for server, want := range map[string]bool{
		"": true, "1.1.1.1": true, "2606:4700:4700::1111": true,
		"127.0.0.53": false, "10.0.0.1": false, "192.168.1.1": false, "172.16.0.1": false, "100.64.0.1": false,
		"169.254.169.254": false, "::1": false, "fe80::1": false, "fd00::1": false, "0.0.0.0": false, "dns.google": false,
		"0.1.2.3": false, "198.18.0.1": false, "255.255.255.255": false, "240.0.0.1": false, "192.0.0.1": false,
		"64:ff9b::a00:1": false, "64:ff9b:1::1": false, "::ffff:10.0.0.1": false,
	} {
		if got := borrowedServerAllowed(server); got != want {
			t.Errorf("borrowedServerAllowed(%q) = %v, want %v", server, got, want)
		}
	}
}

const shortOutput = "93.184.215.14\n93.184.215.14\n10.0.0.8\nalias.example.net.\n2606:2800:21f:cb07:6820:80da:af6b:8b2c\n"

const answerOutput = "example.com.\t\t3600\tIN\tA\t93.184.215.14\n" +
	"example.com.\t\t3600\tIN\tMX\t10 mail.example.com.\n" +
	"www.example.com.\t300\tIN\tAAAA\t2606:2800:21f:cb07:6820:80da:af6b:8b2c\n"

// 要查归属地的只有公网 IP，去重、按出现顺序。+short 和完整输出两种格式都认。
func TestAddresses(t *testing.T) {
	want := []string{"93.184.215.14", "2606:2800:21f:cb07:6820:80da:af6b:8b2c"}
	// 非公网的解析结果不发给归属地服务。
	if got := addresses("0.0.0.0\n198.18.0.1\n240.0.0.1\n64:ff9b::a00:1\n100.64.0.1\n"); len(got) != 0 {
		t.Errorf("non-public answers would be looked up: %q", got)
	}
	if got := addresses(shortOutput); !reflect.DeepEqual(got, want) {
		t.Errorf("+short: %q", got)
	}
	if got := addresses(answerOutput); !reflect.DeepEqual(got, want) {
		t.Errorf("answer section: %q", got)
	}
	var many strings.Builder
	for i := 0; i < 80; i++ {
		many.WriteString("1.1.1." + strconv.Itoa(i) + "\n")
	}
	if got := addresses(many.String()); len(got) != maxLookups {
		t.Errorf("expected at most %d lookups, got %d", maxLookups, len(got))
	}
}

// 这是 ip-api 批量接口的应答格式：失败的（内网）只有 status 和 query。
const batchResponse = `[
 {"status":"success","country":"美国","regionName":"马萨诸塞州","city":"Boston","as":"AS15133 Edgecast Inc.","query":"93.184.215.14"},
 {"status":"success","country":"新加坡","regionName":"新加坡","city":"新加坡","as":"AS13335 Cloudflare, Inc.","query":"2606:2800:21f:cb07:6820:80da:af6b:8b2c"},
 {"status":"fail","message":"private range","query":"10.0.0.8"}
]`

func TestParseLocationsAndAnnotate(t *testing.T) {
	labels, err := parseLocations([]byte(batchResponse))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"93.184.215.14":                          "美国 · 马萨诸塞州 · Boston · AS15133 Edgecast Inc.",
		"2606:2800:21f:cb07:6820:80da:af6b:8b2c": "新加坡 · AS13335 Cloudflare, Inc.",
	}
	if !reflect.DeepEqual(labels, want) {
		t.Errorf("labels = %q", labels)
	}
	got := annotate(strings.TrimSpace(shortOutput), labels)
	wantText := "93.184.215.14\n  美国 · 马萨诸塞州 · Boston · AS15133 Edgecast Inc.\n93.184.215.14\n  美国 · 马萨诸塞州 · Boston · AS15133 Edgecast Inc.\n" +
		"10.0.0.8\nalias.example.net.\n2606:2800:21f:cb07:6820:80da:af6b:8b2c\n  新加坡 · AS13335 Cloudflare, Inc."
	if got != wantText {
		t.Errorf("annotate:\n%s", got)
	}
	if _, err := parseLocations([]byte("<html>")); err == nil {
		t.Error("a non-JSON answer should be an error")
	}
}

// lookup 用假的 dig 和假的归属地服务走一遍：批量接口只收到公网 IP，结果里有归属地。
func TestLookupWithFakeDigAndLocationService(t *testing.T) {
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || json.Unmarshal(body, &asked) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, batchResponse)
	}))
	defer server.Close()
	defer swap(&locateURL, server.URL)()
	var ran []string
	defer swap(&runner, func(_ context.Context, args []string) ([]byte, bool, error) {
		ran = args
		return []byte(shortOutput), false, nil
	})()

	q, err := parseArgs([]string{"example.com", "@1.1.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	text, err := lookup(context.Background(), nil, q)
	if err != nil {
		t.Fatal(err)
	}
	if ran[0] != "@1.1.1.1" || !reflect.DeepEqual(asked, []string{"93.184.215.14", "2606:2800:21f:cb07:6820:80da:af6b:8b2c"}) {
		t.Errorf("dig ran with %q, locations asked for %q", ran, asked)
	}
	for _, wanted := range []string{"🧭 <b>DNS 查询</b>", "<code>example.com</code> · <code>A</code> · <code>@1.1.1.1</code>", "<pre>", "Edgecast", "新加坡 · AS13335"} {
		if !strings.Contains(text, wanted) {
			t.Errorf("result lost %q:\n%s", wanted, text)
		}
	}
	if _, _, err := bot.ParseHTML(text); err != nil {
		t.Errorf("result does not parse: %v", err)
	}
}

// 归属地服务出错时照样给出 DNS 结果，只是不带归属地。
func TestLookupWithoutLocations(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	defer swap(&locateURL, server.URL)()
	defer swap(&runner, func(context.Context, []string) ([]byte, bool, error) { return []byte(answerOutput), false, nil })()
	q, _ := parseArgs([]string{"example.com", "+noall", "+answer"})
	text, err := lookup(context.Background(), nil, q)
	if err != nil || !strings.Contains(text, "mail.example.com.") || strings.Contains(text, "  美国") {
		t.Errorf("lookup = %v:\n%s", err, text)
	}
}

func TestLookupFailures(t *testing.T) {
	q, _ := parseArgs([]string{"example.com"})
	cases := []struct {
		err  error
		want string
	}{
		{errNoDig, "没有 dig"},
		{errTimeout, "超时"},
		{&digError{cause: errors.New("exit status 10"), stderr: "dig: couldn't get address for 'nope.invalid': not found"}, "解析不了"},
		{&digError{cause: errors.New("exit status 10"), stderr: "/usr/lib/x.so: something"}, "DNS 查询失败"},
	}
	if exit9 := exitError(t, 9); exit9 != nil {
		cases = append(cases, struct {
			err  error
			want string
		}{&digError{cause: exit9, stdout: ";; connection timed out; no servers could be reached"}, "没有应答"})
	}
	defer swap(&runner, runner)()
	for _, c := range cases {
		runner = func(context.Context, []string) ([]byte, bool, error) { return nil, false, c.err }
		_, err := lookup(context.Background(), nil, q)
		text, ok := kit.IsUserError(err)
		if !ok || !strings.Contains(text, c.want) || strings.Contains(text, "/usr") {
			t.Errorf("%v → %q, want it to mention %q", c.err, text, c.want)
		}
	}
}

// exitError 造一个退出码为 code 的 *exec.ExitError；没有 sh 的系统上返回 nil（那条用例跳过）。
func exitError(t *testing.T, code int) error {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		return nil
	}
	err := exec.Command("sh", "-c", "exit "+string(rune('0'+code))).Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("sh did not exit with %d: %v", code, err)
	}
	return exit
}

func TestRenderAndTruncation(t *testing.T) {
	q := query{name: "example.com", kind: "TXT"}
	if text := render(q, "", nil, false); !strings.HasSuffix(text, "没有记录") {
		t.Errorf("empty output:\n%s", text)
	}
	text := render(q, `"v=spf1 <include> & more"`, nil, true)
	if !strings.Contains(text, "&lt;include&gt; &amp; more") || !strings.Contains(text, "⚠️ 输出超过 32 KB") {
		t.Errorf("escaping or truncation note missing:\n%s", text)
	}

	buffer := &capped{limit: 10}
	for _, chunk := range []string{"line one\n", "line two\n", "three\n"} {
		if n, err := buffer.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("capped.Write = %d, %v", n, err)
		}
	}
	if !buffer.truncated || buffer.buf.String() != "line one\nl" {
		t.Errorf("capped kept %q (truncated %v)", buffer.buf.String(), buffer.truncated)
	}
	if got := trimOutput(buffer.buf.Bytes(), true); got != "line one" {
		t.Errorf("trimOutput dropped the wrong part: %q", got)
	}
	// 截断的半个汉字不能留下。
	if got := trimOutput([]byte("好\n\xe5\xa5"), true); got != "好" {
		t.Errorf("trimOutput(half rune) = %q", got)
	}
	// 很长的输出分页后每页都能解析。
	long := strings.Repeat("93.184.215.14\n", 2000)
	for index, page := range command.HTMLPages(render(q, strings.TrimSpace(long), nil, false), command.PageLimit) {
		if _, _, err := bot.ParseHTML(page); err != nil {
			t.Errorf("page %d: %v", index+1, err)
		}
	}
}

func swap[T any](target *T, value T) func() {
	old := *target
	*target = value
	return func() { *target = old }
}
