package encode

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
)

func find(name string) operation {
	for _, op := range operations {
		if op.name == name {
			return op
		}
	}
	panic(name)
}

// 编码再解码要还原出原文，包括换行、连续空格、中文和补充平面的 emoji。
func TestRoundTrip(t *testing.T) {
	for _, text := range []string{"Hello World", "你好世界", "a  b\nc\td", "🎉 emoji 🧬", "~!@#$%^&*()_+-=[]{}|;':\",./<>?"} {
		for _, pair := range [][2]string{{"b64encode", "b64decode"}, {"urlencode", "urldecode"}} {
			encoded, err := find(pair[0]).transform(text)
			if err != nil {
				t.Fatalf("%s(%q): %v", pair[0], text, err)
			}
			decoded, err := find(pair[1]).transform(encoded)
			if err != nil || decoded != text {
				t.Errorf("%s(%s(%q)) = %q, %v", pair[1], pair[0], text, decoded, err)
			}
		}
	}
}

// URL 编码的结果要和 JavaScript 的 encodeURIComponent 一模一样（MiBox 用的就是它）。
func TestEncodeURIComponentMatchesJavaScript(t *testing.T) {
	for input, want := range map[string]string{
		"你好世界":             "%E4%BD%A0%E5%A5%BD%E4%B8%96%E7%95%8C",
		"a b&c=d/é":        "a%20b%26c%3Dd%2F%C3%A9",
		"-_.!~*'()":        "-_.!~*'()",
		"?#[]@$+,;:\n":     "%3F%23%5B%5D%40%24%2B%2C%3B%3A%0A",
		"🎉":                "%F0%9F%8E%89",
		"AZaz09":           "AZaz09",
		"100% sure":        "100%25%20sure",
		"https://x.y/?q=1": "https%3A%2F%2Fx.y%2F%3Fq%3D1",
	} {
		if got := encodeURIComponent(input); got != want {
			t.Errorf("encodeURIComponent(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDecodeURIComponent(t *testing.T) {
	for input, want := range map[string]string{
		"%E4%BD%A0%e5%a5%bd": "你好",
		"a+b":                "a+b", // 和 decodeURIComponent 一样，+ 不当空格
		"plain":              "plain",
		"100%25":             "100%",
	} {
		if got, err := decodeURIComponent(input); err != nil || got != want {
			t.Errorf("decodeURIComponent(%q) = %q, %v, want %q", input, got, err, want)
		}
	}
	for _, bad := range []string{"%", "%4", "%zz", "abc%E4", "%FF", "%C3%28"} {
		if got, err := decodeURIComponent(bad); err == nil {
			t.Errorf("decodeURIComponent(%q) = %q, want an error", bad, got)
		}
	}
}

func TestDecodeBase64(t *testing.T) {
	for input, want := range map[string]string{
		"SGVsbG8gV29ybGQ=":      "Hello World",
		"SGVsbG8gV29ybGQ":       "Hello World", // 不带填充
		"SGVs\nbG8g V29y\nbGQ=": "Hello World", // 折过行
		"5L2g5aW9":              "你好",
		"Pz8/":                  "???",
		"Pz8_":                  "???", // URL 安全的字母表
		"fn5-":                  "~~~",
	} {
		if got, err := decodeBase64(input); err != nil || got != want {
			t.Errorf("decodeBase64(%q) = %q, %v, want %q", input, got, err, want)
		}
	}
	for _, bad := range []string{
		"", "   ", "A", "abc$", "SGVsbG8gV29ybGQ==",
		"SGVsbG8gV29ybGR=", // 末尾多余的位不是 0：解得出来，编码回去对不上
		"Pz8/Pz8_",         // 两种字母表混用
		"/w==",             // 0xFF，不是 UTF-8
	} {
		if got, err := decodeBase64(bad); err == nil {
			t.Errorf("decodeBase64(%q) = %q, want an error", bad, got)
		}
	}
}

func TestRenderPreviewAndEscaping(t *testing.T) {
	long := strings.Repeat("字", previewRunes+5)
	text := render(find("b64encode"), long, "<&>")
	if !strings.Contains(text, strings.Repeat("字", previewRunes)+"…</code>") || strings.Contains(text, strings.Repeat("字", previewRunes+1)) {
		t.Errorf("the preview was not cut at %d characters:\n%s", previewRunes, text)
	}
	if !strings.Contains(text, "<code>&lt;&amp;&gt;</code>") {
		t.Errorf("the result was not escaped:\n%s", text)
	}
	if !strings.HasPrefix(text, "🔣 <b>Base64 编码</b>") {
		t.Errorf("unexpected title:\n%s", text)
	}
	// 很长的结果分页后每一页都要能被发送时的 HTML 解析器接受。
	output, _ := find("b64encode").transform(strings.Repeat("长文本", 4000))
	pages := command.HTMLPages(render(find("b64encode"), "x", output), command.PageLimit)
	if len(pages) < 2 {
		t.Fatalf("expected several pages, got %d", len(pages))
	}
	for index, page := range pages {
		if _, _, err := bot.ParseHTML(page); err != nil {
			t.Errorf("page %d does not parse: %v", index+1, err)
		}
	}
}

// 五条命令共用一份帮助；四个转换命令收正文，.encode 不收。
func TestRegister(t *testing.T) {
	a := &app.App{Root: t.TempDir(), Registry: command.New([]string{"."}, slog.New(slog.NewTextHandler(io.Discard, nil)))}
	Register(a)
	for _, name := range []string{"encode", "b64encode", "b64decode", "urlencode", "urldecode"} {
		cmd, ok := a.Registry.Lookup(name)
		if !ok {
			t.Fatalf(".%s is not registered", name)
		}
		if cmd.Group != command.GroupTools || cmd.Help == nil || !strings.HasPrefix(cmd.Help("."), "🔣 <b>") {
			t.Errorf(".%s: group %q, help %v", name, cmd.Group, cmd.Help != nil)
		}
		if cmd.FreeText != (name != "encode") {
			t.Errorf(".%s: FreeText = %v", name, cmd.FreeText)
		}
		if n := len([]rune(cmd.Description)); n < 4 || n > 16 {
			t.Errorf(".%s: description %q has %d characters", name, cmd.Description, n)
		}
	}
}
