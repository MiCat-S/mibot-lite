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

// acnUser 是一个账号的动态昵称设置。JSON 字段名与 MiBox 的
// autochangename.json 一致，导入的文件可以原样读取。
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

// validZone 判断一个时区标识符能否加载。
func validZone(zone string) bool {
	if strings.TrimSpace(zone) == "" {
		return false
	}
	_, err := time.LoadLocation(zone)
	return err == nil
}

// zoneLabel 按所选的格式显示时区。
//
// MiBox 为 "simp" 带了一张 200 条的缩写表；Go 自带的时区数据库本来就
// 知道缩写，所以 time.Format("MST") 一句就顶替了整张表，
// 遇到夏令时切换也照样正确。
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

// clockEmoji 返回该时区当前钟点对应的 🕐 系列钟面表情。
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

// fetchWeather 读取当前天气，任何一步失败都返回 ""。
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
			current.WeatherCompact, current.WeatherCacheTS = weather, time.Now().UnixMilli()
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
		"acn time on</code> / <code>off</code> 时间显示\n• <code>" + p + "acn style normal|italic|double|sans|mono|outline</code>\n• <code>" + p +
		"acn order name,text,time,weather,emoji,timezone</code>\n\n<b>文案</b>\n• <code>" + p + "acn text add 摸鱼中</code>（支持多行）\n• <code>" + p +
		"acn text list</code> / <code>del 序号</code> / <code>clear</code>\n• <code>" + p + "acn text on</code> / <code>off</code>\n\n<b>天气</b>\n• <code>" + p +
		"acn weather set 北京</code> 设置地点并开启\n• <code>" + p + "acn weather on</code> / <code>off</code>\n天气缓存 30 分钟。"
}

// Acn 注册 .acn 以及每分钟刷新一次的后台任务。
func Acn(a *app.App) {
	service := &acnService{store: newStore(a, "acn.json", acnDefaults)}
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
		// 在整分钟触发，这样显示的时间和真实时钟同时跳变，
		// 而不是晚上随机的几秒。
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

// mutate 修改自己那一份配置并存盘。
func (c *acnCall) mutate(apply func(*acnUser)) error {
	return c.service.store.Update(func(state *acnState) error {
		if current := state.Users[c.userID]; current != nil {
			apply(current)
		}
		return nil
	})
}

func acnStatus(ctx context.Context, inv *command.Invocation, state acnState) error {
	enabled := 0
	for _, user := range state.Users {
		if user != nil && user.Enabled {
			enabled++
		}
	}
	return inv.Edit(ctx, "📊 自动更新: "+command.Code("运行中")+"\n启用用户: "+command.Code(strconv.Itoa(enabled)))
}

// acnSave 记下现在的名字，作为以后恢复用的「原始昵称」。其他子命令都要先有它。
// 第一次保存和之后更新回复不同：更新只换名字，时区、样式这些设置都保留。
func acnSave(ctx context.Context, inv *command.Invocation, service *acnService, userID string) error {
	self := inv.Client.Self()
	first := false
	var saved acnUser
	if err := service.store.Update(func(state *acnState) error {
		current := state.Users[userID]
		if current == nil {
			current = &acnUser{UserID: userID, Timezone: "Asia/Shanghai", Mode: "time"}
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
	names := "姓名：" + command.Code(saved.OriginalFirstName) + "\n姓氏：" + command.Code(orDefault(saved.OriginalLastName, "(空)"))
	if first {
		return inv.Edit(ctx, "🎉 <b>昵称已保存</b>\n\n"+names+"\n\n接下来用 "+command.Code(inv.Prefix+"acn on")+" 开启自动更新。")
	}
	return inv.Edit(ctx, "✅ <b>原始昵称已更新</b>，其他设置保留\n\n"+names)
}

// toggle 开启或关闭自动改名。开启时立刻改一次；关闭时换回原始昵称。
func (c *acnCall) toggle() error {
	enabled := c.sub == "on" || c.sub == "enable"
	if err := c.mutate(func(user *acnUser) { user.Enabled = enabled }); err != nil {
		return err
	}
	if enabled {
		if _, err := c.service.apply(c.ctx, c.inv.Client, c.userID, true); err != nil {
			return c.inv.EditText(c.ctx, "❌ 启用后首次更新失败："+rpcCode(err))
		}
		return c.inv.EditText(c.ctx, "✅ 动态昵称已启用")
	}
	if err := c.service.restore(c.ctx, c.inv.Client, c.user); err != nil {
		return c.inv.EditText(c.ctx, "❌ 恢复原始昵称失败："+rpcCode(err))
	}
	return c.inv.EditText(c.ctx, "✅ 动态昵称已禁用")
}

// mode 在 time → text → both 之间轮换。
func (c *acnCall) mode() error {
	next := map[string]string{"time": "text", "text": "both", "both": "time"}[c.user.Mode]
	if next == "" {
		next = "time"
	}
	if err := c.mutate(func(user *acnUser) { user.Mode = next }); err != nil {
		return err
	}
	return c.inv.Edit(c.ctx, "✅ 显示模式: "+command.Code(next))
}

// timezone 处理 tz 的几种写法：list、on/off、format，其余当成要设的时区。
func (c *acnCall) timezone() error {
	value := strings.ToLower(c.inv.Arg(1))
	switch value {
	case "list":
		return c.inv.EditText(c.ctx, "Asia/Shanghai\nAsia/Tokyo\nEurope/London\nAmerica/New_York")
	case "on", "off":
		if err := c.mutate(func(user *acnUser) { user.ShowTimezone = value == "on" }); err != nil {
			return err
		}
		return c.inv.EditText(c.ctx, "✅ 时区显示已"+map[bool]string{true: "开启", false: "关闭"}[value == "on"])
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
	if err := c.mutate(func(user *acnUser) { user.Timezone = zone }); err != nil {
		return err
	}
	return c.inv.Edit(c.ctx, "✅ 时区已更新为: "+command.Code(zone))
}

// timezoneFormat 设时区的显示格式：GMT、UTC、SIMP、OFFSET，或者 custom: 后面跟自定义文字。
func (c *acnCall) timezoneFormat() error {
	format := strings.TrimSpace(c.inv.Rest(2))
	lower := strings.ToLower(format)
	valid := lower == "gmt" || lower == "utc" || lower == "simp" || lower == "offset" || strings.HasPrefix(lower, "custom:")
	if !valid {
		return c.inv.EditText(c.ctx, "❌ 无效的时区格式")
	}
	if err := c.mutate(func(user *acnUser) {
		if strings.HasPrefix(lower, "custom:") {
			user.TimezoneFormat = "custom:" + format[len("custom:"):]
		} else {
			user.TimezoneFormat = strings.ToUpper(format)
		}
	}); err != nil {
		return err
	}
	return c.inv.EditText(c.ctx, "✅ 时区格式已更新")
}

// flag 开关时钟表情（emoji）或时间显示（time）。
func (c *acnCall) flag() error {
	enabled, err := onOff(c.inv.Arg(1))
	if err != nil {
		return c.inv.EditText(c.ctx, "请使用 "+c.inv.Prefix+"acn "+c.sub+" on/off")
	}
	if err := c.mutate(func(user *acnUser) {
		if c.sub == "emoji" {
			user.ShowClockEmoji = enabled
		} else {
			value := enabled
			user.ShowTime = &value
		}
	}); err != nil {
		return err
	}
	label := "时间显示"
	if c.sub == "emoji" {
		label = "时钟Emoji"
	}
	return c.inv.EditText(c.ctx, "✅ "+label+"已"+map[bool]string{true: "开启", false: "关闭"}[enabled])
}

func (c *acnCall) style() error {
	style := strings.ToLower(c.inv.Arg(1))
	if style != "normal" {
		if _, ok := styleRanges[style]; !ok {
			return c.inv.EditText(c.ctx, "可用样式: normal, italic, double, sans, mono, outline")
		}
	}
	if err := c.mutate(func(user *acnUser) { user.TextStyle = style }); err != nil {
		return err
	}
	return c.inv.EditText(c.ctx, "✅ 文字样式: "+style)
}

// order 查看或设置昵称里各部分的顺序；重复的只留第一次出现的。
func (c *acnCall) order() error {
	values := strings.FieldsFunc(c.inv.Rest(1), func(r rune) bool { return r == ',' || r == ' ' })
	if len(values) == 0 {
		order := c.user.DisplayOrder
		if order == "" {
			order = acnDefaultOrder
		}
		return c.inv.Edit(c.ctx, "当前顺序: "+command.Code(order))
	}
	for _, value := range values {
		if !containsString(acnComponents, value) {
			return c.inv.EditText(c.ctx, "❌ 无效组件")
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
	if err := c.mutate(func(user *acnUser) { user.DisplayOrder = order }); err != nil {
		return err
	}
	return c.inv.Edit(c.ctx, "✅ 显示顺序: "+command.Code(order))
}

func (c *acnCall) config() error {
	user := c.user
	rows := [][2]string{
		{"用户", user.UserID}, {"自动更新", onOffText(user.Enabled)}, {"原始姓名", user.OriginalFirstName},
		{"原始姓氏", orDefault(user.OriginalLastName, "(空)")}, {"模式", user.Mode}, {"时区", user.Timezone},
		{"时间显示", onOffText(user.showTime())}, {"时钟表情", onOffText(user.ShowClockEmoji)},
		{"时区显示", onOffText(user.ShowTimezone)}, {"时区格式", orDefault(user.TimezoneFormat, "GMT")},
		{"文字样式", orDefault(user.TextStyle, "normal")}, {"组件顺序", orDefault(user.DisplayOrder, acnDefaultOrder)},
		{"文案数", strconv.Itoa(len(c.state.RandomTexts))},
		{"天气显示", onOffText(user.WeatherEnabled)}, {"天气地点", orDefault(user.WeatherLocation, "未设置")},
		{"天气预览", orDefault(user.WeatherCompact, "暂无缓存")}, {"昵称更新时间", orDefault(user.LastUpdate, "尚未更新")},
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
		return c.inv.EditText(c.ctx, "❌ 更新失败："+rpcCode(err))
	}
	if !ok {
		return c.inv.EditText(c.ctx, "❌ 更新失败")
	}
	return c.inv.EditText(c.ctx, "✅ 昵称已手动更新")
}

// reset 关掉自动改名并换回原始昵称。
func (c *acnCall) reset() error {
	if err := c.mutate(func(user *acnUser) { user.Enabled = false }); err != nil {
		return err
	}
	if err := c.service.restore(c.ctx, c.inv.Client, c.user); err != nil {
		return c.inv.EditText(c.ctx, "❌ 恢复原始昵称失败："+rpcCode(err))
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
		return acnText(ctx, inv, service, state, userID, call.mutate)
	case "emoji", "time":
		return call.flag()
	case "style":
		return call.style()
	case "order":
		return call.order()
	case "weather":
		return acnWeather(ctx, inv, user, call.mutate)
	case "config":
		return call.config()
	case "update", "now":
		return call.update()
	case "reset":
		return call.reset()
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

// cleanNickname 去掉上一次运行追加的钟面表情和 HH:MM，
// 免得重新保存时把过去的时间固化进原始昵称里。
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

// clockTimePattern 匹配旧昵称里带着的 "9:30" 或 "09:30 PM"。
var clockTimePattern = regexp.MustCompile(`(?i)\b\d{1,2}:\d{2}(\s?(AM|PM))?\b`)
