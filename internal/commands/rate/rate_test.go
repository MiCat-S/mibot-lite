package rate

import (
	"strings"
	"testing"
)

func TestParseRateArgs(t *testing.T) {
	base, quote, amount, err := parseRateArgs([]string{"BTC", "CNY", "0.5"})
	if err != nil || base != "btc" || quote != "cny" || amount != 0.5 {
		t.Fatalf("got %s %s %v err=%v", base, quote, amount, err)
	}
	if base, quote, amount, _ := parseRateArgs(nil); base != "btc" || quote != "usd" || amount != 1 {
		t.Fatalf("defaults are %s %s %v", base, quote, amount)
	}
	// 别名会解析成对应的货币代码。
	if base, _, _, _ := parseRateArgs([]string{"rmb"}); base != "cny" {
		t.Fatalf("rmb resolved to %s", base)
	}
	for _, args := range [][]string{{"../etc/passwd"}, {"<script>"}, {"nan"}, {strings.Repeat("a", 80)}} {
		if _, _, _, err := parseRateArgs(args); err == nil {
			t.Errorf("%v should be rejected", args)
		}
	}
}

func TestFormatPriceAndAmount(t *testing.T) {
	if got := formatAmount(1234567.891); got != "1,234,567.89" {
		t.Fatalf("formatAmount = %q", got)
	}
	if got := formatPrice(0.00001234); !strings.Contains(got, "e") {
		t.Fatalf("a tiny price should use exponent form, got %q", got)
	}
	if got := formatPrice(0.5); got != "0.5000" {
		t.Fatalf("formatPrice(0.5) = %q", got)
	}
}
