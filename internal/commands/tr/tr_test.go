package tr

import (
	"testing"
)

// 这个接口的应答有两种结构，取决于请求里有没有要求它检测源语言。
func TestParseTranslation(t *testing.T) {
	detected, err := parseTranslation([]byte(`[["你好世界","en"]]`))
	if err != nil {
		t.Fatal(err)
	}
	if detected.Text != "你好世界" || detected.Source != "en" {
		t.Fatalf("got %+v", detected)
	}
	// 指明了源语言的请求，拿回来的是一个裸字符串。
	named, err := parseTranslation([]byte(`["早上好"]`))
	if err != nil {
		t.Fatal(err)
	}
	if named.Text != "早上好" || named.Source != "" {
		t.Fatalf("got %+v", named)
	}
	for _, raw := range []string{``, `{}`, `[]`, `[[]]`, `[[""]]`, `[""]`, `not json`} {
		if _, err := parseTranslation([]byte(raw)); err == nil {
			t.Errorf("%q should not parse into a translation", raw)
		}
	}
}

// 第一个参数只有确实是某个语言名时，才算目标语言。如果按外形判断，
// "tr is this correct" 里的第一个词就会被吞掉。
func TestNamedLanguage(t *testing.T) {
	for input, want := range map[string]string{
		"en": "en", "EN": "en", "english": "en", "英文": "en",
		"zh": "zh-CN", "cn": "zh-CN", "中文": "zh-CN", "tw": "zh-TW",
		"jp": "ja", "ja": "ja", "ko": "ko", "ru": "ru",
	} {
		got, ok := namedLanguage(input)
		if !ok || got != want {
			t.Errorf("namedLanguage(%q) = %q %v, want %q", input, got, ok, want)
		}
	}
	for _, input := range []string{"", "hello", "this", "翻译一下", "zzz", "xyzzy"} {
		if code, ok := namedLanguage(input); ok {
			t.Errorf("%q should be text, not the language %q", input, code)
		}
	}
	// "is" 和 "tr" 都是真实的语言代码，但被当成语言吞掉会让人意外，
	// 所以故意没放进列表。
	for _, ambiguous := range []string{"is", "no", "it"} {
		_, ok := namedLanguage(ambiguous)
		if ambiguous == "is" && ok {
			t.Error(`"is" should stay text: it is also an English word`)
		}
	}
}

// 翻译中文又没指定目标语言时，不能再要求译成中文。
func TestHasHan(t *testing.T) {
	if !hasHan("今天天气不错") || !hasHan("mixed 中文 text") {
		t.Error("Chinese text should be detected")
	}
	if hasHan("hello world") || hasHan("こんにちは") || hasHan("") {
		t.Error("non-Han text should not be detected as Chinese")
	}
}

func TestLanguageName(t *testing.T) {
	if got := languageName("en"); got != "英语" {
		t.Fatalf("languageName(en) = %q", got)
	}
	if got := languageName("qqq"); got != "qqq" {
		t.Fatalf("an unknown code should render as itself, got %q", got)
	}
}
