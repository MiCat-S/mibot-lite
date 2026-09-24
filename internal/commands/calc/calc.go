// Package calc 实现 .calc：计算四则运算表达式。
package calc

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
)

const maxExpressionLength = 120

// calcParser 是递归下降求值器，支持 + - * / 和括号。
type calcParser struct {
	text  []rune
	index int
}

func (p *calcParser) parse() (float64, error) {
	value, err := p.additive()
	if err != nil {
		return 0, err
	}
	p.skipSpace()
	if p.index != len(p.text) {
		return 0, errors.New("表达式格式错误")
	}
	if math.IsInf(value, 0) || math.IsNaN(value) || math.Abs(value) > 1<<53-1 {
		return 0, errors.New("计算结果超出安全范围")
	}
	return value, nil
}

func (p *calcParser) additive() (float64, error) {
	value, err := p.multiplicative()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpace()
		op := p.peek()
		if op != '+' && op != '-' {
			return value, nil
		}
		p.index++
		right, err := p.multiplicative()
		if err != nil {
			return 0, err
		}
		if op == '+' {
			value += right
		} else {
			value -= right
		}
	}
}

func (p *calcParser) multiplicative() (float64, error) {
	value, err := p.unary()
	if err != nil {
		return 0, err
	}
	for {
		p.skipSpace()
		op := p.peek()
		if op != '*' && op != '/' {
			return value, nil
		}
		p.index++
		right, err := p.unary()
		if err != nil {
			return 0, err
		}
		if op == '/' && right == 0 {
			return 0, errors.New("除零错误")
		}
		if op == '*' {
			value *= right
		} else {
			value /= right
		}
		if math.IsInf(value, 0) || math.IsNaN(value) {
			return 0, errors.New("计算结果无效")
		}
	}
}

func (p *calcParser) unary() (float64, error) {
	p.skipSpace()
	op := p.peek()
	if op == '+' || op == '-' {
		p.index++
		value, err := p.unary()
		if err != nil {
			return 0, err
		}
		if op == '-' {
			return -value, nil
		}
		return value, nil
	}
	return p.primary()
}

func (p *calcParser) primary() (float64, error) {
	p.skipSpace()
	if p.peek() == '(' {
		p.index++
		value, err := p.additive()
		if err != nil {
			return 0, err
		}
		p.skipSpace()
		if p.peek() != ')' {
			return 0, errors.New("括号不匹配")
		}
		p.index++
		return value, nil
	}
	start := p.index
	for p.index < len(p.text) && (p.text[p.index] >= '0' && p.text[p.index] <= '9' || p.text[p.index] == '.') {
		p.index++
	}
	token := string(p.text[start:p.index])
	if !numberToken(token) {
		return 0, errors.New("数字格式错误")
	}
	value, err := strconv.ParseFloat(token, 64)
	if err != nil || math.IsInf(value, 0) {
		return 0, errors.New("数字超出范围")
	}
	return value, nil
}

func numberToken(token string) bool {
	if token == "" {
		return false
	}
	whole, fraction, dot := strings.Cut(token, ".")
	if whole == "" || (dot && fraction == "") {
		return false
	}
	for _, r := range whole + fraction {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (p *calcParser) peek() rune {
	if p.index < len(p.text) {
		return p.text[p.index]
	}
	return 0
}

func (p *calcParser) skipSpace() {
	for p.index < len(p.text) && (p.text[p.index] == ' ' || p.text[p.index] == '\t' || p.text[p.index] == '\n' || p.text[p.index] == '\r') {
		p.index++
	}
}

// formatCalc 格式化结果，与 MiBox 一致：整数原样输出；其余先四舍五入到小数点后
// 12 位（Math.round(value*1e12)/1e12），再按 JavaScript 的数字转字符串规则输出。
//
// 这是按小数位而不是有效数字取舍：1/3 显示 0.333333333333；而 100000/3 乘上 1e12
// 之后超出了双精度能表示小数的范围，取整不起作用，显示 33333.333333333336，
// MiBox 的输出也是这样。
func formatCalc(value float64) string {
	if value == math.Trunc(value) {
		if value == 0 {
			// -0 也显示成 0。
			return "0"
		}
		return strconv.FormatFloat(value, 'f', 0, 64)
	}
	rounded := roundHalfUp(value*1e12) / 1e12
	if rounded == 0 {
		return "0"
	}
	return jsNumber(rounded)
}

// roundHalfUp 与 JavaScript 的 Math.round 一致：恰好一半时往正无穷方向进位（-2.5 → -2）。
func roundHalfUp(value float64) float64 {
	floor := math.Floor(value)
	if value-floor >= 0.5 {
		return floor + 1
	}
	return floor
}

// jsNumber 按 JavaScript 的 Number.prototype.toString 规则输出一个有限数：
// 取能还原出原值的最短数字串，十进制指数 n 在 -6 < n ≤ 21 时用定点写法，
// 其余写成 1.2345e-7 这样的指数形式。
func jsNumber(value float64) string {
	text := strconv.FormatFloat(value, 'e', -1, 64)
	sign := ""
	if strings.HasPrefix(text, "-") {
		sign, text = "-", text[1:]
	}
	mantissa, exponent, _ := strings.Cut(text, "e")
	power, _ := strconv.Atoi(exponent)
	digits := strings.Replace(mantissa, ".", "", 1)
	k, n := len(digits), power+1
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	}
	exponentSign := "+"
	if power < 0 {
		exponentSign, power = "-", -power
	}
	if k == 1 {
		return sign + digits + "e" + exponentSign + strconv.Itoa(power)
	}
	return sign + digits[:1] + "." + digits[1:] + "e" + exponentSign + strconv.Itoa(power)
}

// Register 注册 .calc。
func Register(a *app.App) {
	help := func(prefix string) string {
		return "🧮 <b>计算器</b>\n\n• " + command.Code(prefix+"calc 2+2*5") + "\n• " + command.Code(prefix+"calc (10-3)*4") + "\n• " + command.Code(prefix+"calc -(2-5)/3") + "\n支持括号、小数和负数。"
	}
	a.Registry.Register(&command.Command{Name: "calc", Description: "计算四则运算表达式", Usage: "表达式", Help: help,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			expression := inv.Rest(0)
			lower := strings.ToLower(expression)
			if expression == "" || lower == "help" || lower == "h" {
				return inv.Edit(ctx, help(inv.Prefix))
			}
			if len([]rune(expression)) > maxExpressionLength {
				return inv.Edit(ctx, "<b>计算失败</b>\n表达式长度不能超过 "+command.Code(strconv.Itoa(maxExpressionLength))+" 个字符")
			}
			parser := &calcParser{text: []rune(expression)}
			result, err := parser.parse()
			if err != nil {
				return inv.Edit(ctx, "<b>计算失败</b>\n"+command.Code(expression)+"\n"+command.Escape(err.Error()))
			}
			return inv.Edit(ctx, "<b>计算结果</b>\n"+command.Code(expression)+" = "+command.Code(formatCalc(result)))
		}})
}
