package acn

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestApplyTextStyle(t *testing.T) {
	if got := applyTextStyle("ab12", "mono"); got != "𝚊𝚋𝟷𝟸" {
		t.Fatalf("mono style = %q", got)
	}
	if got := applyTextStyle("CZ", "double"); got != "ℂℤ" {
		t.Fatalf("double-struck capitals outside the block are wrong: %q", got)
	}
	if got := applyTextStyle("中文 ok", "sans"); !strings.HasPrefix(got, "中文 ") {
		t.Fatalf("non-latin text must pass through: %q", got)
	}
	if got := applyTextStyle("abc", "normal"); got != "abc" {
		t.Fatalf("normal style changed the text: %q", got)
	}
}

// 固定一个时刻：2026-01-15 是北半球冬天，2026-07-15 是夏天。
var (
	winter = time.Date(2026, time.January, 15, 6, 32, 0, 0, time.UTC)
	summer = time.Date(2026, time.July, 15, 6, 32, 0, 0, time.UTC)
)

func TestZoneLabel(t *testing.T) {
	for format, want := range map[string]string{"GMT": "GMT+8", "UTC": "UTC+8", "OFFSET": "+08:00", "SIMP": "CST", "custom:北京时间": "北京时间"} {
		if got := zoneLabel("Asia/Shanghai", format, winter); got != want {
			t.Errorf("zoneLabel(%q) = %q, want %q", format, got, want)
		}
	}
	if got := zoneLabel("UTC", "GMT", winter); got != "GMT" {
		t.Fatalf("UTC label = %q", got)
	}
	if got := zoneLabel("Asia/Kolkata", "GMT", winter); got != "GMT+05:30" {
		t.Fatalf("half-hour offset = %q", got)
	}
	if got := zoneLabel("Not/AZone", "GMT", winter); got != "" {
		t.Fatalf("an unknown zone should render empty, got %q", got)
	}
}

// 时区数据库只给 "+08" 这类数字缩写的地区，要按 MiBox 的表补上字母缩写。
func TestZoneAbbreviationFallsBackToMiBoxTables(t *testing.T) {
	cases := []struct {
		zone string
		at   time.Time
		want string
	}{
		{"Asia/Singapore", winter, "SGT"},     // 地区表
		{"Asia/Bangkok", winter, "ICT"},       // 地区表
		{"Asia/Dubai", winter, "GST"},         // 地区表
		{"America/New_York", summer, "EDT"},   // 时区数据库自己就有
		{"America/New_York", winter, "EST"},   // 时区数据库自己就有
		{"Pacific/Chatham", winter, "CHADT"},  // 冬夏令时表：南半球一月是夏令时
		{"Pacific/Chatham", summer, "CHAST"},  // 冬夏令时表
		{"America/Santiago", summer, "CLT"},   // 冬夏令时表
		{"Etc/GMT-5", winter, "PKT"},          // 偏移量表
		{"Etc/GMT-3", winter, "MSK"},          // 偏移量表
		{"Asia/Kathmandu", winter, "NPT"},     // 地区表，+05:45
		{"Pacific/Marquesas", winter, "MART"}, // 地区表，-09:30
	}
	for _, c := range cases {
		if got := zoneLabel(c.zone, "simp", c.at); got != c.want {
			t.Errorf("%s at %s: simp = %q, want %q", c.zone, c.at.Format("Jan"), got, c.want)
		}
	}
}

func TestZoneTimeHourFormat(t *testing.T) {
	if got := zoneTime("Asia/Shanghai", "24", winter); got != "14:32" {
		t.Errorf("24h = %q", got)
	}
	if got := zoneTime("Asia/Shanghai", "12", winter); got != "02:32 PM" {
		t.Errorf("12h = %q", got)
	}
	if got := zoneTime("Asia/Shanghai", "", winter.Add(-14*time.Hour)); got != "00:32" {
		t.Errorf("midnight should read 00, got %q", got)
	}
}

// 气温按 JavaScript 的 Math.round 取整：-3.2 是 -3，不是把负数往零截断的 -2。
func TestRoundHalfUpMatchesMathRound(t *testing.T) {
	for value, want := range map[float64]float64{
		-3.2: -3, -3.5: -3, -3.6: -4, -2.5: -2, -0.4: 0, 0.5: 1, 2.5: 3, 21.49: 21, 0.49999999999999994: 0,
	} {
		if got := roundHalfUp(value); got != want {
			t.Errorf("roundHalfUp(%v) = %v, want %v", value, got, want)
		}
	}
	if got := formatWeather(0, -3.2); got != "☀️ -3°C" {
		t.Errorf("formatWeather = %q", got)
	}
	if got := formatWeather(999, -0.4); got != "🌤️ 0°C" {
		t.Errorf("-0.4 should read 0°C, got %q", got)
	}
}

func newTestUser() *acnUser {
	user := newAcnUser("100")
	user.OriginalFirstName = "Cat"
	return user
}

// text 模式也显示时间，只要没关掉时间显示；这与 MiBox 一致。
func TestComposeNameShowsTimeInEveryMode(t *testing.T) {
	texts := []string{"摸鱼中"}
	for mode, want := range map[string]string{"time": "Cat 14:32", "text": "Cat 14:32 摸鱼中", "both": "Cat 14:32 摸鱼中"} {
		user := newTestUser()
		user.Mode = mode
		if got := composeName(user, texts, "", winter); got != want {
			t.Errorf("mode %s: %q, want %q", mode, got, want)
		}
	}
	user := newTestUser()
	user.Mode = "text"
	off := false
	user.ShowTime = &off
	if got := composeName(user, texts, "", winter); got != "Cat 摸鱼中" {
		t.Errorf("time off: %q", got)
	}
}

// 没设过顺序时按 name,time 起头，其余组件按固定次序接在后面。
func TestComposeNameDefaultOrder(t *testing.T) {
	user := newTestUser()
	user.DisplayOrder = ""
	user.Mode = "both"
	user.ShowClockEmoji = true
	user.ShowTimezone = true
	got := composeName(user, []string{"摸鱼中"}, "☀️ 21°C", winter)
	if want := "Cat 14:32 摸鱼中 ☀️ 21°C 🕑 GMT+8"; got != want {
		t.Errorf("default order: %q, want %q", got, want)
	}
}

// acn show 选定组件后，时间和文案只在选中时出现。
func TestComposeNameHonoursDisplayComponents(t *testing.T) {
	user := newTestUser()
	user.Mode = "time"
	user.DisplayComponents = []string{"text"}
	if got := composeName(user, []string{"摸鱼中"}, "", winter); got != "Cat 摸鱼中" {
		t.Errorf("only text selected: %q", got)
	}
	toggleShown(user, "text", false)
	if user.DisplayComponents == nil || len(user.DisplayComponents) != 0 {
		t.Errorf("turning off the last component should leave an empty list, got %#v", user.DisplayComponents)
	}
	fresh := newTestUser()
	fresh.Mode = "both"
	toggleShown(fresh, "time", false)
	if strings.Join(fresh.DisplayComponents, ",") != "text" {
		t.Errorf("toggling starts from the mode defaults, got %v", fresh.DisplayComponents)
	}
	withWeather := newTestUser()
	toggleShown(withWeather, "weather", true)
	if !withWeather.WeatherEnabled || strings.Join(withWeather.DisplayComponents, ",") != "time,weather" {
		t.Errorf("show weather on: enabled=%v components=%v", withWeather.WeatherEnabled, withWeather.DisplayComponents)
	}
}

// acn order 不只排顺序，还按列表开关时间、时钟表情和时区，列出天气就打开天气。
func TestApplyOrderTogglesComponents(t *testing.T) {
	user := newTestUser()
	user.ShowTimezone = true
	applyOrder(user, []string{"emoji", "name", "weather"})
	if user.DisplayOrder != "emoji,name,weather" {
		t.Errorf("order = %q", user.DisplayOrder)
	}
	if user.showTime() || !user.ShowClockEmoji || user.ShowTimezone || !user.WeatherEnabled {
		t.Errorf("flags not synced: time=%v emoji=%v tz=%v weather=%v", user.showTime(), user.ShowClockEmoji, user.ShowTimezone, user.WeatherEnabled)
	}
	applyOrder(user, []string{"name", "time"})
	if !user.showTime() || user.ShowClockEmoji || !user.WeatherEnabled {
		t.Errorf("leaving weather out must not turn it off: time=%v emoji=%v weather=%v", user.showTime(), user.ShowClockEmoji, user.WeatherEnabled)
	}
}

func TestWithComponent(t *testing.T) {
	cases := []struct {
		order, component string
		enabled          bool
		want             string
	}{
		{"", "emoji", true, "name,time,emoji"},
		{"name,time,emoji", "emoji", true, "name,time,emoji"},
		{"name,time,emoji", "time", false, "name,emoji"},
		{"", "weather", false, "name,time"},
	}
	for _, c := range cases {
		if got := withComponent(c.order, c.component, c.enabled); got != c.want {
			t.Errorf("withComponent(%q, %q, %v) = %q, want %q", c.order, c.component, c.enabled, got, c.want)
		}
	}
}

// MiBox v1 把 user_id 写成数字；读得进来，写出时换成字符串。
// 本程序不认识的字段原样保留，切回 MiBox 时设置不丢。
func TestAcnUserDecodesNumericIDAndKeepsUnknownFields(t *testing.T) {
	raw := `{"users":{"123456789":{"user_id":123456789,"timezone":"Asia/Tokyo","original_first_name":"Cat",` +
		`"is_enabled":true,"mode":"both","last_update":null,"text_index":2,"hour_format":"12",` +
		`"displayComponents":["time","text"],"future_flag":{"nested":[1,2]}}},"random_texts":["a"]}`
	var state acnState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatalf("v1 file did not decode: %v", err)
	}
	user := state.Users["123456789"]
	if user == nil || user.UserID != "123456789" || user.HourFormat != "12" || strings.Join(user.DisplayComponents, ",") != "time,text" {
		t.Fatalf("decoded user = %+v", user)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, want := range []string{`"user_id":"123456789"`, `"future_flag":{"nested":[1,2]}`, `"hour_format":"12"`, `"displayComponents":["time","text"]`} {
		if !strings.Contains(text, want) {
			t.Errorf("re-encoded file is missing %s:\n%s", want, text)
		}
	}
	var again acnState
	if err := json.Unmarshal(encoded, &again); err != nil || string(again.Users["123456789"].extra["future_flag"]) != `{"nested":[1,2]}` {
		t.Errorf("unknown field did not survive a round trip: %v %v", err, again.Users["123456789"])
	}
	var nullID acnUser
	if err := json.Unmarshal([]byte(`{"user_id":null}`), &nullID); err != nil || nullID.UserID != "" {
		t.Errorf("null user_id: %v %q", err, nullID.UserID)
	}
}

func TestNormalizeUser(t *testing.T) {
	user := &acnUser{Timezone: "Nowhere/Nope", Mode: "weird", TextStyle: "bold", TextIndex: -3, Enabled: true,
		DisplayComponents: []string{"time", "bogus", "time", "text"}}
	normalizeUser("42", user)
	if user.UserID != "42" || user.Timezone != "Asia/Shanghai" || user.Mode != "time" || user.TextStyle != "normal" ||
		user.TextIndex != 0 || user.HourFormat != "24" || user.Enabled {
		t.Errorf("normalized = %+v", user)
	}
	if strings.Join(user.DisplayComponents, ",") != "time,text" {
		t.Errorf("components = %v", user.DisplayComponents)
	}
}

// 取天气失败也要记下时间，5 分钟内不再重试；取到的天气缓存 30 分钟。
func TestWeatherCacheFresh(t *testing.T) {
	now := winter
	at := func(ago time.Duration) int64 { return now.Add(-ago).UnixMilli() }
	cases := []struct {
		compact string
		ago     time.Duration
		want    bool
	}{
		{"", 3 * time.Minute, true},
		{"", 6 * time.Minute, false},
		{"☀️ 21°C", 20 * time.Minute, true},
		{"☀️ 21°C", 31 * time.Minute, false},
	}
	for _, c := range cases {
		user := &acnUser{WeatherCompact: c.compact, WeatherCacheTS: at(c.ago)}
		if got := weatherCacheFresh(user, now); got != c.want {
			t.Errorf("compact=%q age=%s: fresh=%v, want %v", c.compact, c.ago, got, c.want)
		}
	}
	if weatherCacheFresh(&acnUser{}, now) {
		t.Error("a never-fetched cache is not fresh")
	}
}

// 重新保存昵称时，不能把上一次运行追加的时钟也存进去，否则基础昵称
// 每次都会多长出一个时间戳。
func TestCleanNickname(t *testing.T) {
	if got := cleanNickname("我要一直陪着我 🕘 09:30"); got != "我要一直陪着我" {
		t.Fatalf("cleanNickname = %q", got)
	}
	if got := cleanNickname("  Alice   Smith  "); got != "Alice Smith" {
		t.Fatalf("cleanNickname = %q", got)
	}
	if got := cleanNickname("Cat " + zoneTime("Asia/Shanghai", "12", winter)); got != "Cat" {
		t.Fatalf("a 12-hour clock should be cleaned too, got %q", got)
	}
}

func TestClockEmoji(t *testing.T) {
	// 14:32 在北京是下午两点多，钟面是 🕑。
	if got := clockEmoji("Asia/Shanghai", winter); got != "🕑" {
		t.Errorf("clockEmoji = %q (%s)", got, strconv.QuoteToASCII(got))
	}
}
