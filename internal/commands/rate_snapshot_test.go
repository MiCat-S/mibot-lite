package commands

import (
	"context"
	"fmt"
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
