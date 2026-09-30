package bgp

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// kind 是要查的东西的种类。
type kind int

const (
	kindIP kind = iota
	kindPrefix
	kindASN
)

// target 是认出来的一次查询：一个 IP、一个前缀或一个 AS 号。
type target struct {
	kind   kind
	addr   netip.Addr   // kindIP
	prefix netip.Prefix // kindPrefix，已经把主机位清零
	asn    uint32       // kindASN
}

// String 是给用户看的写法，也是交给 RIPEstat 的 resource：它认 1.1.1.1、1.1.1.0/24、AS13335。
func (t target) String() string {
	switch t.kind {
	case kindPrefix:
		return t.prefix.String()
	case kindASN:
		return asLabel(t.asn)
	}
	return t.addr.String()
}

func asLabel(asn uint32) string { return "AS" + strconv.FormatUint(uint64(asn), 10) }

// parseTarget 认出命令参数里要查的东西。整段就是一个 IP、前缀或 AS 号（13335 和 AS13335 都行）
// 时直接用；否则像被回复的消息一样从文字里找，这样贴一段带地址的链接或日志也能查。
func parseTarget(text string) (target, bool) {
	value := strings.TrimSpace(text)
	if asn, ok := parseASN(value); ok {
		return target{kind: kindASN, asn: asn}, true
	}
	if t, ok := parseAddress(value); ok {
		return t, true
	}
	return findTarget(value)
}

// parseASN 认 AS13335、as13335、13335。纯数字只在命令参数里当 AS 号，在消息正文里
// 一串数字多半是别的东西，那里要求带 AS。
func parseASN(value string) (uint32, bool) {
	if len(value) > 2 && strings.EqualFold(value[:2], "AS") {
		value = value[2:]
	}
	if value == "" || len(value) > 10 || strings.Trim(value, "0123456789") != "" {
		return 0, false
	}
	number, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(number), true
}

// parseAddress 认一个 IP 或前缀。/32、/128 的前缀就是单个地址，按 IP 查（能多查一项反向解析）；
// 前缀写成 1.1.1.1/24 这样带主机位的，清零后照查。带区域（fe80::1%eth0）的是本机网卡上的地址，不算。
func parseAddress(value string) (target, bool) {
	if strings.Contains(value, "/") {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || prefix.Addr().Zone() != "" {
			return target{}, false
		}
		if prefix.Addr().Is4In6() {
			return target{}, false
		}
		prefix = prefix.Masked()
		if prefix.IsSingleIP() {
			return target{kind: kindIP, addr: prefix.Addr()}, true
		}
		return target{kind: kindPrefix, prefix: prefix}, true
	}
	addr, err := netip.ParseAddr(value)
	if err != nil || addr.Zone() != "" {
		return target{}, false
	}
	return target{kind: kindIP, addr: addr.Unmap()}, true
}

var (
	// addressInText 是文字里像 IPv4、IPv6 地址或前缀的片段。IPv6 那一支很宽，会匹配到
	// 12:30:45、std::vector 里的 d:: 这类东西，所以每个片段还要过 netip 的解析和前后的边界检查。
	addressInText = regexp.MustCompile(`(?i)(?:\d{1,3}\.){3}\d{1,3}(?:/\d{1,2})?|[0-9a-f]{0,4}(?::[0-9a-f]{0,4}){2,7}(?:/\d{1,3})?`)
	// asnInText 是正文里的 AS 号，必须带 AS（或 ASN）：光一串数字不当 AS 号。
	asnInText = regexp.MustCompile(`(?i)\bASN?(\d{1,10})\b`)
)

// findTarget 从一段文字（被回复的消息、贴进来的链接）里挑出要查的东西：先找 IP 和前缀，
// 取最先出现的那个；没有再找 AS 号。
func findTarget(text string) (target, bool) {
	for _, span := range addressInText.FindAllStringIndex(text, -1) {
		start, end := span[0], span[1]
		// 片段前后紧挨着字母数字，说明它是从一个更长的词中间切出来的（deadbeef:cafe::1、
		// Foo::Bar），不是地址。IPv4 后面可以跟冒号：那是链接里的端口。
		if start > 0 && strings.ContainsRune("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_.:", rune(text[start-1])) {
			continue
		}
		if end < len(text) && strings.ContainsRune("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_", rune(text[end])) {
			continue
		}
		// ::ffff:1.2.3.4 这种嵌着 IPv4 的写法，IPv6 那一支只匹配到 ::ffff:1，后面跟着「.数字」；
		// 句末的「2001:db8::1.」后面跟的不是数字，照常认。
		if strings.Contains(text[start:end], ":") && end+1 < len(text) && text[end] == '.' && text[end+1] >= '0' && text[end+1] <= '9' {
			continue
		}
		if t, ok := parseAddress(text[start:end]); ok {
			return t, true
		}
	}
	if match := asnInText.FindStringSubmatch(text); match != nil {
		if asn, ok := parseASN(match[1]); ok {
			return target{kind: kindASN, asn: asn}, true
		}
	}
	return target{}, false
}

// reservedV4 是不在公网路由的 IPv4 地址段（RFC 6890 里标为非全局的那些，加上组播）。
// netip 的 IsPrivate 之类只覆盖其中一部分，CGNAT、文档用、测试用的段都不算，所以这里列全。
var reservedV4 = prefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
)

// globalV6 是 IPv6 的全球单播段，公网路由只在这里面；内网（fc00::/7）、链路本地、组播、
// 回环都在外面。reservedV6 是它里面仍然不上公网的：文档用和测试用的段。
var (
	globalV6   = netip.MustParsePrefix("2000::/3")
	reservedV6 = prefixes("2001:db8::/32", "3fff::/20", "2001:2::/48")
)

func prefixes(values ...string) []netip.Prefix {
	list := make([]netip.Prefix, len(values))
	for index, value := range values {
		list[index] = netip.MustParsePrefix(value)
	}
	return list
}

// reserved 判断这次查询是不是注定查不到公网路由，是的话返回给用户看的说明。
// 这些地址在 RIPEstat 那里只会得到「没有宣告」，与其让用户以为是网络问题，不如直接说清楚。
func (t target) reserved() string {
	switch t.kind {
	case kindASN:
		if reservedASN(t.asn) {
			return t.String() + " 是私有或保留的 AS 号，公网上没有它的路由"
		}
		return ""
	case kindPrefix:
		// 比 /8、/16 还大的前缀在公网上不会整段宣告，查了也只是一大堆相关前缀。
		if (t.prefix.Addr().Is4() && t.prefix.Bits() < 8) || (t.prefix.Addr().Is6() && t.prefix.Bits() < 16) {
			return "前缀太大，IPv4 最短写到 /8，IPv6 最短写到 /16"
		}
		if reservedPrefix(t.prefix) {
			return t.String() + " 是内网或保留地址段，公网上没有它的路由"
		}
		return ""
	}
	if reservedPrefix(netip.PrefixFrom(t.addr, t.addr.BitLen())) {
		return t.String() + " 是内网或保留地址，公网上没有它的路由"
	}
	return ""
}

// reservedPrefix 判断 p 是否整段落在不上公网的地址段里。只看整段：192.0.0.0/8 虽然
// 以保留的 192.0.0.0/24 开头，其余部分都是公网地址，照查。
func reservedPrefix(p netip.Prefix) bool {
	within := func(outer netip.Prefix) bool { return outer.Bits() <= p.Bits() && outer.Contains(p.Addr()) }
	list := reservedV4
	if p.Addr().Is6() {
		if !within(globalV6) {
			return true
		}
		list = reservedV6
	}
	for _, outer := range list {
		if within(outer) {
			return true
		}
	}
	return false
}

// reservedASN 判断是不是不会出现在公网上的 AS 号：0、AS_TRANS（23456）、文档用、私有、保留的号段
// （RFC 7607、6793、5398、6996、7300）。
func reservedASN(asn uint32) bool {
	switch {
	case asn == 0, asn == 23456:
		return true
	case asn >= 64496 && asn <= 131071: // 文档用、私有、65535、文档用、IANA 保留
		return true
	case asn >= 4200000000: // 私有和 4294967295
		return true
	}
	return false
}
