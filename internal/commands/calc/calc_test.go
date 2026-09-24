package calc

import (
	"testing"
)

func TestCalcEvaluates(t *testing.T) {
	cases := map[string]string{
		"2+2*5":       "12",
		"(10-3)*4":    "28",
		"-(2-5)/3":    "1",
		"3+7":         "10",
		"8/2+5":       "9",
		"1.5*2":       "3",
		"10/4":        "2.5",
		"  7  -  2  ": "5",
	}
	for expression, want := range cases {
		parser := &calcParser{text: []rune(expression)}
		value, err := parser.parse()
		if err != nil {
			t.Errorf("%s: %v", expression, err)
			continue
		}
		if got := formatCalc(value); got != want {
			t.Errorf("%s = %s, want %s", expression, got, want)
		}
	}
}

// 期望值是 MiBox 的格式化函数在 Node 里对同一个数算出的结果：按小数点后 12 位
// 四舍五入，再按 JavaScript 的规则转成字符串。
func TestCalcFormatsLikeMiBox(t *testing.T) {
	cases := map[string]string{
		"100000/3":            "33333.333333333336",
		"1/3":                 "0.333333333333",
		"2/3":                 "0.666666666667",
		"-1/3":                "-0.333333333333",
		"0.1+0.2":             "0.3",
		"0.1*3":               "0.3",
		"0.0000001*1":         "1e-7",
		"0.000001*1":          "0.000001",
		"0.00000012345*1":     "1.2345e-7",
		"-0.0000000000025*1":  "-2e-12",
		"0.0000000000004*1":   "0",
		"1000000000000000.5":  "1000000000000000.6",
		"9007199254740991":    "9007199254740991",
		"1234567.891":         "1234567.891",
		"22/7":                "3.142857142857",
		"-0":                  "0",
		"5000000000000*3":     "15000000000000",
		"1/7":                 "0.142857142857",
		"123456789.123456789": "123456789.12345679",
	}
	for expression, want := range cases {
		parser := &calcParser{text: []rune(expression)}
		value, err := parser.parse()
		if err != nil {
			t.Errorf("%s: %v", expression, err)
			continue
		}
		if got := formatCalc(value); got != want {
			t.Errorf("%s = %s, want %s", expression, got, want)
		}
	}
}

func TestCalcRejects(t *testing.T) {
	for _, expression := range []string{"1/0", "(1+2", "1++", "2**3", "abc", "", "1 2", "0x10", "1.2.3"} {
		parser := &calcParser{text: []rune(expression)}
		if _, err := parser.parse(); err == nil {
			t.Errorf("%q should not evaluate", expression)
		}
	}
}
