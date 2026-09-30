package dig

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

// batchAPI 是 ip-api.com 的批量接口：一个 POST 查一批 IP（最多 100 个），和 .ip 用的是同一家、
// 同样的中文字段。MiBox 是逐个 IP 先问 api.ip.sb 再问 ipinfo.io，一次 dig 结果有十几个 IP 时
// 要发十几到几十个请求；批量接口一个请求就够。免费版只有明文 HTTP，明文传出去的只有这些 IP。
const batchAPI = "http://ip-api.com/batch?fields=status,country,regionName,city,as,query&lang=zh-CN"

// maxLookups 是一次最多查归属地的 IP 数。批量接口上限 100；dig 的结果一般只有几个 IP，
// 多到几十个的（大型 CDN 的 A 记录）后面的不查也不影响读结果。
const maxLookups = 50

// answerLine 是 dig 标准输出里的一条 A/AAAA 记录：名字、TTL、IN、类型、地址。
var answerLine = regexp.MustCompile(`^\S+\s+\d+\s+IN\s+(?:A|AAAA)\s+(\S+)\s*$`)

// parseIP 认出一个 IP 地址，返回规范写法。带区域（fe80::1%eth0）的不算：
// 那是本机网卡上的地址，不是能查的东西。
func parseIP(value string) (string, bool) {
	addr, err := netip.ParseAddr(value)
	if err != nil || addr.Zone() != "" {
		return "", false
	}
	return addr.Unmap().String(), true
}

// publicIP 判断是不是公网地址。它有两个用处：借用的人指定的 DNS 服务器必须是公网地址
// （不能借本机去探内网），查归属地时也只把公网地址发出去。所以宁可多挡：netip 自带的判断
// 之外，再按 IANA 的特殊用途地址表挡掉不会出现在公网上、或者会被转到内网的几段。
func publicIP(value string) bool {
	addr, err := netip.ParseAddr(value)
	if err != nil || addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, prefix := range nonPublic {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// nonPublic 是 netip 的方法没有覆盖、但也不算公网的地址段（IANA 特殊用途地址表里
// 不能全球路由的那些），加上几段会把目标换成别的地址的转换前缀。
var nonPublic = func() []netip.Prefix {
	var prefixes []netip.Prefix
	for _, value := range []string{
		"0.0.0.0/8",       // 「本网络」，0.x.x.x 在很多系统上等于本机
		"100.64.0.0/10",   // 运营商级 NAT
		"192.0.0.0/24",    // IETF 协议分配
		"192.0.2.0/24",    // 文档示例 TEST-NET-1
		"192.88.99.0/24",  // 已废弃的 6to4 中继任播
		"198.18.0.0/15",   // 网络设备基准测试
		"198.51.100.0/24", // 文档示例 TEST-NET-2
		"203.0.113.0/24",  // 文档示例 TEST-NET-3
		"240.0.0.0/4",     // 保留，含广播地址 255.255.255.255
		"::/96",           // 已废弃的 IPv4 兼容地址（::10.0.0.1）
		"64:ff9b::/96",    // NAT64：后 32 位是 IPv4 地址，会被网关转到那个地址（可能是内网）
		"64:ff9b:1::/48",  // 本地 NAT64
		"100::/64",        // 丢弃
		"2001::/23",       // IETF 协议分配，含 Teredo（2001::/32）
		"2001:db8::/32",   // 文档示例
		"2002::/16",       // 6to4：地址里嵌着 IPv4
		"3fff::/20",       // 文档示例
		"5f00::/16",       // SRv6 段标识
		"fec0::/10",       // 已废弃的站点本地
	} {
		prefixes = append(prefixes, netip.MustParsePrefix(value))
	}
	return prefixes
}()

// lineAddress 取出一行 dig 输出里的 IP：+short 时整行就是 IP，完整输出时是 A/AAAA 记录的最后一格。
func lineAddress(line string) (string, bool) {
	value := strings.TrimSpace(line)
	if ip, ok := parseIP(value); ok {
		return ip, true
	}
	if match := answerLine.FindStringSubmatch(value); match != nil {
		return parseIP(match[1])
	}
	return "", false
}

// addresses 是 dig 输出里要查归属地的 IP，去重、保持出现顺序，只取公网地址：
// 内网地址查不到归属地，也没有理由发给外面的服务。
func addresses(output string) []string {
	var found []string
	seen := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		ip, ok := lineAddress(line)
		if !ok || seen[ip] || !publicIP(ip) {
			continue
		}
		seen[ip] = true
		found = append(found, ip)
		if len(found) == maxLookups {
			break
		}
	}
	return found
}

// location 是批量接口对一个 IP 的应答。
type location struct {
	Status     string `json:"status"`
	Country    string `json:"country"`
	RegionName string `json:"regionName"`
	City       string `json:"city"`
	AS         string `json:"as"`
	Query      string `json:"query"`
}

// label 是写在 IP 下面的一行：国家 · 地区 · 城市 · AS 号和名称，重复的地名只写一次
// （新加坡 · 新加坡）。查询失败（内网、保留地址）时为空。
func (l location) label() string {
	if l.Status != "success" {
		return ""
	}
	var parts []string
	for _, value := range []string{l.Country, l.RegionName, l.City} {
		value = strings.TrimSpace(value)
		if value != "" && !slices.Contains(parts, value) {
			parts = append(parts, value)
		}
	}
	if as := strings.TrimSpace(l.AS); as != "" {
		parts = append(parts, command.Truncate(as, 60))
	}
	return strings.Join(parts, " · ")
}

// locateURL 是批量接口的地址，测试里换成本地的假服务。
var locateURL = batchAPI

// locate 一次查完 ips 的归属地，返回 IP → 那一行说明。
func locate(ctx context.Context, ips []string) (map[string]string, error) {
	if len(ips) == 0 {
		return nil, nil
	}
	response, err := httpx.PostJSON(ctx, locateURL, nil, ips, 8*time.Second, 256<<10)
	if err != nil {
		return nil, err
	}
	if !response.OK() {
		return nil, &httpx.StatusError{Status: response.Status}
	}
	return parseLocations(response.Body)
}

func parseLocations(body []byte) (map[string]string, error) {
	var results []location
	if err := json.Unmarshal(body, &results); err != nil {
		return nil, fmt.Errorf("invalid location response: %w", err)
	}
	labels := map[string]string{}
	for _, result := range results {
		ip, ok := parseIP(result.Query)
		if !ok {
			continue
		}
		if label := result.label(); label != "" {
			labels[ip] = label
		}
	}
	return labels, nil
}

// annotate 在每个查到归属地的 IP 那一行下面加一行缩进的说明。
func annotate(output string, locations map[string]string) string {
	if len(locations) == 0 {
		return output
	}
	var b strings.Builder
	for index, line := range strings.Split(output, "\n") {
		if index > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		if ip, ok := lineAddress(line); ok {
			if label := locations[ip]; label != "" {
				b.WriteString("\n  " + label)
			}
		}
	}
	return b.String()
}
