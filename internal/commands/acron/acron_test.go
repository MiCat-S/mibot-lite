package acron

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/robfig/cron/v3"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

func TestParseCron(t *testing.T) {
	for _, good := range []string{"0 0 2 * * *", "*/30 * * * * *", "0 30 9 * * 1-5", "0 0 9 * * 7", "0 0 9 * * 1-7", "0 0 0 1 JAN SUN"} {
		if _, err := parseCron(good); err != nil {
			t.Errorf("parseCron(%q)：%v", good, err)
		}
	}
	for _, bad := range []string{"0 2 * * *", "@daily", "0 0 2 * * * *", "0 0 25 * * *", "a b c d e f", ""} {
		if _, err := parseCron(bad); err == nil {
			t.Errorf("parseCron(%q) 应该不合法", bad)
		}
	}
	for field, want := range map[string]string{"7": "0", "1-7": "1-6,0", "1,7": "1,0", "5-7/2": "5-7/2", "*": "*", "17": "17"} {
		if got := sundaySeven(field); got != want {
			t.Errorf("sundaySeven(%q) = %q，应为 %q", field, got, want)
		}
	}
	// 按北京时间算：北京时间 9 月 30 日（星期三）10:00 之后。
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, shanghai).UTC()
	for expression, want := range map[string]string{
		"0 0 2 * * *":   "2026-10-01 02:00",
		"0 0 9 * * 7":   "2026-10-04 09:00",
		"30 0 10 * * *": "2026-09-30 10:00:30",
	} {
		next, ok := nextRun(expression, now)
		if !ok || formatTime(next) != want {
			t.Errorf("nextRun(%q) = %s %v，应为 %s", expression, formatTime(next), ok, want)
		}
	}
}

func TestCompileRegex(t *testing.T) {
	for raw, cases := range map[string]map[string]bool{
		"/^test/i":   {"TEST 1": true, "a test": false},
		"^abc":       {"abcd": true, "ABC": false},
		"/a.b/s":     {"a\nb": true},
		"/^x$/m":     {"a\nx\nb": true},
		"/\\u4f60/g": {"你好": true, "好": false},
		"/^$/":       {"": true, "x": false},
	} {
		pattern, err := compileRegex(raw)
		if err != nil {
			t.Errorf("compileRegex(%q)：%v", raw, err)
			continue
		}
		for text, want := range cases {
			if got := pattern.MatchString(text); got != want {
				t.Errorf("%s 匹配 %q = %v，应为 %v", raw, text, got, want)
			}
		}
	}
	for _, bad := range []string{"/a/x", "(?=a)", "(a)\\1", "", "//i"} {
		if _, err := compileRegex(bad); err == nil {
			t.Errorf("compileRegex(%q) 应该报错", bad)
		} else if _, ok := kit.IsUserError(err); !ok {
			t.Errorf("compileRegex(%q) 的错误应是给用户看的：%v", bad, err)
		}
	}
}

func TestHelpers(t *testing.T) {
	for value, want := range map[string][2]string{"@a": {"@a", ""}, "@a|12": {"@a", "12"}, "-1001｜5": {"-1001", "5"}, "|": {"", ""}} {
		if chat, reply := splitTarget(value); chat != want[0] || reply != want[1] {
			t.Errorf("splitTarget(%q) = %q %q", value, chat, reply)
		}
	}
	line := ".acron del 0 0 2 * * * me 12   早上  清理 "
	if got := remarkAfter(strings.TrimPrefix(line, "."), 10); got != "早上  清理" {
		t.Errorf("备注：%q", got)
	}
	if got := remarkAfter("acron send 0 0 2 * * * me", 9); got != "" {
		t.Errorf("没有备注：%q", got)
	}
	for value, want := range map[string]bool{"1": true, "TRUE": true, "y": true, "yes": true, "0": false, "no": false, "": false} {
		if parseFlag(value) != want {
			t.Errorf("parseFlag(%q)", value)
		}
	}
	// 复制出来的命令和 V2 的 buildCopy 一样。
	for _, c := range []struct {
		task task
		want string
	}{
		{task{Type: "send", Cron: "0 0 2 * * *", Chat: "@a", ReplyTo: "5", Remark: "早安"}, ".acron send 0 0 2 * * * @a|5 早安"},
		{task{Type: "cmd", Cron: "0 0 2 * * *", Chat: "me", Message: ".bf"}, ".acron cmd 0 0 2 * * * me\n.bf"},
		{task{Type: "del_re", Cron: "0 0 2 * * *", Chat: "me", Limit: "100", Regex: "/^x/i"}, ".acron del_re 0 0 2 * * * me 100 /^x/i"},
		{task{Type: "pin", Cron: "0 0 2 * * *", Chat: "me", MsgID: "9", Notify: true}, ".acron pin 0 0 2 * * * me 9 1 0"},
		{task{Type: "unpin", Cron: "0 0 2 * * *", Chat: "me", MsgID: "9", Remark: "r"}, ".acron unpin 0 0 2 * * * me 9 r"},
	} {
		if got := copyCommand(c.task, "."); got != c.want {
			t.Errorf("copyCommand = %q，应为 %q", got, c.want)
		}
	}
	if got := copyHTML(task{Type: "cmd", Cron: "0 0 2 * * *", Chat: "me", Message: ".a <b>"}, "."); got != "<pre>.acron cmd 0 0 2 * * * me\n.a &lt;b&gt;</pre>" {
		t.Errorf("多行的复制命令：%q", got)
	}
}

// v2Entities 是 teleproto 把几种格式实体 JSON.stringify 之后的原样输出。
const v2Entities = `[{"offset":0,"length":2,"className":"MessageEntityBold"},{"offset":2,"length":3,"url":"https://x.y","className":"MessageEntityTextUrl"},{"offset":5,"length":2,"documentId":"5368324170671202286","className":"MessageEntityCustomEmoji"},{"offset":7,"length":1,"userId":"12345","className":"MessageEntityMentionName"},{"offset":8,"length":1,"language":"go","className":"MessageEntityPre"},{"offset":9,"length":1,"collapsed":true,"className":"MessageEntityBlockquote"}]`

func TestEntitiesRoundTrip(t *testing.T) {
	entities := decodeEntities(json.RawMessage(v2Entities))
	if len(entities) != 6 {
		t.Fatalf("读回 %d 个实体：%#v", len(entities), entities)
	}
	if emoji, ok := entities[2].(*tg.MessageEntityCustomEmoji); !ok || emoji.DocumentID != 5368324170671202286 {
		t.Errorf("自定义表情：%#v", entities[2])
	}
	if got := string(encodeEntities(entities)); got != v2Entities {
		t.Errorf("写回的和 V2 的不一样：\n%s\n%s", got, v2Entities)
	}
	// 坏条目跳过，数字写成数字也认。
	mixed := `[{"className":"Nope","offset":0,"length":1},{"className":"MessageEntityItalic","offset":0},{"className":"MessageEntityMentionName","offset":1,"length":2,"userId":42}]`
	entities = decodeEntities(json.RawMessage(mixed))
	if len(entities) != 1 || entities[0].(*tg.MessageEntityMentionName).UserID != 42 {
		t.Errorf("混合数据：%#v", entities)
	}
	// 提及只在缓存里有这个人时才发出去。
	peers := bot.NewPeerCache()
	peers.RememberUsers([]tg.UserClass{&tg.User{ID: 42, AccessHash: 7}})
	input := inputEntities(peers, []tg.MessageEntityClass{&tg.MessageEntityBold{Length: 1},
		&tg.MessageEntityMentionName{Offset: 1, Length: 2, UserID: 42}, &tg.MessageEntityMentionName{UserID: 99, Length: 1}})
	if len(input) != 2 {
		t.Fatalf("发送用的实体：%#v", input)
	}
	if mention, ok := input[1].(*tg.InputMessageEntityMentionName); !ok || mention.UserID.(*tg.InputUser).AccessHash != 7 {
		t.Errorf("提及：%#v", input[1])
	}
}

// v2Data 是 MiBox V2 的 assets/acron/acron_config.json 的样子，其中一个任务的 id 是旧版的数字。
const v2Data = `{"schemaVersion":1,"seq":"4","tasks":[
{"id":"1","type":"send","cron":"0 0 8 * * *","chat":"me","chatId":"1","resolvedPeer":true,"createdAt":"1759000000000","delivery":"sent","display":"<code>me</code>","message":"早上好","entities":` + v2Entities + `,"remark":"早安","lastRunAt":"1759100000000","lastResult":"已发送 1 条消息"},
{"id":2,"type":"pin","cron":"0 0 9 * * 1-5","chat":"@friend","chatId":42,"resolvedPeer":true,"createdAt":"1759000000001","delivery":"pending","msgId":"77","notify":true,"pmOneSide":false,"disabled":true},
{"id":"3","type":"del_re","cron":"0 */5 * * * *","chat":"-100100","createdAt":"1759000000002","limit":"50","regex":"/广告/i","lastError":"DEL_RE_TIMEOUT"},
{"id":"4","type":"forward","cron":"0 0 12 * * *","chat":"-100100","chatId":"-100100","resolvedPeer":true,"replyTo":"3","fromChatId":"42","fromMsgId":"5","createdAt":"1759000000003","delivery":"prepared"}]}`

func TestImportV2Data(t *testing.T) {
	var st state
	if err := json.Unmarshal([]byte(v2Data), &st); err != nil {
		t.Fatal(err)
	}
	st.normalize()
	if len(st.Tasks) != 4 || st.Seq != "4" || st.nextID() != "5" {
		t.Fatalf("读出来的：%+v", st)
	}
	pin := st.Tasks[1]
	if pin.ID != "2" || pin.ChatID != "42" || !pin.Notify || !pin.Disabled || pin.MsgID != "77" {
		t.Errorf("数字 id 的任务：%+v", pin)
	}
	if st.Tasks[2].Delivery != deliveryPending || st.Tasks[3].Delivery != deliveryPrepared {
		t.Errorf("delivery：%q %q", st.Tasks[2].Delivery, st.Tasks[3].Delivery)
	}
	// 写回去的字段名和 V2 一样，格式实体原样保留。
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"schemaVersion":1`, `"seq":"4"`, `"id":"2"`, `"chatId":"42"`, `"resolvedPeer":true`, `"createdAt":"1759000000000"`,
		`"lastRunAt":"1759100000000"`, `"fromChatId":"42"`, `"fromMsgId":"5"`, `"replyTo":"3"`, `"msgId":"77"`, `"limit":"50"`,
		`"regex":"/广告/i"`, `"notify":true`, `"delivery":"prepared"`, `"entities":` + v2Entities} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("写回的数据里没有 %s", key)
		}
	}
	// 列表：开启的在前，关闭的在后；HTML 能被发送时的解析器接受。
	html := renderList(st.Tasks, true, "", ".", func(t task) string { return command.Code(t.Chat) }, time.UnixMilli(1759200000000))
	if _, _, err := bot.ParseHTML(html); err != nil {
		t.Fatalf("列表的 HTML：%v", err)
	}
	if strings.Index(html, "<b>关闭</b>") < strings.Index(html, "<code>4</code>") || !strings.Contains(html, "（4 个）") {
		t.Errorf("列表：\n%s", html)
	}
	for _, want := range []string{"复制：<code>.acron send 0 0 8 * * * me 早安</code>", "错误：DEL_RE_TIMEOUT", "话题：<a href=\"https://t.me/c/100/3\">3</a>",
		"源消息：<code>5</code>", "上次执行：2025-09-29"} {
		if !strings.Contains(html, want) {
			t.Errorf("列表里没有 %q：\n%s", want, html)
		}
	}
}

// fakeTelegram 冒充 Telegram：本账号 ID 1，好友 @friend（ID 42），频道 -100100。
// 每个请求都记下来，测试核对任务到底发了什么。
type fakeTelegram struct {
	mu       sync.Mutex
	nextID   int
	requests []bin.Encoder
	// stored 是按 ID 能读到的消息（GetMessages 用），history 是好友对话的历史（从旧到新）。
	stored  map[int]*tg.Message
	history []*tg.Message
	edits   []string
}

var friend = &tg.User{ID: 42, AccessHash: 7, Username: "friend", FirstName: "Friend"}

func respond(output bin.Decoder, value bin.Encoder) error {
	var buffer bin.Buffer
	if err := value.Encode(&buffer); err != nil {
		return err
	}
	return output.Decode(&buffer)
}

func (f *fakeTelegram) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, input)
	switch request := input.(type) {
	case *tg.ContactsResolveUsernameRequest:
		if request.Username != "friend" {
			return tgerr.New(400, "USERNAME_NOT_OCCUPIED")
		}
		return respond(output, &tg.ContactsResolvedPeer{Peer: &tg.PeerUser{UserID: 42}, Users: []tg.UserClass{friend}})
	case *tg.MessagesSendMessageRequest:
		f.nextID++
		return respond(output, &tg.UpdateShortSentMessage{ID: f.nextID, Date: int(time.Now().Unix())})
	case *tg.MessagesGetMessagesRequest:
		var found []tg.MessageClass
		for _, item := range request.ID {
			if message, ok := f.stored[item.(*tg.InputMessageID).ID]; ok {
				found = append(found, message)
			}
		}
		return respond(output, &tg.MessagesMessages{Messages: found, Users: []tg.UserClass{friend}})
	case *tg.MessagesGetHistoryRequest:
		var page []tg.MessageClass
		for index := len(f.history) - 1; index >= 0 && len(page) < request.Limit; index-- {
			if request.OffsetID == 0 || f.history[index].ID < request.OffsetID {
				page = append(page, f.history[index])
			}
		}
		return respond(output, &tg.MessagesMessages{Messages: page})
	case *tg.MessagesDeleteMessagesRequest:
		return respond(output, &tg.MessagesAffectedMessages{})
	case *tg.MessagesEditMessageRequest:
		f.edits = append(f.edits, request.Message)
		return respond(output, &tg.Updates{})
	case *tg.MessagesSendMediaRequest, *tg.MessagesForwardMessagesRequest, *tg.MessagesUpdatePinnedMessageRequest:
		return respond(output, &tg.Updates{})
	}
	return tgerr.New(400, "UNEXPECTED_REQUEST")
}

// sent 返回某一类请求，按发出的顺序。
func sent[T bin.Encoder](f *fakeTelegram) []T {
	f.mu.Lock()
	defer f.mu.Unlock()
	var found []T
	for _, request := range f.requests {
		if typed, ok := request.(T); ok {
			found = append(found, typed)
		}
	}
	return found
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func fakeClient(fake *fakeTelegram) *bot.Client {
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	peers.RememberChats([]tg.ChatClass{&tg.Channel{ID: 100, AccessHash: 9, Title: "频道"}})
	return bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 1}, discard)
}

// testService 建一个用临时文件存任务的 service，dispatched 收到 cmd 任务派发的消息。
// 注册表的前缀是「.」和「,」，有一个别名 ac → acron。
func testService(t *testing.T, tasks ...task) (*service, *[]*tg.Message) {
	t.Helper()
	var dispatched []*tg.Message
	registry := command.New([]string{".", ","}, discard)
	registry.SetAliases(map[string]string{"ac": "acron", "backup": "bf"})
	s := newService(nil, discard, store.New(filepath.Join(t.TempDir(), "acron.json"), defaults), registry, func(_ context.Context, message *tg.Message) bool {
		dispatched = append(dispatched, message)
		return strings.HasPrefix(message.Message, ".")
	})
	if err := s.store.Update(func(st *state) error { st.Tasks = tasks; return nil }); err != nil {
		t.Fatal(err)
	}
	return s, &dispatched
}

func taskByID(t *testing.T, s *service, id string) task {
	t.Helper()
	st, err := s.read()
	if err != nil {
		t.Fatal(err)
	}
	index := st.find(id)
	if index < 0 {
		t.Fatalf("没有任务 %s", id)
	}
	return st.Tasks[index]
}

// send：文字和格式原样发出，回复指定的消息；解析出来的对话 ID 记进任务。
func TestExecuteSend(t *testing.T) {
	fake := &fakeTelegram{nextID: 10}
	client := fakeClient(fake)
	s, _ := testService(t, task{ID: "1", Type: "send", Cron: "0 0 8 * * *", Chat: "@friend", ReplyTo: "5", Delivery: deliveryPending,
		Message: "早上好 1.2.3.4", Entities: json.RawMessage(`[{"offset":0,"length":3,"className":"MessageEntityBold"}]`)})
	s.execute(context.Background(), client, "1")
	messages := sent[*tg.MessagesSendMessageRequest](fake)
	if len(messages) != 1 {
		t.Fatalf("发出 %d 条", len(messages))
	}
	request := messages[0]
	reply, _ := request.ReplyTo.(*tg.InputReplyToMessage)
	if request.Message != "早上好 1.2.3.4" || len(request.Entities) != 1 || reply == nil || reply.ReplyToMsgID != 5 {
		t.Errorf("发送请求：%+v", request)
	}
	if peer, ok := request.Peer.(*tg.InputPeerUser); !ok || peer.UserID != 42 {
		t.Errorf("发往：%#v", request.Peer)
	}
	got := taskByID(t, s, "1")
	if got.Delivery != deliverySent || got.LastResult != "已发送 1 条消息" || got.LastError != "" || got.LastRunAt == "" ||
		got.ChatID != "42" || !got.ResolvedPeer {
		t.Errorf("执行后的任务：%+v", got)
	}
}

// cmd：命令发出去之后，照发出的样子派发给注册表；不是命令的记成「未执行」。
func TestExecuteCommand(t *testing.T) {
	fake := &fakeTelegram{nextID: 20}
	client := fakeClient(fake)
	s, dispatched := testService(t,
		task{ID: "1", Type: "cmd", Cron: "0 0 2 * * *", Chat: "me", ChatID: "1", ResolvedPeer: true, Message: ".bf", ReplyTo: "7"},
		task{ID: "2", Type: "cmd", Cron: "0 0 2 * * *", Chat: "me", ChatID: "1", ResolvedPeer: true, Message: "hello"})
	s.execute(context.Background(), client, "1")
	s.execute(context.Background(), client, "2")
	if len(*dispatched) != 2 {
		t.Fatalf("派发了 %d 条", len(*dispatched))
	}
	message := (*dispatched)[0]
	peer, _ := message.PeerID.(*tg.PeerUser)
	header, _ := message.ReplyTo.(*tg.MessageReplyHeader)
	if message.ID != 21 || !message.Out || peer == nil || peer.UserID != 1 || message.Message != ".bf" || header == nil || header.ReplyToMsgID != 7 {
		t.Errorf("派发的消息：%+v", message)
	}
	if request := sent[*tg.MessagesSendMessageRequest](fake)[0]; request.Message != ".bf" {
		t.Errorf("发出的命令：%q", request.Message)
	}
	if got := taskByID(t, s, "1"); got.LastResult != "已执行命令" || got.Delivery != deliverySent {
		t.Errorf("执行了的：%+v", got)
	}
	if got := taskByID(t, s, "2"); !strings.HasPrefix(got.LastResult, "已发送命令（未执行：") {
		t.Errorf("没执行的：%+v", got)
	}
}

// del_re：只看最近 limit 条，删掉文字匹配的；服务消息也算条数。
func TestExecuteDeleteMatching(t *testing.T) {
	message := func(id int, text string) *tg.Message {
		return &tg.Message{ID: id, PeerID: &tg.PeerUser{UserID: 42}, Message: text}
	}
	fake := &fakeTelegram{history: []*tg.Message{message(1, "广告 旧的"), message(2, "广告"), message(3, "正常"), message(4, "广告 AD"), message(5, "ad 广告")}}
	client := fakeClient(fake)
	s, _ := testService(t, task{ID: "1", Type: "del_re", Cron: "0 * * * * *", Chat: "@friend", Limit: "3", Regex: "/^广告/i"},
		task{ID: "2", Type: "del_re", Cron: "0 * * * * *", Chat: "@friend", Limit: "5000", Regex: "x"})
	s.execute(context.Background(), client, "1")
	deletes := sent[*tg.MessagesDeleteMessagesRequest](fake)
	if len(deletes) != 1 || !slices.Equal(deletes[0].ID, []int{4}) || !deletes[0].Revoke {
		t.Fatalf("删除请求：%+v", deletes)
	}
	if got := taskByID(t, s, "1"); got.LastResult != "匹配并删除 1 条" {
		t.Errorf("结果：%+v", got)
	}
	// 存下的条数超出范围时不截断，报错。
	s.execute(context.Background(), client, "2")
	if got := taskByID(t, s, "2"); got.LastError != "条数要在 1–1000 之间" || got.Delivery != deliveryPending {
		t.Errorf("条数超出范围：%+v", got)
	}
}

func TestExecutePinForwardCopy(t *testing.T) {
	photo := &tg.Message{ID: 5, PeerID: &tg.PeerUser{UserID: 42}, Message: "图片说明",
		Media: &tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 88, AccessHash: 3, FileReference: []byte{1}}}}
	text := &tg.Message{ID: 6, PeerID: &tg.PeerUser{UserID: 42}, Message: "纯文字", Entities: []tg.MessageEntityClass{&tg.MessageEntityItalic{Length: 1}}}
	fake := &fakeTelegram{stored: map[int]*tg.Message{5: photo, 6: text}}
	client := fakeClient(fake)
	client.Peers().RememberUsers([]tg.UserClass{friend})
	s, _ := testService(t,
		task{ID: "1", Type: "pin", Cron: "0 0 9 * * *", Chat: "-100100", ChatID: "-100100", ResolvedPeer: true, MsgID: "77", Notify: false, PmOneSide: true},
		task{ID: "2", Type: "unpin", Cron: "0 0 9 * * *", Chat: "-100100", ChatID: "-100100", ResolvedPeer: true, MsgID: "77"},
		task{ID: "3", Type: "forward", Cron: "0 0 9 * * *", Chat: "-100100", ChatID: "-100100", ResolvedPeer: true, ReplyTo: "3", FromChatID: "42", FromMsgID: "5"},
		task{ID: "4", Type: "copy", Cron: "0 0 9 * * *", Chat: "me", FromChatID: "42", FromMsgID: "5", ReplyTo: "9"},
		task{ID: "5", Type: "copy", Cron: "0 0 9 * * *", Chat: "me", FromChatID: "42", FromMsgID: "6"},
		task{ID: "6", Type: "pin", Cron: "0 0 9 * * *", Chat: "-100100", MsgID: "1", Delivery: deliveryPrepared})
	for _, id := range []string{"1", "2", "3", "4", "5", "6"} {
		s.execute(context.Background(), client, id)
	}
	pins := sent[*tg.MessagesUpdatePinnedMessageRequest](fake)
	if len(pins) != 2 || !pins[0].Silent || !pins[0].PmOneside || pins[0].Unpin || !pins[1].Unpin || pins[1].ID != 77 {
		t.Errorf("置顶请求：%+v", pins)
	}
	forwards := sent[*tg.MessagesForwardMessagesRequest](fake)
	if len(forwards) != 1 || forwards[0].TopMsgID != 3 || !slices.Equal(forwards[0].ID, []int{5}) {
		t.Errorf("转发请求：%+v", forwards)
	}
	media := sent[*tg.MessagesSendMediaRequest](fake)
	if len(media) != 1 || media[0].Message != "图片说明" {
		t.Fatalf("复制图片：%+v", media)
	}
	if input, ok := media[0].Media.(*tg.InputMediaPhoto); !ok || input.ID.(*tg.InputPhoto).ID != 88 {
		t.Errorf("按引用重发图片：%#v", media[0].Media)
	}
	if reply, _ := media[0].ReplyTo.(*tg.InputReplyToMessage); reply == nil || reply.ReplyToMsgID != 9 {
		t.Errorf("复制时回复：%#v", media[0].ReplyTo)
	}
	texts := sent[*tg.MessagesSendMessageRequest](fake)
	if len(texts) != 1 || texts[0].Message != "纯文字" || len(texts[0].Entities) != 1 {
		t.Errorf("复制文字：%+v", texts)
	}
	for id, want := range map[string]string{"1": "已置顶消息 77", "2": "已取消置顶消息 77", "3": "已转发 1 条消息", "4": "已复制发送 1 条消息", "5": "已复制发送 1 条消息"} {
		if got := taskByID(t, s, id); got.LastResult != want || got.Delivery != deliverySent {
			t.Errorf("任务 %s：%+v", id, got)
		}
	}
	// 上次执行到一半中断的任务：这次不执行，只清掉标记、记下原因。
	if got := taskByID(t, s, "6"); got.Delivery != deliveryPending || !strings.Contains(got.LastError, "跳过") || got.LastRunAt != "" {
		t.Errorf("中断过的任务：%+v", got)
	}
}

// 连上后只挂开着的、Cron 有效的任务；到点触发时用连上时拿到的客户端执行，同一个任务不重叠。
func TestStartAndFire(t *testing.T) {
	fake := &fakeTelegram{}
	client := fakeClient(fake)
	s, _ := testService(t, task{ID: "1", Type: "send", Cron: "0 0 8 * * *", Chat: "me", Message: "hi"},
		task{ID: "2", Type: "send", Cron: "0 8 * * *", Chat: "me", Message: "五段"},
		task{ID: "3", Type: "send", Cron: "0 0 8 * * *", Chat: "me", Message: "关着", Disabled: true})
	s.fire("1") // 还没连上：什么也不做
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.start(ctx, client); close(done) }()
	for deadline := time.Now().Add(2 * time.Second); ; {
		s.mu.Lock()
		ready := s.client != nil && len(s.entries) == 1
		_, first := s.entries["1"]
		s.mu.Unlock()
		if ready && first {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("没挂上任务：%v", s.entries)
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.mu.Lock()
	s.running["1"] = true
	s.mu.Unlock()
	s.fire("1") // 上一次还在跑：跳过
	s.mu.Lock()
	delete(s.running, "1")
	s.mu.Unlock()
	s.fire("1")
	if messages := sent[*tg.MessagesSendMessageRequest](fake); len(messages) != 1 || messages[0].Message != "hi" {
		t.Errorf("到点发出的：%+v", messages)
	}
	cancel()
	<-done
}

// cmd 任务的命令不能是 .acron 本身（换前缀、用别名也不行）：添加时拒绝；
// 从 MiBox 搬来的这种任务到时间也不执行，记下原因。
func TestCommandCannotInvokeItself(t *testing.T) {
	fake := &fakeTelegram{nextID: 10}
	client := fakeClient(fake)
	s, dispatched := testService(t, task{ID: "1", Type: "cmd", Cron: "0 * * * * *", Chat: "me", ChatID: "1", ResolvedPeer: true,
		Message: ".acron cmd 0 * * * * * me\n.acron ls"})
	for _, body := range []string{".acron ls", ",acron la", ".ac cmd 0 * * * * * me", ".acron"} {
		err := s.handle(context.Background(), invocation(client, ".acron cmd 0 0 2 * * * me\n"+body, 0))
		if message, ok := kit.IsUserError(err); !ok || !strings.Contains(message, "不能是 .acron") {
			t.Errorf("命令 %q：%v", body, err)
		}
	}
	// 别的命令照常能加，包括别名展开成别的命令的。
	for _, body := range []string{".help acron", ".backup", "acron ls"} {
		if err := s.handle(context.Background(), invocation(client, ".acron cmd 0 0 2 * * * me\n"+body, 0)); err != nil {
			t.Errorf("命令 %q 应该能加：%v", body, err)
		}
	}
	s.execute(context.Background(), client, "1")
	if len(*dispatched) != 0 || len(sent[*tg.MessagesSendMessageRequest](fake)) != 0 {
		t.Errorf("不该发出或执行：%v", *dispatched)
	}
	if got := taskByID(t, s, "1"); !strings.Contains(got.LastError, "命令是 .acron 本身") || got.Delivery != deliveryPending {
		t.Errorf("搬来的自调用任务：%+v", got)
	}
}

// 任务里 panic 了不能带倒进程：调度器接住，记一条错误日志，之后照常调度。
func TestCronRecoversPanic(t *testing.T) {
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	s := newService(nil, logger, store.New(filepath.Join(t.TempDir(), "acron.json"), defaults), nil, nil)
	ran := make(chan struct{}, 4)
	s.cron.Schedule(soon{}, cron.FuncJob(func() {
		ran <- struct{}{}
		panic("boom")
	}))
	s.cron.Start()
	defer s.cron.Stop()
	for range 2 {
		select {
		case <-ran:
		case <-time.After(2 * time.Second):
			t.Fatal("任务没有再次执行")
		}
	}
	time.Sleep(20 * time.Millisecond)
	if text := logs.String(); !strings.Contains(text, "acron.cron_panic") || !strings.Contains(text, "boom") {
		t.Errorf("没记下 panic：%s", text)
	}
}

// soon 是每隔 10 毫秒触发一次的调度，测试用。
type soon struct{}

func (soon) Next(now time.Time) time.Time { return now.Add(10 * time.Millisecond) }

// syncBuffer 是可以同时写和读的日志缓冲。
type syncBuffer struct {
	mu     sync.Mutex
	buffer strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// 找不到对话：记下给用户看的原因，下次照样执行。
func TestExecuteUnresolvable(t *testing.T) {
	fake := &fakeTelegram{}
	s, _ := testService(t, task{ID: "1", Type: "send", Cron: "0 0 8 * * *", Chat: "@nobody", Message: "x"})
	s.execute(context.Background(), fakeClient(fake), "1")
	if got := taskByID(t, s, "1"); got.LastError != "找不到对话 @nobody（USERNAME_NOT_OCCUPIED）" || got.Delivery != deliveryPending || got.ResolvedPeer {
		t.Errorf("%+v", got)
	}
}

// invocation 造一次在收藏夹里发的命令调用；replyTo 不为 0 时它回复那条消息。
func invocation(client *bot.Client, text string, replyTo int) *command.Invocation {
	message := &bot.Message{ID: 500, Peer: &tg.PeerUser{UserID: 1}, ChatID: "1", Text: text, Out: true, ReplyToID: replyTo}
	fields := strings.Fields(strings.TrimPrefix(text, "."))
	return &command.Invocation{Prefix: ".", Command: fields[0], Args: fields[1:], Text: text, Message: message, Client: client, Log: discard}
}

func TestAddListAndManage(t *testing.T) {
	source := &tg.Message{ID: 30, PeerID: &tg.PeerUser{UserID: 1}, Message: "早上好 https://example.com",
		Entities: []tg.MessageEntityClass{&tg.MessageEntityBold{Length: 3}}, Media: &tg.MessageMediaWebPage{Webpage: &tg.WebPageEmpty{ID: 1}}}
	photo := &tg.Message{ID: 31, PeerID: &tg.PeerUser{UserID: 1}, Media: &tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 1}}}
	fake := &fakeTelegram{stored: map[int]*tg.Message{30: source, 31: photo}}
	client := fakeClient(fake)
	s, _ := testService(t)
	ctx := context.Background()
	run := func(text string, replyTo int) error {
		return s.handle(ctx, invocation(client, text, replyTo))
	}
	lastEdit := func() string { return fake.edits[len(fake.edits)-1] }

	// 链接预览不算媒体（V2 会拒）。
	if err := run(".acron send 0 0 8 * * * me 早安 问候", 30); err != nil {
		t.Fatal(err)
	}
	if edit := lastEdit(); !strings.HasPrefix(edit, "✅ 已添加定时发送任务\nID：1\n对话：收藏夹") || !strings.Contains(edit, "备注：早安 问候") ||
		!strings.Contains(edit, "复制：.acron send 0 0 8 * * * me 早安 问候") {
		t.Errorf("添加 send：\n%s", edit)
	}
	sendTask := taskByID(t, s, "1")
	if sendTask.ChatID != "1" || sendTask.Message != source.Message || string(sendTask.Entities) != `[{"offset":0,"length":3,"className":"MessageEntityBold"}]` {
		t.Errorf("存下的 send：%+v", sendTask)
	}
	if err := run(".acron cmd 0 0 2 * * * @friend|12 定时备份\n.checkin add a 名字 @bot\n/sign", 0); err != nil {
		t.Fatal(err)
	}
	if got := taskByID(t, s, "2"); got.Message != ".checkin add a 名字 @bot\n/sign" || got.ChatID != "42" || got.ReplyTo != "12" || got.Remark != "定时备份" {
		t.Errorf("存下的 cmd：%+v", got)
	}
	if err := run(".acron pin 0 0 9 * * 1-5 me 77 yes 0", 0); err != nil {
		t.Fatal(err)
	}
	if edit := lastEdit(); !strings.Contains(edit, "通知：开启") || !strings.Contains(edit, "只对自己置顶：关闭") {
		t.Errorf("添加 pin：\n%s", edit)
	}
	// 找不到的对话照样添加，到时间再找。
	if err := run(".acron del 0 0 2 * * * @nobody 5", 0); err != nil {
		t.Fatal(err)
	}
	if got := taskByID(t, s, "4"); got.ChatID != "" || got.ResolvedPeer || !strings.Contains(lastEdit(), "现在找不到这个对话") {
		t.Errorf("找不到对话的：%+v %s", got, lastEdit())
	}
	s.mu.Lock()
	scheduled := len(s.entries)
	s.mu.Unlock()
	if scheduled != 4 {
		t.Errorf("挂上调度器的任务：%d", scheduled)
	}

	for text, want := range map[string]string{
		".acron send 0 2 * * * me":           "Cron 表达式",
		".acron send 0 0 2 * * *":            "请写上对话",
		".acron send 0 0 2 * * * me":         "请回复一条文字消息",
		".acron del 0 0 2 * * * me abc":      "消息 ID",
		".acron del_re 0 0 2 * * * me 0 x":   "1–1000",
		".acron del_re 0 0 2 * * * me 5 (?=": "正则表达式无效",
		".acron pin 0 0 2 * * * me 5 1":      "通知",
		".acron cmd 0 0 2 * * * me":          "第二行",
		".acron copy 0 0 2 * * * me":         "请回复",
		".acron nope":                        "没有这个子命令",
		".acron rm 99":                       "没有 ID 是 99 的定时任务",
		".acron ls bogus":                    "筛选",
	} {
		err := run(text, 0)
		if message, ok := kit.IsUserError(err); !ok || !strings.Contains(message, want) {
			t.Errorf("%q：%v，应包含 %q", text, err, want)
		}
	}
	if err := run(".acron send 0 0 2 * * * me", 31); err == nil || !strings.Contains(err.Error(), "copy 或 forward") {
		t.Errorf("带图片的 send：%v", err)
	}

	// 当前对话（收藏夹）只有 send 和 pin；ls pin 只看置顶，la 看全部。
	if err := run(".acron ls", 0); err != nil {
		t.Fatal(err)
	}
	if edit := lastEdit(); !strings.Contains(edit, "当前对话的定时任务（2 个）") || strings.Contains(edit, "定时备份") {
		t.Errorf("ls：\n%s", edit)
	}
	if err := run(".acron ls pin", 0); err != nil {
		t.Fatal(err)
	}
	if edit := lastEdit(); !strings.Contains(edit, "当前对话的定时任务 · 置顶（1 个）") {
		t.Errorf("ls pin：\n%s", edit)
	}
	if err := run(".acron la", 0); err != nil {
		t.Fatal(err)
	}
	if edit := lastEdit(); !strings.Contains(edit, "全部定时任务（4 个）") || !strings.Contains(edit, "Friend（@friend）") {
		t.Errorf("la：\n%s", edit)
	}

	// 关闭、开启、删除。
	if err := run(".acron off 2", 0); err != nil {
		t.Fatal(err)
	}
	if got := taskByID(t, s, "2"); !got.Disabled || lastEdit() != "✅ 已关闭定时任务 2" {
		t.Errorf("关闭：%+v %s", got, lastEdit())
	}
	if err := run(".acron enable 2", 0); err != nil {
		t.Fatal(err)
	}
	if got := taskByID(t, s, "2"); got.Disabled || !strings.HasPrefix(lastEdit(), "✅ 已开启定时任务 2\n下次执行：") {
		t.Errorf("开启：%+v %s", got, lastEdit())
	}
	if err := run(".acron rm 1", 0); err != nil {
		t.Fatal(err)
	}
	st, _ := s.read()
	s.mu.Lock()
	_, stillScheduled := s.entries["1"]
	s.mu.Unlock()
	if st.find("1") >= 0 || stillScheduled || st.Seq != "4" {
		t.Errorf("删除后：%+v", st)
	}
	// 新任务接着 seq 编号，不复用删掉的。
	if err := run(".acron unpin 0 0 2 * * * me 8", 0); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.read(); st.find("5") < 0 {
		t.Errorf("新任务的编号：%+v", st.Tasks)
	}
}

// 帮助和说明按 STYLE.md 写：internal/commands 的 TestCommandTextStyle 注册全部命令时会查，
// 这里先在本包里查一遍能查的几条，并确认 HTML 能被发送时的解析器接受。
func TestHelpStyle(t *testing.T) {
	a := &app.App{Root: t.TempDir(), Registry: command.New([]string{"."}, discard)}
	Register(a)
	cmd, ok := a.Registry.Lookup("acron")
	if !ok || len(a.Registry.Jobs()) != 1 {
		t.Fatal("没注册上 .acron 和它的后台任务")
	}
	text := cmd.Help(".")
	if !strings.HasPrefix(text, "⏰ <b>定时任务</b>") {
		t.Errorf("帮助的开头：%q", text[:40])
	}
	if _, _, err := bot.ParseHTML(text); err != nil {
		t.Errorf("帮助的 HTML：%v", err)
	}
	hanThenASCII := regexp.MustCompile(`\p{Han}[:(]`)
	for _, value := range []string{text, cmd.Description} {
		if match := hanThenASCII.FindString(value); match != "" || strings.Contains(value, "...") || strings.Contains(value, "</code> - ") {
			t.Errorf("标点：%q", match)
		}
	}
	if count := utf8.RuneCountInString(cmd.Description); count < 4 || count > 16 || strings.ContainsAny(cmd.Description, "，,。") {
		t.Errorf("说明 %q", cmd.Description)
	}
	if strings.Contains(cmd.Usage, " | ") || strings.Contains(cmd.Usage, "...") {
		t.Errorf("用法 %q", cmd.Usage)
	}
}
