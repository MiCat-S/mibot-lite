// Package acn 实现 .acn（全称 .autochangename）：自动更新昵称，可带时间、时区和天气。
package acn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// acnID 是账号 id。MiBox 的 v1 插件把它写成数字，v2 写成字符串，两种都要能读；
// 写出时一律是字符串。
type acnID string

// UnmarshalJSON 接受字符串、数字和 null。
func (id *acnID) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	switch {
	case string(trimmed) == "null":
		*id = ""
		return nil
	case len(trimmed) > 0 && trimmed[0] == '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		*id = acnID(text)
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(trimmed, &number); err != nil {
		return err
	}
	*id = acnID(number.String())
	return nil
}

// acnUser 是一个账号的动态昵称设置。JSON 字段名与 MiBox 的
// autochangename.json 一致，导入的文件可以原样读取。
type acnUser struct {
	UserID            acnID  `json:"user_id"`
	Timezone          string `json:"timezone"`
	OriginalFirstName string `json:"original_first_name"`
	OriginalLastName  string `json:"original_last_name"`
	Enabled           bool   `json:"is_enabled"`
	Mode              string `json:"mode"`
	LastUpdate        string `json:"last_update"`
	TextIndex         int    `json:"text_index"`
	ShowClockEmoji    bool   `json:"show_clock_emoji"`
	ShowTime          *bool  `json:"show_time"`
	// HourFormat 是 "12" 或 "24"，来自 MiBox v2 的 acn time 12|24。
	HourFormat     string `json:"hour_format,omitempty"`
	ShowTimezone   bool   `json:"show_timezone"`
	TimezoneFormat string `json:"timezone_format"`
	DisplayOrder   string `json:"display_order"`
	// DisplayComponents 是 acn show 选定的组件。nil 表示没选过，文件里不写这个字段；
	// 空列表照样写成 []。拼昵称时两者都按不限定处理，与 MiBox 一致。
	DisplayComponents []string `json:"displayComponents,omitzero"`
	TextStyle         string   `json:"text_style"`
	WeatherEnabled    bool     `json:"weather_enabled"`
	WeatherLocation   string   `json:"weather_location"`
	WeatherCompact    string   `json:"weather_compact"`
	WeatherCacheTS    int64    `json:"weather_cache_ts"`

	// extra 是本程序不认识的字段。写回时原样带上，免得切回 MiBox 时丢设置。
	extra map[string]json.RawMessage
}

// acnUserKeys 是 acnUser 认识的 JSON 字段名。
var acnUserKeys = func() map[string]bool {
	keys := map[string]bool{}
	fields := reflect.TypeOf(acnUser{})
	for index := 0; index < fields.NumField(); index++ {
		name, _, _ := strings.Cut(fields.Field(index).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			keys[name] = true
		}
	}
	return keys
}()

// UnmarshalJSON 读出认识的字段，其余字段留在 extra 里。
func (u *acnUser) UnmarshalJSON(data []byte) error {
	type plain acnUser
	var known plain
	if err := json.Unmarshal(data, &known); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	for key := range all {
		if acnUserKeys[key] {
			delete(all, key)
		}
	}
	known.extra = nil
	if len(all) > 0 {
		known.extra = all
	}
	*u = acnUser(known)
	return nil
}

// MarshalJSON 写出认识的字段，再按键名顺序接上 extra 里的字段。
func (u acnUser) MarshalJSON() ([]byte, error) {
	type plain acnUser
	encoded, err := json.Marshal(plain(u))
	if err != nil || len(u.extra) == 0 {
		return encoded, err
	}
	keys := make([]string, 0, len(u.extra))
	for key := range u.extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out bytes.Buffer
	out.Write(encoded[:len(encoded)-1])
	for _, key := range keys {
		name, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		out.WriteByte(',')
		out.Write(name)
		out.WriteByte(':')
		out.Write(u.extra[key])
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

type acnState struct {
	SchemaVersion int                 `json:"schemaVersion"`
	Users         map[string]*acnUser `json:"users"`
	RandomTexts   []string            `json:"random_texts"`
}

type acnService struct {
	store *store.Store[acnState]
}

// acnDefaultOrder 是没设过顺序时的顺序，与 MiBox 新建用户时写入的一致。
// 顺序里没列出的组件按 acnComponents 的次序接在后面，所以实际是
// name,time,text,weather,emoji,timezone。
const acnDefaultOrder = "name,time"

var acnComponents = []string{"name", "text", "time", "weather", "emoji", "timezone"}

var acnStyles = []string{"normal", "italic", "double", "sans", "mono", "outline"}

// showDefaults 是 acn show 在各模式下默认显示的组件。
var showDefaults = map[string][]string{"time": {"time"}, "text": {"text", "time"}, "both": {"text", "time"}}

func acnDefaults() acnState {
	return acnState{SchemaVersion: 1, Users: map[string]*acnUser{}, RandomTexts: []string{}}
}

// newAcnUser 是第一次 acn save 时建立的设置，默认值与 MiBox v2 相同。
func newAcnUser(userID string) *acnUser {
	showTime := true
	return &acnUser{UserID: acnID(userID), Timezone: "Asia/Shanghai", Mode: "time", ShowTime: &showTime,
		HourFormat: "24", TimezoneFormat: "GMT", DisplayOrder: acnDefaultOrder, TextStyle: "normal"}
}

func (u *acnUser) showTime() bool { return u.ShowTime == nil || *u.ShowTime }

func validMode(mode string) bool { return mode == "time" || mode == "text" || mode == "both" }

// validZone 判断一个时区标识符能否加载。
func validZone(zone string) bool {
	if strings.TrimSpace(zone) == "" {
		return false
	}
	_, err := time.LoadLocation(zone)
	return err == nil
}

// cleanComponents 只留认识的组件并去重；nil 保持 nil。
func cleanComponents(components []string) []string {
	if components == nil {
		return nil
	}
	cleaned := []string{}
	for _, component := range components {
		if slices.Contains(acnComponents, component) && !slices.Contains(cleaned, component) {
			cleaned = append(cleaned, component)
		}
	}
	return cleaned
}

// normalizeUser 规整从文件读到的一份设置，规则与 MiBox v2 的 normalizeUser 相同。
func normalizeUser(id string, user *acnUser) {
	user.UserID = acnID(id)
	if !validZone(user.Timezone) {
		user.Timezone = "Asia/Shanghai"
	}
	if !validMode(user.Mode) {
		user.Mode = "time"
	}
	if user.HourFormat != "12" {
		user.HourFormat = "24"
	}
	if !slices.Contains(acnStyles, user.TextStyle) {
		user.TextStyle = "normal"
	}
	if user.TextIndex < 0 {
		user.TextIndex = 0
	}
	if user.OriginalFirstName == "" {
		user.Enabled = false
	}
	user.DisplayComponents = cleanComponents(user.DisplayComponents)
}

// clockEmoji 返回该时区在 now 这一刻的钟点对应的 🕐 系列钟面表情。
func clockEmoji(zone string, now time.Time) string {
	location, err := time.LoadLocation(zone)
	if err != nil {
		return ""
	}
	hour := now.In(location).Hour() % 12
	return string(rune(0x1f550 + (hour+11)%12))
}

// zoneTime 按 12 或 24 小时制显示该时区在 now 这一刻的时间，如 "02:32 PM" 或 "14:32"。
func zoneTime(zone, hourFormat string, now time.Time) string {
	location, err := time.LoadLocation(zone)
	if err != nil {
		return ""
	}
	if hourFormat == "12" {
		return now.In(location).Format("03:04 PM")
	}
	return now.In(location).Format("15:04")
}

// styleRanges 给出每种文字样式下，各类字符要平移到的 Unicode 区段。
var styleRanges = map[string][3]rune{
	"italic":  {0x1D7CE, 0x1D400, 0x1D41A},
	"double":  {0x1D7D8, 0, 0x1D552},
	"sans":    {0x1D7EC, 0x1D5D4, 0x1D5EE},
	"mono":    {0x1D7F6, 0x1D670, 0x1D68A},
	"outline": {0x1D7E2, 0x1D5A0, 0x1D5BA},
}

// doubleStruckUpper 收录不在连续区段里的那几个双线体大写字母。
var doubleStruckUpper = map[rune]rune{'C': 'ℂ', 'H': 'ℍ', 'N': 'ℕ', 'P': 'ℙ', 'Q': 'ℚ', 'R': 'ℝ', 'Z': 'ℤ'}

func applyTextStyle(text, style string) string {
	ranges, ok := styleRanges[strings.ToLower(strings.TrimSpace(style))]
	if !ok || text == "" {
		return text
	}
	var b strings.Builder
	for _, r := range text {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(ranges[0] + (r - '0'))
		case r >= 'A' && r <= 'Z':
			if style == "double" {
				if mapped, ok := doubleStruckUpper[r]; ok {
					b.WriteRune(mapped)
					continue
				}
				b.WriteRune(0x1D538 + (r - 'A'))
				continue
			}
			b.WriteRune(ranges[1] + (r - 'A'))
		case r >= 'a' && r <= 'z':
			b.WriteRune(ranges[2] + (r - 'a'))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// composeName 按设置拼出新昵称，规则与 MiBox v2 的 updateUser 相同：
//   - 时间在任何模式下都显示，只要没关掉时间显示；
//   - 文案在 text/both 模式下显示；
//   - acn show 选定了组件时，时间和文案只在选中时显示；
//   - 时钟表情、时区、天气各由自己的开关决定；
//   - 顺序按 display_order，没列出的组件按 acnComponents 的次序接在后面，空的跳过。
//
// weather 是已经取好的天气文字。
func composeName(user *acnUser, texts []string, weather string, now time.Time) string {
	selected := map[string]bool{}
	for _, component := range user.DisplayComponents {
		selected[component] = true
	}
	includes := func(component string) bool { return len(selected) == 0 || selected[component] }
	parts := map[string]string{"name": user.OriginalFirstName, "weather": weather}
	if (user.Mode == "text" || user.Mode == "both" || selected["text"]) && includes("text") && len(texts) > 0 {
		index := user.TextIndex % len(texts)
		if index < 0 {
			index += len(texts)
		}
		parts["text"] = texts[index]
	}
	if user.showTime() && includes("time") {
		parts["time"] = zoneTime(user.Timezone, user.HourFormat, now)
	}
	if user.ShowClockEmoji {
		parts["emoji"] = clockEmoji(user.Timezone, now)
	}
	if user.ShowTimezone {
		parts["timezone"] = zoneLabel(user.Timezone, user.TimezoneFormat, now)
	}

	order := user.DisplayOrder
	if strings.TrimSpace(order) == "" {
		order = acnDefaultOrder
	}
	var pieces []string
	seen := map[string]bool{}
	for _, key := range append(strings.Split(order, ","), acnComponents...) {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		value := parts[key]
		if value == "" {
			continue
		}
		if key != "name" {
			value = applyTextStyle(value, user.TextStyle)
		}
		pieces = append(pieces, value)
	}
	return strings.Join(pieces, " ")
}

// withComponent 在顺序里加上或去掉一个组件：开启时接到末尾，关闭时拿掉。
// 顺序为空时从默认顺序算起，与 MiBox v2 的 updateOrderComponent 相同。
func withComponent(order, component string, enabled bool) string {
	if strings.TrimSpace(order) == "" {
		order = acnDefaultOrder
	}
	var parts []string
	present := false
	for _, part := range strings.Split(order, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == component {
			present = true
			if !enabled {
				continue
			}
		}
		parts = append(parts, part)
	}
	if enabled && !present {
		parts = append(parts, component)
	}
	return strings.Join(parts, ",")
}

// applyOrder 设置顺序，并让各开关与顺序一致：列出的时间、时钟表情、时区打开，
// 没列出的关掉；列出了天气就打开天气。与 MiBox 的 acn order 相同。
func applyOrder(user *acnUser, components []string) {
	user.DisplayOrder = strings.Join(components, ",")
	showTime := slices.Contains(components, "time")
	user.ShowTime = &showTime
	user.ShowClockEmoji = slices.Contains(components, "emoji")
	user.ShowTimezone = slices.Contains(components, "timezone")
	if slices.Contains(components, "weather") {
		user.WeatherEnabled = true
	}
}

// toggleShown 在 acn show 的组件列表里开启或关闭一个组件；还没选过时从当前模式的默认值算起。
func toggleShown(user *acnUser, component string, on bool) {
	current := slices.Clone(user.DisplayComponents)
	if current == nil {
		current = slices.Clone(showDefaults[user.Mode])
	}
	if on {
		if !slices.Contains(current, component) {
			current = append(current, component)
		}
	} else {
		current = slices.DeleteFunc(current, func(value string) bool { return value == component })
	}
	if current == nil {
		current = []string{}
	}
	user.DisplayComponents = current
	if component == "weather" {
		user.WeatherEnabled = on
	}
}

// acnCities 把常见的中文城市名换成地理编码接口认识的名字。
var acnCities = map[string]string{"北京": "Beijing", "上海": "Shanghai", "广州": "Guangzhou", "深圳": "Shenzhen",
	"成都": "Chengdu", "杭州": "Hangzhou", "武汉": "Wuhan", "西安": "Xi'an", "重庆": "Chongqing", "南京": "Nanjing",
	"天津": "Tianjin", "苏州": "Suzhou", "长沙": "Changsha", "郑州": "Zhengzhou", "青岛": "Qingdao", "大连": "Dalian",
	"厦门": "Xiamen", "香港": "Hong Kong", "澳门": "Macau", "台北": "Taipei", "东京": "Tokyo", "大阪": "Osaka",
	"京都": "Kyoto", "首尔": "Seoul", "曼谷": "Bangkok", "新加坡": "Singapore", "吉隆坡": "Kuala Lumpur",
	"雅加达": "Jakarta", "伦敦": "London", "巴黎": "Paris", "柏林": "Berlin", "罗马": "Rome", "纽约": "New York",
	"洛杉矶": "Los Angeles", "旧金山": "San Francisco", "芝加哥": "Chicago", "多伦多": "Toronto", "悉尼": "Sydney", "墨尔本": "Melbourne"}

var weatherIcons = map[int]string{0: "☀️", 1: "🌤️", 2: "⛅", 3: "☁️", 45: "🌫️", 48: "🌫️", 51: "🌦️", 53: "🌦️",
	55: "🌧️", 56: "🌨️", 57: "🌨️", 61: "🌧️", 63: "🌧️", 65: "🌧️", 66: "🌨️", 67: "🌨️", 71: "❄️", 73: "❄️",
	75: "❄️", 77: "🌨️", 80: "🌦️", 81: "🌧️", 82: "⛈️", 85: "🌨️", 86: "🌨️", 95: "⛈️", 96: "⛈️", 99: "⛈️"}

// roundHalfUp 与 JavaScript 的 Math.round 一致：恰好一半时往正无穷方向进位，
// 负数也一样（-2.5 → -2）。int(x+0.5) 会把负数往零截断，-3.2 得到 -2。
func roundHalfUp(value float64) float64 {
	floor := math.Floor(value)
	if value-floor >= 0.5 {
		return floor + 1
	}
	return floor
}

// formatWeather 把天气代码和气温排成昵称里的样子，如 "☀️ 21°C"。
func formatWeather(code int, temperature float64) string {
	icon, ok := weatherIcons[code]
	if !ok {
		icon = "🌤️"
	}
	return icon + " " + strconv.Itoa(int(roundHalfUp(temperature))) + "°C"
}

// fetchWeather 读取当前天气，任何一步失败都返回 ""。
func fetchWeather(ctx context.Context, location string) string {
	city := strings.TrimSpace(location)
	if mapped, ok := acnCities[city]; ok {
		city = mapped
	}
	var geo struct {
		Results []struct {
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		} `json:"results"`
	}
	if err := httpx.GetJSON(ctx, "https://geocoding-api.open-meteo.com/v1/search?count=1&language=zh&format=json&name="+url.QueryEscape(city),
		10*time.Second, 256<<10, &geo); err != nil || len(geo.Results) == 0 {
		return ""
	}
	var forecast struct {
		Current struct {
			Temperature *float64 `json:"temperature_2m"`
			Code        *int     `json:"weather_code"`
		} `json:"current"`
	}
	endpoint := "https://api.open-meteo.com/v1/forecast?current=temperature_2m,weather_code&timezone=auto" +
		"&latitude=" + strconv.FormatFloat(geo.Results[0].Latitude, 'f', 4, 64) +
		"&longitude=" + strconv.FormatFloat(geo.Results[0].Longitude, 'f', 4, 64)
	if err := httpx.GetJSON(ctx, endpoint, 10*time.Second, 256<<10, &forecast); err != nil ||
		forecast.Current.Temperature == nil || forecast.Current.Code == nil {
		return ""
	}
	return formatWeather(*forecast.Current.Code, *forecast.Current.Temperature)
}

// weatherCacheFresh 判断缓存的天气还能不能用：取到过天气的缓存 30 分钟，
// 没取到（地点查不到、接口出错）的也记下来，5 分钟内不再重试。
func weatherCacheFresh(user *acnUser, now time.Time) bool {
	if user.WeatherCacheTS <= 0 {
		return false
	}
	ttl := 5 * time.Minute
	if user.WeatherCompact != "" {
		ttl = 30 * time.Minute
	}
	return now.Sub(time.UnixMilli(user.WeatherCacheTS)) < ttl
}

// currentWeather 返回要显示的天气。缓存可用就用缓存，否则现取；fetched 为真表示
// 这次是现取的，结果不论成败都要写回缓存。includeDisabled 为真时，天气关着也取（给预览用）。
func currentWeather(ctx context.Context, user *acnUser, includeDisabled bool, now time.Time) (string, bool) {
	if (!includeDisabled && !user.WeatherEnabled) || user.WeatherLocation == "" {
		return "", false
	}
	if weatherCacheFresh(user, now) {
		return user.WeatherCompact, false
	}
	text := fetchWeather(ctx, user.WeatherLocation)
	return text, ctx.Err() == nil
}

// apply 为一个账号重新拼出昵称并提交上去。
func (s *acnService) apply(ctx context.Context, client *bot.Client, userID string, force bool) (bool, error) {
	state, err := s.store.Read()
	if err != nil {
		return false, err
	}
	user := state.Users[userID]
	if user == nil || user.OriginalFirstName == "" || (!force && !user.Enabled) {
		return false, nil
	}
	if !force && user.LastUpdate != "" {
		if when, err := time.Parse(time.RFC3339, user.LastUpdate); err == nil && time.Since(when) < 30*time.Second {
			return false, nil
		}
	}
	now := time.Now()
	weather, fetched := currentWeather(ctx, user, false, now)
	firstName := kit.TruncateRunes(composeName(user, state.RandomTexts, weather, now), 64)

	request := &tg.AccountUpdateProfileRequest{}
	request.SetFirstName(firstName)
	if user.OriginalLastName != "" {
		request.SetLastName(user.OriginalLastName)
	}
	if _, err := client.API().AccountUpdateProfile(ctx, request); err != nil {
		if tgerr.Is(err, "USERNAME_NOT_MODIFIED") {
			return true, nil
		}
		if _, flood := tgerr.AsFloodWait(err); flood {
			// 在这里被限流，说明这个账号改资料改得太频繁了；
			// 与其一再触发限流，不如直接停掉。
			_ = s.store.Update(func(state *acnState) error {
				if current := state.Users[userID]; current != nil {
					current.Enabled = false
				}
				return nil
			})
			client.Logger().Warn("acn.flood_disabled", slog.String("user", userID))
		}
		return false, err
	}
	textCount := len(state.RandomTexts)
	return true, s.store.Update(func(state *acnState) error {
		current := state.Users[userID]
		if current == nil {
			return nil
		}
		current.LastUpdate = time.Now().UTC().Format(time.RFC3339)
		if fetched {
			current.WeatherCompact, current.WeatherCacheTS = weather, now.UnixMilli()
		}
		if textCount > 0 && current.Mode != "time" {
			current.TextIndex = (current.TextIndex + 1) % textCount
		}
		return nil
	})
}

// restore 把保存下来的原始昵称换回去。
func (s *acnService) restore(ctx context.Context, client *bot.Client, user *acnUser) error {
	request := &tg.AccountUpdateProfileRequest{}
	request.SetFirstName(user.OriginalFirstName)
	request.SetLastName(user.OriginalLastName)
	_, err := client.API().AccountUpdateProfile(ctx, request)
	if err != nil && tgerr.Is(err, "USERNAME_NOT_MODIFIED") {
		return nil
	}
	return err
}

func acnHelp(prefix string) string {
	p := command.Escape(prefix)
	return "🤖 <b>自动昵称更新</b>\n\n<b>快速开始</b>\n1. <code>" + p + "acn save</code> 保存当前昵称（首次必须）\n2. <code>" + p +
		"acn on</code> 开启，之后每分钟自动更新\n\n<b>基础</b>\n• <code>" + p + "acn on</code> / <code>off</code> 开关\n• <code>" + p +
		"acn mode</code> 循环切换 time → text → both\n• <code>" + p + "acn update</code> 立即更新一次\n• <code>" + p +
		"acn reset</code> 恢复原始昵称并停用\n• <code>" + p + "acn status</code> / <code>config</code> 查看状态\n\n<b>时区</b>\n• <code>" + p +
		"acn tz Asia/Shanghai</code> 设置时区\n• <code>" + p + "acn tz on</code> / <code>off</code> 是否显示时区\n• <code>" + p +
		"acn tz format GMT|UTC|simp|offset|custom:文字</code>\n\n<b>外观</b>\n• <code>" + p + "acn emoji on</code> / <code>off</code> 时钟表情\n• <code>" + p +
		"acn time on</code> / <code>off</code> 时间显示\n• <code>" + p + "acn time 12</code> / <code>24</code> 12 或 24 小时制\n• <code>" + p +
		"acn style normal|italic|double|sans|mono|outline</code>\n• <code>" + p +
		"acn order name,text,time,weather,emoji,timezone</code>（同时开关其中的时间、表情、时区）\n• <code>" + p +
		"acn show time|text|weather on</code> / <code>off</code> 只显示选中的组件，<code>" + p + "acn show reset</code> 恢复\n\n<b>文案</b>\n• <code>" + p +
		"acn text add 摸鱼中</code>（支持多行）\n• <code>" + p +
		"acn text list</code> / <code>del 序号</code> / <code>clear</code>\n• <code>" + p + "acn text on</code> / <code>off</code>\n\n<b>天气</b>\n• <code>" + p +
		"acn weather set 北京</code> 设置地点并开启\n• <code>" + p + "acn weather on</code> / <code>off</code>\n天气缓存 30 分钟。"
}

// Register 注册 .acn 以及每分钟刷新一次的后台任务。
func Register(a *app.App) {
	service := &acnService{store: kit.NewStore(a, "acn.json", acnDefaults)}
	// MiBox 写出的文件可能带着无法识别的时区，也可能缺少 users 表；
	// 启动时统一规整一次，而不是每次定时触发都做。
	_ = service.store.Update(func(state *acnState) error {
		state.SchemaVersion = 1
		if state.Users == nil {
			state.Users = map[string]*acnUser{}
		}
		for id, user := range state.Users {
			if user == nil {
				delete(state.Users, id)
				continue
			}
			normalizeUser(id, user)
		}
		if len(state.RandomTexts) > 100 {
			state.RandomTexts = state.RandomTexts[:100]
		}
		return nil
	})

	handle := func(ctx context.Context, inv *command.Invocation) error { return acnHandle(ctx, inv, service) }
	a.Registry.Register(
		&command.Command{Name: "acn", Description: "管理动态昵称", Usage: "save|on|off|mode|tz|text|show|time|weather|update|reset|status", Help: acnHelp, Handle: handle},
		&command.Command{Name: "autochangename", Description: "acn 的全称", Hidden: true, Help: acnHelp, Handle: handle},
	)

	a.Registry.AddJob(func(ctx context.Context, client *bot.Client) {
		// 在整分钟触发，这样显示的时间和真实时钟同时跳变，
		// 而不是晚上随机的几秒。
		for {
			now := time.Now()
			if err := kit.Sleep(ctx, now.Truncate(time.Minute).Add(time.Minute).Sub(now)); err != nil {
				return
			}
			state, err := service.store.Read()
			if err != nil {
				continue
			}
			for id, user := range state.Users {
				if user == nil || !user.Enabled {
					continue
				}
				if _, err := service.apply(ctx, client, id, false); err != nil && ctx.Err() == nil {
					client.Logger().Warn("acn.update_failed", slog.String("user", id), slog.String("error", err.Error()))
				}
			}
		}
	})
}

// acnCall 是一次 .acn 调用要用到的东西：谁在改、改的是哪一份配置、当前配置是什么。
type acnCall struct {
	ctx     context.Context
	inv     *command.Invocation
	service *acnService
	state   acnState
	userID  string
	user    *acnUser
	sub     string
}

// mutate 修改自己那一份配置并存盘，返回改好的设置。
func (c *acnCall) mutate(apply func(*acnUser)) (acnUser, error) {
	var updated acnUser
	err := c.service.store.Update(func(state *acnState) error {
		if current := state.Users[c.userID]; current != nil {
			apply(current)
			updated = *current
		}
		return nil
	})
	return updated, err
}

// change 修改设置；自动更新开着的话，立刻按新设置改一次名字，不用等到下一分钟。
// 这一次改名失败只记日志，设置照样生效，下一分钟还会再试。
func (c *acnCall) change(apply func(*acnUser)) (acnUser, error) {
	updated, err := c.mutate(apply)
	if err != nil || !updated.Enabled {
		return updated, err
	}
	if _, err := c.service.apply(c.ctx, c.inv.Client, c.userID, true); err != nil && c.ctx.Err() == nil {
		c.inv.Log.Warn("acn.refresh_failed", slog.String("error", err.Error()))
	}
	return updated, nil
}

func acnStatus(ctx context.Context, inv *command.Invocation, state acnState) error {
	enabled := 0
	for _, user := range state.Users {
		if user != nil && user.Enabled {
			enabled++
		}
	}
	running := "已停止"
	if enabled > 0 {
		running = "运行中"
	}
	return inv.Edit(ctx, "📊 自动更新: "+command.Code(running)+"\n启用用户: "+command.Code(strconv.Itoa(enabled)))
}

// currentSelf 现查一次自己的资料。连接时缓存的那份不会随着在 Telegram 里改名而更新，
// 保存「原始昵称」必须用现在的名字。
func currentSelf(ctx context.Context, client *bot.Client) (*tg.User, error) {
	users, err := client.API().UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}})
	if err != nil {
		return nil, err
	}
	for _, entry := range users {
		if user, ok := entry.(*tg.User); ok {
			return user, nil
		}
	}
	return nil, errors.New("users.getUsers 没有返回自己的资料")
}

// acnSave 记下现在的名字，作为以后恢复用的「原始昵称」。其他子命令都要先有它。
// 第一次保存和之后更新回复不同：更新只换名字，时区、样式这些设置都保留。
func acnSave(ctx context.Context, inv *command.Invocation, service *acnService, userID string) error {
	self, err := currentSelf(ctx, inv.Client)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return inv.EditText(ctx, "❌ 读取当前昵称失败："+kit.RPCCode(err))
	}
	first := false
	var saved acnUser
	if err := service.store.Update(func(state *acnState) error {
		current := state.Users[userID]
		if current == nil {
			current = newAcnUser(userID)
			state.Users[userID] = current
		}
		first = current.OriginalFirstName == ""
		current.OriginalFirstName = cleanNickname(self.FirstName)
		current.OriginalLastName = cleanNickname(self.LastName)
		if current.Timezone == "" {
			current.Timezone = "Asia/Shanghai"
		}
		saved = *current
		return nil
	}); err != nil {
		return err
	}
	names := "姓名：" + command.Code(saved.OriginalFirstName) + "\n姓氏：" + command.Code(kit.OrDefault(saved.OriginalLastName, "(空)"))
	if first {
		return inv.Edit(ctx, "🎉 <b>昵称已保存</b>\n\n"+names+"\n\n接下来用 "+command.Code(inv.Prefix+"acn on")+" 开启自动更新。")
	}
	return inv.Edit(ctx, "✅ <b>原始昵称已更新</b>，其他设置保留\n\n"+names)
}

// toggle 开启或关闭自动改名。开启时立刻改一次；关闭时换回原始昵称。
func (c *acnCall) toggle() error {
	enabled := c.sub == "on" || c.sub == "enable"
	if _, err := c.mutate(func(user *acnUser) { user.Enabled = enabled }); err != nil {
		return err
	}
	if enabled {
		if _, err := c.service.apply(c.ctx, c.inv.Client, c.userID, true); err != nil {
			return c.inv.EditText(c.ctx, "❌ 启用后首次更新失败："+kit.RPCCode(err))
		}
		return c.inv.EditText(c.ctx, "✅ 动态昵称已启用")
	}
	if err := c.service.restore(c.ctx, c.inv.Client, c.user); err != nil {
		return c.inv.EditText(c.ctx, "❌ 恢复原始昵称失败："+kit.RPCCode(err))
	}
	return c.inv.EditText(c.ctx, "✅ 动态昵称已禁用")
}

// mode 在 time → text → both 之间轮换。
func (c *acnCall) mode() error {
	next := map[string]string{"time": "text", "text": "both", "both": "time"}[c.user.Mode]
	if next == "" {
		next = "time"
	}
	if _, err := c.change(func(user *acnUser) { user.Mode = next }); err != nil {
		return err
	}
	return c.inv.Edit(c.ctx, "✅ 显示模式: "+command.Code(next))
}

// timezone 处理 tz 的几种写法：不带参数看说明，list、on/off、format，其余当成要设的时区。
func (c *acnCall) timezone() error {
	value := strings.ToLower(c.inv.Arg(1))
	switch value {
	case "":
		p := command.Escape(c.inv.Prefix)
		return c.inv.Edit(c.ctx, "🌍 <b>时区管理</b>\n\n• <code>"+p+"acn tz Asia/Shanghai</code> - 设置时区\n• <code>"+p+
			"acn tz list</code> - 时区列表\n• <code>"+p+"acn tz on/off</code> - 显示控制\n• <code>"+p+"acn tz format GMT</code> - 格式设置")
	case "list":
		return c.inv.EditText(c.ctx, "Asia/Shanghai\nAsia/Tokyo\nEurope/London\nAmerica/New_York")
	case "on", "off":
		on := value == "on"
		if _, err := c.change(func(user *acnUser) {
			user.ShowTimezone = on
			user.DisplayOrder = withComponent(user.DisplayOrder, "timezone", on)
		}); err != nil {
			return err
		}
		return c.inv.EditText(c.ctx, "✅ 时区显示已"+map[bool]string{true: "开启", false: "关闭"}[on])
	case "format":
		return c.timezoneFormat()
	}
	zone := c.inv.Rest(1)
	if value == "set" {
		zone = c.inv.Rest(2)
	}
	if !validZone(zone) {
		return c.inv.EditText(c.ctx, "❌ 无效的时区标识符")
	}
	if _, err := c.change(func(user *acnUser) { user.Timezone = zone }); err != nil {
		return err
	}
	return c.inv.Edit(c.ctx, "✅ 时区已更新为: "+command.Code(zone))
}

// timezoneFormat 设时区的显示格式：GMT、UTC、SIMP、OFFSET，或者 custom: 后面跟自定义文字。
// 不带参数时显示当前格式。
func (c *acnCall) timezoneFormat() error {
	format := strings.TrimSpace(c.inv.Rest(2))
	if format == "" {
		return c.inv.Edit(c.ctx, "🌐 <b>时区格式设置</b>\n当前: "+command.Code(kit.OrDefault(c.user.TimezoneFormat, "GMT"))+
			"\n可用: GMT, UTC, simp, offset, custom:文字")
	}
	lower := strings.ToLower(format)
	custom := strings.HasPrefix(lower, "custom:") && len(format) > len("custom:")
	if !custom && lower != "gmt" && lower != "utc" && lower != "simp" && lower != "offset" {
		return c.inv.EditText(c.ctx, "❌ 无效的时区格式")
	}
	if _, err := c.change(func(user *acnUser) {
		if custom {
			user.TimezoneFormat = "custom:" + format[len("custom:"):]
		} else {
			user.TimezoneFormat = strings.ToUpper(format)
		}
	}); err != nil {
		return err
	}
	return c.inv.EditText(c.ctx, "✅ 时区格式已更新")
}

// flag 开关时钟表情（emoji）或时间显示（time）；time 还能切换 12/24 小时制。
// 不带参数时显示当前状态。
func (c *acnCall) flag() error {
	arg := strings.ToLower(c.inv.Arg(1))
	label := "时间显示"
	usage := "请使用 " + c.inv.Prefix + "acn time on/off 或 " + c.inv.Prefix + "acn time 12/24"
	current := c.user.showTime()
	if c.sub == "emoji" {
		label, usage, current = "时钟Emoji", "请使用 "+c.inv.Prefix+"acn emoji on/off", c.user.ShowClockEmoji
	}
	if arg == "" {
		text := "<b>" + label + "</b>\n当前: " + command.Code(map[bool]string{true: "开启", false: "关闭"}[current])
		if c.sub == "time" {
			text += "\n时间制式: " + command.Code(hourFormatOf(c.user)+" 小时制")
		}
		return c.inv.Edit(c.ctx, text+"\n"+command.Escape(usage))
	}
	if c.sub == "time" {
		format := arg
		if arg == "format" {
			format = strings.ToLower(c.inv.Arg(2))
		}
		format = strings.TrimSuffix(format, "h")
		if format == "12" || format == "24" {
			if _, err := c.change(func(user *acnUser) { user.HourFormat = format }); err != nil {
				return err
			}
			example := "（如 14:32）"
			if format == "12" {
				example = "（如 02:32 PM）"
			}
			return c.inv.EditText(c.ctx, "✅ 已切换为 "+format+" 小时制"+example)
		}
	}
	enabled, err := kit.OnOff(arg)
	if err != nil {
		return c.inv.EditText(c.ctx, usage)
	}
	if _, err := c.change(func(user *acnUser) {
		if c.sub == "emoji" {
			user.ShowClockEmoji = enabled
			user.DisplayOrder = withComponent(user.DisplayOrder, "emoji", enabled)
			return
		}
		value := enabled
		user.ShowTime = &value
		user.DisplayOrder = withComponent(user.DisplayOrder, "time", enabled)
	}); err != nil {
		return err
	}
	return c.inv.EditText(c.ctx, "✅ "+label+"已"+map[bool]string{true: "开启", false: "关闭"}[enabled])
}

func hourFormatOf(user *acnUser) string {
	if user.HourFormat == "12" {
		return "12"
	}
	return "24"
}

func (c *acnCall) style() error {
	style := strings.ToLower(c.inv.Arg(1))
	if !slices.Contains(acnStyles, style) {
		return c.inv.EditText(c.ctx, "可用样式: "+strings.Join(acnStyles, ", "))
	}
	if _, err := c.change(func(user *acnUser) { user.TextStyle = style }); err != nil {
		return err
	}
	return c.inv.EditText(c.ctx, "✅ 文字样式: "+style)
}

// order 查看或设置昵称里各部分的顺序；不分大小写，重复的只留第一次出现的。
// 设置顺序的同时按顺序开关时间、时钟表情和时区，列出天气就打开天气。
func (c *acnCall) order() error {
	values := strings.FieldsFunc(strings.ToLower(c.inv.Rest(1)), func(r rune) bool { return r == ',' || r == ' ' })
	if len(values) == 0 {
		return c.inv.Edit(c.ctx, "当前顺序: "+command.Code(kit.OrDefault(c.user.DisplayOrder, acnDefaultOrder)))
	}
	var invalid, unique []string
	for _, value := range values {
		if !slices.Contains(acnComponents, value) {
			invalid = append(invalid, value)
		} else if !slices.Contains(unique, value) {
			unique = append(unique, value)
		}
	}
	if len(invalid) > 0 {
		return c.inv.Edit(c.ctx, "❌ 无效组件: "+command.Code(strings.Join(invalid, ", "))+"\n可用: "+strings.Join(acnComponents, ", "))
	}
	if _, err := c.change(func(user *acnUser) { applyOrder(user, unique) }); err != nil {
		return err
	}
	return c.inv.Edit(c.ctx, "✅ 显示顺序: "+command.Code(strings.Join(unique, ",")))
}

// show 管理 acn show 选定的组件：只管时间、文案和天气，表情和时区有各自的命令。
func (c *acnCall) show() error {
	action := strings.ToLower(c.inv.Arg(1))
	target := strings.ToLower(c.inv.Arg(2))
	p := command.Escape(c.inv.Prefix)
	if action == "" || action == "help" || action == "h" {
		current := c.user.DisplayComponents
		if current == nil {
			current = showDefaults[c.user.Mode]
		}
		return c.inv.Edit(c.ctx, "🎛️ <b>显示组件管理</b>\n\n当前组件: "+command.Code(strings.Join(current, ", "))+
			"\n\n• <code>"+p+"acn show time on/off</code>\n• <code>"+p+"acn show text on/off</code>\n• <code>"+p+
			"acn show weather on/off</code>\n• <code>"+p+"acn show reset</code>")
	}
	if action == "reset" {
		updated, err := c.change(func(user *acnUser) { user.DisplayComponents = slices.Clone(showDefaults[user.Mode]) })
		if err != nil {
			return err
		}
		return c.inv.Edit(c.ctx, "✅ <b>已重置为默认值</b>\n\n当前模式默认组件: "+command.Code(strings.Join(updated.DisplayComponents, ", ")))
	}
	if action != "time" && action != "text" && action != "weather" {
		return c.inv.Edit(c.ctx, "❌ <b>acn show 仅支持管理 time/text/weather</b>")
	}
	if target != "on" && target != "off" {
		return c.inv.Edit(c.ctx, "❌ <b>请指定 on 或 off</b>\n使用: <code>"+p+"acn show "+action+" on/off</code>")
	}
	if action == "weather" && target == "on" && strings.TrimSpace(c.user.WeatherLocation) == "" {
		return c.inv.Edit(c.ctx, "❌ <b>请先设置天气地点</b>\n使用 <code>"+p+"acn weather set 北京</code>")
	}
	updated, err := c.change(func(user *acnUser) { toggleShown(user, action, target == "on") })
	if err != nil {
		return err
	}
	return c.inv.Edit(c.ctx, "✅ <b>组件已"+map[bool]string{true: "开启", false: "关闭"}[target == "on"]+"</b>\n当前组件: "+
		command.Code(strings.Join(updated.DisplayComponents, ", ")))
}

func (c *acnCall) config() error {
	user := c.user
	nextText := "(空)"
	if count := len(c.state.RandomTexts); count > 0 {
		nextText = strconv.Itoa((user.TextIndex%count+count)%count + 1)
	}
	weatherTime := "尚未获取"
	if user.WeatherCacheTS > 0 {
		weatherTime = time.UnixMilli(user.WeatherCacheTS).UTC().Format(time.RFC3339)
	}
	rows := [][2]string{
		{"用户", string(user.UserID)}, {"自动更新", kit.OnOffText(user.Enabled)}, {"原始姓名", user.OriginalFirstName},
		{"原始姓氏", kit.OrDefault(user.OriginalLastName, "(空)")}, {"模式", user.Mode}, {"时区", user.Timezone},
		{"时间显示", kit.OnOffText(user.showTime())}, {"时间制式", hourFormatOf(user) + " 小时制"},
		{"时钟表情", kit.OnOffText(user.ShowClockEmoji)},
		{"时区显示", kit.OnOffText(user.ShowTimezone)}, {"时区格式", kit.OrDefault(user.TimezoneFormat, "GMT")},
		{"文字样式", kit.OrDefault(user.TextStyle, "normal")}, {"组件顺序", kit.OrDefault(user.DisplayOrder, acnDefaultOrder)},
		{"文案数", strconv.Itoa(len(c.state.RandomTexts))}, {"下条文案序号", nextText},
		{"天气显示", kit.OnOffText(user.WeatherEnabled)}, {"天气地点", kit.OrDefault(user.WeatherLocation, "未设置")},
		{"天气预览", kit.OrDefault(user.WeatherCompact, "暂无缓存")}, {"天气更新时间", weatherTime},
		{"昵称更新时间", kit.OrDefault(user.LastUpdate, "尚未更新")},
	}
	lines := []string{"<b>🔧 您的配置状态</b>"}
	for _, row := range rows {
		lines = append(lines, command.Escape(row[0])+": "+command.Code(row[1]))
	}
	return c.inv.Edit(c.ctx, strings.Join(lines, "\n"))
}

// update 立刻按当前设置改一次名字。
func (c *acnCall) update() error {
	ok, err := c.service.apply(c.ctx, c.inv.Client, c.userID, true)
	if err != nil {
		return c.inv.EditText(c.ctx, "❌ 更新失败："+kit.RPCCode(err))
	}
	if !ok {
		return c.inv.EditText(c.ctx, "❌ 更新失败")
	}
	return c.inv.EditText(c.ctx, "✅ 昵称已手动更新")
}

// reset 关掉自动改名并换回原始昵称。
func (c *acnCall) reset() error {
	if _, err := c.mutate(func(user *acnUser) { user.Enabled = false }); err != nil {
		return err
	}
	if err := c.service.restore(c.ctx, c.inv.Client, c.user); err != nil {
		return c.inv.EditText(c.ctx, "❌ 恢复原始昵称失败："+kit.RPCCode(err))
	}
	return c.inv.EditText(c.ctx, "✅ 已恢复原始昵称并禁用自动更新")
}

func acnHandle(ctx context.Context, inv *command.Invocation, service *acnService) error {
	userID := strconv.FormatInt(inv.Client.SelfID(), 10)
	sub := strings.ToLower(inv.Arg(0))
	if sub == "" || sub == "help" || sub == "h" {
		return inv.Edit(ctx, acnHelp(inv.Prefix))
	}
	state, err := service.store.Read()
	if err != nil {
		return err
	}
	switch sub {
	case "status":
		return acnStatus(ctx, inv, state)
	case "save":
		return acnSave(ctx, inv, service, userID)
	}
	user := state.Users[userID]
	if user == nil || user.OriginalFirstName == "" {
		return inv.Edit(ctx, "❌ 请先 "+command.Code(inv.Prefix+"acn save"))
	}
	call := &acnCall{ctx: ctx, inv: inv, service: service, state: state, userID: userID, user: user, sub: sub}
	switch sub {
	case "on", "enable", "off", "disable":
		return call.toggle()
	case "mode":
		return call.mode()
	case "tz", "timezone":
		return call.timezone()
	case "text":
		return call.text()
	case "emoji", "time":
		return call.flag()
	case "style":
		return call.style()
	case "order":
		return call.order()
	case "show":
		return call.show()
	case "weather":
		return call.weather()
	case "config":
		return call.config()
	case "update", "now":
		return call.update()
	case "reset":
		return call.reset()
	}
	return inv.Edit(ctx, "❌ 未知命令: "+command.Code(sub))
}

func (c *acnCall) text() error {
	ctx, inv, service, state := c.ctx, c.inv, c.service, c.state
	action := strings.ToLower(inv.Arg(1))
	switch action {
	case "list":
		if len(state.RandomTexts) == 0 {
			return inv.EditText(ctx, "📝 无随机文本")
		}
		var lines []string
		for index, text := range state.RandomTexts {
			lines = append(lines, strconv.Itoa(index+1)+". "+command.Escape(text))
		}
		return inv.Edit(ctx, strings.Join(lines, "\n"))
	case "clear":
		if err := service.store.Update(func(state *acnState) error { state.RandomTexts = []string{}; return nil }); err != nil {
			return err
		}
		return inv.EditText(ctx, "✅ 所有文本已清空")
	case "add":
		// "text add" 之后的内容原样收下，每行一条，
		// 所以一条多行消息能一次添加好几条。
		body := inv.Text
		if index := strings.Index(strings.ToLower(body), "add"); index >= 0 {
			body = body[index+len("add"):]
		}
		var additions []string
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && len([]rune(line)) <= 50 {
				additions = append(additions, line)
			}
		}
		if len(additions) == 0 {
			return inv.EditText(ctx, "❌ 没有可添加的文案（每条最长 50 字符）")
		}
		added := 0
		if err := service.store.Update(func(state *acnState) error {
			existing := map[string]bool{}
			for _, text := range state.RandomTexts {
				existing[text] = true
			}
			for _, text := range additions {
				if existing[text] || len(state.RandomTexts) >= 100 {
					continue
				}
				existing[text] = true
				state.RandomTexts = append(state.RandomTexts, text)
				added++
			}
			return nil
		}); err != nil {
			return err
		}
		return inv.EditText(ctx, "✅ 成功添加 "+strconv.Itoa(added)+" 条")
	case "del":
		index, err := strconv.Atoi(inv.Arg(2))
		if err != nil || index < 1 || index > len(state.RandomTexts) {
			return inv.EditText(ctx, "❌ 无效的索引号")
		}
		if err := service.store.Update(func(state *acnState) error {
			if index <= len(state.RandomTexts) {
				state.RandomTexts = append(state.RandomTexts[:index-1], state.RandomTexts[index:]...)
			}
			return nil
		}); err != nil {
			return err
		}
		return inv.EditText(ctx, "✅ 文本已删除")
	case "on", "off":
		enabled := action == "on"
		if _, err := c.change(func(user *acnUser) {
			if enabled {
				user.Mode = "both"
				if !user.showTime() {
					user.Mode = "text"
				}
				if user.DisplayComponents != nil && !slices.Contains(user.DisplayComponents, "text") {
					user.DisplayComponents = append(user.DisplayComponents, "text")
				}
			} else {
				user.Mode = "time"
				if user.DisplayComponents != nil {
					user.DisplayComponents = slices.DeleteFunc(user.DisplayComponents, func(value string) bool { return value == "text" })
				}
			}
			user.DisplayOrder = withComponent(user.DisplayOrder, "text", enabled)
		}); err != nil {
			return err
		}
		return inv.EditText(ctx, "✅ 随机文案已"+map[bool]string{true: "开启", false: "关闭"}[enabled])
	}
	return inv.EditText(ctx, "用法："+inv.Prefix+"acn text add|list|del|clear|on|off")
}

// weather 查看或设置天气。不带参数或 help 时显示当前设置和一次现取的预览。
func (c *acnCall) weather() error {
	ctx, inv, user := c.ctx, c.inv, c.user
	action := strings.ToLower(inv.Arg(1))
	if action == "" || action == "help" {
		now := time.Now()
		preview, fetched := currentWeather(ctx, user, true, now)
		if fetched {
			if _, err := c.mutate(func(user *acnUser) { user.WeatherCompact, user.WeatherCacheTS = preview, now.UnixMilli() }); err != nil {
				return err
			}
		}
		return inv.Edit(ctx, "天气: "+kit.OnOffText(user.WeatherEnabled)+"\n地点: "+command.Escape(kit.OrDefault(user.WeatherLocation, "未设置"))+
			"\n预览: "+command.Escape(kit.OrDefault(preview, "暂无缓存")))
	}
	if action == "on" && user.WeatherLocation == "" {
		return inv.EditText(ctx, "❌ 请先设置地点")
	}
	if action == "on" || action == "off" {
		on := action == "on"
		if _, err := c.change(func(user *acnUser) {
			user.WeatherEnabled = on
			user.DisplayOrder = withComponent(user.DisplayOrder, "weather", on)
		}); err != nil {
			return err
		}
		return inv.EditText(ctx, "✅ 天气配置已更新")
	}
	location := inv.Rest(1)
	if action == "set" {
		location = inv.Rest(2)
	}
	if strings.TrimSpace(location) == "" {
		return inv.EditText(ctx, "❌ 请提供地点")
	}
	if _, err := c.change(func(user *acnUser) {
		user.WeatherLocation, user.WeatherEnabled, user.WeatherCompact, user.WeatherCacheTS = location, true, "", 0
		user.DisplayOrder = withComponent(user.DisplayOrder, "weather", true)
	}); err != nil {
		return err
	}
	return inv.EditText(ctx, "✅ 天气配置已更新")
}

// cleanNickname 去掉上一次运行追加的钟面表情和 HH:MM，
// 免得重新保存时把过去的时间固化进原始昵称里。
func cleanNickname(name string) string {
	runes := []rune(kit.TruncateRunes(name, 128))
	var b strings.Builder
	for _, r := range runes {
		if r >= 0x1F550 && r <= 0x1F567 {
			continue
		}
		b.WriteRune(r)
	}
	cleaned := clockTimePattern.ReplaceAllString(b.String(), "")
	return strings.TrimSpace(strings.Join(strings.Fields(cleaned), " "))
}

// clockTimePattern 匹配旧昵称里带着的 "9:30" 或 "09:30 PM"。
var clockTimePattern = regexp.MustCompile(`(?i)\b\d{1,2}:\d{2}(\s?(AM|PM))?\b`)
