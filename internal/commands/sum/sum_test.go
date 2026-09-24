package sum

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// TestSumCronParsing 检查五字段和六字段（带秒）的 Cron 都能解析，时间点正确。
func TestSumCronParsing(t *testing.T) {
	base := time.Date(2026, 9, 24, 10, 7, 30, 0, time.Local)
	cases := []struct {
		spec string
		want time.Time
	}{
		{"0 */2 * * *", time.Date(2026, 9, 24, 12, 0, 0, 0, time.Local)},
		{"0 0 */2 * * *", time.Date(2026, 9, 24, 12, 0, 0, 0, time.Local)},
		{"0 */30 * * * *", time.Date(2026, 9, 24, 10, 30, 0, 0, time.Local)},
		{"15 0 9,21 * * *", time.Date(2026, 9, 24, 21, 0, 15, 0, time.Local)},
		{"0 0 * * *", time.Date(2026, 9, 25, 0, 0, 0, 0, time.Local)},
		{"30 8 * * MON-FRI", time.Date(2026, 9, 25, 8, 30, 0, 0, time.Local)},
	}
	for _, test := range cases {
		next, ok := sumNextRun(test.spec, base)
		if !ok || !next.Equal(test.want) {
			t.Errorf("%q: got %v %v, want %v", test.spec, next, ok, test.want)
		}
	}
	if _, ok := sumNextRun("61 * * * *", base); ok {
		t.Error("无效的表达式应解析失败")
	}
}

// TestSumIntervalCron 检查简写间隔和原样接受的 Cron。
func TestSumIntervalCron(t *testing.T) {
	valid := map[string]string{
		"30m": "*/30 * * * *", "2h": "0 */2 * * *", "1d": "0 0 * * *",
		"0 0 */2 * * *": "0 0 */2 * * *", "30  */2 * * *": "30 */2 * * *",
	}
	for interval, want := range valid {
		if got, err := sumIntervalCron(interval); err != nil || got != want {
			t.Errorf("%q: got %q %v, want %q", interval, got, err, want)
		}
	}
	for _, interval := range []string{"7m", "5h", "2d", "abc", "0 0 99 * * *", "1 2 3"} {
		if _, err := sumIntervalCron(interval); err == nil {
			t.Errorf("%q 应报错", interval)
		}
	}
}

// TestSumAddSyntax 检查 add 的参数拆分：简写间隔、带引号的 Cron、直接写开的五/六字段 Cron。
func TestSumAddSyntax(t *testing.T) {
	cases := []struct {
		args             []string
		target, interval string
		rest             []string
	}{
		{[]string{"here", "2h", "100"}, "here", "2h", []string{"100"}},
		{[]string{"@g", "30 */2 * * *", "--spoiler"}, "@g", "30 */2 * * *", []string{"--spoiler"}},
		{[]string{"here", "0", "0", "9,21", "*", "*", "*", "200"}, "here", "0 0 9,21 * * *", []string{"200"}},
		{[]string{"here", "30", "*/2", "*", "*", "*", "100"}, "here", "30 */2 * * *", []string{"100"}},
		{[]string{"here", "0", "9", "*", "*", "MON-FRI", "--time", "12"}, "here", "0 9 * * MON-FRI", []string{"--time", "12"}},
		{[]string{"here"}, "here", "", []string{}},
	}
	for _, test := range cases {
		target, interval, rest := sumAddSyntax(test.args)
		if target != test.target || interval != test.interval || strings.Join(rest, ",") != strings.Join(test.rest, ",") {
			t.Errorf("%v: got %q %q %v", test.args, target, interval, rest)
		}
	}
	inv := &command.Invocation{Prefix: ".", Text: `.sum add here "0 0 */2 * * *" 100 '备注 一'` + "\n第二行", Args: []string{"add", "here"}}
	if got := sumAddArguments(inv); strings.Join(got, "|") != "here|0 0 */2 * * *|100|备注 一" {
		t.Errorf("引号切词不对：%q", got)
	}
}

// TestSumAddOptions 检查 --time、--provider、--spoiler/--no-spoiler、消息数和备注。
func TestSumAddOptions(t *testing.T) {
	options, err := sumParseAddOptions([]string{"200", "--time", "12", "--provider", "sum-main", "--no-spoiler", "早报", "群"})
	if err != nil || options.count != 200 || options.timeRange != 12 || options.provider != "sum-main" || options.spoiler == nil || *options.spoiler || options.remark != "早报 群" {
		t.Fatalf("got %+v %v", options, err)
	}
	for _, rest := range [][]string{{"--time"}, {"--time", "0"}, {"--time", "721"}, {"--provider"}} {
		if _, err := sumParseAddOptions(rest); err == nil {
			t.Errorf("%v 应报错", rest)
		}
	}
}

// TestSumTargetKind 检查目标的规整：数字 ID、私有群链接、公开链接、@用户名和邀请链接。
func TestSumTargetKind(t *testing.T) {
	cases := []struct {
		target, want string
		invite       bool
	}{
		{"here", "here", false},
		{"-1001234", "-1001234", false},
		{"https://t.me/c/1234/56", "-1001234", false},
		{"t.me/somegroup", "@somegroup", false},
		{"https://t.me/somegroup/12", "@somegroup", false},
		{"@somegroup", "@somegroup", false},
		{"somegroup", "@somegroup", false},
		{"https://t.me/+AbC_d-1", "AbC_d-1", true},
		{"t.me/joinchat/XyZ", "XyZ", true},
	}
	for _, test := range cases {
		got, invite := sumTargetKind(test.target)
		if got != test.want || invite != test.invite {
			t.Errorf("%q: got %q %v", test.target, got, invite)
		}
	}
}

// TestSumMessageText 检查发给 AI 的每行都带时间，附件注明文件名。
func TestSumMessageText(t *testing.T) {
	date := int(time.Date(2026, 9, 24, 9, 5, 0, 0, time.Local).Unix())
	if got := sumMessageText(date, "小明", "你好", ""); got != "[2026-09-24 09:05] 小明: 你好" {
		t.Errorf("got %q", got)
	}
	if got := sumMessageText(date, "小明", "看这个", "a.pdf"); got != "[2026-09-24 09:05] 小明: 看这个 [文件: a.pdf]" {
		t.Errorf("got %q", got)
	}
	if got := sumMessageText(date, "小明", "", "[图片]"); got != "[2026-09-24 09:05] 小明: [文件: [图片]]" {
		t.Errorf("got %q", got)
	}
}

// v2Database 是 MiBox 新版 sum 迁移后的数据：服务商挪进了 ai，任务存 ai 标签。
const v2Database = `{"seq":"3","tasks":[
 {"id":"1","cron":"0 0 */2 * * *","chatId":"-1001","interval":"2h","messageCount":100,"pushTarget":"me","aiProvider":"sum-main","useSpoiler":false,"createdAt":"2026-09-01T00:00:00.000Z","lastRunAt":"2026-09-20T01:02:03.000Z","lastError":"总结执行失败"},
 {"id":"3","cron":"0 */30 * * * *","chatId":"-1002","interval":"30m","messageCount":50,"timeRange":6,"useSpoiler":true,"createdAt":"2026-09-02T00:00:00.000Z"}],
 "aiConfig":{"providers":{},"default_prompt":"p","default_spoiler":false,"max_output_length":0,"link_preview":false,"aiMigrated":true}}`

// TestSumV2Data 检查 MiBox 新版数据：能读、写回时不丢 timeRange，服务商按 ai 标签解析。
func TestSumV2Data(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sum.json")
	if err := os.WriteFile(path, []byte(v2Database), 0o600); err != nil {
		t.Fatal(err)
	}
	service := &sumService{store: store.New(path, sumDefaults)}
	db, err := service.read()
	if err != nil {
		t.Fatal(err)
	}
	if db.Tasks[1].TimeRange != 6 || !db.AIConfig.AIMigrated || string(db.Seq) != "3" {
		t.Fatalf("读取不对：%+v", db)
	}
	if err := service.update(func(db *sumDB) error { db.Tasks[0].Remark = "x"; return nil }); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"timeRange": 6`) {
		t.Fatalf("写回时丢了 timeRange：%s", raw)
	}
	for _, task := range db.Tasks {
		if _, ok := sumNextRun(task.Cron, time.Now()); !ok {
			t.Errorf("任务 %s 的六字段 Cron 应能调度：%q", task.ID, task.Cron)
		}
	}
	aiTags := map[string]bool{"sum-main": true, "main": true}
	has := func(tag string) bool { return aiTags[tag] }
	backend, err := sumResolveBackend(db, db.Tasks[0].AIProvider, true, has)
	if err != nil || backend.own != nil || backend.aiTag != "sum-main" {
		t.Fatalf("任务的 ai 标签应交给 ai：%+v %v", backend, err)
	}
	backend, err = sumResolveBackend(db, "", true, has)
	if err != nil || backend.own != nil || backend.aiTag != "" {
		t.Fatalf("没指定时用 ai 当前的聊天模型：%+v %v", backend, err)
	}
	if _, err := sumResolveBackend(db, "gone", true, has); err == nil {
		t.Fatal("找不到的名称应报错")
	}
	if _, err := sumResolveBackend(db, "", false, has); err == nil {
		t.Fatal("没有任何配置时应报错")
	}
	if last, ok := sumParseTime(db.Tasks[0].LastRunAt); !ok || last.UTC().Hour() != 1 {
		t.Fatalf("RFC3339 的 lastRunAt 读不出：%v", last)
	}
	if last, ok := sumParseTime("1758330000000"); !ok || last.Unix() != 1758330000 {
		t.Fatalf("毫秒时间戳的 lastRunAt 读不出：%v", last)
	}
}

// TestSumResolveOwnProvider 检查 sum 自己的服务商优先，缺 Key 时交给 ai。
func TestSumResolveOwnProvider(t *testing.T) {
	db := sumDefaults()
	db.AIConfig.Providers["a"] = sumProvider{Name: "a", APIKey: "k", Model: "m"}
	db.AIConfig.Providers["empty"] = sumProvider{Name: "empty", Model: "m"}
	db.AIConfig.DefaultProvider = "a"
	none := func(string) bool { return false }
	if backend, err := sumResolveBackend(db, "", true, none); err != nil || backend.own == nil || backend.name != "a" {
		t.Fatalf("got %+v %v", backend, err)
	}
	db.AIConfig.DefaultProvider = "empty"
	if backend, err := sumResolveBackend(db, "", true, none); err != nil || backend.own != nil {
		t.Fatalf("默认服务商缺 Key 时应改用 ai：%+v %v", backend, err)
	}
	if _, err := sumResolveBackend(db, "empty", false, none); err == nil {
		t.Fatal("没有 ai 时缺 Key 应报错")
	}
}

// TestRemoveProvider 检查删除服务商时，用它的任务改回全局默认。
func TestRemoveProvider(t *testing.T) {
	db := sumDefaults()
	db.AIConfig.Providers["a"] = sumProvider{Name: "a"}
	db.AIConfig.Providers["b"] = sumProvider{Name: "b"}
	db.AIConfig.DefaultProvider = "a"
	db.Tasks = []sumTask{{ID: "1", AIProvider: "a"}, {ID: "2", AIProvider: "b"}, {ID: "3", AIProvider: "a"}}
	reset := removeProvider(&db, "a")
	if strings.Join(reset, ",") != "1,3" || db.AIConfig.DefaultProvider != "" || db.Tasks[1].AIProvider != "b" || db.Tasks[0].AIProvider != "" {
		t.Fatalf("got %v %+v", reset, db)
	}
	if _, ok := db.AIConfig.Providers["a"]; ok {
		t.Fatal("服务商没删掉")
	}
}

// TestSumCallAIErrorDetail 检查服务商的错误说明会显示出来，密钥打码。
func TestSumCallAIErrorDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "Incorrect API key provided: sk-live-abcdef"}})
	}))
	defer server.Close()
	provider := sumProvider{Name: "a", BaseURL: server.URL, APIKey: "sk-live-abcdef", Model: "gpt-4o", Type: "chat"}
	_, err := sumCallAI(context.Background(), provider, "消息", "提示", "auto", "auto", 5*time.Second)
	message, ok := kit.IsUserError(err)
	if !ok || message != "AI 调用失败：AI 接口返回 HTTP 401：Incorrect API key provided: ***" {
		t.Fatalf("got %q", message)
	}
	if sumErrorText(err) != message {
		t.Fatal("定时任务应记下同样的错误说明")
	}
}

// TestSumTimeout 检查超时的换算：毫秒原样，10-300 视为秒。
func TestSumTimeout(t *testing.T) {
	db := sumDefaults()
	for value, want := range map[int]time.Duration{60000: time.Minute, 120: 2 * time.Minute, 0: time.Minute} {
		db.AIConfig.DefaultTimeout = value
		if got := sumTimeout(db); got != want {
			t.Errorf("%d: got %v", value, got)
		}
	}
	if pushTarget(db, sumTask{}) != "me" {
		t.Fatal("没有推送目标时应推到收藏夹")
	}
	db.DefaultPushTarget = "@channel"
	if pushTarget(db, sumTask{}) != "@channel" || pushTarget(db, sumTask{PushTarget: "-100"}) != "-100" {
		t.Fatal("推送目标的优先级不对")
	}
}
