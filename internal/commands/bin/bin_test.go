package bin

import (
	"encoding/json"
	"strings"
	"testing"
)

// 只查发卡行前缀；粘贴进来的卡号其余部分绝不能进入请求。
func TestBINDigitsNeverSendsMoreThanEight(t *testing.T) {
	if got, ok := binDigits("4150 4212 3456 7890"); !ok || got != "41504212" {
		t.Errorf("a full card number became %q", got)
	}
	if got, ok := binDigits("415042"); !ok || got != "415042" {
		t.Errorf("six digits became %q", got)
	}
	if _, ok := binDigits("41504"); ok {
		t.Error("five digits were accepted")
	}
}

// 下面的应答是在部署环境上查 415042 时 binlist.net 返回的内容，
// 留着它是为了按真实结构检查渲染结果。
func TestRenderBINFromARealResponse(t *testing.T) {
	var result binResult
	raw := `{"number":{},"scheme":"visa","type":"credit","brand":"Visa Rewards","country":{"numeric":"643","alpha2":"RU","name":"Russian Federation (the)","emoji":"🇷🇺","currency":"RUB"},"bank":{"name":"(Ofac Sanctioned) Vtb Bank Pjsc"}}`
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	text := renderBIN("415042", result, bincheckInfo{})
	for _, wanted := range []string{"Visa", "贷记卡", "REWARDS", "Russian Federation", "卢布（RUB）", "Vtb Bank", "预付卡：未知"} {
		if !strings.Contains(text, wanted) {
			t.Errorf("rendering lost %q:\n%s", wanted, text)
		}
	}
	if strings.Contains(text, "(the)") {
		t.Errorf("the country kept binlist's article:\n%s", text)
	}

	// bincheck 有结果时，卡组织、发卡行和国家用它的，其余仍取 binlist。
	checked := bincheckInfo{Scheme: "MASTERCARD", Bank: "GAZPROMBANK", Country: "RUSSIAN FEDERATION"}
	text = renderBIN("545807", result, checked)
	for _, wanted := range []string{"Mastercard", "GAZPROMBANK", "🇷🇺 RUSSIAN FEDERATION", "卢布（RUB）", "贷记卡"} {
		if !strings.Contains(text, wanted) {
			t.Errorf("rendering with bincheck lost %q:\n%s", wanted, text)
		}
	}
	if strings.Contains(text, "Vtb Bank") {
		t.Errorf("binlist's bank was preferred over bincheck's:\n%s", text)
	}
	// binlist 查不成时只有 bincheck 的几项。
	text = renderBIN("545807", binResult{}, checked)
	if !strings.Contains(text, "RUSSIAN FEDERATION") || !strings.Contains(text, "GAZPROMBANK") {
		t.Errorf("a bincheck-only result lost its fields:\n%s", text)
	}
}

// bincheck.io 详情页的 og:description 两种写法，以及属性顺序反过来的 meta。
func TestParseBincheck(t *testing.T) {
	want := bincheckInfo{Scheme: "MASTERCARD", Bank: "GAZPROMBANK", Country: "RUSSIAN FEDERATION"}
	pages := []string{
		`<head><meta property="og:description" content="This number: 545807 is a valid BIN number MASTERCARD issued by GAZPROMBANK in RUSSIAN FEDERATION"></head>`,
		`<meta content="545807 is a valid BIN number 545807 is a valid BIN number MASTERCARD issued by GAZPROMBANK in RUSSIAN FEDERATION." property="og:description" />`,
	}
	for _, page := range pages {
		if got := parseBincheck(page); got != want {
			t.Errorf("parseBincheck = %+v, want %+v\n%s", got, want, page)
		}
	}
	amex := parseBincheck(`<meta property='og:description' content='valid BIN number AMERICAN EXPRESS issued by AMERICAN EXPRESS US CONSUMER in UNITED STATES'>`)
	if schemeName(amex.Scheme) != "American Express" || amex.Bank != "AMERICAN EXPRESS US CONSUMER" {
		t.Errorf("amex parsed as %+v", amex)
	}
	if got := parseBincheck(`<html><body>Not found</body></html>`); got != (bincheckInfo{}) {
		t.Errorf("a page without the description parsed as %+v", got)
	}
}
