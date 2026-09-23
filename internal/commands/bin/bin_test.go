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
	text := renderBIN("415042", result)
	for _, wanted := range []string{"Visa", "贷记卡", "REWARDS", "Russian Federation", "卢布（RUB）", "Vtb Bank", "预付卡：未知"} {
		if !strings.Contains(text, wanted) {
			t.Errorf("rendering lost %q:\n%s", wanted, text)
		}
	}
	if strings.Contains(text, "(the)") {
		t.Errorf("the country kept binlist's article:\n%s", text)
	}
}
