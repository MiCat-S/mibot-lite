package keyword

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

func TestParseTask(t *testing.T) {
	full, err := parseTask("  \\d{11} \n+++\n请勿发送手机号码\n第二行\n+++ \nregexp CASE\n+++\ndelete ban3600\n+++\n10\n+++\n2.5")
	if err != nil {
		t.Fatal(err)
	}
	want := task{Key: `\d{11}`, Response: "请勿发送手机号码\n第二行", Include: true, Regexp: true, CaseSensitive: true,
		Reply: true, DeleteSource: true, BanSeconds: 3600, DeleteReplyAfter: 10, DeleteSourceAfter: 2.5}
	if full != want {
		t.Errorf("完整格式：%+v", full)
	}
	// 只写两段就是默认值：包含匹配、引用回复。
	if simple, err := parseTask("你好\n+++\n欢迎"); err != nil || simple != (task{Key: "你好", Response: "欢迎", Include: true, Reply: true}) {
		t.Errorf("两段：%+v %v", simple, err)
	}
	// exact 关掉 include；中间空着的段按默认值，v2 这里会报格式无效。
	if exact, err := parseTask("违规\n+++\n注意\n+++\n\n+++\nrestrict600"); err != nil || exact.Exact || exact.RestrictSeconds != 600 {
		t.Errorf("空的匹配选项：%+v %v", exact, err)
	}
	if exact, _ := parseTask("违规\n+++\n注意\n+++\nexact"); exact.Include || !exact.Exact {
		t.Errorf("exact：%+v", exact)
	}
	for text, want := range map[string]string{
		"只有关键词":                                            "关键词和回复之间要有单独一行 +++",
		"\n+++\n回复":                                        "关键词不能为空",
		"词\n+++\n ":                                        "回复内容不能为空",
		"词\n+++\n回\n+++\nfuzzy":                            "不认识的匹配选项 fuzzy",
		"词\n+++\n回\n+++\n\n+++\nkick":                      "不认识的动作 kick",
		"词\n+++\n回\n+++\n\n+++\nban":                       "ban 后面要紧跟秒数",
		"词\n+++\n回\n+++\n\n+++\nban0":                      "ban 后面要紧跟秒数",
		"词\n+++\n回\n+++\n\n+++\nrestrict":                  "restrict 后面要紧跟秒数",
		"词\n+++\n回\n+++\n\n+++\n\n+++\n-1":                 "第 5 段要写不小于 0 的秒数",
		"词\n+++\n回\n+++\n\n+++\n\n+++\n\n+++\nabc":         "第 6 段要写不小于 0 的秒数",
		"词\n+++\n回\n+++\n\n+++\n\n+++\n1\n+++\n1\n+++\n多余": "任务最多 6 段",
		"[\n+++\n回\n+++\nregexp":                           "正则表达式无效",
	} {
		_, err := parseTask(text)
		message, ok := kit.IsUserError(err)
		if !ok || !strings.Contains(message, want) {
			t.Errorf("%q：%v，应包含 %q", text, err, want)
		}
	}
}

func TestMatches(t *testing.T) {
	check := func(name string, tk task, text string, forwarded, want bool) {
		t.Helper()
		pattern, _ := compilePattern(tk.Key, tk.CaseSensitive)
		if !tk.Regexp {
			pattern = nil
		}
		got, err := matches(tk, pattern, text, forwarded)
		if err != nil || got != want {
			t.Errorf("%s：%q → %v %v，应为 %v", name, text, got, err, want)
		}
	}
	include := task{Key: "Hello", Include: true}
	check("包含，不分大小写", include, "oh HELLO there", false, true)
	check("不包含", include, "hi", false, false)
	check("区分大小写", task{Key: "Hello", Include: true, CaseSensitive: true}, "hello", false, false)
	check("整条一致", task{Key: "你好", Exact: true}, "你好", false, true)
	check("整条不一致", task{Key: "你好", Exact: true}, "你好呀", false, false)
	check("旧数据两者都关", task{Key: "你好"}, "你好", false, false)
	check("忽略转发", task{Key: "广告", Include: true, IgnoreForward: true}, "广告", true, false)
	check("不忽略转发", task{Key: "广告", Include: true}, "广告", true, true)
	check("空消息", include, "", false, false)
	// v2 的正则是 JS 的：先行断言、\d 这些要照样能用。
	check("正则", task{Key: `\d{11}`, Regexp: true}, "电话 13800138000", false, true)
	check("先行断言", task{Key: `^(?!.*白名单).*广告`, Regexp: true}, "白名单里的广告", false, false)
	check("先行断言命中", task{Key: `^(?!.*白名单).*广告`, Regexp: true}, "卖广告", false, true)
	check("正则不分大小写", task{Key: `^HI$`, Regexp: true}, "hi", false, true)
	check("正则区分大小写", task{Key: `^HI$`, Regexp: true, CaseSensitive: true}, "hi", false, false)
	check("超长消息不跑正则", task{Key: `a`, Regexp: true}, strings.Repeat("a", maxInputLength+1), false, false)
}

func TestRender(t *testing.T) {
	tk := task{Response: "$mention 你好 $mention，ID $code_id，名字 $code_name，$delay_delete 秒后删除", DeleteReplyAfter: 10}
	got := render(tk, sender{id: "42", name: "<Cat>", user: true})
	want := `<a href="tg://user?id=42">&lt;Cat&gt;</a> 你好 <a href="tg://user?id=42">&lt;Cat&gt;</a>，ID 42，名字 &lt;Cat&gt;，10 秒后删除`
	if got != want {
		t.Errorf("用户：\n%s\n应为\n%s", got, want)
	}
	// 名字里的 $code_id 不会被再替换一遍。
	if got := render(task{Response: "$code_name"}, sender{id: "1", name: "$code_id", user: true}); got != "$code_id" {
		t.Errorf("一次替换完：%q", got)
	}
	// 频道身份没法提及，只写名字；没有发送者时变量都是空的。
	if got := render(task{Response: "$mention"}, sender{id: "7", name: "频道"}); got != "频道" {
		t.Errorf("频道：%q", got)
	}
	if got := render(task{Response: "[$mention][$code_id][$delay_delete]"}, sender{}); got != "[][][]" {
		t.Errorf("没有发送者：%q", got)
	}
}

// v2 写的文件原样能读；v1 留下的旧字段名照 normalizeTask 认；不完整的任务丢掉。
func TestDocumentReadsMiBoxData(t *testing.T) {
	raw := `{
  "schemaVersion": 1, "nextId": 4, "importedLegacy": true,
  "aliases": {"-1002": "-1001", "-1003": -1001},
  "tasks": [
    {"id": 1, "chatId": "-1001", "key": "你好", "response": "欢迎", "include": true, "regexp": false, "exact": false,
     "caseSensitive": false, "ignoreForward": false, "reply": true, "deleteSource": true, "banSeconds": 0,
     "restrictSeconds": 600, "deleteReplyAfter": 1.5, "deleteSourceAfter": 0},
    {"task_id": "2", "cid": -1001234567890, "key": "旧", "msg": "旧回复", "case": true, "ignore_forward": true,
     "delete": true, "ban": "300", "delay_delete": 10, "source_delay_delete": -5, "reply": false, "include": false},
    {"id": 3, "chatId": "-1001", "key": null, "response": "没有关键词"},
    {"id": 0, "chatId": "-1001", "key": "k", "response": "r"},
    {"id": 5, "key": "k", "response": "r"},
    "不是对象"
  ]
}`
	var doc document
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.NextID != 4 || !doc.ImportedLegacy || doc.Aliases["-1002"] != "-1001" || doc.Aliases["-1003"] != "-1001" {
		t.Errorf("文档：%+v", doc)
	}
	if len(doc.Tasks) != 2 {
		t.Fatalf("应只留下两条任务：%+v", doc.Tasks)
	}
	if first := doc.Tasks[0]; !first.DeleteSource || first.RestrictSeconds != 600 || first.DeleteReplyAfter != 1.5 || !first.Include || !first.Reply {
		t.Errorf("v2 任务：%+v", first)
	}
	legacy := task{ID: 2, ChatID: "-1001234567890", Key: "旧", Response: "旧回复", CaseSensitive: true, IgnoreForward: true,
		DeleteSource: true, BanSeconds: 300, DeleteReplyAfter: 10}
	if doc.Tasks[1] != legacy {
		t.Errorf("v1 任务：%+v", doc.Tasks[1])
	}
	// 写回去再读，字段名是 v2 的，值不变。
	encoded, _ := json.Marshal(doc)
	if !strings.Contains(string(encoded), `"caseSensitive":true`) || strings.Contains(string(encoded), `"cid"`) {
		t.Errorf("写回的字段名：%s", encoded)
	}
	var again document
	if err := json.Unmarshal(encoded, &again); err != nil || !slices.Equal(again.Tasks, doc.Tasks) {
		t.Errorf("往返之后变了：%+v %v", again.Tasks, err)
	}
}

// fakeTelegram 记下账号发出的每个请求。
type fakeTelegram struct {
	mu    sync.Mutex
	calls []string
	// failSend 让发消息失败，看后面的动作是否照做。
	failSend bool
}

func (f *fakeTelegram) log(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeTelegram) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var value bin.Encoder
	switch request := input.(type) {
	case *tg.MessagesSendMessageRequest:
		reply, topic := 0, 0
		if header, ok := request.GetReplyTo(); ok {
			if to, ok := header.(*tg.InputReplyToMessage); ok {
				reply = to.ReplyToMsgID
				topic, _ = to.GetTopMsgID()
			}
		}
		mention := ""
		for _, entity := range request.Entities {
			if user, ok := entity.(*tg.InputMessageEntityMentionName); ok {
				mention = fmt.Sprintf(" mention=%d", user.UserID.(*tg.InputUser).UserID)
			}
		}
		f.log("send %q reply=%d topic=%d%s", request.Message, reply, topic, mention)
		if f.failSend {
			return tgerr.New(403, "CHAT_WRITE_FORBIDDEN")
		}
		value = &tg.UpdateShortSentMessage{ID: 900, Date: 1}
	case *tg.ChannelsDeleteMessagesRequest:
		f.log("delete %v", request.ID)
		value = &tg.MessagesAffectedMessages{Pts: 1, PtsCount: 1}
	case *tg.MessagesDeleteMessagesRequest:
		f.log("delete %v", request.ID)
		value = &tg.MessagesAffectedMessages{Pts: 1, PtsCount: 1}
	case *tg.ChannelsEditBannedRequest:
		rights := request.BannedRights
		f.log("edit-banned user=%d view=%v send=%v media=%v until=%d", request.Participant.(*tg.InputPeerUser).UserID,
			rights.ViewMessages, rights.SendMessages, rights.SendMedia, rights.UntilDate)
		value = &tg.Updates{}
	case *tg.MessagesDeleteChatUserRequest:
		f.log("remove-chat-user %d", request.UserID.(*tg.InputUser).UserID)
		value = &tg.Updates{}
	case *tg.MessagesEditMessageRequest:
		text, _ := request.GetMessage()
		f.log("edit %d %q", request.ID, text)
		value = &tg.Updates{}
	default:
		return fmt.Errorf("假 Telegram 不认识 %T", input)
	}
	var buffer bin.Buffer
	if err := value.Encode(&buffer); err != nil {
		return err
	}
	return output.Decode(&buffer)
}

func (f *fakeTelegram) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

const (
	selfID    = 1
	senderID  = 42
	channelID = 1001
)

func newFixture(t *testing.T) (*service, *fakeTelegram, *bot.Client, *[]string) {
	t.Helper()
	fake := &fakeTelegram{}
	peers := bot.NewPeerCache()
	peers.SetSelf(selfID)
	peers.RememberUsers([]tg.UserClass{&tg.User{ID: senderID, AccessHash: 7, FirstName: "Cat"}})
	peers.RememberChats([]tg.ChatClass{&tg.Channel{ID: channelID, AccessHash: 9, Title: "测试群", Megagroup: true},
		&tg.Channel{ID: 1002, AccessHash: 8, Title: "分群", Megagroup: true}})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: selfID, Self: true}, logger)
	s := newService(store.New(filepath.Join(t.TempDir(), "keyword.json"), defaults), logger)
	var delayed []string
	s.background = func(work func()) { work() }
	s.later = func(delay time.Duration, run func()) {
		delayed = append(delayed, delay.String())
		run()
	}
	s.now = func() time.Time { return time.Unix(1000, 0) }
	return s, fake, client, &delayed
}

func groupMessage(chat int64, id int, text string) *bot.Message {
	return &bot.Message{ID: id, Peer: &tg.PeerChannel{ChannelID: chat}, ChatID: bot.PeerID(&tg.PeerChannel{ChannelID: chat}),
		ChatType: bot.ChatSupergroup, Text: text, Sender: &tg.PeerUser{UserID: senderID}}
}

func (s *service) seed(t *testing.T, doc document) {
	t.Helper()
	if err := s.update(func(current *document) error { *current = doc; return nil }); err != nil {
		t.Fatal(err)
	}
}

// 命中之后：引用回复（带提及）、封禁、删原消息、到时删回复；监听者不认领这条消息。
func TestOfferRunsActions(t *testing.T) {
	s, fake, client, delayed := newFixture(t)
	s.seed(t, document{NextID: 2, Tasks: []task{{ID: 1, ChatID: "-1001001", Key: "广告", Response: "$mention 别发广告，$delay_delete 秒后删",
		Include: true, Reply: true, DeleteSource: true, BanSeconds: 3600, DeleteReplyAfter: 10}}})
	message := groupMessage(channelID, 50, "卖广告了")
	message.TopicID = 3
	if s.offer(context.Background(), client, message) {
		t.Error("关键词监听者不该认领消息，否则 .sudo、.sure 就看不到了")
	}
	want := []string{
		`send "Cat 别发广告，10 秒后删" reply=50 topic=3 mention=42`,
		"edit-banned user=42 view=true send=true media=true until=4600",
		"delete [50]",
		"delete [900]",
	}
	if got := fake.take(); !slices.Equal(got, want) {
		t.Errorf("请求：\n%s\n应为\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !slices.Equal(*delayed, []string{"10s"}) {
		t.Errorf("延时删除：%v", *delayed)
	}
	// 没命中、别的对话、空消息：什么都不做。
	s.offer(context.Background(), client, groupMessage(channelID, 51, "你好"))
	s.offer(context.Background(), client, groupMessage(1002, 52, "广告"))
	s.offer(context.Background(), client, groupMessage(channelID, 53, ""))
	if got := fake.take(); len(got) != 0 {
		t.Errorf("不该有请求：%v", got)
	}
}

// 回复发不出去（账号在群里被禁言）时，删消息和禁言照做；不引用时回复留在同一个话题。
func TestOfferContinuesAfterSendFailure(t *testing.T) {
	s, fake, client, _ := newFixture(t)
	fake.failSend = true
	s.seed(t, document{NextID: 2, Tasks: []task{{ID: 1, ChatID: "-1001001", Key: "spam", Response: "no", Include: true,
		DeleteSource: true, RestrictSeconds: 60}}})
	message := groupMessage(channelID, 60, "SPAM!")
	message.TopicID = 7
	s.offer(context.Background(), client, message)
	want := []string{`send "no" reply=7 topic=7`, "edit-banned user=42 view=false send=true media=true until=1060", "delete [60]"}
	if got := fake.take(); !slices.Equal(got, want) {
		t.Errorf("请求：\n%s\n应为\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// 继承：先查继承来的任务，再查自己的；匿名管理员（发送者就是群组）不处置。
func TestOfferInheritsAndSkipsAnonymousAdmin(t *testing.T) {
	s, fake, client, _ := newFixture(t)
	s.seed(t, document{NextID: 3, Aliases: map[string]string{"-1001002": "-1001001"}, Tasks: []task{
		{ID: 1, ChatID: "-1001002", Key: "hi", Response: "分群自己的", Include: true, Reply: true},
		{ID: 2, ChatID: "-1001001", Key: "hi", Response: "继承来的", Include: true, Reply: true, BanSeconds: 60},
	}})
	message := groupMessage(1002, 70, "hi")
	message.Sender = &tg.PeerChannel{ChannelID: 1002}
	s.offer(context.Background(), client, message)
	want := []string{`send "继承来的" reply=70 topic=0`, `send "分群自己的" reply=70 topic=0`}
	if got := fake.take(); !slices.Equal(got, want) {
		t.Errorf("请求：\n%s\n应为\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func invoke(t *testing.T, s *service, client *bot.Client, chat int64, text string) error {
	t.Helper()
	fields := strings.Fields(strings.TrimPrefix(text, ".keyword"))
	inv := &command.Invocation{Prefix: ".", Command: "keyword", Args: fields, Text: text,
		Message: &bot.Message{ID: 10, Peer: &tg.PeerChannel{ChannelID: chat}, ChatID: bot.PeerID(&tg.PeerChannel{ChannelID: chat}), Out: true},
		Client:  client, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return s.command(context.Background(), inv)
}

func TestCommands(t *testing.T) {
	s, fake, client, _ := newFixture(t)
	// 文件被手改过，nextId 落后于已有编号：新编号要跳过去。
	s.seed(t, document{NextID: 2, Tasks: []task{{ID: 5, ChatID: "-1001002", Key: "别处", Response: "x", Include: true, Reply: true}}})
	if err := invoke(t, s, client, channelID, ".keyword 你好\n+++\n欢迎 <b>$mention</b>\n+++\nexact\n+++\ndelete"); err != nil {
		t.Fatal(err)
	}
	// 关键词恰好是 list：有 +++ 就是添加。
	if err := invoke(t, s, client, channelID, ".keyword list\n+++\n列表"); err != nil {
		t.Fatal(err)
	}
	if got := fake.take(); !slices.Equal(got, []string{`edit 10 "✅ 已添加关键词任务 6"`, `edit 10 "✅ 已添加关键词任务 7"`}) {
		t.Errorf("添加：%v", got)
	}
	doc, _ := s.store.Read()
	if doc.NextID != 8 || len(doc.Tasks) != 3 || doc.Tasks[1].ChatID != "-1001001" || doc.Tasks[1].Response != "欢迎 <b>$mention</b>" || !doc.Tasks[1].Exact {
		t.Errorf("存下的任务：%+v", doc)
	}
	// 新加的任务马上生效。
	s.offer(context.Background(), client, groupMessage(channelID, 80, "你好"))
	if got := fake.take(); len(got) != 2 || !strings.HasPrefix(got[0], `send "欢迎 Cat"`) || got[1] != "delete [80]" {
		t.Errorf("新任务没生效：%v", got)
	}

	if err := invoke(t, s, client, channelID, ".keyword list"); err != nil {
		t.Fatal(err)
	}
	listed := strings.Join(fake.take(), "\n")
	for _, part := range []string{"当前对话的关键词任务（2 个）", "6 你好 → 欢迎 <b>$mention</b>", "整条一致 · 删除原消息", "7 list → 列表"} {
		if !strings.Contains(listed, part) {
			t.Errorf("列表里没有 %q：%s", part, listed)
		}
	}
	if err := invoke(t, s, client, channelID, ".keyword list all"); err != nil {
		t.Fatal(err)
	}
	if listed := strings.Join(fake.take(), "\n"); !strings.Contains(listed, "全部关键词任务（3 个）") || !strings.Contains(listed, "分群 -1001002") {
		t.Errorf("全部列表：%s", listed)
	}

	if err := invoke(t, s, client, channelID, ".keyword alias -1001002"); err != nil {
		t.Fatal(err)
	}
	if err := invoke(t, s, client, channelID, ".keyword alias"); err != nil {
		t.Fatal(err)
	}
	if got := fake.take(); !slices.Equal(got, []string{`edit 10 "✅ 已继承 -1001002（分群）的任务"`, `edit 10 "当前对话继承自 -1001002（分群）"`}) {
		t.Errorf("继承：%v", got)
	}
	for text, want := range map[string]string{
		".keyword alias @group":   "要写对话 ID",
		".keyword alias -1001001": "不能继承当前对话自己",
		".keyword rm":             "用法：.keyword rm 编号[,编号…]",
		".keyword rm 1,x":         "任务编号要是正整数，x 不是",
		".keyword rm 99":          "没有编号为 99 的任务",
		".keyword 只有关键词":          "keyword help 看格式",
	} {
		message, ok := kit.IsUserError(invoke(t, s, client, channelID, text))
		if !ok || !strings.Contains(message, want) {
			t.Errorf("%s：%q，应包含 %q", text, message, want)
		}
	}
	if err := invoke(t, s, client, channelID, ".keyword rm 6，7 99"); err != nil {
		t.Fatal(err)
	}
	if err := invoke(t, s, client, channelID, ".keyword alias rm"); err != nil {
		t.Fatal(err)
	}
	if got := fake.take(); !slices.Equal(got, []string{`edit 10 "✅ 已删除 2 个任务（没有编号 99）"`, `edit 10 "✅ 已取消继承"`}) {
		t.Errorf("删除：%v", got)
	}
	if doc, _ := s.store.Read(); len(doc.Tasks) != 1 || len(doc.Aliases) != 0 || doc.NextID != 8 {
		t.Errorf("删除之后：%+v", doc)
	}
}

// 搬过来的正则 regexp2 编译不了时，列表里标出来，匹配时不命中也不出错。
func TestBrokenPatternIsMarked(t *testing.T) {
	s, fake, client, _ := newFixture(t)
	path := s.store.Path()
	raw := `{"nextId":2,"tasks":[{"id":1,"chatId":"-1001001","key":"(","response":"r","regexp":true}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s.store = store.New(path, defaults)
	s.offer(context.Background(), client, groupMessage(channelID, 90, "("))
	if err := invoke(t, s, client, channelID, ".keyword list"); err != nil {
		t.Fatal(err)
	}
	if got := fake.take(); len(got) != 1 || !strings.Contains(got[0], "正则无法使用") {
		t.Errorf("列表：%v", got)
	}
}

// lockedBuffer 是可以并发写的日志缓冲。
type lockedBuffer struct {
	mu   sync.Mutex
	text strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.Write(p)
}

func (b *lockedBuffer) count(needle string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.text.String(), needle)
}

// 名额都被占着时，命中的消息直接丢掉、不排队；同一个对话一分钟只警告一次；没命中的消息不占名额也不警告。
func TestOfferDropsWhenBusy(t *testing.T) {
	s, fake, client, _ := newFixture(t)
	logs := &lockedBuffer{}
	s.logger = slog.New(slog.NewTextHandler(logs, nil))
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	s.seed(t, document{NextID: 2, Tasks: []task{{ID: 1, ChatID: "-1001001", Key: "广告", Response: "别发", Include: true, Reply: true}}})
	ran := 0
	s.background = func(work func()) { ran++; work() }
	for range cap(s.slots) {
		s.slots <- struct{}{}
	}
	s.offer(context.Background(), client, groupMessage(channelID, 1, "广告"))
	s.offer(context.Background(), client, groupMessage(channelID, 2, "广告"))
	s.offer(context.Background(), client, groupMessage(channelID, 3, "闲聊"))
	if ran != 0 || len(fake.take()) != 0 {
		t.Fatalf("名额满时不该执行：ran=%d", ran)
	}
	if got := logs.count("keyword.dropped_busy"); got != 1 {
		t.Errorf("一分钟内应只警告一次，记了 %d 次", got)
	}
	now = now.Add(61 * time.Second)
	s.offer(context.Background(), client, groupMessage(channelID, 4, "广告"))
	if got := logs.count("keyword.dropped_busy"); got != 2 {
		t.Errorf("过了一分钟应再警告一次，记了 %d 次", got)
	}
	// 名额空出来之后照常执行，执行完把名额还回去。
	for range cap(s.slots) {
		<-s.slots
	}
	s.offer(context.Background(), client, groupMessage(channelID, 5, "广告"))
	if got := fake.take(); ran != 1 || len(got) != 1 || !strings.HasPrefix(got[0], `send "别发" reply=5`) {
		t.Errorf("名额空出来后：ran=%d %v", ran, got)
	}
	if len(s.slots) != 0 {
		t.Errorf("执行完应还回名额，还占着 %d 个", len(s.slots))
	}
}
