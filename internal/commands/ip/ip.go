// Package ip 实现 .ip：查 IP 或域名的位置与运营商。
package ip

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

// ipResult 是 ip-api.com 的应答，只含请求时指定的字段。
type ipResult struct {
	Status     string `json:"status"`
	Message    string `json:"message"`
	Country    string `json:"country"`
	RegionName string `json:"regionName"`
	City       string `json:"city"`
	ISP        string `json:"isp"`
	Org        string `json:"org"`
	AS         string `json:"as"`
	Query      string `json:"query"`
	Timezone   string `json:"timezone"`
	Proxy      bool   `json:"proxy"`
	Hosting    bool   `json:"hosting"`
}

// ip-api 的免费接口只有明文 HTTP，HTTPS 要付费。明文传输的只有要查的
// 那个地址，没有别的。
const ipAPI = "http://ip-api.com/json/%s?lang=zh-CN&fields=status,message,country,regionName,city,isp,org,as,query,timezone,proxy,hosting"

var (
	ipv4Pattern  = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	ipv6Pattern  = regexp.MustCompile(`\b[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}\b`)
	domainInText = regexp.MustCompile(`\b(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]{2,}\b`)
)

// findAddress 从被回复的消息里挑出要查的东西：有 IP 地址就用地址，
// 没有就用域名。URL 按其中的主机名算。
func findAddress(text string) string {
	for _, field := range strings.Fields(text) {
		if parsed, err := url.Parse(field); err == nil && parsed.Host != "" {
			return parsed.Hostname()
		}
	}
	if match := ipv4Pattern.FindString(text); match != "" && net.ParseIP(match) != nil {
		return match
	}
	for _, match := range ipv6Pattern.FindAllString(text, -1) {
		if net.ParseIP(match) != nil {
			return match
		}
	}
	return domainInText.FindString(text)
}

func renderIP(result ipResult) string {
	var lines []string
	if result.Proxy {
		lines = append(lines, "⚠️ 可能是代理 IP")
	}
	if result.Hosting {
		lines = append(lines, "🖥 可能是数据中心 IP")
	}
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	place := strings.Join(nonEmpty(result.Country, result.RegionName, result.City), " · ")
	lines = append(lines, "🌍 <b>IP 查询</b>", "",
		"🔍 地址："+command.Code(result.Query),
		"📍 位置："+command.Escape(kit.OrDash(place)),
		"🏢 ISP："+command.Escape(kit.OrDash(result.ISP)),
		"🏦 组织："+command.Escape(kit.OrDash(result.Org)),
		"🔢 AS："+command.Code(kit.OrDash(result.AS)))
	if result.Timezone != "" {
		lines = append(lines, "⏰ 时区："+command.Escape(result.Timezone))
	}
	if number, _, ok := strings.Cut(strings.TrimPrefix(result.AS, "AS"), " "); ok && strings.HasPrefix(result.AS, "AS") {
		lines = append(lines, "", `<a href="https://bgp.he.net/AS`+command.Escape(number)+`">在 bgp.he.net 查看 AS`+command.Escape(number)+`</a>`)
	}
	return strings.Join(lines, "\n")
}

func nonEmpty(values ...string) []string {
	var kept []string
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			kept = append(kept, value)
		}
	}
	return kept
}

func ipHelp(prefix string) string {
	p := command.Escape(prefix)
	return "🌍 <b>IP 查询</b>\n\n查 IP 或域名的地理位置、运营商和 AS 号。\n\n" +
		"• <code>" + p + "ip 8.8.8.8</code>\n• <code>" + p + "ip example.com</code>\n" +
		"• 回复一条含 IP、域名或链接的消息发 <code>" + p + "ip</code>\n\n数据来自 ip-api.com。"
}

// Register 注册 .ip。
func Register(a *app.App) {
	ipHandle := func(ctx context.Context, inv *command.Invocation) error {
		query := strings.TrimSpace(inv.Rest(0))
		switch strings.ToLower(query) {
		case "help", "h":
			return inv.Edit(ctx, ipHelp(inv.Prefix))
		}
		if query == "" && inv.Message.ReplyToID != 0 {
			if reply, err := inv.Client.GetReply(ctx, inv.Message); err == nil && reply != nil {
				query = findAddress(reply.Text)
			}
		}
		if query == "" {
			return inv.Edit(ctx, ipHelp(inv.Prefix))
		}
		if found := findAddress(query); found != "" {
			query = found
		}
		if err := inv.EditText(ctx, "🔍 正在查询 "+query+"…"); err != nil {
			return err
		}
		response, err := httpx.Do(ctx, httpx.Request{URL: strings.Replace(ipAPI, "%s", url.PathEscape(query), 1), Timeout: 15 * time.Second, MaxBytes: 64 << 10})
		if err != nil || !response.OK() {
			return inv.EditText(ctx, "❌ 查询服务暂时连不上，稍后再试")
		}
		var result ipResult
		if json.Unmarshal(response.Body, &result) != nil {
			return inv.EditText(ctx, "❌ 查询服务返回了看不懂的内容")
		}
		if result.Status != "success" {
			return inv.EditText(ctx, "❌ 查不到 "+query+"："+kit.OrDash(result.Message))
		}
		return inv.Edit(ctx, renderIP(result))
	}
	a.Registry.Register(&command.Command{Name: "ip", Description: "查 IP 或域名的位置与运营商", Usage: "[IP|域名]", Help: ipHelp, Handle: ipHandle})
}
