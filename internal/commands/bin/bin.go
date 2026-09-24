// Package bin 实现 .bin：查卡头对应的发卡行。
package bin

import (
	"context"
	"encoding/json"
	"html"
	"regexp"
	"strings"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

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

// schemeNames 的键是去掉空格的小写卡组织名：binlist 给的是 amex 这样的简写，
// bincheck 给的是 AMERICAN EXPRESS 这样的全称。
var schemeNames = map[string]string{
	"visa": "Visa", "mastercard": "Mastercard", "amex": "American Express", "diners": "Diners Club",
	"discover": "Discover", "jcb": "JCB", "unionpay": "UnionPay", "maestro": "Maestro", "mir": "MIR",
	"americanexpress": "American Express", "dinersclub": "Diners Club", "dinersclubinternational": "Diners Club",
	"chinaunionpay": "UnionPay",
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

// bincheckInfo 是从 bincheck.io 详情页里读出的卡组织、发卡行和国家。
type bincheckInfo struct {
	Scheme  string
	Bank    string
	Country string
}

var (
	// bincheck.io 是网页不是接口，要的信息都在 og:description 那一句里；
	// 属性的先后顺序不固定，两种都认。
	ogDescription = []*regexp.Regexp{
		regexp.MustCompile(`(?i)<meta[^>]+property=["']og:description["'][^>]+content=["']([^"']*)["']`),
		regexp.MustCompile(`(?i)<meta[^>]+content=["']([^"']*)["'][^>]+property=["']og:description["']`),
	}
	// 描述有两种写法，和 MiBox 一样先试号码重复出现的那种，再试普通的那种，比如
	// "This number: 545807 is a valid BIN number MASTERCARD issued by GAZPROMBANK in RUSSIAN FEDERATION"。
	bincheckPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)valid BIN number\s+\d+\s+(?:is\s+)?(?:a\s+)?valid BIN number\s+([A-Z ]+)\s+issued by\s+(.+?)\s+in\s+(.+)`),
		regexp.MustCompile(`(?i)valid BIN number\s+([A-Z ]+)\s+issued by\s+(.+?)\s+in\s+(.+)`),
	}
)

// parseBincheck 从 bincheck.io 的详情页里读出卡组织、发卡行和国家，读不出来返回零值。
func parseBincheck(page string) bincheckInfo {
	var description string
	for _, pattern := range ogDescription {
		if match := pattern.FindStringSubmatch(page); match != nil {
			description = html.UnescapeString(match[1])
			break
		}
	}
	for _, pattern := range bincheckPatterns {
		if match := pattern.FindStringSubmatch(description); match != nil {
			return bincheckInfo{
				Scheme:  strings.Join(strings.Fields(match[1]), " "),
				Bank:    strings.TrimSpace(match[2]),
				Country: strings.TrimSpace(strings.TrimRight(match[3], ". ")),
			}
		}
	}
	return bincheckInfo{}
}

// bincheck 查 bincheck.io，只发前 6 位。查不到或出错返回零值，这时全用 binlist 的结果。
func bincheck(ctx context.Context, bin string) bincheckInfo {
	response, err := httpx.Do(ctx, httpx.Request{URL: "https://bincheck.io/details/" + bin[:6], Timeout: 8 * time.Second, MaxBytes: 2 << 20})
	if err != nil || response.Status != 200 {
		return bincheckInfo{}
	}
	return parseBincheck(string(response.Body))
}

// schemeName 把卡组织名写成常见的样子；表里没有的，每个词首字母大写。
func schemeName(raw string) string {
	if name, ok := schemeNames[strings.ToLower(strings.ReplaceAll(raw, " ", ""))]; ok {
		return name
	}
	words := strings.Fields(strings.ToLower(raw))
	for index, word := range words {
		words[index] = strings.ToUpper(word[:1]) + word[1:]
	}
	return kit.OrDash(strings.Join(words, " "))
}

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

// renderBIN 写出查询结果。卡组织、发卡行和国家优先用 bincheck.io 的，和 MiBox 一样；
// 其余各项只有 binlist 有。
func renderBIN(bin string, result binResult, checked bincheckInfo) string {
	scheme := schemeName(kit.OrDefault(checked.Scheme, result.Scheme))
	kind := cardTypes[strings.ToLower(result.Type)]
	if kind == "" {
		kind = kit.OrDash(result.Type)
	}
	level := "—"
	if match := cardLevel.FindString(result.Brand); match != "" {
		level = strings.ToUpper(match)
	}
	country, currency := kit.OrDash(checked.Country), "—"
	if result.Country != nil {
		name := kit.OrDefault(checked.Country, strings.TrimSpace(strings.TrimSuffix(result.Country.Name, " (the)")))
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
	if checked.Bank != "" {
		bank = checked.Bank
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
		"只会用前 8 位去查，多输入的数字在发请求之前就丢掉了。数据来自 bincheck.io（只发前 6 位）和 binlist.net；" +
		"卡组织、发卡行和国家优先用 bincheck.io 的。binlist.net 免费额度很小，查多了会限流。"
}

// binlist 查 lookup.binlist.net。查不成时返回给用户看的说明。
func binlist(ctx context.Context, bin string) (binResult, string) {
	var result binResult
	response, err := httpx.Do(ctx, httpx.Request{URL: "https://lookup.binlist.net/" + bin,
		Headers: map[string]string{"Accept-Version": "3"}, Timeout: 15 * time.Second, MaxBytes: 64 << 10})
	switch {
	case err != nil:
		return result, "❌ 查询服务暂时连不上，稍后再试"
	case response.Status == 404:
		return result, "❌ 没有这个卡头的记录：" + bin
	case response.Status == 429:
		return result, "⏳ binlist.net 限流了，免费额度每小时只有几次，过一阵再试"
	case !response.OK():
		return result, "❌ 查询服务出错了，稍后再试"
	}
	if json.Unmarshal(response.Body, &result) != nil {
		return binResult{}, "❌ 查询服务返回了看不懂的内容"
	}
	return result, ""
}

// Register 注册 .bin。
func Register(a *app.App) {
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
		checked := make(chan bincheckInfo, 1)
		go func() { checked <- bincheck(ctx, bin) }()
		result, failure := binlist(ctx, bin)
		fromBincheck := <-checked
		// 两个来源同时查。binlist 查不成时，只要 bincheck 有结果就照样给出它那几项；
		// 两边都没有才报错。
		if failure != "" && fromBincheck == (bincheckInfo{}) {
			return inv.EditText(ctx, failure)
		}
		return inv.Edit(ctx, renderBIN(bin, result, fromBincheck))
	}
	a.Registry.Register(&command.Command{Name: "bin", Description: "查卡头对应的发卡行", Usage: "卡号前6-8位", Help: binHelp, Handle: binHandle})
}
