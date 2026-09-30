package bgp

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

const (
	// holderWidth 是 AS 名称最多显示的字数。RIPEstat 的名称是「CLOUDFLARENET - Cloudflare, Inc.」
	// 这种 AS 名加机构名，个别很长。
	holderWidth = 60
	// maxListed 是相关前缀、地区、PTR 一行里最多列的个数。
	maxListed = 3
)

// render 把查询结果写成一条消息。
func render(r report) string {
	lines := []string{"🛰 <b>BGP 路由信息</b>", "", "查询：" + command.Code(r.target.String())}
	if r.target.kind == kindASN {
		lines = append(lines, asLines(r)...)
	} else {
		lines = append(lines, prefixLines(r)...)
	}
	if r.upstream != nil {
		lines = append(lines, "", upstreamHeading(*r.upstream))
		if len(r.upstream.top) == 0 {
			lines = append(lines, "无")
		}
		for _, p := range r.upstream.top {
			lines = append(lines, "• "+asWithName(p.asn, p.name))
		}
	}
	lines = append(lines, "", "在 bgp.he.net 查看："+strings.Join(links(r), " · "))
	if len(r.failed) > 0 {
		lines = append(lines, "", "⚠️ 部分数据没查到（"+strings.Join(r.failed, "、")+"），稍后再试")
	}
	return strings.Join(lines, "\n")
}

// prefixLines 是 IP 和前缀查询的几行：前缀、起源、相关前缀、地区、RIR、反向解析。
func prefixLines(r report) []string {
	var lines []string
	if p := r.prefix; p != nil {
		switch {
		case p.Announced:
			lines = append(lines, "前缀："+command.Code(p.Resource))
		case len(p.Related) > 0:
			// 8.0.0.0/8 这种：整段没有宣告，里面的小段各有各的宣告，说「未宣告」会让人以为整段都不通。
			lines = append(lines, "前缀：没有整段宣告")
		default:
			lines = append(lines, "前缀：未在公网宣告")
		}
		for _, o := range p.ASNs {
			lines = append(lines, "起源 AS："+asWithName(o.ASN, o.Holder))
		}
		if len(p.Related) > 0 {
			lines = append(lines, "相关前缀："+listCodes(p.Related, max(p.RelatedCount, len(p.Related))))
		}
	}
	if r.geo != nil {
		lines = append(lines, "地区："+places(*r.geo))
	}
	lines = append(lines, rirLine(r.rir)...)
	if r.ptr != nil {
		lines = append(lines, "反向解析："+ptrText(*r.ptr))
	}
	return lines
}

// asLines 是 AS 号查询的几行：名称、RIR、宣告的前缀数。
func asLines(r report) []string {
	var lines []string
	if a := r.as; a != nil {
		lines = append(lines, "名称："+command.Escape(kit.OrDash(command.Truncate(a.Holder, holderWidth))))
	}
	lines = append(lines, rirLine(r.rir)...)
	if c := r.counts; c != nil {
		v4, v6 := c.Counts.V4.Originating, c.Counts.V6.Originating
		if v4+v6 == 0 {
			lines = append(lines, "宣告前缀：无")
		} else {
			lines = append(lines, fmt.Sprintf("宣告前缀：IPv4 %d 个 · IPv6 %d 个", v4, v6))
		}
	}
	return lines
}

func upstreamHeading(u upstreams) string {
	heading := "<b>" + asLabel(u.of) + " 的上游</b>"
	if u.total > len(u.top) {
		heading += fmt.Sprintf("（共 %d 个，按可见度列前 %d 个）", u.total, len(u.top))
	}
	return heading
}

// asWithName 是「AS13335 CLOUDFLARENET - Cloudflare, Inc.」：号码可以点一下复制，名字查不到时只有号码。
func asWithName(asn uint32, name string) string {
	text := command.Code(asLabel(asn))
	if name = strings.TrimSpace(name); name != "" {
		text += " " + command.Escape(command.Truncate(name, holderWidth))
	}
	return text
}

// listCodes 列出前几个，总数更多时加「等 N 个」。
func listCodes(values []string, total int) string {
	shown := values[:min(len(values), maxListed)]
	codes := make([]string, len(shown))
	for index, value := range shown {
		codes[index] = command.Code(value)
	}
	text := strings.Join(codes, "、")
	if total > len(shown) {
		text += " 等 " + strconv.Itoa(total) + " 个"
	}
	return text
}

// places 是「🇺🇸 US」；前缀跨好几个国家时按覆盖比例列前几个：「🇺🇸 US 66% · 🇨🇳 CN 15%」。
// 同一国家的几条合并。国家是 "?"（任播）或查不到时写「—」。
func places(g geoLite) string {
	covered := map[string]float64{}
	city := map[string]string{}
	for _, located := range g.Located {
		for _, l := range located.Locations {
			if len(l.Country) != 2 {
				continue
			}
			covered[l.Country] += l.Covered
			if city[l.Country] == "" {
				city[l.Country] = strings.TrimSpace(l.City)
			}
		}
	}
	countries := make([]string, 0, len(covered))
	for country := range covered {
		countries = append(countries, country)
	}
	slices.SortFunc(countries, func(a, b string) int {
		return cmp.Or(cmp.Compare(covered[b], covered[a]), cmp.Compare(a, b))
	})
	if len(countries) == 0 {
		return kit.OrDash("")
	}
	if len(countries) == 1 {
		text := flag(countries[0]) + command.Escape(countries[0])
		if city[countries[0]] != "" {
			text += " · " + command.Escape(city[countries[0]])
		}
		return text
	}
	var parts []string
	for _, country := range countries[:min(len(countries), maxListed)] {
		parts = append(parts, fmt.Sprintf("%s%s %.0f%%", flag(country), command.Escape(country), covered[country]))
	}
	return strings.Join(parts, " · ")
}

// flag 把两个字母的国家代码换成国旗（区域指示符号），后面带一个空格；不是两个大写字母时为空。
func flag(code string) string {
	if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		return ""
	}
	return string([]rune{0x1F1E6 + rune(code[0]-'A'), 0x1F1E6 + rune(code[1]-'A')}) + " "
}

// rirLine 是「RIR：APNIC」，跨几个 RIR 的前缀都列出来。没有查或没有分配给任何 RIR 时不写这一行。
func rirLine(list *rirList) []string {
	if list == nil {
		return nil
	}
	var names []string
	for _, item := range list.RIRs {
		if name := strings.TrimSpace(item.RIR); name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return []string{"RIR：" + command.Escape(strings.Join(names, "、"))}
}

// ptrText 是反向解析那一格：有记录列记录；没有记录（NXDOMAIN 或没有 PTR）写「无」；
// 解析器超时、SERVFAIL 这类写「无法解析」，它们说明不了有没有记录。解析器的英文原话不发出去。
func ptrText(ptr reverseDNS) string {
	if len(ptr.Result) > 0 {
		return listCodes(ptr.Result, len(ptr.Result))
	}
	if ptrUnresolvable(ptr.Error) {
		return "无法解析"
	}
	return "无"
}

func ptrUnresolvable(message string) bool {
	lower := strings.ToLower(message)
	for _, word := range []string{"timed out", "timeout", "servfail", "failed", "refused"} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

// renderPTR 是 .bgp dns 的结果：每条 PTR 一行。
func renderPTR(ip string, ptr *reverseDNS) string {
	lines := []string{"🛰 <b>反向解析</b>", "", "地址：" + command.Code(ip)}
	if len(ptr.Result) == 0 {
		return strings.Join(append(lines, "PTR："+ptrText(*ptr)), "\n")
	}
	for _, name := range ptr.Result[:min(len(ptr.Result), 20)] {
		lines = append(lines, "PTR："+command.Code(name))
	}
	return strings.Join(lines, "\n")
}

// links 是 bgp.he.net 上对应的页面：前缀（没有宣告时是所查的 IP 或前缀）和起源 AS，或者所查的 AS。
func links(r report) []string {
	link := func(path, text string) string {
		return `<a href="https://bgp.he.net/` + command.Escape(path) + `">` + command.Escape(text) + `</a>`
	}
	switch {
	case r.target.kind == kindASN:
		return []string{link(asLabel(r.target.asn), asLabel(r.target.asn))}
	case r.prefix != nil && r.prefix.Announced && r.prefix.Resource != "":
		list := []string{link("net/"+r.prefix.Resource, r.prefix.Resource)}
		if len(r.prefix.ASNs) > 0 {
			list = append(list, link(asLabel(r.prefix.ASNs[0].ASN), asLabel(r.prefix.ASNs[0].ASN)))
		}
		return list
	case r.target.kind == kindIP:
		return []string{link("ip/"+r.target.String(), r.target.String())}
	}
	return []string{link("net/"+r.target.String(), r.target.String())}
}
