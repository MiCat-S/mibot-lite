package commands

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

const bgpHost = "bgp.tools"

var (
	ipv4Pattern   = regexp.MustCompile(`(?:^|\D)(\d{1,3}(?:\.\d{1,3}){3})(?:/\d{1,2})?(?:\D|$)`)
	dnsRowPattern = regexp.MustCompile(`(\d{1,3}(?:\.\d{1,3}){3})\s+([a-z0-9.-]+\.[a-z]{2,})`)
	tagStripper   = regexp.MustCompile(`<[^>]+>`)
	ipInText      = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
)

func validIPv4(ip string) bool {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return false
	}
	for _, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 || number > 255 || (len(part) > 1 && part[0] == '0') {
			return false
		}
	}
	return true
}

func extractIPv4(text string) string {
	match := ipv4Pattern.FindStringSubmatch(text)
	if len(match) == 2 && validIPv4(match[1]) {
		return match[1]
	}
	return ""
}

func ipPrefix(ip string, bits int) string {
	parts := strings.Split(ip, ".")
	var n uint32
	for _, part := range parts {
		value, _ := strconv.Atoi(part)
		n = n<<8 | uint32(value&255)
	}
	mask := ^uint32(0) << (32 - bits)
	x := n & mask
	return strconv.Itoa(int((x>>24)&255)) + "." + strconv.Itoa(int((x>>16)&255)) + "." + strconv.Itoa(int((x>>8)&255)) + "." + strconv.Itoa(int(x&255)) + "/" + strconv.Itoa(bits)
}

// maskIP replaces every IPv4 literal in text with a masked form, so a
// public paste never leaks the queried address's neighbours.
func maskIP(text string) string {
	return ipInText.ReplaceAllStringFunc(text, func(ip string) string {
		parts := strings.Split(ip, ".")
		return parts[0] + "." + parts[1] + ".x.x"
	})
}

func bgpGet(ctx context.Context, url string) (string, bool, error) {
	response, err := httpx.Do(ctx, httpx.Request{URL: url, Headers: map[string]string{"Accept": "text/html,application/xhtml+xml"}, Timeout: 15 * time.Second, MaxBytes: 2 << 20})
	if err != nil {
		return "", false, err
	}
	if response.Status == 404 {
		return "", false, nil
	}
	if !response.OK() {
		return "", false, &httpx.StatusError{Status: response.Status}
	}
	return string(response.Body), true, nil
}

// Bgp registers .bgp. The route graph is bgp.tools' SVG, IP-masked and sent
// as a document — no rasterizer, so no heavy image dependency.
func Bgp(a *app.App) {
	help := func(prefix string) string {
		return "🌐 <b>BGP 路由查询</b>\n• " + command.Code(prefix+"bgp 1.1.1.1") + " 路由图\n• " + command.Code(prefix+"bgp dns 1.1.1.1") + " DNS 记录\n也可回复含 IPv4 的消息。"
	}
	a.Registry.Register(&command.Command{Name: "bgp", Description: "查询 IPv4 的 BGP 路由图与 DNS 记录", Usage: "[dns] IP", Help: help, Timeout: time.Minute,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			isDNS := strings.ToLower(inv.Arg(0)) == "dns"
			args := inv.Args
			if isDNS {
				args = inv.Args[1:]
			}
			ip := extractIPv4(strings.Join(args, " "))
			if ip == "" {
				if reply, err := inv.Client.GetReply(ctx, inv.Message); err == nil && reply != nil {
					ip = extractIPv4(reply.Text)
				}
			}
			if ip == "" {
				return inv.Edit(ctx, help(inv.Prefix))
			}
			if isDNS {
				if err := inv.EditText(ctx, "🔍 正在查询 DNS 记录…"); err != nil {
					return err
				}
				for _, prefix := range []string{ipPrefix(ip, 24), ipPrefix(ip, 23)} {
					html, ok, err := bgpGet(ctx, "https://"+bgpHost+"/prefix/"+prefix+"#dns")
					if err != nil {
						return inv.EditText(ctx, "❌ BGP 查询失败，请稍后重试")
					}
					if !ok {
						continue
					}
					plain := strings.ReplaceAll(tagStripper.ReplaceAllString(html, " "), "&nbsp;", " ")
					roots := map[string]int{}
					type row struct{ ip, domain, root string }
					var rows []row
					for _, match := range dnsRowPattern.FindAllStringSubmatch(plain, -1) {
						if !validIPv4(match[1]) {
							continue
						}
						domain := strings.ToLower(match[2])
						segments := strings.Split(domain, ".")
						root := strings.Join(segments[max(0, len(segments)-2):], ".")
						roots[root]++
						rows = append(rows, row{match[1], domain, root})
					}
					var lines []string
					for _, r := range rows {
						if roots[r.root] <= 2 {
							lines = append(lines, r.ip+"\t"+r.domain)
						}
					}
					if len(lines) == 0 {
						continue
					}
					body := truncateRunes("A\tDNS\n"+strings.Join(lines, "\n"), 3500)
					return inv.Edit(ctx, "<blockquote expandable>"+command.Escape(body)+"</blockquote>\n\n🌐 <b>DNS解析记录</b>\n"+command.Code(ip)+"\n<i>使用前缀: "+command.Escape(prefix)+"</i>")
				}
				return inv.EditText(ctx, "❌ 未找到 DNS 解析记录")
			}
			if err := inv.EditText(ctx, "🔍 正在获取 BGP 路由图…"); err != nil {
				return err
			}
			for _, prefix := range []string{ipPrefix(ip, 24), ipPrefix(ip, 23)} {
				svg, ok, err := bgpGet(ctx, "https://"+bgpHost+"/pathimg/rt-"+strings.Replace(prefix, "/", "_", 1)+"?loggedin")
				if err != nil {
					return inv.EditText(ctx, "❌ BGP 查询失败，请稍后重试")
				}
				if !ok || (strings.Contains(svg, "Not_Visible") && strings.Contains(svg, "in_DFZ")) {
					continue
				}
				peer, err := inv.Client.InputPeer(inv.Message.Peer)
				if err != nil {
					return err
				}
				caption := "🌐 <b>BGP路由图</b>\n" + command.Code(ip) + "\n<i>使用前缀: " + command.Escape(prefix) + "</i>"
				if err := inv.Client.SendDocument(ctx, peer, "bgp-"+strings.Replace(prefix, "/", "_", 1)+".svg", "image/svg+xml", []byte(maskIP(svg)), caption, inv.Message.ID); err != nil {
					return err
				}
				return inv.Client.DeleteMessage(ctx, inv.Message)
			}
			return inv.Edit(ctx, "❌ 没有可用的 BGP 路由图\n"+command.Code(ipPrefix(ip, 24)))
		}})
}
