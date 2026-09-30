package keyword

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dlclark/regexp2"

	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// task 是一条关键词任务。字段名和 MiBox v2 的 assets/keyword/config.json 一致，那边的文件
// 原样搬过来就能用。几个秒数存成浮点数：v2 用 JS 的 Number 解析，写过 1.5 这样的值也得读得进来。
type task struct {
	ID                int     `json:"id"`
	ChatID            string  `json:"chatId"`
	Key               string  `json:"key"`
	Response          string  `json:"response"`
	Include           bool    `json:"include"`
	Regexp            bool    `json:"regexp"`
	Exact             bool    `json:"exact"`
	CaseSensitive     bool    `json:"caseSensitive"`
	IgnoreForward     bool    `json:"ignoreForward"`
	Reply             bool    `json:"reply"`
	DeleteSource      bool    `json:"deleteSource"`
	BanSeconds        float64 `json:"banSeconds"`
	RestrictSeconds   float64 `json:"restrictSeconds"`
	DeleteReplyAfter  float64 `json:"deleteReplyAfter"`
	DeleteSourceAfter float64 `json:"deleteSourceAfter"`
}

// document 是 data/keyword.json。Aliases 把一个对话映射到它继承任务的那个对话。
// ImportedLegacy 是 v2 记「已并入 v1 的 SQLite 数据」的标记，这边用不到，只是原样留着。
type document struct {
	SchemaVersion  int               `json:"schemaVersion"`
	NextID         int               `json:"nextId"`
	Tasks          []task            `json:"tasks"`
	Aliases        map[string]string `json:"aliases"`
	ImportedLegacy bool              `json:"importedLegacy"`
}

func defaults() document {
	return document{SchemaVersion: 1, NextID: 1, Tasks: []task{}, Aliases: map[string]string{}}
}

// UnmarshalJSON 照 v2 的 normalizeTask 读文件：认 v1 留下的旧字段名（task_id、cid、msg、
// case、delete、ban、delay_delete……），缺字段的补默认值，缺编号、对话或文字的任务整条丢掉。
// v2 启动时已经规整过一遍，照理不会遇到旧字段名；这里再兜一次，是为了手改过或只跑过 v1 的文件
// 也不至于让整个文件读不进来——读不进来关键词就全都不生效了。
func (d *document) UnmarshalJSON(raw []byte) error {
	var loose struct {
		SchemaVersion  int                        `json:"schemaVersion"`
		NextID         json.RawMessage            `json:"nextId"`
		Tasks          []json.RawMessage          `json:"tasks"`
		Aliases        map[string]json.RawMessage `json:"aliases"`
		ImportedLegacy bool                       `json:"importedLegacy"`
	}
	if err := json.Unmarshal(raw, &loose); err != nil {
		return err
	}
	*d = defaults()
	if loose.SchemaVersion > 0 {
		d.SchemaVersion = loose.SchemaVersion
	}
	if next, ok := jsNumber(loose.NextID); ok && next >= 1 && next == math.Trunc(next) && next < math.MaxInt32 {
		d.NextID = int(next)
	}
	d.ImportedLegacy = loose.ImportedLegacy
	for _, item := range loose.Tasks {
		var value fields
		if json.Unmarshal(item, &value) != nil {
			continue
		}
		if normalized, ok := value.task(); ok {
			d.Tasks = append(d.Tasks, normalized)
		}
	}
	for from, to := range loose.Aliases {
		if target, ok := jsString(to); ok && target != "" {
			d.Aliases[from] = target
		}
	}
	return nil
}

// fields 是一条任务的原始字段，按 v2 normalizeTask 的规则取值。
type fields map[string]json.RawMessage

// first 取第一个出现、而且不是 null 的字段，同 JS 的 a ?? b。
func (f fields) first(names ...string) (json.RawMessage, bool) {
	for _, name := range names {
		if raw, ok := f[name]; ok && string(raw) != "null" {
			return raw, true
		}
	}
	return nil, false
}

// isTrue 是 value === true：只有 JSON 的 true 算。
func (f fields) isTrue(names ...string) bool {
	for _, name := range names {
		if string(f[name]) == "true" {
			return true
		}
	}
	return false
}

// notFalse 是 value !== false：没写也算开着。
func (f fields) notFalse(name string) bool { return string(f[name]) != "false" }

// seconds 是 Math.max(0, Number(a ?? b) || 0)。
func (f fields) seconds(names ...string) float64 {
	raw, ok := f.first(names...)
	if !ok {
		return 0
	}
	value, ok := jsNumber(raw)
	if !ok || value < 0 || math.IsInf(value, 0) {
		return 0
	}
	return value
}

func (f fields) task() (task, bool) {
	idRaw, ok := f.first("id", "task_id")
	if !ok {
		return task{}, false
	}
	id, ok := jsNumber(idRaw)
	if !ok || id < 1 || id != math.Trunc(id) || id > math.MaxInt32 {
		return task{}, false
	}
	chatRaw, ok := f.first("chatId", "cid")
	if !ok {
		return task{}, false
	}
	chatID, ok := jsString(chatRaw)
	if !ok {
		return task{}, false
	}
	key, ok := stringValue(f["key"])
	if !ok {
		return task{}, false
	}
	responseRaw, _ := f.first("response", "msg")
	response, ok := stringValue(responseRaw)
	if !ok {
		return task{}, false
	}
	return task{
		ID: int(id), ChatID: chatID, Key: key, Response: response,
		Include:           f.notFalse("include"),
		Regexp:            f.isTrue("regexp"),
		Exact:             f.isTrue("exact"),
		CaseSensitive:     f.isTrue("caseSensitive", "case"),
		IgnoreForward:     f.isTrue("ignoreForward", "ignore_forward"),
		Reply:             f.notFalse("reply"),
		DeleteSource:      f.isTrue("deleteSource", "delete"),
		BanSeconds:        f.seconds("banSeconds", "ban"),
		RestrictSeconds:   f.seconds("restrictSeconds", "restrict"),
		DeleteReplyAfter:  f.seconds("deleteReplyAfter", "delay_delete"),
		DeleteSourceAfter: f.seconds("deleteSourceAfter", "source_delay_delete"),
	}, true
}

// stringValue 要求值是 JSON 字符串（v2 的 typeof value === "string"），null 和缺失都不算。
func stringValue(raw json.RawMessage) (string, bool) {
	var text string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &text) != nil {
		return "", false
	}
	return text, true
}

// jsNumber 读一个数字或数字字符串（JS 的 Number 对后者也照样转换），其余都不算数。
func jsNumber(raw json.RawMessage) (float64, bool) {
	var number float64
	if json.Unmarshal(raw, &number) == nil {
		return number, true
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return 0, false
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, true
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) {
		return 0, false
	}
	return value, true
}

// integerLiteral 是一个 JSON 整数的写法。
var integerLiteral = regexp.MustCompile(`^-?\d+$`)

// jsString 同 JS 的 String(value)，只接受字符串和数字：v1 的对话 ID 存的是数字。
// 数字直接用 JSON 里的写法，-1001234567890 这样的大数转成浮点数再写回来会走样。
func jsString(raw json.RawMessage) (string, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, true
	}
	literal := strings.TrimSpace(string(raw))
	if integerLiteral.MatchString(literal) {
		return literal, true
	}
	if value, ok := jsNumber(raw); ok {
		return strconv.FormatFloat(value, 'f', -1, 64), true
	}
	return "", false
}

// v2 的正则在 worker 里跑，这几个上限照 SAFE_REGEXP_LIMITS：
// 正则最长 512 个字符、只匹配 4096 个字符以内的消息。执行时限 v2 是 50 毫秒，
// regexp2 的计时器精度是 100 毫秒，这里取 100 毫秒。
const (
	maxPatternLength = 512
	maxInputLength   = 4096
	matchTimeout     = 100 * time.Millisecond
)

// compilePattern 按 JavaScript 的语法编译正则。用 regexp2 而不是标准库：v2 的正则是 JS 的，
// 先行断言、反向引用这些 Go 的 RE2 不支持，换成标准库，搬过来的任务会悄悄失效。
// regexp2 会回溯，所以和 v2 一样设执行时限。
func compilePattern(pattern string, caseSensitive bool) (*regexp2.Regexp, error) {
	options := regexp2.RegexOptions(regexp2.ECMAScript)
	if !caseSensitive {
		options |= regexp2.IgnoreCase
	}
	re, err := regexp2.Compile(pattern, options)
	if err != nil {
		return nil, err
	}
	re.MatchTimeout = matchTimeout
	return re, nil
}

// maxDelay 是秒数的上限。v2 用 Node 的定时器，超过 24.8 天会立刻触发；Telegram 的封禁
// 超过 366 天就算永久。这里一律按 366 天封顶，免得换算成 time.Duration 时溢出。
const maxDelay = 366 * 24 * time.Hour

// duration 把存着的秒数换成时长。
func duration(seconds float64) time.Duration {
	if seconds <= 0 || math.IsNaN(seconds) {
		return 0
	}
	if seconds >= maxDelay.Seconds() {
		return maxDelay
	}
	return time.Duration(seconds * float64(time.Second))
}

// formatSeconds 同 JS 的 String(number)：整数不带小数点。
func formatSeconds(seconds float64) string {
	return strconv.FormatFloat(seconds, 'f', -1, 64)
}

// sections 把添加任务的正文按单独一行的 +++ 切开。v2 要求分隔符正好是「换行 +++ 换行」，
// 这里容许 +++ 前后带空格：手机上输入时常常多敲一个空格，结果整条任务报格式无效。
func sections(text string) []string {
	var parts []string
	var current []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "+++" {
			parts = append(parts, strings.Join(current, "\n"))
			current = nil
			continue
		}
		current = append(current, line)
	}
	return append(parts, strings.Join(current, "\n"))
}

// hasSeparator 判断正文是不是添加任务的格式（至少有一行 +++）。
func hasSeparator(text string) bool { return len(sections(text)) > 1 }

// parseTask 读添加任务的多行格式：依次是关键词、回复、匹配选项、动作、
// 回复几秒后删除、原消息几秒后删除，前两段必填。
//
// 和 v2 不同的几处：v2 任何一段为空都报格式无效，这里后四段空着就是用默认值；ban、restrict
// 不带秒数时 v2 当成 0 秒、实际什么也不做，这里直接报错；段数多于六段 v2 忽略多出来的，
// 这里报错。报错都说清楚是哪里不对，v2 只说「任务格式无效」。
func parseTask(text string) (task, error) {
	parts := sections(text)
	if len(parts) < 2 {
		return task{}, kit.Fail("任务格式不对：关键词和回复之间要有单独一行 +++")
	}
	if len(parts) > 6 {
		return task{}, kit.Fail("任务最多 6 段：关键词、回复、匹配选项、动作和两个删除秒数")
	}
	t := task{Key: strings.TrimSpace(parts[0]), Response: strings.TrimSpace(parts[1]), Include: true, Reply: true}
	if t.Key == "" {
		return task{}, kit.Fail("关键词不能为空")
	}
	if t.Response == "" {
		return task{}, kit.Fail("回复内容不能为空")
	}
	part := func(index int) string {
		if index < len(parts) {
			return strings.TrimSpace(parts[index])
		}
		return ""
	}
	for _, option := range strings.Fields(part(2)) {
		switch strings.ToLower(option) {
		case "include":
			t.Include = true
		case "exact":
			t.Include, t.Exact = false, true
		case "regexp":
			t.Regexp = true
		case "case":
			t.CaseSensitive = true
		case "ignore_forward":
			t.IgnoreForward = true
		default:
			return task{}, kit.Failf("不认识的匹配选项 %s，只能是 include、exact、regexp、case、ignore_forward", option)
		}
	}
	for _, action := range strings.Fields(part(3)) {
		lower := strings.ToLower(action)
		switch {
		case lower == "reply":
			t.Reply = true
		case lower == "delete":
			t.DeleteSource = true
		case strings.HasPrefix(lower, "restrict"):
			seconds, err := actionSeconds(lower, "restrict")
			if err != nil {
				return task{}, err
			}
			t.RestrictSeconds = seconds
		case strings.HasPrefix(lower, "ban"):
			seconds, err := actionSeconds(lower, "ban")
			if err != nil {
				return task{}, err
			}
			t.BanSeconds = seconds
		default:
			return task{}, kit.Failf("不认识的动作 %s，只能是 reply、delete、ban秒数、restrict秒数", action)
		}
	}
	for index, target := range []*float64{&t.DeleteReplyAfter, &t.DeleteSourceAfter} {
		value := part(4 + index)
		if value == "" {
			continue
		}
		seconds, err := strconv.ParseFloat(value, 64)
		if err != nil || seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
			return task{}, kit.Failf("第 %d 段要写不小于 0 的秒数，现在是 %s", 5+index, value)
		}
		*target = seconds
	}
	if t.Regexp {
		if kit.UTF16Len(t.Key) > maxPatternLength {
			return task{}, kit.Failf("正则表达式太长，最多 %d 个字符", maxPatternLength)
		}
		if _, err := compilePattern(t.Key, t.CaseSensitive); err != nil {
			return task{}, kit.Fail("正则表达式无效")
		}
	}
	return t, nil
}

// actionSeconds 读 ban3600、restrict600 这样的动作里的秒数，要求是正整数。
func actionSeconds(action, name string) (float64, error) {
	digits := strings.TrimPrefix(action, name)
	seconds, err := strconv.Atoi(digits)
	if err != nil || !kit.IsDigits(digits) || seconds <= 0 {
		return 0, kit.Failf("%s 后面要紧跟秒数，如 %s3600", name, name)
	}
	return float64(seconds), nil
}

// matches 照 v2 判断一条消息是否命中：正则按 JS 语法匹配；否则不区分大小写时两边都转小写，
// exact 要求整条一致，include 要求包含。include 和 exact 都没开（只有 v1 的旧数据会这样）时不命中。
func matches(t task, pattern *regexp2.Regexp, text string, forwarded bool) (bool, error) {
	if text == "" || (t.IgnoreForward && forwarded) {
		return false, nil
	}
	if t.Regexp {
		if pattern == nil || kit.UTF16Len(text) > maxInputLength {
			return false, nil
		}
		return pattern.MatchString(text)
	}
	key := t.Key
	if !t.CaseSensitive {
		text, key = strings.ToLower(text), strings.ToLower(key)
	}
	if t.Exact {
		return text == key, nil
	}
	return t.Include && strings.Contains(text, key), nil
}

// sender 是回复里变量要用到的发送者信息。
type sender struct {
	// id 是用户 ID 或频道 ID，没有发送者时为空。
	id   string
	name string
	// user 为真时 $mention 写成提及链接；以频道身份发言的没法提及，只写名字。
	user bool
}

// render 替换回复里的变量。回复本身是使用者写的 HTML，原样保留；替换进去的名字要转义。
// v2 用 String.replace，每个变量只替换第一次出现的；这里全部替换，而且一次替换完，
// 名字里恰好有 $code_id 这样的字也不会被再替换一遍。
func render(t task, from sender) string {
	mention := ""
	if from.id != "" {
		mention = command.Escape(from.name)
		if from.user {
			mention = `<a href="tg://user?id=` + from.id + `">` + mention + "</a>"
		}
	}
	delay := ""
	if t.DeleteReplyAfter > 0 {
		delay = formatSeconds(t.DeleteReplyAfter)
	}
	return strings.NewReplacer(
		"$mention", mention,
		"$code_id", command.Escape(from.id),
		"$code_name", command.Escape(from.name),
		"$delay_delete", delay,
	).Replace(t.Response)
}

// summary 是任务在列表里的附加说明：只列和默认不同的选项与动作。
func summary(t task) string {
	var items []string
	switch {
	case t.Regexp:
		items = append(items, "正则")
	case t.Exact:
		items = append(items, "整条一致")
	case !t.Include:
		items = append(items, "不匹配（旧数据）")
	}
	if t.CaseSensitive {
		items = append(items, "区分大小写")
	}
	if t.IgnoreForward {
		items = append(items, "忽略转发")
	}
	if !t.Reply {
		items = append(items, "不引用原消息")
	}
	if t.DeleteSource {
		if t.DeleteSourceAfter > 0 {
			items = append(items, formatSeconds(t.DeleteSourceAfter)+" 秒后删除原消息")
		} else {
			items = append(items, "删除原消息")
		}
	}
	if t.BanSeconds > 0 {
		items = append(items, "封禁 "+formatSeconds(t.BanSeconds)+" 秒")
	} else if t.RestrictSeconds > 0 {
		items = append(items, "禁言 "+formatSeconds(t.RestrictSeconds)+" 秒")
	}
	if t.DeleteReplyAfter > 0 {
		items = append(items, "回复 "+formatSeconds(t.DeleteReplyAfter)+" 秒后删除")
	}
	return strings.Join(items, " · ")
}
