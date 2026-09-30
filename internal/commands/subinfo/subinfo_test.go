package subinfo

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

func mustParse(t *testing.T, raw string) summary {
	t.Helper()
	result, err := parseSubscription(raw)
	if err != nil {
		t.Fatalf("解析失败：%v\n%s", err, raw)
	}
	return result
}

func TestParseClashYAML(t *testing.T) {
	raw := `port: 7890
proxies:
  - {name: "🇭🇰 香港 01", server: hk.example.com, port: 443, type: ss, cipher: aes-128-gcm, password: "secret"}
  - name: US-West
    type: VMess
    server: us.example.com
    port: 443
    uuid: 0000
    ws-opts: {path: /, headers: {Host: example.com}}
  - {type: trojan, server: x, port: 443, password: p, name: 123}
proxy-groups: []
`
	result := mustParse(t, raw)
	if !slices.Equal(result.names, []string{"🇭🇰 香港 01", "US-West", "TROJAN 3"}) {
		t.Errorf("节点名：%q", result.names)
	}
	if got := result.protocols.lines(); !slices.Equal(got, []string{"ss：1", "vmess：1", "trojan：1"}) {
		t.Errorf("协议：%q", got)
	}
	if got := result.regions.lines(); !slices.Equal(got, []string{"香港：1", "美国：1", "其他：1"}) {
		t.Errorf("地区：%q", got)
	}
}

func TestParseClashJSON(t *testing.T) {
	result := mustParse(t, `{"proxies":[{"name":"日本 Tokyo","type":"hysteria2","server":"a","port":1},{"name":"x","type":"tuic"}]}`)
	if len(result.names) != 2 || !slices.Equal(result.protocols.lines(), []string{"hysteria2：1", "tuic：1"}) {
		t.Errorf("%+v", result)
	}
	for raw, want := range map[string]string{
		"proxies: 3\n":                           "无效的 Clash 节点列表",
		"proxies:\n  - {name: a}\n":              "无效的 Clash 节点",
		"proxies:\n  - {name: a, type: 'x y'}\n": "无效的 Clash 协议",
		"{ not json":                             "订阅不是有效的 Clash 配置",
		"   ":                                    "订阅内容为空",
	} {
		_, err := parseSubscription(raw)
		if text, ok := kit.IsUserError(err); !ok || text != want {
			t.Errorf("%q：%v，应为 %q", raw, err, want)
		}
	}
}

// Base64 编码的节点链接：名字取 #、VMess 的 ps、SSR 的 remarks，都没有写「协议 序号」。
func TestParseLinks(t *testing.T) {
	vmess := "vmess://" + base64.StdEncoding.EncodeToString([]byte(`{"v":"2","ps":"新加坡 SG 01","add":"a"}`))
	ssrBody := "host:443:origin:aes-256-cfb:plain:cGFzcw/?remarks=" + base64.RawURLEncoding.EncodeToString([]byte("韩国 Seoul"))
	ssr := "ssr://" + base64.RawURLEncoding.EncodeToString([]byte(ssrBody))
	list := strings.Join([]string{
		vmess,
		"vless://uuid@host:443?security=tls#%E5%8F%B0%E6%B9%BE%20TW",
		"trojan://pass@host:443#UK%20London",
		"ss://YWVzOnBhc3M@host:8388#bad%zz",
		ssr,
		"hysteria2://auth@host:443",
		"tuic://uuid:pass@host:443#Thkx",
		"wireguard://key@host:51820#Germany",
		"not a node",
	}, "\r\n")
	for name, raw := range map[string]string{"明文": list, "Base64": base64.StdEncoding.EncodeToString([]byte(list)), "URL 安全 Base64": base64.RawURLEncoding.EncodeToString([]byte(list + "\n"))} {
		result := mustParse(t, raw)
		want := []string{"新加坡 SG 01", "台湾 TW", "UK London", "bad%zz", "韩国 Seoul", "HYSTERIA2 6", "Thkx", "Germany"}
		if !slices.Equal(result.names, want) {
			t.Errorf("%s 节点名：%q", name, result.names)
		}
		if got := result.protocols.lines(); !slices.Equal(got, []string{"vmess：1", "vless：1", "trojan：1", "ss：1", "ssr：1", "hysteria2：1", "tuic：1", "wireguard：1"}) {
			t.Errorf("%s 协议：%q", name, got)
		}
		// Thkx 里的 hk 不是独立的词，不算香港。
		if got := result.regions.lines(); !slices.Equal(got, []string{"新加坡：1", "台湾：1", "英国：1", "韩国：1", "德国：1", "其他：3"}) {
			t.Errorf("%s 地区：%q", name, got)
		}
	}
}

func TestTrafficSummary(t *testing.T) {
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	header := "upload=1073741824; download=2147483648; total=10737418240; expire=1798761600"
	want := "上传：1.00 GB\n下载：2.00 GB\n已用：3.00 GB\n总量：10.00 GB\n剩余：7.00 GB\n到期：2027-01-01 00:00 UTC"
	if got := trafficSummary(header, true, now); got != want {
		t.Errorf("正常：\n%s", got)
	}
	if got := trafficSummary("", false, now); got != "流量统计：未提供" {
		t.Errorf("没有响应头：%s", got)
	}
	got := trafficSummary("upload=5; download=10; total=10; expire=1000", true, now)
	if !strings.Contains(got, "剩余：0 B\n流量状态：已耗尽") || !strings.Contains(got, "到期：1970-01-01 00:16 UTC（已过期）") {
		t.Errorf("耗尽且过期：\n%s", got)
	}
	got = trafficSummary("UPLOAD = 1;total=0;expire=0;junk=5", true, now)
	if got != "上传：1 B\n下载：未知\n总量：未设限\n到期：未设期限" {
		t.Errorf("部分字段：\n%s", got)
	}
	if got := trafficSummary("expire=99999999999999", true, now); !strings.HasSuffix(got, "到期：无效时间") {
		t.Errorf("无效到期：%s", got)
	}
}

func TestHelpers(t *testing.T) {
	for value, want := range map[string]string{
		`attachment; filename*=UTF-8''%E6%9C%BA%E5%9C%BA`: "机场",
		`attachment; filename="My Airport"`:               "My Airport",
		`attachment; filename=bad%zz`:                     "",
		`inline`:                                          "",
	} {
		if got := headerName(value); got != want {
			t.Errorf("headerName(%q) = %q", value, got)
		}
	}
	for page, want := range map[string]string{
		"<html><TITLE lang=zh>登录 — 某某云</TITLE>":    "某某云",
		"<title>\n  Nice &amp; Fast | 登录 </title>": "Nice & Fast",
		"<p>no title</p>": "",
	} {
		if got := titleOf(page); got != want {
			t.Errorf("titleOf(%q) = %q", page, got)
		}
	}
	for raw, want := range map[string]string{
		"https://sub.example.com/api/v1/client/subscribe?token=abcdef123456": "https://sub.example.com/…3456",
		"https://sub.example.com/s": "https://sub.example.com/…",
		"not a link":                "***",
	} {
		if got := maskLink(raw); got != want {
			t.Errorf("maskLink(%q) = %q", raw, got)
		}
	}
	// 紧跟的中文标点不算进链接；协议不分大小写；重复的只留一个。
	got := links(`看这个 https://a.example/s?t=1，还有"http://b.example/x" 和 https://a.example/s?t=1 HTTPS://C.example/Sub。`)
	if !slices.Equal(got, []string{"https://a.example/s?t=1", "http://b.example/x", "HTTPS://C.example/Sub"}) {
		t.Errorf("links：%q", got)
	}
	names := []mapping{{"a.example", "甲"}, {"example", "乙"}}
	if mappedName("https://a.example/s", names) != "甲" || mappedName("https://c.test/s", names) != "" {
		t.Error("mappedName")
	}
}

func TestPublicAddress(t *testing.T) {
	for text, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false, "172.16.0.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "::1": false, "fd00::1": false, "fe80::1": false, "::ffff:127.0.0.1": false,
		"64:ff9b::a00:1": false, "224.0.0.1": false, "255.255.255.255": false,
	} {
		if got := publicAddress(netip.MustParseAddr(text)); got != want {
			t.Errorf("publicAddress(%s) = %v", text, got)
		}
	}
}

// 真正的防护：httptest 在 127.0.0.1 上，带防护的取不到；跳转到别的域名也不行。
func TestFetchGuard(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jump" {
			http.Redirect(w, r, "http://localhost:1/elsewhere", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	guarded := &fetcher{transport: guardedTransport(true)}
	link, _ := url.Parse(server.URL + "/s")
	if _, err := guarded.fetchSubscription(context.Background(), link); !errors.Is(err, errPrivateAddress) {
		t.Errorf("本机地址应被拒绝：%v", err)
	}
	if failureText(errPrivateAddress) != "只能读取公网地址的订阅" {
		t.Error("失败说明不对")
	}
	open := &fetcher{transport: guardedTransport(false)}
	jump, _ := url.Parse(server.URL + "/jump")
	_, err := open.fetchSubscription(context.Background(), jump)
	if !errors.Is(err, errRedirect) || strings.Contains(failureText(err), "localhost") {
		t.Errorf("跳到别的域名应被拒绝：%v", err)
	}
}

// fakeTelegram 接编辑、上传、发文件和删除，记下显示的文字和上传的内容。
type fakeTelegram struct {
	mu       sync.Mutex
	edits    []string
	uploaded []byte
	captions []string
	deleted  []int
}

func respond(output bin.Decoder, value bin.Encoder) error {
	var buffer bin.Buffer
	if err := value.Encode(&buffer); err != nil {
		return err
	}
	return output.Decode(&buffer)
}

func (f *fakeTelegram) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch request := input.(type) {
	case *tg.MessagesEditMessageRequest:
		f.edits = append(f.edits, request.Message)
		return respond(output, &tg.Updates{})
	case *tg.MessagesSendMessageRequest:
		f.edits = append(f.edits, request.Message)
		return respond(output, &tg.UpdateShortSentMessage{ID: 100})
	case *tg.UploadSaveFilePartRequest:
		f.uploaded = append(f.uploaded, request.Bytes...)
		return respond(output, &tg.BoolTrue{})
	case *tg.MessagesSendMediaRequest:
		f.captions = append(f.captions, request.Message)
		return respond(output, &tg.Updates{})
	case *tg.MessagesDeleteMessagesRequest:
		f.deleted = append(f.deleted, request.ID...)
		return respond(output, &tg.MessagesAffectedMessages{Pts: 1, PtsCount: 1})
	}
	return tgerr.New(400, "UNEXPECTED_REQUEST")
}

func (f *fakeTelegram) all() string { return strings.Join(f.edits, "\n---\n") }

func run(t *testing.T, s *service, args ...string) (*fakeTelegram, error) {
	t.Helper()
	fake := &fakeTelegram{}
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	text := ".subinfo " + strings.Join(args, " ")
	inv := &command.Invocation{Prefix: ".", Command: "subinfo", Args: args, Text: text, Client: client,
		Message: &bot.Message{ID: 7, Peer: &tg.PeerUser{UserID: 1}, ChatID: "1", Text: text, Out: true, Saved: true},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil))}
	return fake, s.handle(context.Background(), inv, args[0] == "cha" || (len(args) > 1 && args[1] == "cha"))
}

// 整个流程：取对照表、官网标题和订阅，结果里只出现打过码的订阅链接。
func TestHandle(t *testing.T) {
	const token = "tok3n-secret-9f8e"
	nodes := base64.StdEncoding.EncodeToString([]byte("trojan://p@h:443#香港 01\nss://x@h:1#Japan 02\n"))
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ymys.txt":
			_, _ = w.Write([]byte("# 对照表\nnothing.example=别的机场\n"))
		case "/auth/login":
			_, _ = w.Write([]byte("<title>登录 - 喵云</title>"))
		case "/sub":
			if r.Header.Get("User-Agent") != "Mi Box" || r.URL.Query().Get("token") != token {
				w.WriteHeader(403)
				return
			}
			w.Header().Set("Subscription-Userinfo", "upload=0; download=1048576; total=0")
			_, _ = w.Write([]byte(nodes))
		case "/empty":
			_, _ = w.Write([]byte(""))
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	s := &service{fetch: &fetcher{transport: guardedTransport(false), mappingsURL: server.URL + "/ymys.txt"},
		now: func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }}
	link := server.URL + "/sub?token=" + token

	fake, err := run(t, s, link)
	if err != nil {
		t.Fatal(err)
	}
	output := fake.all()
	for _, want := range []string{"订阅信息", "机场名称：喵云", "节点总数：2", "下载：1.0 MB", "总量：未设限", "trojan：1\nss：1", "香港：1 · 日本：1", "1. 香港 01\n2. Japan 02"} {
		if !strings.Contains(output, want) {
			t.Errorf("缺少 %q：\n%s", want, output)
		}
	}
	if strings.Contains(output, token) || strings.Contains(output, "订阅链接") {
		t.Errorf("单个完整查询不该带订阅链接：\n%s", output)
	}

	// 简要版、批量：带打码链接；失败的那个单独写原因，不影响另一个。
	fake, err = run(t, s, link, server.URL+"/missing?token=zzzz")
	if err != nil {
		t.Fatal(err)
	}
	output = fake.all()
	if strings.Contains(output, token) || !strings.Contains(output, "/…9f8e") || !strings.Contains(output, "❌ 订阅读取失败：服务返回 HTTP 404") {
		t.Errorf("批量：\n%s", output)
	}

	// 单个失败直接报错，不带地址。
	_, err = run(t, s, server.URL+"/empty")
	if text, ok := kit.IsUserError(err); !ok || text != "订阅解析失败：订阅内容为空" {
		t.Errorf("空订阅：%v", err)
	}

	// TXT：发文件、删命令，文件里也只有打码的链接。
	fake, err = run(t, s, "txt", link)
	if err != nil {
		t.Fatal(err)
	}
	report := string(fake.uploaded)
	if len(fake.captions) != 1 || fake.captions[0] != "✅ 已生成订阅报告（共 1 个链接）" || !slices.Equal(fake.deleted, []int{7}) {
		t.Errorf("TXT 发送：%q %v", fake.captions, fake.deleted)
	}
	if strings.Contains(report, token) || strings.Contains(report, "<b>") || !strings.Contains(report, "机场名称：喵云") {
		t.Errorf("TXT 内容：\n%s", report)
	}

	// 没有链接：显示帮助。
	fake, err = run(t, s, "txt")
	if err != nil || !strings.HasPrefix(fake.all(), "📈 订阅信息查询") {
		t.Errorf("没有链接：%v\n%s", err, fake.all())
	}
}

// 超过上限的链接数直接拒绝，一个请求也不发。
func TestTooManyLinks(t *testing.T) {
	s := &service{fetch: &fetcher{transport: http.DefaultTransport}, now: time.Now}
	args := make([]string, maxLinks+1)
	for index := range args {
		args[index] = "https://example.invalid/" + strings.Repeat("x", index+1)
	}
	_, err := run(t, s, args...)
	if text, ok := kit.IsUserError(err); !ok || !strings.Contains(text, "一次最多查询 20 个订阅") {
		t.Errorf("%v", err)
	}
}

// 上传加下载超出 uint64 时按最大值算，照样判为耗尽。
func TestTrafficOverflow(t *testing.T) {
	got := trafficSummary("upload=18446744073709551615; download=10; total=100", true, time.Now())
	if !strings.Contains(got, "已用：17179869184.00 GB") || !strings.Contains(got, "流量状态：已耗尽") {
		t.Errorf("溢出：\n%s", got)
	}
}

// 标题里的全角空格、不换行空格和 ASCII 空白一样处理，与 MiBox（JavaScript 的 \s）一致。
func TestTitleUnicodeSpaces(t *testing.T) {
	for page, want := range map[string]string{
		"<title>登录\u3000—\u3000某某云</title>":    "某某云",
		"<title>Fast\u00a0\u00a0Cloud</title>": "Fast Cloud",
	} {
		if got := titleOf(page); got != want {
			t.Errorf("titleOf(%q) = %q", page, got)
		}
	}
	if !clashStart.MatchString("proxies\u3000:\n") {
		t.Error("全角空格后的冒号也该认作 Clash")
	}
}

// 聊天里只列前 200 个节点和前 20 种协议，名字截到 64 个字；TXT 报告里是完整的。
func TestReportLimits(t *testing.T) {
	var content summary
	for index := range 300 {
		content.names = append(content.names, fmt.Sprintf("节点%d%s", index+1, strings.Repeat("长", 100)))
		content.protocols.add(fmt.Sprintf("p%d", index%25))
	}
	r := &report{name: "喵", website: "https://a.example", traffic: "流量统计：未提供", summary: content}
	short := renderReport(r, false, false, false)
	if !strings.Contains(short, "200. 节点200") || strings.Contains(short, "201. ") || !strings.Contains(short, "…共 300 个，完整列表用 txt 参数查看") {
		t.Error("节点没有截断")
	}
	if !strings.Contains(short, "…还有 5 种") || strings.Contains(short, "p20：") {
		t.Error("协议没有截断")
	}
	if strings.Contains(short, strings.Repeat("长", 70)) {
		t.Error("节点名没有截断")
	}
	full := renderReport(r, false, false, true)
	if !strings.Contains(full, "300. 节点300") || !strings.Contains(full, "p24：") {
		t.Error("完整版应列出全部")
	}
}

// 批量查询结果超过 5 页时改发 TXT；机场名截到 64 个字。
func TestLongResultBecomesFile(t *testing.T) {
	var list strings.Builder
	for index := range 300 {
		fmt.Fprintf(&list, "trojan://p@h:443#%s%d\n", strings.Repeat("n", 60), index)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/big" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Disposition", "attachment; filename="+strings.Repeat("A", 100))
		_, _ = w.Write([]byte(list.String()))
	}))
	defer server.Close()
	s := &service{fetch: &fetcher{transport: guardedTransport(false), mappingsURL: server.URL + "/none"}, now: time.Now}
	fake, err := run(t, s, server.URL+"/big?t=1", server.URL+"/big?t=2")
	if err != nil {
		t.Fatal(err)
	}
	report := string(fake.uploaded)
	if len(fake.captions) != 1 || !strings.Contains(fake.captions[0], "结果太长") || !strings.Contains(report, "300. ") {
		t.Errorf("应改发 TXT：%q", fake.captions)
	}
	if !strings.Contains(report, "机场名称："+strings.Repeat("A", 64)+"…") || strings.Contains(report, strings.Repeat("A", 65)) {
		t.Error("机场名没有截断")
	}
}
