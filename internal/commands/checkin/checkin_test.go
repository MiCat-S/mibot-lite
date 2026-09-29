package checkin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

func TestParseClockAndMatcher(t *testing.T) {
	for value, want := range map[string]int{"10:00": 600, "9:05": 545, "23:59": 1439, "0:00": 0, "10：30": 630} {
		if got, ok := parseClock(value); !ok || got != want {
			t.Errorf("parseClock(%q) = %d %v，应为 %d", value, got, ok, want)
		}
	}
	for _, bad := range []string{"", "24:00", "10:60", "10:5", "123:00", "ab:cd", "10"} {
		if _, ok := parseClock(bad); ok {
			t.Errorf("parseClock(%q) 应该不合法", bad)
		}
	}
	for raw, want := range map[string][2]string{
		"":             {"", ""},
		"data:checkin": {"checkin", ""},
		"text: 签到":     {"", "签到"},
		"daily_sign":   {"daily_sign", ""},
	} {
		data, text := parseMatcher(raw)
		if data != want[0] || text != want[1] {
			t.Errorf("parseMatcher(%q) = %q %q", raw, data, text)
		}
	}
}

// 每天只挑一次；结束早于开始是跨午夜。
func TestPick(t *testing.T) {
	fixed := config{RunTime: "10:00"}
	if got := pick(fixed, func(int) int { t.Fatal("固定时间不该随机"); return 0 }); got != "10:00" {
		t.Errorf("固定时间：%s", got)
	}
	ranged := config{RunTime: "10:00", RunTimeEnd: "11:30"}
	if got := pick(ranged, func(n int) int {
		if n != 90 {
			t.Errorf("范围应是 90 分钟，得到 %d", n)
		}
		return 37
	}); got != "10:37" {
		t.Errorf("范围内：%s", got)
	}
	overnight := config{RunTime: "22:00", RunTimeEnd: "02:00"}
	if got := pick(overnight, func(n int) int { return n - 1 }); got != "01:59" {
		t.Errorf("跨午夜：%s", got)
	}
}

func signButton(text, data string) tg.ReplyMarkupClass {
	return &tg.ReplyInlineMarkup{Rows: []tg.KeyboardInlineButtonRow{{Buttons: []tg.KeyboardInlineButton{
		{Text: "帮助", Type: &tg.InlineButtonTypeCallback{Data: []byte("help")}},
		{Text: text, Type: &tg.InlineButtonTypeCallback{Data: []byte(data)}},
	}}}}
}

func TestButtonData(t *testing.T) {
	message := &tg.Message{ID: 1, ReplyMarkup: signButton("每日签到", "checkin")}
	if data, ok := buttonData(message, signTarget{CallbackData: "checkin"}); !ok || string(data) != "checkin" {
		t.Error("按回调数据没找到")
	}
	if data, ok := buttonData(message, signTarget{ButtonText: "每日签到"}); !ok || string(data) != "checkin" {
		t.Error("按按钮文字没找到")
	}
	if _, ok := buttonData(message, signTarget{CallbackData: "nope"}); ok {
		t.Error("不存在的回调数据不该找到")
	}
	if _, ok := buttonData(&tg.Message{ID: 2}, signTarget{CallbackData: "checkin"}); ok {
		t.Error("没有键盘的消息不该找到")
	}
}

// fakeBot 冒充 Telegram 和对话里的签到机器人 @sign_bot（ID 42）：收到签到命令就回一条
// 消息，reply 决定回什么；有人点回调按钮时用 answer 答复。
type fakeBot struct {
	mu       sync.Mutex
	messages []*tg.Message
	sent     []string
	clicked  []string
	nextID   int
	reply    func(id int) *tg.Message
	answer   func(data string) (*tg.MessagesBotCallbackAnswer, error)
}

func respond(output bin.Decoder, value bin.Encoder) error {
	var buffer bin.Buffer
	if err := value.Encode(&buffer); err != nil {
		return err
	}
	return output.Decode(&buffer)
}

var signBot = &tg.User{ID: 42, AccessHash: 7, Bot: true, Username: "sign_bot"}

func (f *fakeBot) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch request := input.(type) {
	case *tg.ContactsResolveUsernameRequest:
		if request.Username != "sign_bot" {
			return tgerr.New(400, "USERNAME_NOT_OCCUPIED")
		}
		signBot.SetFlags()
		return respond(output, &tg.ContactsResolvedPeer{Peer: &tg.PeerUser{UserID: 42}, Users: []tg.UserClass{signBot}})
	case *tg.MessagesSendMessageRequest:
		f.nextID++
		id := f.nextID
		f.sent = append(f.sent, request.Message)
		f.messages = append(f.messages, &tg.Message{ID: id, Out: true, PeerID: &tg.PeerUser{UserID: 42}, Message: request.Message})
		if f.reply != nil {
			if reply := f.reply(id); reply != nil {
				f.nextID = reply.ID
				f.messages = append(f.messages, reply)
			}
		}
		return respond(output, &tg.UpdateShortSentMessage{ID: id, Date: int(time.Now().Unix())})
	case *tg.MessagesGetHistoryRequest:
		var history []tg.MessageClass
		for index := len(f.messages) - 1; index >= 0; index-- {
			history = append(history, f.messages[index])
		}
		return respond(output, &tg.MessagesMessages{Messages: history, Users: []tg.UserClass{signBot}})
	case *tg.MessagesGetBotCallbackAnswerRequest:
		f.clicked = append(f.clicked, string(request.Data))
		answer, err := f.answer(string(request.Data))
		if err != nil {
			return err
		}
		return respond(output, answer)
	}
	return tgerr.New(400, "UNEXPECTED_REQUEST")
}

// edit 模拟机器人把一条消息改成新的内容。
func (f *fakeBot) edit(id int, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, message := range f.messages {
		if message.ID == id {
			message.Message = text
			message.SetEditDate(int(time.Now().Unix()) + 1)
		}
	}
}

func fakeClient(t *testing.T, fake *fakeBot) *bot.Client {
	t.Helper()
	replyWait, pollEvery, gap = 300*time.Millisecond, 10*time.Millisecond, time.Millisecond
	t.Cleanup(func() { replyWait, pollEvery, gap = 10*time.Second, time.Second, 2*time.Second })
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	return bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func botMessage(id int, text string, markup tg.ReplyMarkupClass) *tg.Message {
	message := &tg.Message{ID: id, PeerID: &tg.PeerUser{UserID: 42}, Message: text, Date: int(time.Now().Unix())}
	if markup != nil {
		message.SetReplyMarkup(markup)
	}
	message.SetFromID(&tg.PeerUser{UserID: 42})
	return message
}

// 不用点按钮的：对方的回复就是结果。
func TestSignOnePlain(t *testing.T) {
	fake := &fakeBot{nextID: 10, reply: func(id int) *tg.Message { return botMessage(id+1, "签到成功\n获得 5 积分", nil) }}
	client := fakeClient(t, fake)
	reply, err := signOne(context.Background(), client, signTarget{ID: "a", Name: "A", Target: "@sign_bot", Command: "/sign 1.2.3.4 abc"})
	if err != nil || reply != "签到成功\n获得 5 积分" {
		t.Fatalf("%q %v", reply, err)
	}
	// 签到命令原样发出，里面的 IP 不打码。
	if len(fake.sent) != 1 || fake.sent[0] != "/sign 1.2.3.4 abc" {
		t.Errorf("发出去的命令：%q", fake.sent)
	}
	if got := brief(reply); got != "签到成功 获得 5 积分" {
		t.Errorf("汇总里的回复：%q", got)
	}
}

// 要点按钮的：点对回调数据，机器人弹出来的答复就是结果。
func TestSignOneClicksButton(t *testing.T) {
	fake := &fakeBot{nextID: 10,
		reply: func(id int) *tg.Message {
			return botMessage(id+1, "请点按钮签到", signButton("每日签到", "checkin"))
		},
		answer: func(data string) (*tg.MessagesBotCallbackAnswer, error) {
			answer := &tg.MessagesBotCallbackAnswer{CacheTime: 0}
			answer.SetMessage("签到成功 +10")
			return answer, nil
		}}
	client := fakeClient(t, fake)
	reply, err := signOne(context.Background(), client, signTarget{Target: "@sign_bot", Command: "/start", ButtonText: "每日签到"})
	if err != nil || reply != "签到成功 +10" {
		t.Fatalf("%q %v", reply, err)
	}
	if len(fake.clicked) != 1 || fake.clicked[0] != "checkin" {
		t.Errorf("点的按钮：%q", fake.clicked)
	}
}

// 机器人没在时限里答复按钮，而是把带按钮的消息改成了结果：读改过的那条。
func TestSignOneReadsEditedMessage(t *testing.T) {
	fake := &fakeBot{nextID: 10, reply: func(id int) *tg.Message { return botMessage(id+1, "请点按钮", signButton("签到", "checkin")) }}
	fake.answer = func(string) (*tg.MessagesBotCallbackAnswer, error) {
		go func() {
			time.Sleep(30 * time.Millisecond)
			fake.edit(12, "今日已签到，连续 3 天")
		}()
		return nil, tgerr.New(400, "BOT_RESPONSE_TIMEOUT")
	}
	client := fakeClient(t, fake)
	reply, err := signOne(context.Background(), client, signTarget{Target: "@sign_bot", Command: "/start", CallbackData: "checkin"})
	if err != nil || reply != "今日已签到，连续 3 天" {
		t.Fatalf("%q %v", reply, err)
	}
}

func TestSignOneFailures(t *testing.T) {
	client := fakeClient(t, &fakeBot{nextID: 10})
	for name, c := range map[string]struct {
		target signTarget
		want   string
	}{
		"没有回复":  {signTarget{Target: "@sign_bot", Command: "/sign"}, "秒内没收到回复"},
		"没有按钮":  {signTarget{Target: "@sign_bot", Command: "/sign", CallbackData: "checkin"}, "秒内没收到带这个按钮的回复（回调数据 checkin）"},
		"找不到目标": {signTarget{Target: "@nobody_here", Command: "/sign"}, "找不到目标 @nobody_here（USERNAME_NOT_OCCUPIED）"},
	} {
		_, err := signOne(context.Background(), client, c.target)
		if message, ok := kit.IsUserError(err); !ok || !strings.Contains(message, c.want) {
			t.Errorf("%s：%v，应包含 %q", name, err, c.want)
		}
	}
}

// 定时：没到今天挑好的时刻不签；到了签一次、记下日期；同一天不再签。汇总没地方发时进收藏夹。
func TestTickRunsOncePerDay(t *testing.T) {
	fake := &fakeBot{nextID: 10, reply: func(id int) *tg.Message { return botMessage(id+1, "OK", nil) }}
	client := fakeClient(t, fake)
	s := &service{store: store.New(filepath.Join(t.TempDir(), "checkin.json"), defaults)}
	if err := s.store.Update(func(cfg *config) error {
		cfg.RunTime, cfg.RunTimeEnd = "10:00", ""
		cfg.Targets = []signTarget{{ID: "a", Name: "A", Target: "@sign_bot", Command: "/sign", Enabled: true},
			{ID: "b", Name: "B", Target: "@sign_bot", Command: "/off", Enabled: false}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 9, 29, 0, 0, 0, 0, shanghai)
	s.tick(context.Background(), client, day.Add(9*time.Hour+59*time.Minute))
	if len(fake.sent) != 0 {
		t.Fatalf("没到时间就签了：%q", fake.sent)
	}
	cfg, _ := s.store.Read()
	if cfg.PlannedDate != "2026/9/29" || cfg.CurrentRunTime != "10:00" {
		t.Errorf("今天的计划没存下来：%+v", cfg)
	}
	// 服务在 10:00 那一分钟停着，10:07 补上。
	s.tick(context.Background(), client, day.Add(10*time.Hour+7*time.Minute))
	s.tick(context.Background(), client, day.Add(10*time.Hour+8*time.Minute))
	var signs []string
	for _, text := range fake.sent {
		if !strings.Contains(text, "签到汇总") {
			signs = append(signs, text)
		}
	}
	if len(signs) != 1 || signs[0] != "/sign" {
		t.Fatalf("应只签一次开启的目标：%q", fake.sent)
	}
	if last := fake.sent[len(fake.sent)-1]; !strings.Contains(last, "签到汇总") || !strings.Contains(last, "成功 1 个，失败 0 个") || !strings.Contains(last, "✅ A：OK") {
		t.Errorf("汇总不对：%q", last)
	}
	if cfg, _ := s.store.Read(); cfg.LastRunDate != "2026/9/29" {
		t.Errorf("日期没记下来：%+v", cfg)
	}
}

// 机器人推送：地址里带 Token，失败时的错误不带。
func TestPushViaBot(t *testing.T) {
	var got struct {
		path string
		body map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		if got.body["chat_id"] == "bad" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: chat not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	botAPI = server.URL
	defer func() { botAPI = "https://api.telegram.org" }()

	if err := pushViaBot(context.Background(), "123:SECRET", "-1001", []string{"<b>汇总</b>"}); err != nil {
		t.Fatal(err)
	}
	if got.path != "/bot123:SECRET/sendMessage" || got.body["text"] != "<b>汇总</b>" || got.body["parse_mode"] != "HTML" {
		t.Errorf("请求不对：%s %v", got.path, got.body)
	}
	err := pushViaBot(context.Background(), "123:SECRET", "bad", []string{"x"})
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("失败时的错误：%v", err)
	}
}
