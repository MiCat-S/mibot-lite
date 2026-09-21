package commands

import (
	"context"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/httpx"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// acnUser is one account's dynamic-nickname settings. The JSON names match
// MiBox's autochangename.json so an imported file is read as it stands.
type acnUser struct {
	UserID            string `json:"user_id"`
	Timezone          string `json:"timezone"`
	OriginalFirstName string `json:"original_first_name"`
	OriginalLastName  string `json:"original_last_name"`
	Enabled           bool   `json:"is_enabled"`
	Mode              string `json:"mode"`
	LastUpdate        string `json:"last_update"`
	TextIndex         int    `json:"text_index"`
	ShowClockEmoji    bool   `json:"show_clock_emoji"`
	ShowTime          *bool  `json:"show_time"`
	ShowTimezone      bool   `json:"show_timezone"`
	TimezoneFormat    string `json:"timezone_format"`
	DisplayOrder      string `json:"display_order"`
	TextStyle         string `json:"text_style"`
	WeatherEnabled    bool   `json:"weather_enabled"`
	WeatherLocation   string `json:"weather_location"`
	WeatherCompact    string `json:"weather_compact"`
	WeatherCacheTS    int64  `json:"weather_cache_ts"`
}

type acnState struct {
	SchemaVersion int                 `json:"schemaVersion"`
	Users         map[string]*acnUser `json:"users"`
	RandomTexts   []string            `json:"random_texts"`
}

type acnService struct {
	store *store.Store[acnState]
}

const acnDefaultOrder = "name,text,time,weather,emoji,timezone"

var acnComponents = []string{"name", "text", "time", "weather", "emoji", "timezone"}

func acnDefaults() acnState {
	return acnState{SchemaVersion: 1, Users: map[string]*acnUser{}, RandomTexts: []string{}}
}

func (u *acnUser) showTime() bool { return u.ShowTime == nil || *u.ShowTime }

// validZone reports whether a timezone identifier loads.
func validZone(zone string) bool {
	if strings.TrimSpace(zone) == "" {
		return false
	}
	_, err := time.LoadLocation(zone)
	return err == nil
}

// zoneLabel renders the timezone the way the chosen format asks.
//
// MiBox carried a 200-entry abbreviation table for "simp"; Go's own zone
// database already knows the abbreviation, so time.Format("MST") replaces
// the whole table and stays correct across daylight saving.
func zoneLabel(zone, format string) string {
	location, err := time.LoadLocation(zone)
	if err != nil {
		return ""
	}
	now := time.Now().In(location)
	if strings.HasPrefix(strings.ToLower(format), "custom:") {
		return format[len("custom:"):]
	}
	_, offset := now.Zone()
	sign := "+"
	if offset < 0 {
		sign, offset = "-", -offset
	}
	hours, minutes := offset/3600, (offset%3600)/60
	switch strings.ToUpper(strings.TrimSpace(format)) {
	case "SIMP":
		abbreviation := now.Format("MST")
		if strings.HasPrefix(abbreviation, "+") || strings.HasPrefix(abbreviation, "-") {
			break
		}
		return abbreviation
	case "OFFSET":
		return sign + pad2(hours) + ":" + pad2(minutes)
	case "UTC":
		if hours == 0 && minutes == 0 {
			return "UTC"
		}
		if minutes != 0 {
			return "UTC" + sign + pad2(hours) + ":" + pad2(minutes)
		}
		return "UTC" + sign + strconv.Itoa(hours)
	}
	if hours == 0 && minutes == 0 {
		return "GMT"
	}
	if minutes != 0 {
		return "GMT" + sign + pad2(hours) + ":" + pad2(minutes)
	}
	return "GMT" + sign + strconv.Itoa(hours)
}

func pad2(value int) string {
	if value < 10 {
		return "0" + strconv.Itoa(value)
	}
	return strconv.Itoa(value)
}

// clockEmoji is the 🕐-series face for the hour in a zone.
func clockEmoji(zone string) string {
	location, err := time.LoadLocation(zone)
	if err != nil {
		return ""
	}
	hour := time.Now().In(location).Hour() % 12
	return string(rune(0x1f550 + (hour+11)%12))
}

func zoneTime(zone string) string {
	location, err := time.LoadLocation(zone)
	if err != nil {
		return ""
	}
	return time.Now().In(location).Format("15:04")
}

// styleRanges maps a text style to the Unicode block each character class
// is shifted into.
var styleRanges = map[string][3]rune{
	"italic":  {0x1D7CE, 0x1D400, 0x1D41A},
	"double":  {0x1D7D8, 0, 0x1D552},
	"sans":    {0x1D7EC, 0x1D5D4, 0x1D5EE},
	"mono":    {0x1D7F6, 0x1D670, 0x1D68A},
	"outline": {0x1D7E2, 0x1D5A0, 0x1D5BA},
}

// doubleStruckUpper holds the double-struck capitals that live outside the
// contiguous block.
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

// acnCities maps common Chinese city names to what the geocoder knows.
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

// fetchWeather reads the current conditions, or "" when anything fails.
func fetchWeather(ctx context.Context, location string) (string, bool) {
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
		return "", false
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
		return "", true
	}
	icon, ok := weatherIcons[*forecast.Current.Code]
	if !ok {
		icon = "🌤️"
	}
	return icon + " " + strconv.Itoa(int(*forecast.Current.Temperature+0.5)) + "°C", true
}

// apply rebuilds and pushes the nickname for one account.
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
	parts := map[string]string{"name": user.OriginalFirstName}
	if (user.Mode == "text" || user.Mode == "both") && len(state.RandomTexts) > 0 {
		parts["text"] = state.RandomTexts[user.TextIndex%len(state.RandomTexts)]
	}
	if user.showTime() && (user.Mode == "time" || user.Mode == "both") {
		parts["time"] = zoneTime(user.Timezone)
	}
	if user.ShowClockEmoji {
		parts["emoji"] = clockEmoji(user.Timezone)
	}
	if user.ShowTimezone {
		parts["timezone"] = zoneLabel(user.Timezone, user.TimezoneFormat)
	}
	weather, fetched := "", false
	if user.WeatherEnabled && user.WeatherLocation != "" {
		cacheFor := 5 * time.Minute
		if user.WeatherCompact != "" {
			cacheFor = 30 * time.Minute
		}
		if user.WeatherCacheTS > 0 && time.Since(time.UnixMilli(user.WeatherCacheTS)) < cacheFor {
			weather = user.WeatherCompact
		} else {
			weather, fetched = fetchWeather(ctx, user.WeatherLocation)
		}
	}
	parts["weather"] = weather

	order := user.DisplayOrder
	if strings.TrimSpace(order) == "" {
		order = acnDefaultOrder
	}
	var sequence []string
	seen := map[string]bool{}
	for _, key := range append(strings.Split(order, ","), acnComponents...) {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] || parts[key] == "" && key != "name" {
			continue
		}
		seen[key] = true
		sequence = append(sequence, key)
	}
	var pieces []string
	for _, key := range sequence {
		value := parts[key]
		if value == "" {
			continue
		}
		if key != "name" {
			value = applyTextStyle(value, user.TextStyle)
		}
		pieces = append(pieces, value)
	}
	firstName := truncateRunes(strings.Join(pieces, " "), 64)

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
			// Being rate-limited here means the account is pushing the
			// profile far too often; stop rather than keep tripping it.
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
			current.WeatherCompact, current.WeatherCacheTS = weather, time.Now().UnixMilli()
		}
		if textCount > 0 && current.Mode != "time" {
			current.TextIndex = (current.TextIndex + 1) % textCount
		}
		return nil
	})
}

// restore puts the saved original name back.
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
		"acn time on</code> / <code>off</code> 时间显示\n• <code>" + p + "acn style normal|italic|double|sans|mono|outline</code>\n• <code>" + p +
		"acn order name,text,time,weather,emoji,timezone</code>\n\n<b>文案</b>\n• <code>" + p + "acn text add 摸鱼中</code>（支持多行）\n• <code>" + p +
		"acn text list</code> / <code>del 序号</code> / <code>clear</code>\n• <code>" + p + "acn text on</code> / <code>off</code>\n\n<b>天气</b>\n• <code>" + p +
		"acn weather set 北京</code> 设置地点并开启\n• <code>" + p + "acn weather on</code> / <code>off</code>\n天气缓存 30 分钟。"
}

// Acn registers .acn and the per-minute refresh job.
func Acn(a *app.App) {
	service := &acnService{store: newStore(a, "acn.json", acnDefaults)}
	// A file written by MiBox may carry an unknown timezone or a missing
	// users map; normalise once at startup rather than on every tick.
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
			user.UserID = id
			if !validZone(user.Timezone) {
				user.Timezone = "Asia/Shanghai"
			}
			if user.Mode == "" {
				user.Mode = "time"
			}
		}
		if len(state.RandomTexts) > 100 {
			state.RandomTexts = state.RandomTexts[:100]
		}
		return nil
	})

	handle := func(ctx context.Context, inv *command.Invocation) error { return acnHandle(ctx, inv, service) }
	a.Registry.Register(
		&command.Command{Name: "acn", Description: "管理动态昵称", Usage: "save|on|off|mode|tz|text|weather|update|reset|status", Help: acnHelp, Handle: handle},
		&command.Command{Name: "autochangename", Description: "acn 的全称", Hidden: true, Help: acnHelp, Handle: handle},
	)

	a.Registry.AddJob(func(ctx context.Context, client *bot.Client) {
		// Tick on the minute, so the displayed clock changes when the real
		// one does rather than a random number of seconds later.
		for {
			now := time.Now()
			if err := sleepCtx(ctx, now.Truncate(time.Minute).Add(time.Minute).Sub(now)); err != nil {
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
	if sub == "status" {
		enabled := 0
		for _, user := range state.Users {
			if user != nil && user.Enabled {
				enabled++
			}
		}
		return inv.Edit(ctx, "📊 自动更新: "+command.Code("运行中")+"\n启用用户: "+command.Code(strconv.Itoa(enabled)))
	}
	if sub == "save" {
		self := inv.Client.Self()
		return service.store.Update(func(state *acnState) error {
			current := state.Users[userID]
			if current == nil {
				current = &acnUser{UserID: userID, Timezone: "Asia/Shanghai", Mode: "time"}
				state.Users[userID] = current
			}
			current.OriginalFirstName = cleanNickname(self.FirstName)
			current.OriginalLastName = cleanNickname(self.LastName)
			if current.Timezone == "" {
				current.Timezone = "Asia/Shanghai"
			}
			return nil
		})
	}

	user := state.Users[userID]
	if user == nil || user.OriginalFirstName == "" {
		return inv.Edit(ctx, "❌ 请先 "+command.Code(inv.Prefix+"acn save"))
	}
	mutate := func(apply func(user *acnUser)) error {
		return service.store.Update(func(state *acnState) error {
			if current := state.Users[userID]; current != nil {
				apply(current)
			}
			return nil
		})
	}

	switch sub {
	case "on", "enable", "off", "disable":
		enabled := sub == "on" || sub == "enable"
		if err := mutate(func(user *acnUser) { user.Enabled = enabled }); err != nil {
			return err
		}
		if enabled {
			if _, err := service.apply(ctx, inv.Client, userID, true); err != nil {
				return inv.EditText(ctx, "❌ 启用后首次更新失败："+rpcCode(err))
			}
			return inv.EditText(ctx, "✅ 动态昵称已启用")
		}
		if err := service.restore(ctx, inv.Client, user); err != nil {
			return inv.EditText(ctx, "❌ 恢复原始昵称失败："+rpcCode(err))
		}
		return inv.EditText(ctx, "✅ 动态昵称已禁用")
	case "mode":
		next := map[string]string{"time": "text", "text": "both", "both": "time"}[user.Mode]
		if next == "" {
			next = "time"
		}
		if err := mutate(func(user *acnUser) { user.Mode = next }); err != nil {
			return err
		}
		return inv.Edit(ctx, "✅ 显示模式: "+command.Code(next))
	case "tz", "timezone":
		value := strings.ToLower(inv.Arg(1))
		switch value {
		case "list":
			return inv.EditText(ctx, "Asia/Shanghai\nAsia/Tokyo\nEurope/London\nAmerica/New_York")
		case "on", "off":
			if err := mutate(func(user *acnUser) { user.ShowTimezone = value == "on" }); err != nil {
				return err
			}
			return inv.EditText(ctx, "✅ 时区显示已"+map[bool]string{true: "开启", false: "关闭"}[value == "on"])
		case "format":
			format := strings.TrimSpace(inv.Rest(2))
			lower := strings.ToLower(format)
			valid := lower == "gmt" || lower == "utc" || lower == "simp" || lower == "offset" || strings.HasPrefix(lower, "custom:")
			if !valid {
				return inv.EditText(ctx, "❌ 无效的时区格式")
			}
			if err := mutate(func(user *acnUser) {
				if strings.HasPrefix(lower, "custom:") {
					user.TimezoneFormat = "custom:" + format[len("custom:"):]
				} else {
					user.TimezoneFormat = strings.ToUpper(format)
				}
			}); err != nil {
				return err
			}
			return inv.EditText(ctx, "✅ 时区格式已更新")
		}
		zone := inv.Rest(1)
		if value == "set" {
			zone = inv.Rest(2)
		}
		if !validZone(zone) {
			return inv.EditText(ctx, "❌ 无效的时区标识符")
		}
		if err := mutate(func(user *acnUser) { user.Timezone = zone }); err != nil {
			return err
		}
		return inv.Edit(ctx, "✅ 时区已更新为: "+command.Code(zone))
	case "text":
		return acnText(ctx, inv, service, state, userID, mutate)
	case "emoji", "time":
		enabled, err := onOff(inv.Arg(1))
		if err != nil {
			return inv.EditText(ctx, "请使用 "+inv.Prefix+"acn "+sub+" on/off")
		}
		if err := mutate(func(user *acnUser) {
			if sub == "emoji" {
				user.ShowClockEmoji = enabled
			} else {
				value := enabled
				user.ShowTime = &value
			}
		}); err != nil {
			return err
		}
		label := "时间显示"
		if sub == "emoji" {
			label = "时钟Emoji"
		}
		return inv.EditText(ctx, "✅ "+label+"已"+map[bool]string{true: "开启", false: "关闭"}[enabled])
	case "style":
		style := strings.ToLower(inv.Arg(1))
		if style != "normal" {
			if _, ok := styleRanges[style]; !ok {
				return inv.EditText(ctx, "可用样式: normal, italic, double, sans, mono, outline")
			}
		}
		if err := mutate(func(user *acnUser) { user.TextStyle = style }); err != nil {
			return err
		}
		return inv.EditText(ctx, "✅ 文字样式: "+style)
	case "order":
		values := strings.FieldsFunc(inv.Rest(1), func(r rune) bool { return r == ',' || r == ' ' })
		if len(values) == 0 {
			order := user.DisplayOrder
			if order == "" {
				order = acnDefaultOrder
			}
			return inv.Edit(ctx, "当前顺序: "+command.Code(order))
		}
		for _, value := range values {
			if !containsString(acnComponents, value) {
				return inv.EditText(ctx, "❌ 无效组件")
			}
		}
		var unique []string
		seen := map[string]bool{}
		for _, value := range values {
			if !seen[value] {
				seen[value] = true
				unique = append(unique, value)
			}
		}
		order := strings.Join(unique, ",")
		if err := mutate(func(user *acnUser) { user.DisplayOrder = order }); err != nil {
			return err
		}
		return inv.Edit(ctx, "✅ 显示顺序: "+command.Code(order))
	case "weather":
		return acnWeather(ctx, inv, user, mutate)
	case "config":
		rows := [][2]string{
			{"用户", user.UserID}, {"自动更新", onOffText(user.Enabled)}, {"原始姓名", user.OriginalFirstName},
			{"原始姓氏", orDefault(user.OriginalLastName, "(空)")}, {"模式", user.Mode}, {"时区", user.Timezone},
			{"时间显示", onOffText(user.showTime())}, {"时钟表情", onOffText(user.ShowClockEmoji)},
			{"时区显示", onOffText(user.ShowTimezone)}, {"时区格式", orDefault(user.TimezoneFormat, "GMT")},
			{"文字样式", orDefault(user.TextStyle, "normal")}, {"组件顺序", orDefault(user.DisplayOrder, acnDefaultOrder)},
			{"文案数", strconv.Itoa(len(state.RandomTexts))},
			{"天气显示", onOffText(user.WeatherEnabled)}, {"天气地点", orDefault(user.WeatherLocation, "未设置")},
			{"天气预览", orDefault(user.WeatherCompact, "暂无缓存")}, {"昵称更新时间", orDefault(user.LastUpdate, "尚未更新")},
		}
		lines := []string{"<b>🔧 您的配置状态</b>"}
		for _, row := range rows {
			lines = append(lines, command.Escape(row[0])+": "+command.Code(row[1]))
		}
		return inv.Edit(ctx, strings.Join(lines, "\n"))
	case "update", "now":
		ok, err := service.apply(ctx, inv.Client, userID, true)
		if err != nil {
			return inv.EditText(ctx, "❌ 更新失败："+rpcCode(err))
		}
		if !ok {
			return inv.EditText(ctx, "❌ 更新失败")
		}
		return inv.EditText(ctx, "✅ 昵称已手动更新")
	case "reset":
		if err := mutate(func(user *acnUser) { user.Enabled = false }); err != nil {
			return err
		}
		if err := service.restore(ctx, inv.Client, user); err != nil {
			return inv.EditText(ctx, "❌ 恢复原始昵称失败："+rpcCode(err))
		}
		return inv.EditText(ctx, "✅ 已恢复原始昵称并禁用自动更新")
	}
	return inv.Edit(ctx, "❌ 未知命令: "+command.Code(sub))
}

func acnText(ctx context.Context, inv *command.Invocation, service *acnService, state acnState, userID string, mutate func(func(*acnUser)) error) error {
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
		// Everything after "text add" is taken verbatim, one entry per
		// line, so a multi-line message adds several at once.
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
		if err := mutate(func(user *acnUser) {
			if enabled {
				if user.showTime() {
					user.Mode = "both"
				} else {
					user.Mode = "text"
				}
			} else {
				user.Mode = "time"
			}
		}); err != nil {
			return err
		}
		return inv.EditText(ctx, "✅ 随机文案已"+map[bool]string{true: "开启", false: "关闭"}[enabled])
	}
	return inv.EditText(ctx, "用法："+inv.Prefix+"acn text add|list|del|clear|on|off")
}

func acnWeather(ctx context.Context, inv *command.Invocation, user *acnUser, mutate func(func(*acnUser)) error) error {
	action := strings.ToLower(inv.Arg(1))
	if action == "" {
		return inv.Edit(ctx, "天气: "+onOffText(user.WeatherEnabled)+"\n地点: "+command.Escape(orDefault(user.WeatherLocation, "未设置"))+
			"\n预览: "+command.Escape(orDefault(user.WeatherCompact, "暂无缓存")))
	}
	if action == "on" && user.WeatherLocation == "" {
		return inv.EditText(ctx, "❌ 请先设置地点")
	}
	if action == "on" || action == "off" {
		if err := mutate(func(user *acnUser) { user.WeatherEnabled = action == "on" }); err != nil {
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
	if err := mutate(func(user *acnUser) {
		user.WeatherLocation, user.WeatherEnabled, user.WeatherCompact, user.WeatherCacheTS = location, true, "", 0
	}); err != nil {
		return err
	}
	return inv.EditText(ctx, "✅ 天气配置已更新")
}

// cleanNickname strips a clock face and a HH:MM the previous run appended,
// so re-saving does not bake yesterday's time into the base name.
func cleanNickname(name string) string {
	runes := []rune(truncateRunes(name, 128))
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

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// clockTimePattern matches a "9:30" or "09:30 PM" a previous nickname
// carried.
var clockTimePattern = regexp.MustCompile(`(?i)\b\d{1,2}:\d{2}(\s?(AM|PM))?\b`)
