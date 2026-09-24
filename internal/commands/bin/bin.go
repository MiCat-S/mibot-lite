// Package bin 实现 .bin：查卡头对应的发卡行。
package bin

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

// binResult 是 binlist.net 的应答。
//
// MiBox 的原版还会抓 bincheck.io 的详情页，并优先用它的发卡行和国家。那个网站现在
// 挡掉所有非浏览器请求（2026-09-24 从本机和服务器请求都是 403），所以这里只用 binlist。
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
		scheme = kit.OrDash(result.Scheme)
	}
	kind := cardTypes[strings.ToLower(result.Type)]
	if kind == "" {
		kind = kit.OrDash(result.Type)
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
	a.Registry.Register(&command.Command{Name: "bin", Description: "查卡头对应的发卡行", Usage: "卡号前6-8位", Help: binHelp, Handle: binHandle})
}
