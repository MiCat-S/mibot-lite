package commands

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
)

// fakeMarket 是假的行情接口：币安只认几个交易对，第一个法币接口故意挂掉，
// 好让法币汇率走到备用接口。记下每一个被请求的地址。
func fakeMarket(requested *[]string) func(context.Context, httpx.Request) (httpx.Response, error) {
	tickers := map[string]string{"BTCUSDT": "65000.5", "ETHUSDT": "3000.25", "USDTUSD": "1"}
	fiat := map[string]string{
		"usd": `{"result":"success","rates":{"USD":1,"CNY":7.1,"EUR":0.92,"TRY":32.5}}`,
		"cny": `{"result":"success","rates":{"CNY":1,"TRY":4.58,"USD":0.1408,"EUR":0.13}}`,
	}
	return func(_ context.Context, request httpx.Request) (httpx.Response, error) {
		*requested = append(*requested, request.URL)
		switch {
		case strings.HasPrefix(request.URL, "https://api.binance.com/api/v3/ticker/price?symbol="):
			pair := strings.TrimPrefix(request.URL, "https://api.binance.com/api/v3/ticker/price?symbol=")
			if price, ok := tickers[pair]; ok {
				return httpx.Response{Status: 200, Body: []byte(`{"symbol":"` + pair + `","price":"` + price + `"}`)}, nil
			}
			return httpx.Response{Status: 400, Body: []byte(`{"code":-1121,"msg":"Invalid symbol."}`)}, nil
		case strings.HasPrefix(request.URL, "https://api.exchangerate.host/"):
			return httpx.Response{Status: 500, Body: []byte(`{}`)}, nil
		case strings.HasPrefix(request.URL, "https://open.er-api.com/v6/latest/"):
			if body, ok := fiat[strings.TrimPrefix(request.URL, "https://open.er-api.com/v6/latest/")]; ok {
				return httpx.Response{Status: 200, Body: []byte(body)}, nil
			}
		case request.URL == "https://api.coingecko.com/api/v3/simple/supported_vs_currencies":
			return httpx.Response{Status: 200, Body: []byte(`["usd","cny","eur","try"]`)}, nil
		}
		return httpx.Response{Status: 404, Body: []byte(`{}`)}, nil
	}
}

var rateQueries = []string{
	"", "help", "BTC", "ETH CNY", "CNY TRY", "BTC CNY 0.5", "CNY USDT 7000", "BTC ETH",
	"XYZ CNY", "CNY ABC", "BTC CNY abc", "BTC CNY 0",
}

var clock = regexp.MustCompile(`\d{4}/\d{1,2}/\d{1,2} \d{2}:\d{2}:\d{2}`)

func runRate(t *testing.T, query string, handle func(context.Context, *command.Invocation, *rateService) error) ([]string, []string) {
	t.Helper()
	fake := newFakeTelegram(t, dmeSelf)
	client := fakeClient(fake, otherUser, nil)
	var requested []string
	service := newRateService()
	service.fetch = fakeMarket(&requested)
	message := &bot.Message{ID: dmeCommand, Peer: dmePrivate, ChatID: bot.PeerID(dmePrivate), Out: true}
	if err := handle(context.Background(), fakeInvocation(client, message, strings.Fields(query)...), service); err != nil {
		fake.log("error %v", err)
	}
	calls := make([]string, len(fake.calls))
	for index, call := range fake.calls {
		calls[index] = clock.ReplaceAllString(call, "#time")
	}
	return calls, requested
}

// TestRateSnapshot 把每条查询的回复和请求过的行情地址与快照比较。
func TestRateSnapshot(t *testing.T) {
	var out strings.Builder
	for _, query := range rateQueries {
		calls, requested := runRate(t, query, rateHandle)
		out.WriteString(section(fmt.Sprintf("%q", query), append(calls, requested...), ""))
	}
	golden(t, "rate", out.String())
}

// 稳定币本身作为币种时，要按「自己对自己是 1」计价；BUSD 已停牌，不能再拿来中转。
func TestRateStablecoins(t *testing.T) {
	for query, wanted := range map[string]string{
		"CNY USDT 7000": "985.92 USDT",
		"USDT CNY":      "1 USDT = 7.10 CNY",
		"USDT EUR 100":  "92.00 EUR",
	} {
		calls, requested := runRate(t, query, rateHandle)
		reply := calls[len(calls)-1]
		if !strings.Contains(reply, wanted) {
			t.Errorf("%q：回复里没有 %q\n%s", query, wanted, reply)
		}
		for _, url := range requested {
			if strings.HasSuffix(url, "=USDTUSDT") || strings.Contains(url, "BUSD") {
				t.Errorf("%q：不该请求 %s", query, url)
			}
		}
	}
}

// TestRateLive 用真实的币安和汇率接口跑几条稳定币查询，确认修好的是线上真实的情况。
// 默认跳过：MIBOT_RATE_LIVE=1 ./commands.test -test.run RateLive -test.v
func TestRateLive(t *testing.T) {
	if os.Getenv("MIBOT_RATE_LIVE") != "1" {
		t.Skip("设置 MIBOT_RATE_LIVE=1 才连真实接口")
	}
	for _, query := range []string{"CNY USDT 7000", "USDT CNY", "USDC CNY", "BTC USDT", "USDT USDC"} {
		fake := newFakeTelegram(t, dmeSelf)
		client := fakeClient(fake, otherUser, nil)
		message := &bot.Message{ID: dmeCommand, Peer: dmePrivate, ChatID: bot.PeerID(dmePrivate), Out: true}
		if err := rateHandle(context.Background(), fakeInvocation(client, message, strings.Fields(query)...), newRateService()); err != nil {
			t.Fatalf("%q: %v", query, err)
		}
		reply := fake.calls[len(fake.calls)-1]
		t.Logf("%-14s %s", query, strings.ReplaceAll(reply, "\\n", " "))
		if strings.Contains(reply, "失败") {
			t.Errorf("%q 在真实接口上失败了", query)
		}
	}
}
