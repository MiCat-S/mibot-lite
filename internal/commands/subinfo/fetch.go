package subinfo

import (
	"context"
	"errors"
	"html"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

// 订阅链接是别人发来的任意地址，由主机去取。为了不让它被用来探内网（SSRF），
// 所有请求都走 guardedTransport：连接前检查实际要连的 IP，只放行公网地址；
// 跳转只许留在同一个域名，最多 5 次，与 MiBox 的 denyPrivateAddresses 和 allowedHosts 一致。

var (
	// errPrivateAddress 表示目标解析到了内网、本机或保留地址。
	errPrivateAddress = errors.New("refusing to connect to a non-public address")
	// errRedirect 表示跳转到了别的域名，或者跳转太多次。
	errRedirect = errors.New("redirect not allowed")
)

// blockedPrefixes 是 netip 自带判断之外还要挡掉的保留网段：运营商 NAT、文档示例、
// 基准测试、未分配，以及 NAT64 这类能绕回 IPv4 内网的前缀。
var blockedPrefixes = func() []netip.Prefix {
	var list []netip.Prefix
	for _, text := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64",
		"2001:db8::/32", "2002::/16"} {
		list = append(list, netip.MustParsePrefix(text))
	}
	return list
}()

// publicAddress 判断一个 IP 是不是能放行的公网地址。
func publicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() {
		return false
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

// guardedTransport 建一个只连公网地址的 transport。检查放在 Dialer.Control 里，看的是
// DNS 解析之后真正要连的 IP，域名在两次解析之间换成内网地址（DNS rebinding）也挡得住。
// 不读代理环境变量：走代理时连的是代理，检查就落空了。guard 为假只给测试用。
func guardedTransport(guard bool) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if guard {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			parsed, err := netip.ParseAddr(host)
			if err != nil || !publicAddress(parsed) {
				return errPrivateAddress
			}
			return nil
		}
	}
	return &http.Transport{DialContext: dialer.DialContext, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second, ForceAttemptHTTP2: true}
}

// mappingsURL 是社区维护的「订阅域名=机场名」对照表，MiBox 用的同一份。
const mappingsURL = "https://raw.githubusercontent.com/Hyy800/Quantumult-X/refs/heads/Nana/ymys.txt"

// fetcher 取订阅、机场官网和名称对照表。
type fetcher struct {
	transport   http.RoundTripper
	mappingsURL string

	// 对照表取到后缓存一小时：每次查询都重新下载 256 KB 的表没有必要，
	// 表本身也很少更新。取失败不缓存，下次再试。
	mu       sync.Mutex
	cached   []mapping
	cachedAt time.Time
}

// mappingsTTL 是名称对照表的缓存时长。
const mappingsTTL = time.Hour

func newFetcher() *fetcher {
	return &fetcher{transport: guardedTransport(true), mappingsURL: mappingsURL}
}

type fetched struct {
	status int
	header http.Header
	body   string
}

func (f fetched) ok() bool { return f.status >= 200 && f.status < 300 }

// get 取一个地址，响应体超过 limit 字节算失败。跳转只许到 allowedHost。
func (f *fetcher) get(ctx context.Context, target string, headers map[string]string, limit int64, timeout time.Duration, allowedHost string) (fetched, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := &http.Client{Transport: f.transport, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		scheme := request.URL.Scheme
		if len(via) > 5 || (scheme != "https" && scheme != "http") || !strings.EqualFold(request.URL.Hostname(), allowedHost) {
			return errRedirect
		}
		return nil
	}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fetched{}, err
	}
	request.Header.Set("User-Agent", httpx.UserAgent)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := client.Do(request)
	if err != nil {
		return fetched{}, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return fetched{}, err
	}
	if int64(len(data)) > limit {
		return fetched{}, httpx.ErrTooLarge
	}
	return fetched{status: response.StatusCode, header: response.Header, body: string(data)}, nil
}

// subscription 是取回来的订阅：正文和几个响应头。
type subscription struct {
	text               string
	userinfo           string
	hasUserinfo        bool
	contentDisposition string
	profileURL         string
}

// fetchSubscription 取订阅，User-Agent 与 MiBox 一样写 Mi Box：有的机场按 UA 决定返回什么格式。
func (f *fetcher) fetchSubscription(ctx context.Context, link *url.URL) (*subscription, error) {
	response, err := f.get(ctx, link.String(), map[string]string{"User-Agent": "Mi Box"}, 2<<20, 15*time.Second, link.Hostname())
	if err != nil {
		return nil, err
	}
	if !response.ok() {
		return nil, &httpx.StatusError{Status: response.status}
	}
	userinfo := response.header.Values("Subscription-Userinfo")
	result := &subscription{text: response.body, hasUserinfo: len(userinfo) > 0,
		contentDisposition: response.header.Get("Content-Disposition"), profileURL: response.header.Get("Profile-Web-Page-Url")}
	if result.hasUserinfo {
		result.userinfo = userinfo[0]
	}
	return result, nil
}

// site 是订阅所在域名的官网信息：name 为空表示没认出名字。
type site struct {
	website string
	name    string
}

// Go 的 \s 只认 ASCII 空白，JavaScript 的 \s 还包括全角空格、不换行空格这些 Unicode 空白；
// 标题里常有它们，所以空白都写成 [\s\p{Z}\x{FEFF}]。
var (
	titlePattern = regexp.MustCompile(`(?is)<title(?:\s[^>]*)?>(.*?)</title>`)
	spaceRun     = regexp.MustCompile(`[\s\p{Z}\x{FEFF}]+`)
	loginPrefix  = regexp.MustCompile(`^登录[\s\p{Z}]*[—|-][\s\p{Z}]*`)
	loginSuffix  = regexp.MustCompile(`[\s\p{Z}]*[|][\s\p{Z}]*登录$`)
)

// titleOf 取网页标题，去掉「登录 - 」「| 登录」这类前后缀。
func titleOf(page string) string {
	match := titlePattern.FindStringSubmatch(page)
	if match == nil {
		return ""
	}
	title := strings.TrimSpace(spaceRun.ReplaceAllString(html.UnescapeString(match[1]), " "))
	title = loginSuffix.ReplaceAllString(loginPrefix.ReplaceAllString(title, ""), "")
	return title
}

// siteInfo 看订阅域名的官网：先试面板常见的 /auth/login，再试首页，取网页标题当机场名。
// Cloudflare 的验证页、「Access denied」这类页面单独说明。两个都打不开时名字写「连接失败」。
func (f *fetcher) siteInfo(ctx context.Context, link *url.URL) site {
	origin := link.Scheme + "://" + link.Host
	headers := map[string]string{"User-Agent": "Mozilla/5.0"}
	for _, candidate := range []string{origin + "/auth/login", origin + "/"} {
		response, err := f.get(ctx, candidate, headers, 512<<10, 5*time.Second, link.Hostname())
		if err != nil || !response.ok() {
			continue
		}
		title := titleOf(response.body)
		switch {
		case strings.Contains(title, "Cloudflare") || strings.Contains(title, "Just a moment"):
			title = "Cloudflare 防护"
		case strings.Contains(title, "Access denied") || strings.Contains(title, "404 Not Found"):
			title = "不是机场面板域名"
		}
		return site{website: origin, name: title}
	}
	return site{website: origin, name: "连接失败"}
}

// mapping 是「订阅地址片段=机场名」对照表的一项，保持文件里的顺序：先写的先匹配。
type mapping struct{ key, name string }

// mappings 取名称对照表，失败时返回空表：少了对照只是认不出名字。
func (f *fetcher) mappings(ctx context.Context) []mapping {
	f.mu.Lock()
	if f.cached != nil && time.Since(f.cachedAt) < mappingsTTL {
		defer f.mu.Unlock()
		return f.cached
	}
	f.mu.Unlock()
	list := f.fetchMappings(ctx)
	if len(list) > 0 {
		f.mu.Lock()
		f.cached, f.cachedAt = list, time.Now()
		f.mu.Unlock()
	}
	return list
}

func (f *fetcher) fetchMappings(ctx context.Context) []mapping {
	link, err := url.Parse(f.mappingsURL)
	if err != nil {
		return nil
	}
	response, err := f.get(ctx, f.mappingsURL, nil, 256<<10, 10*time.Second, link.Hostname())
	if err != nil || !response.ok() {
		return nil
	}
	var list []mapping
	for _, line := range strings.Split(response.body, "\n") {
		line = strings.TrimSpace(line)
		at := strings.Index(line, "=")
		if line == "" || strings.HasPrefix(line, "#") || at <= 0 {
			continue
		}
		list = append(list, mapping{key: strings.TrimSpace(line[:at]), name: strings.TrimSpace(line[at+1:])})
	}
	return list
}

// mappedName 在对照表里找订阅地址包含的第一个片段。
func mappedName(link string, list []mapping) string {
	for _, item := range list {
		if item.key != "" && strings.Contains(link, item.key) {
			return item.name
		}
	}
	return ""
}

// headerName 从 Content-Disposition 里取文件名当机场名：RFC 5987 的 filename* 或普通的 filename。
// 百分号编码解不开时当作没有，与 MiBox 一致。
func headerName(value string) string {
	for _, raw := range strings.Split(value, ";") {
		part := strings.TrimSpace(raw)
		lower := strings.ToLower(part)
		var name string
		switch {
		case strings.HasPrefix(lower, "filename*="):
			pieces := strings.Split(part, "''")
			name = pieces[len(pieces)-1]
		case strings.HasPrefix(lower, "filename="):
			name = strings.TrimSpace(part[strings.Index(part, "=")+1:])
			if name != "" && (name[0] == '"' || name[0] == '\'') {
				name = name[1:]
			}
			if name != "" && (name[len(name)-1] == '"' || name[len(name)-1] == '\'') {
				name = name[:len(name)-1]
			}
		default:
			continue
		}
		if name == "" {
			continue
		}
		decoded, err := url.PathUnescape(name)
		if err != nil {
			return ""
		}
		return decoded
	}
	return ""
}
