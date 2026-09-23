package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// 翻译走的是 Chrome 自带翻译扩展所用的接口。它不需要 key，也不需要
// 账号，用它图的就是这个：.gt 已经能通过配置好的模型翻译，但在有人
// 配好模型并为它付费之前，.gt 是用不了的。
//
// 最常见的接口 translate.googleapis.com/translate_a/single，从机房 IP
// 请求会返回 429，这是实测结果，不是猜测。这个接口从同一台主机请求
// 能正常返回，而且用的是程序自己的 user agent，不必冒充浏览器。
const translateEndpoint = "https://clients5.google.com/translate_a/t?client=dict-chrome-ex"

// translateLimit 是可接受的最大输入长度。接口能处理更长的文本，但一条
// Telegram 消息本来就超不过约 4096 个字符，设个上限也能防止一条命令
// 发出一大篇文章。
const translateLimit = 5000

type translateConfig struct {
	// Target 是命令没有指定语言时的目标语言。
	Target string `json:"target"`
}

func translateDefaults() translateConfig { return translateConfig{Target: "zh-CN"} }

// languages 是第一个参数可以指定的语言代码。这里用手工挑选的列表，
// 而不是按模式匹配："is" 是冰岛语的代码，也是一个英文单词，按形状
// 判断就会吞掉 "tr is this correct" 的第一个词。这里的每一项都是
// 有意挑选的，其他的一律当作正文。
var languages = map[string]string{
	"ar": "阿拉伯语", "bg": "保加利亚语", "cs": "捷克语", "da": "丹麦语", "de": "德语",
	"el": "希腊语", "en": "英语", "es": "西班牙语", "fa": "波斯语", "fi": "芬兰语",
	"fr": "法语", "he": "希伯来语", "hi": "印地语", "hu": "匈牙利语", "id": "印尼语",
	"it": "意大利语", "ja": "日语", "ko": "韩语", "ms": "马来语", "nl": "荷兰语",
	"no": "挪威语", "pl": "波兰语", "pt": "葡萄牙语", "ro": "罗马尼亚语", "ru": "俄语",
	"sv": "瑞典语", "th": "泰语", "tr": "土耳其语", "uk": "乌克兰语", "vi": "越南语",
	"zh-CN": "简体中文", "zh-TW": "繁体中文",
}

// languageName 把语言代码转成显示用的名称，找不到时直接用代码。
func languageName(code string) string {
	if name, ok := languages[canonicalLanguage(code)]; ok {
		return name
	}
	return code
}

// canonicalLanguage 把用户输入的写法换成接口要求的代码。
func canonicalLanguage(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "zh", "cn", "zh-cn", "zh-hans", "chinese", "中文", "简体":
		return "zh-CN"
	case "tw", "zh-tw", "zh-hant", "繁体", "繁體":
		return "zh-TW"
	case "jp", "ja", "日语", "日文":
		return "ja"
	case "kr", "ko", "韩语":
		return "ko"
	case "en", "eng", "english", "英语", "英文":
		return "en"
	}
	lower := strings.ToLower(strings.TrimSpace(value))
	if _, ok := languages[lower]; ok {
		return lower
	}
	return value
}

// namedLanguage 返回第一个参数指定的目标语言，前提是它确实指定了一个。
func namedLanguage(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	code := canonicalLanguage(value)
	_, ok := languages[code]
	return code, ok
}

// hasHan 判断文本里是否含有汉字。
//
// 它决定没指定语言的翻译该往哪个方向走：把本来就是中文的文本翻成
// 中文，只会原样返回，这从来不是用户想要的。这个判断没有开销，
// 也不用多请求一次接口。
func hasHan(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// translateResult 是一次翻译的结果。
type translateResult struct {
	Text string
	// Source 是接口检测出的源语言；请求里指定了源语言时为空。
	Source string
}

// translateText 发一次请求。指定了源语言就原样传过去；为空时让接口
// 自动检测。
func translateText(ctx context.Context, text, target string) (*translateResult, error) {
	form := url.Values{"q": {text}}
	endpoint := translateEndpoint + "&sl=auto&tl=" + url.QueryEscape(target)
	response, err := httpx.Do(ctx, httpx.Request{
		Method:   "POST",
		URL:      endpoint,
		Headers:  map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		Body:     []byte(form.Encode()),
		Timeout:  30 * time.Second,
		MaxBytes: 4 << 20,
	})
	if err != nil {
		return nil, err
	}
	if response.Status == 429 {
		return nil, fail("翻译服务暂时拒绝了请求（429），请稍后重试")
	}
	if !response.OK() {
		return nil, failf("翻译服务返回 HTTP %d", response.Status)
	}
	return parseTranslation(response.Body)
}

// parseTranslation 解析接口返回的两种格式：请求指定了源语言时是
// ["译文"]，要求自动检测时是 [["译文","检测到的语言"]]。
func parseTranslation(raw []byte) (*translateResult, error) {
	var outer []json.RawMessage
	if err := json.Unmarshal(raw, &outer); err != nil || len(outer) == 0 {
		return nil, fail("翻译服务返回了无法解析的内容")
	}
	var plain string
	if err := json.Unmarshal(outer[0], &plain); err == nil {
		if strings.TrimSpace(plain) == "" {
			return nil, fail("翻译结果为空")
		}
		return &translateResult{Text: plain}, nil
	}
	var pair []string
	if err := json.Unmarshal(outer[0], &pair); err != nil || len(pair) == 0 {
		return nil, fail("翻译服务返回了无法解析的内容")
	}
	if strings.TrimSpace(pair[0]) == "" {
		return nil, fail("翻译结果为空")
	}
	result := &translateResult{Text: pair[0]}
	if len(pair) > 1 {
		result.Source = pair[1]
	}
	return result, nil
}

func translateHelp(prefix string) string {
	p := command.Escape(prefix)
	var codes []string
	for code := range languages {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return "🌐 <b>翻译</b>\n\n直接调用谷歌翻译，不需要任何配置或 API Key。\n\n• <code>" + p +
		"tr 文本</code> 翻译。中文译成英文，其他语言译成中文\n• <code>" + p + "tr en 文本</code> 指定目标语言\n• 回复一条消息发 <code>" + p +
		"tr</code> 翻译那条消息\n• 回复消息发 <code>" + p + "tr ja</code> 译成指定语言\n• <code>" + p +
		"tr set 语言</code> 设置默认目标语言\n\n<b>语言代码</b>\n<blockquote expandable>" +
		command.Escape(strings.Join(codes, " ")) + "</blockquote>\n单次最多 " +
		command.Code(fmt.Sprint(translateLimit)) + " 字符，长译文自动分段。"
}

// Translate 注册 .tr。
func Translate(a *app.App) {
	settings := newStore(a, "translate.json", translateDefaults)
	a.Registry.Register(&command.Command{
		Name: "tr", Description: "谷歌翻译，无需配置", Usage: "[语言] 文本",
		Help: translateHelp, Timeout: 2 * time.Minute,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			err := runTranslate(ctx, inv, settings)
			if err == nil || ctx.Err() != nil {
				return err
			}
			if detail, ok := isUserError(err); ok {
				return inv.EditText(ctx, "❌ "+detail)
			}
			inv.Log.Error("tr.failed", "error", err.Error())
			return inv.EditText(ctx, "❌ 翻译失败："+httpx.Reason(err))
		}})
}

func runTranslate(ctx context.Context, inv *command.Invocation, settings *store.Store[translateConfig]) error {
	first := strings.ToLower(inv.Arg(0))
	if first == "help" || first == "h" {
		return inv.Edit(ctx, translateHelp(inv.Prefix))
	}
	config, err := settings.Read()
	if err != nil {
		return err
	}
	if config.Target == "" {
		config.Target = "zh-CN"
	}

	if first == "set" {
		code, ok := namedLanguage(inv.Arg(1))
		if !ok {
			return failf("不认识的语言代码：%s。可用代码见 %str help", inv.Arg(1), inv.Prefix)
		}
		if err := settings.Update(func(c *translateConfig) error { c.Target = code; return nil }); err != nil {
			return err
		}
		return inv.Edit(ctx, feedback("success", "默认目标语言已设置", languageName(code)+"（"+code+"）"))
	}

	// 指定了语言就占掉第一个参数；否则每个词都是正文。
	target, named := namedLanguage(first)
	rest := 0
	if named {
		rest = 1
	}
	text := inv.Rest(rest)
	if strings.TrimSpace(text) == "" {
		reply, err := inv.Client.GetReply(ctx, inv.Message)
		if err != nil {
			return err
		}
		if reply != nil {
			text = reply.Text
		}
	}
	if strings.TrimSpace(text) == "" {
		return inv.Edit(ctx, translateHelp(inv.Prefix))
	}
	if utf16Len(text) > translateLimit {
		return failf("文本过长，请保持在 %d 字符以内", translateLimit)
	}
	if !named {
		// 中文翻成中文只会原样返回，所以没指定语言时就反过来翻。
		target = config.Target
		if hasHan(text) && canonicalLanguage(target) == "zh-CN" {
			target = "en"
		}
	}

	if err := inv.Edit(ctx, feedback("working", "翻译中", "")); err != nil {
		return err
	}
	result, err := translateText(ctx, text, target)
	if err != nil {
		return err
	}

	source := "自动检测"
	if result.Source != "" {
		source = languageName(result.Source)
	}
	preview := truncateRunes(text, 60)
	suffix := ""
	if len([]rune(text)) > 60 {
		suffix = "…"
	}
	pages := command.EscapedPages(result.Text, 3400)
	pages[0] = "🌐 <b>翻译</b>（" + command.Escape(source) + " → " + command.Escape(languageName(target)) +
		"）\n\n<b>原文:</b>\n<code>" + command.Escape(preview) + suffix + "</code>\n\n<b>译文:</b>\n" + pages[0]
	return sendPages(ctx, inv, pages)
}
