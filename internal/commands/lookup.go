package commands

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
		"📍 位置："+command.Escape(orDash(place)),
		"🏢 ISP："+command.Escape(orDash(result.ISP)),
		"🏦 组织："+command.Escape(orDash(result.Org)),
		"🔢 AS："+command.Code(orDash(result.AS)))
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

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

func ipHelp(prefix string) string {
	p := command.Escape(prefix)
	return "🌍 <b>IP 查询</b>\n\n查 IP 或域名的地理位置、运营商和 AS 号。\n\n" +
		"• <code>" + p + "ip 8.8.8.8</code>\n• <code>" + p + "ip example.com</code>\n" +
		"• 回复一条含 IP、域名或链接的消息发 <code>" + p + "ip</code>\n\n数据来自 ip-api.com。"
}

// binResult 是 binlist.net 的应答。
type binResult struct {
	Scheme  string `json:"scheme"`
	Type    string `json:"type"`
	Brand   string `json:"brand"`
	Prepaid *bool  `json:"prepaid"`
	Country *struct {
		Name     string `json:"name"`
		Alpha2   string `json:"alpha2"`
		Emoji    string `json:"emoji"`
		Currency string `json:"currency"`
	} `json:"country"`
	Bank *struct {
		Name string `json:"name"`
	} `json:"bank"`
}

var schemeNames = map[string]string{
	"visa": "Visa", "mastercard": "Mastercard", "amex": "American Express", "diners": "Diners Club",
	"discover": "Discover", "jcb": "JCB", "unionpay": "UnionPay", "maestro": "Maestro", "mir": "MIR",
}

var cardTypes = map[string]string{"credit": "贷记卡", "debit": "借记卡", "charge": "签账卡", "prepaid": "预付卡"}

var currencyNames = map[string]string{
	"USD": "美元", "CNY": "人民币", "HKD": "港币", "TWD": "新台币", "EUR": "欧元", "JPY": "日元",
	"GBP": "英镑", "AUD": "澳元", "CAD": "加元", "SGD": "新加坡元", "KRW": "韩元", "RUB": "卢布",
}

var (
	cardLevel    = regexp.MustCompile(`(?i)BUSINESS|CORPORATE|PLATINUM|GOLD|CLASSIC|SIGNATURE|INFINITE|WORLD|PREMIUM|REWARDS`)
	businessCard = regexp.MustCompile(`(?i)BUSINESS|CORPORATE|COMMERCIAL`)
)

// binDigits 只保留输入内容里的前 8 位数字。用得上的只有发卡行前缀；
// 卡号其余部分没有理由离开这台机器，所以在发出任何请求之前就丢掉。
func binDigits(input string) (string, bool) {
	var digits strings.Builder
	for _, r := range input {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	value := digits.String()
	if len(value) < 6 {
		return "", false
	}
	if len(value) > 8 {
		value = value[:8]
	}
	return value, true
}

func renderBIN(bin string, result binResult) string {
	scheme := schemeNames[strings.ToLower(result.Scheme)]
	if scheme == "" {
		scheme = orDash(result.Scheme)
	}
	kind := cardTypes[strings.ToLower(result.Type)]
	if kind == "" {
		kind = orDash(result.Type)
	}
	level := "—"
	if match := cardLevel.FindString(result.Brand); match != "" {
		level = strings.ToUpper(match)
	}
	country, currency := "—", "—"
	if result.Country != nil {
		name := strings.TrimSpace(strings.TrimSuffix(result.Country.Name, " (the)"))
		country = strings.TrimSpace(result.Country.Emoji + " " + name)
		if result.Country.Currency != "" {
			currency = result.Country.Currency
			if chinese, ok := currencyNames[currency]; ok {
				currency = chinese + "（" + result.Country.Currency + "）"
			}
		}
	}
	bank := "—"
	if result.Bank != nil && result.Bank.Name != "" {
		bank = result.Bank.Name
	}
	prepaid := "未知"
	if result.Prepaid != nil {
		prepaid = map[bool]string{true: "是", false: "否"}[*result.Prepaid]
	}
	business := "否"
	if businessCard.MatchString(result.Brand) {
		business = "是"
	}
	return "💳 <b>卡头查询</b>\n\n" +
		"🔢 卡头：" + command.Code(bin) + "\n" +
		"💳 品牌：" + command.Escape(scheme) + "\n" +
		"🔖 类型：" + command.Escape(kind) + "\n" +
		"💹 等级：" + command.Escape(level) + "\n\n" +
		"🗺 国家：" + command.Escape(country) + "\n" +
		"💸 货币：" + command.Escape(currency) + "\n" +
		"🏦 银行：" + command.Escape(bank) + "\n\n" +
		"💰 预付卡：" + prepaid + "\n" +
		"🧾 商业卡：" + business
}

func binHelp(prefix string) string {
	p := command.Escape(prefix)
	return "💳 <b>卡头查询</b>\n\n查银行卡号前 6–8 位（BIN）对应的卡组织、卡种、国家和发卡行。\n\n" +
		"• <code>" + p + "bin 415042</code>\n\n" +
		"只会用前 8 位去查，多输入的数字在发请求之前就丢掉了。数据来自 binlist.net，免费额度很小，查多了会限流。"
}

// Lookup 注册 .ip 和 .bin。
func Lookup(a *app.App) {
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
			return inv.EditText(ctx, "❌ 查不到 "+query+"："+orDash(result.Message))
		}
		return inv.Edit(ctx, renderIP(result))
	}

	binHandle := func(ctx context.Context, inv *command.Invocation) error {
		input := inv.Rest(0)
		if strings.EqualFold(input, "help") || strings.EqualFold(input, "h") || input == "" {
			return inv.Edit(ctx, binHelp(inv.Prefix))
		}
		bin, ok := binDigits(input)
		if !ok {
			return inv.EditText(ctx, "❌ 卡头至少要 6 位数字")
		}
		if err := inv.EditText(ctx, "🔍 正在查询卡头 "+bin+"…"); err != nil {
			return err
		}
		response, err := httpx.Do(ctx, httpx.Request{URL: "https://lookup.binlist.net/" + bin,
			Headers: map[string]string{"Accept-Version": "3"}, Timeout: 15 * time.Second, MaxBytes: 64 << 10})
		switch {
		case err != nil:
			return inv.EditText(ctx, "❌ 查询服务暂时连不上，稍后再试")
		case response.Status == 404:
			return inv.EditText(ctx, "❌ 没有这个卡头的记录："+bin)
		case response.Status == 429:
			return inv.EditText(ctx, "⏳ binlist.net 限流了，免费额度每小时只有几次，过一阵再试")
		case !response.OK():
			return inv.EditText(ctx, "❌ 查询服务出错了，稍后再试")
		}
		var result binResult
		if json.Unmarshal(response.Body, &result) != nil {
			return inv.EditText(ctx, "❌ 查询服务返回了看不懂的内容")
		}
		return inv.Edit(ctx, renderBIN(bin, result))
	}

	a.Registry.Register(
		&command.Command{Name: "ip", Description: "查 IP 或域名的位置与运营商", Usage: "[IP|域名]", Help: ipHelp, Handle: ipHandle},
		&command.Command{Name: "bin", Description: "查卡头对应的发卡行", Usage: "卡号前6-8位", Help: binHelp, Handle: binHandle},
	)
}
