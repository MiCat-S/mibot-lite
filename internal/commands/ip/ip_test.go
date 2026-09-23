package ip

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFindAddress(t *testing.T) {
	for text, want := range map[string]string{
		"连不上 8.8.8.8 了":                 "8.8.8.8",
		"看这个 https://example.com/a?b=1": "example.com",
		"example.org 挂了吗":               "example.org",
		"2001:4860:4860::8888":          "2001:4860:4860::8888",
		"没有地址":                          "",
	} {
		if got := findAddress(text); got != want {
			t.Errorf("findAddress(%q) = %q, want %q", text, got, want)
		}
	}
}

// 同样，这是 ip-api 对 8.8.8.8 的真实应答。
func TestRenderIPFromARealResponse(t *testing.T) {
	var result ipResult
	raw := `{"status":"success","country":"美国","regionName":"弗吉尼亚州","city":"Ashburn","timezone":"America/New_York","isp":"Google LLC","org":"Google Public DNS","as":"AS15169 Google LLC","proxy":false,"hosting":true,"query":"8.8.8.8"}`
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	text := renderIP(result)
	for _, wanted := range []string{"8.8.8.8", "美国 · 弗吉尼亚州 · Ashburn", "Google LLC", "数据中心", "bgp.he.net/AS15169"} {
		if !strings.Contains(text, wanted) {
			t.Errorf("rendering lost %q:\n%s", wanted, text)
		}
	}
	if strings.Contains(text, "代理") {
		t.Errorf("a non-proxy was flagged as one:\n%s", text)
	}
}
