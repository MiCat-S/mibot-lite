// Package rate 实现 .rate：法币与加密货币的汇率查询和数量换算。
package rate

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

type rateFailure struct{ text string }

func (e rateFailure) Error() string { return e.text }

func rateFail(text string) error { return rateFailure{text: text} }

func rateReason(err error) string {
	var rf rateFailure
	if asRate(err, &rf) {
		return rf.text
	}
	return httpx.Reason(err)
}

func asRate(err error, target *rateFailure) bool {
	rf, ok := err.(rateFailure)
	if ok {
		*target = rf
	}
	return ok
}

var (
	rateCodePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]{0,63}$`)
	rateArgPattern  = regexp.MustCompile(`^[a-zA-Z0-9.+-]+$`)
	// rateBridges 是取美元价时借道的稳定币，按顺序试。
	//
	// 不再用 BUSD：它已经停止交易，币安上 BUSD 交易对的价格还查得到，
	// 但那是停牌前冻结的旧数据（同一时刻 BTCBUSD 报 42769，BTCUSDC 是 85505）。
	// 一个币只要没有 USDT 交易对、却有旧的 BUSD 交易对，就会被报出过时的价格。
	rateBridges = []string{"USDT", "USDC"}
)

type rateCurrency struct {
	Symbol string
	Fiat   bool
}

type rateService struct {
	mu        sync.Mutex
	fiatCache map[string]struct {
		rates map[string]float64
		at    time.Time
	}
	dynamicFiats map[string]bool
	dynamicAt    time.Time
	active       int
	// fetch 是取行情用的，默认就是 httpx.Do；测试里换成假的行情接口。
	fetch func(context.Context, httpx.Request) (httpx.Response, error)
}

func newRateService() *rateService {
	return &rateService{fiatCache: map[string]struct {
		rates map[string]float64
		at    time.Time
	}{}}
}

// rateGetter 在每次查询的请求配额之内获取 JSON 文档。
type rateGetter struct {
	fetch    func(context.Context, httpx.Request) (httpx.Response, error)
	requests int
}

func (g *rateGetter) get(ctx context.Context, target string, timeout time.Duration) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.requests++
	if g.requests > 64 {
		return nil, rateFail("本次查询已达到请求上限，请稍后重试")
	}
	fetch := g.fetch
	if fetch == nil {
		fetch = httpx.Do
	}
	response, err := fetch(ctx, httpx.Request{URL: target, Timeout: timeout, MaxBytes: 128 << 10})
	if err != nil {
		return nil, rateFail(httpx.Reason(err))
	}
	if !response.OK() {
		if response.Status == 429 {
			return nil, rateFail("API请求过于频繁，请等待几分钟后再试")
		}
		return nil, rateFail(fmt.Sprintf("汇率服务 HTTP %d", response.Status))
	}
	var data any
	if err := json.Unmarshal(response.Body, &data); err != nil {
		return nil, rateFail("汇率服务返回的数据格式或大小无效")
	}
	return data, nil
}

func asObject(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok
}

func normalizeCurrency(token string) string {
	key := strings.ToLower(token)
	if key == "rm" {
		return "myr"
	}
	for _, table := range []map[string]currency{fiatCurrencies, cryptoCurrencies} {
		for code, info := range table {
			for _, alias := range info.Aliases {
				if alias == key {
					return code
				}
			}
		}
	}
	return key
}

func parseRateArgs(args []string) (base, quote string, amount float64, err error) {
	if len(args) > 32 {
		return "", "", 0, rateFail("参数过多，请使用货币代码和有限数量")
	}
	amount = 1
	var currencies []string
	for _, arg := range args {
		if len(arg) > 64 || !rateArgPattern.MatchString(arg) {
			return "", "", 0, rateFail("请提供有效的货币代码和有限数量")
		}
		token := normalizeCurrency(arg)
		if number, parseErr := strconv.ParseFloat(token, 64); parseErr == nil && !math.IsInf(number, 0) && !math.IsNaN(number) {
			amount = number
			continue
		}
		lower := strings.ToLower(token)
		if rateCodePattern.MatchString(token) && lower != "nan" && lower != "infinity" {
			currencies = append(currencies, token)
			continue
		}
		return "", "", 0, rateFail("请提供有效的货币代码和有限数量")
	}
	base, quote = "btc", "usd"
	if len(currencies) > 0 {
		base = currencies[0]
	}
	if len(currencies) > 1 {
		quote = currencies[1]
	}
	return base, quote, amount, nil
}

func positiveRate(value float64) (float64, error) {
	if math.IsInf(value, 0) || math.IsNaN(value) || value <= 0 {
		return 0, rateFail("汇率服务返回了无效价格")
	}
	return value, nil
}

func formatThousands(value float64, decimals int) string {
	text := strconv.FormatFloat(value, 'f', decimals, 64)
	whole, fraction, _ := strings.Cut(text, ".")
	negative := strings.HasPrefix(whole, "-")
	whole = strings.TrimPrefix(whole, "-")
	var b strings.Builder
	for index, r := range whole {
		if index > 0 && (len(whole)-index)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	result := b.String()
	if negative {
		result = "-" + result
	}
	if fraction != "" {
		result += "." + fraction
	}
	return result
}

func formatAmount(value float64) string {
	if value >= 1 {
		return formatThousands(value, 2)
	}
	return strconv.FormatFloat(value, 'f', 6, 64)
}

func formatPrice(value float64) string {
	switch {
	case value >= 1:
		return formatAmount(value)
	case value >= 0.01:
		return strconv.FormatFloat(value, 'f', 4, 64)
	case value >= 0.0001:
		return strconv.FormatFloat(value, 'f', 6, 64)
	}
	return strconv.FormatFloat(value, 'e', 2, 64)
}

func (s *rateService) isFiat(ctx context.Context, query string, get *rateGetter) bool {
	s.mu.Lock()
	if s.dynamicFiats != nil && time.Since(s.dynamicAt) < 6*time.Hour {
		known := s.dynamicFiats[query]
		s.mu.Unlock()
		return known
	}
	s.mu.Unlock()
	endpoints := []string{
		"https://api.coingecko.com/api/v3/simple/supported_vs_currencies",
		"https://api.exchangerate.host/symbols",
		"https://api.frankfurter.app/currencies",
	}
	for index, endpoint := range endpoints {
		data, err := get.get(ctx, endpoint, 8*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			continue
		}
		var keys []string
		switch index {
		case 0:
			list, ok := data.([]any)
			if !ok {
				continue
			}
			for _, item := range list {
				if text, ok := item.(string); ok {
					keys = append(keys, text)
				}
			}
		case 1:
			object, _ := asObject(data)
			symbols, _ := asObject(object["symbols"])
			for key := range symbols {
				keys = append(keys, key)
			}
		default:
			object, _ := asObject(data)
			for key := range object {
				keys = append(keys, key)
			}
		}
		if len(keys) == 0 || len(keys) > 1024 {
			continue
		}
		valid := true
		codes := map[string]bool{}
		for _, key := range keys {
			if !rateCodePattern.MatchString(key) {
				valid = false
				break
			}
			codes[strings.ToLower(key)] = true
		}
		if !valid {
			continue
		}
		s.mu.Lock()
		s.dynamicFiats, s.dynamicAt = codes, time.Now()
		s.mu.Unlock()
		return codes[query]
	}
	codes := map[string]bool{}
	for key := range fiatCurrencies {
		codes[key] = true
	}
	s.mu.Lock()
	s.dynamicFiats, s.dynamicAt = codes, time.Now()
	s.mu.Unlock()
	return codes[query]
}

func (s *rateService) currency(ctx context.Context, query string, get *rateGetter) rateCurrency {
	if info, ok := fiatCurrencies[query]; ok {
		return rateCurrency{Symbol: info.Symbol, Fiat: true}
	}
	if info, ok := cryptoCurrencies[query]; ok {
		return rateCurrency{Symbol: info.Symbol}
	}
	return rateCurrency{Symbol: strings.ToUpper(query), Fiat: s.isFiat(ctx, query, get)}
}

func (s *rateService) fiatRates(ctx context.Context, base string, get *rateGetter) (map[string]float64, error) {
	key := strings.ToLower(base)
	s.mu.Lock()
	if cached, ok := s.fiatCache[key]; ok && time.Since(cached.at) < 5*time.Minute {
		s.mu.Unlock()
		return cached.rates, nil
	}
	s.mu.Unlock()
	escaped := url.QueryEscape(key)
	endpoints := []string{
		"https://api.exchangerate.host/latest?base=" + escaped,
		"https://open.er-api.com/v6/latest/" + escaped,
		"https://api.frankfurter.app/latest?from=" + escaped,
		"https://api.coinbase.com/v2/exchange-rates?currency=" + url.QueryEscape(strings.ToUpper(key)),
		"https://cdn.jsdelivr.net/gh/fawazahmed0/currency-api@1/latest/currencies/" + escaped + ".json",
	}
	last := "法币汇率服务不可用"
	for _, endpoint := range endpoints {
		data, err := get.get(ctx, endpoint, 8*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			last = rateReason(err)
			continue
		}
		object, _ := asObject(data)
		source, ok := asObject(object["rates"])
		if !ok {
			inner, _ := asObject(object["data"])
			source, ok = asObject(inner["rates"])
		}
		if !ok {
			source, ok = asObject(object[key])
		}
		if !ok || object["success"] == false || (object["result"] != nil && object["result"] != "success") {
			last = "法币汇率数据格式无效"
			continue
		}
		if len(source) == 0 || len(source) > 1024 {
			last = "法币汇率数据大小无效"
			continue
		}
		rates := map[string]float64{}
		for name, value := range source {
			if !rateCodePattern.MatchString(name) {
				continue
			}
			var number float64
			switch typed := value.(type) {
			case float64:
				number = typed
			case string:
				parsed, err := strconv.ParseFloat(typed, 64)
				if err != nil {
					continue
				}
				number = parsed
			default:
				continue
			}
			if number > 0 && !math.IsInf(number, 0) {
				rates[strings.ToLower(name)] = number
			}
		}
		if len(rates) == 0 {
			last = "法币汇率数据无有效价格"
			continue
		}
		s.mu.Lock()
		if len(s.fiatCache) >= 16 {
			for k := range s.fiatCache {
				delete(s.fiatCache, k)
				break
			}
		}
		s.fiatCache[key] = struct {
			rates map[string]float64
			at    time.Time
		}{rates, time.Now()}
		s.mu.Unlock()
		return rates, nil
	}
	return nil, rateFail("法币汇率服务不可用：" + last)
}

func rateHelp(prefix string) string {
	line := func(args, detail string) string {
		return "• " + command.Code(prefix+"rate "+args) + " - " + detail + "\n"
	}
	return "🚀 <b>智能汇率查询助手</b>\n\n📊 <b>使用示例</b>\n" + line("BTC", "比特币美元价") + line("ETH CNY", "以太坊人民币价") +
		line("CNY TRY", "人民币兑土耳其里拉") + line("BTC CNY 0.5", "0.5个BTC换算") + line("CNY USDT 7000", "7000元换USDT")
}

// enter 占一个查询名额，同时最多 4 个，满了返回 false。用完要 leave。
func (s *rateService) enter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active >= 4 {
		return false
	}
	s.active++
	return true
}

func (s *rateService) leave() {
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
}

// ratePricer 算一次查询的价格。同一个币安交易对在一次查询里只取一次。
type ratePricer struct {
	ctx     context.Context
	service *rateService
	get     *rateGetter
	tickers map[string]float64
}

// binance 取币安一个交易对的现价。
func (p *ratePricer) binance(pair string) (float64, error) {
	if cached, ok := p.tickers[pair]; ok {
		return cached, nil
	}
	data, err := p.get.get(p.ctx, "https://api.binance.com/api/v3/ticker/price?symbol="+url.QueryEscape(pair), 5*time.Second)
	if err != nil {
		return 0, err
	}
	object, _ := asObject(data)
	var price float64
	switch value := object["price"].(type) {
	case float64:
		price = value
	case string:
		price, err = strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, rateFail("币安交易对价格无效")
		}
	default:
		return 0, rateFail("币安交易对价格无效")
	}
	price, err = positiveRate(price)
	if err != nil {
		return 0, err
	}
	p.tickers[pair] = price
	return price, nil
}

// inBridge 取一个币用某种稳定币计价的价格。币本身就是这种稳定币时，价格就是 1：
// 币安上没有 USDTUSDT 这样的交易对，以前查 USDT 本身（比如帮助里的示例
// .rate CNY USDT 7000）一律失败。
func (p *ratePricer) inBridge(coin, bridge string) (float64, error) {
	if coin == bridge {
		return 1, nil
	}
	return p.binance(coin + bridge)
}

// cryptoFiat 算一个币值多少法币：先经稳定币取美元价，再乘美元对该法币的汇率。
// 几个中转稳定币挨个试，全失败时报最后一个原因。
func (p *ratePricer) cryptoFiat(crypto, fiat string) (float64, error) {
	last := "交易对不可用"
	for _, bridge := range rateBridges {
		price, err := p.inBridge(crypto, bridge)
		if err != nil {
			if p.ctx.Err() != nil {
				return 0, err
			}
			last = rateReason(err)
			continue
		}
		rates, err := p.service.fiatRates(p.ctx, "usd", p.get)
		if err != nil {
			if p.ctx.Err() != nil {
				return 0, err
			}
			last = rateReason(err)
			continue
		}
		rate, err := positiveRate(rates[strings.ToLower(fiat)])
		if err != nil {
			last = rateReason(err)
			continue
		}
		return positiveRate(price * rate)
	}
	return 0, rateFail(fmt.Sprintf("无法获取 %s 对 %s 的价格。最后错误: %s", crypto, fiat, last))
}

// cryptoCrypto 算两个币之间的比率：先找直接交易对（正反都试），没有就经稳定币中转。
func (p *ratePricer) cryptoCrypto(first, second string) (float64, error) {
	if price, err := p.binance(first + second); err == nil {
		return price, nil
	} else if p.ctx.Err() != nil {
		return 0, err
	}
	if price, err := p.binance(second + first); err == nil {
		return positiveRate(1 / price)
	} else if p.ctx.Err() != nil {
		return 0, err
	}
	for _, bridge := range rateBridges {
		a, err := p.inBridge(first, bridge)
		if err != nil {
			if p.ctx.Err() != nil {
				return 0, err
			}
			continue
		}
		b, err := p.inBridge(second, bridge)
		if err != nil {
			if p.ctx.Err() != nil {
				return 0, err
			}
			continue
		}
		return positiveRate(a / b)
	}
	return 0, rateFail(fmt.Sprintf("无法找到 %s 和 %s 之间的交易对", first, second))
}

// price 按两种货币的类型选路：币对币、币对法币、法币对币（反过来算再取倒数）、法币对法币。
func (p *ratePricer) price(source, target rateCurrency) (float64, error) {
	switch {
	case !source.Fiat && !target.Fiat:
		return p.cryptoCrypto(source.Symbol, target.Symbol)
	case !source.Fiat:
		return p.cryptoFiat(source.Symbol, target.Symbol)
	case !target.Fiat:
		reverse, err := p.cryptoFiat(target.Symbol, source.Symbol)
		if err == nil {
			return positiveRate(1 / reverse)
		}
		if p.ctx.Err() == nil {
			err = rateFail(fmt.Sprintf("无法获取 %s 对 %s 的价格来计算反向汇率", target.Symbol, source.Symbol))
		}
		return 0, err
	}
	rates, err := p.service.fiatRates(p.ctx, source.Symbol, p.get)
	if err != nil {
		return 0, err
	}
	if rates[strings.ToLower(target.Symbol)] == 0 {
		return 0, rateFail(fmt.Sprintf("无法获取 %s 到 %s 的汇率", source.Symbol, target.Symbol))
	}
	return positiveRate(rates[strings.ToLower(target.Symbol)])
}

// renderPriceFailure 是取价失败时的回复。附上两种货币各被当成了法币还是加密货币，
// 认错了一眼就能看出来；再给一个谷歌搜索的链接兜底。
func renderPriceFailure(err error, source, target rateCurrency, fallback string) string {
	kind := func(c rateCurrency) string {
		if c.Fiat {
			return "fiat"
		}
		return "crypto"
	}
	return kit.Feedback("error", "获取价格失败", rateReason(err)) + "\n\n<b>🔍 调试信息:</b>\n• " + command.Code(source.Symbol) + " (" + kind(source) + ")\n• " +
		command.Code(target.Symbol) + " (" + kind(target) + ")" + fallback
}

// renderRate 排版查询结果。1 个币换法币时只写一行价格；其余写换算结果，再按类型补一行汇率说明。
func renderRate(p *ratePricer, source, target rateCurrency, amount, price, converted float64) string {
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	updated := time.Now().In(shanghai).Format("2006/1/2 15:04:05")
	var b strings.Builder
	b.WriteString("💱 <b>汇率</b>\n\n")
	switch {
	case !source.Fiat && target.Fiat && amount == 1:
		b.WriteString(command.Code(fmt.Sprintf("1 %s = %s %s", source.Symbol, formatPrice(price), target.Symbol)) + "\n\n")
	default:
		b.WriteString(command.Code(fmt.Sprintf("%s %s ≈", formatAmount(amount), source.Symbol)) + "\n" + command.Code(fmt.Sprintf("%s %s", formatAmount(converted), target.Symbol)) + "\n\n")
		switch {
		case source.Fiat && target.Fiat:
			b.WriteString("📊 <b>汇率:</b> " + command.Code(fmt.Sprintf("1 %s = %s %s", source.Symbol, formatAmount(price), target.Symbol)) + "\n")
		case !source.Fiat && !target.Fiat:
			first, _ := p.cryptoFiat(source.Symbol, "USD")
			second, _ := p.cryptoFiat(target.Symbol, "USD")
			b.WriteString("💎 <b>兑换比率:</b> " + command.Code(fmt.Sprintf("1 %s = %s %s", source.Symbol, formatAmount(price), target.Symbol)) + "\n")
			b.WriteString("📊 <b>基准价格:</b> " + command.Code(fmt.Sprintf("%s $%s • %s $%s", source.Symbol, formatPrice(first), target.Symbol, formatPrice(second))) + "\n")
		default:
			if source.Fiat {
				b.WriteString("💎 <b>当前汇率:</b> " + command.Code(fmt.Sprintf("1 %s = %s %s", target.Symbol, formatPrice(1/price), source.Symbol)) + "\n")
			} else {
				b.WriteString("💎 <b>当前汇率:</b> " + command.Code(fmt.Sprintf("1 %s = %s %s", source.Symbol, formatPrice(price), target.Symbol)) + "\n")
			}
		}
	}
	label := "数据更新"
	if source.Fiat && target.Fiat {
		label = "更新时间"
	}
	b.WriteString("⏰ <b>" + label + ":</b> " + updated)
	return b.String()
}

func rateHandle(ctx context.Context, inv *command.Invocation, service *rateService) error {
	if !service.enter() {
		return inv.Edit(ctx, kit.Feedback("error", "汇率查询繁忙", "请稍后重试"))
	}
	defer service.leave()

	first := strings.ToLower(inv.Arg(0))
	if first == "" || first == "help" || first == "h" {
		return inv.Edit(ctx, rateHelp(inv.Prefix))
	}
	base, quote, amount, err := parseRateArgs(inv.Args)
	if err != nil {
		return inv.Edit(ctx, kit.Feedback("error", "操作失败", rateReason(err)))
	}
	query := url.QueryEscape(fmt.Sprintf("%s %s to %s", strconv.FormatFloat(amount, 'f', -1, 64), strings.ToUpper(base), strings.ToUpper(quote)))
	fallback := "\n\n🔎 <b>谷歌兜底:</b> <a href=\"https://www.google.com/search?q=" + query + "\">点击查看</a>"
	get := &rateGetter{fetch: service.fetch}
	if err := inv.Edit(ctx, kit.Feedback("working", "正在查询汇率", "")); err != nil {
		return err
	}
	source := service.currency(ctx, base, get)
	target := service.currency(ctx, quote, get)
	pricer := &ratePricer{ctx: ctx, service: service, get: get, tickers: map[string]float64{}}
	price, err := pricer.price(source, target)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return inv.Edit(ctx, renderPriceFailure(err, source, target, fallback))
	}
	converted := amount * price
	if math.IsInf(converted, 0) || math.IsNaN(converted) {
		return inv.Edit(ctx, kit.Feedback("error", "操作失败", "换算结果超出有限数值范围")+fallback)
	}
	return inv.Edit(ctx, renderRate(pricer, source, target, amount, price, converted))
}

// Register 注册 .rate。
func Register(a *app.App) {
	service := newRateService()
	a.Registry.Register(&command.Command{Name: "rate", Description: "智能汇率查询与数量换算", Usage: "货币 [目标货币] [数量]", Help: rateHelp, Timeout: 2 * time.Minute,
		Handle: func(ctx context.Context, inv *command.Invocation) error { return rateHandle(ctx, inv, service) }})
}
