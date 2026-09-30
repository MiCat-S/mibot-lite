package cleanmember

import (
	"context"
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
)

func TestParseRequest(t *testing.T) {
	for text, want := range map[string]request{
		"1 30 search":            {mode: "1", day: 30, search: true},
		"1 3":                    {mode: "1", day: 7},
		"2 5 limit:10":           {mode: "2", day: 7, limit: 10},
		"3 5 SEARCH":             {mode: "3", day: 5, search: true},
		"4 chat:-1001234 search": {mode: "4", search: true, chat: "-1001234"},
		"5 CHAT：@group LIMIT：2":  {mode: "5", chat: "@group", limit: 2},
	} {
		got, err := parseRequest(strings.Fields(text), ".")
		if err != nil || got != want {
			t.Errorf("%q → %+v %v，应为 %+v", text, got, err, want)
		}
	}
	for text, want := range map[string]string{
		"6":              "没有模式 6",
		"search":         "没有模式 search",
		"1":              "模式 1 后面要写天数",
		"3 x":            "发言条数要是正整数，x 不是",
		"2 -5":           "天数要是正整数",
		"1 1.5":          "天数要是正整数",
		"4 limit:0":      "limit 要是正整数",
		"4 limit:":       "limit 要是正整数",
		"4 chat:":        "chat: 后面要写对话 ID",
		"4 serach":       "不认识的参数 serach",
		"1 30 30 search": "不认识的参数 30",
	} {
		_, err := parseRequest(strings.Fields(text), ".")
		message, ok := kit.IsUserError(err)
		if !ok || !strings.Contains(message, want) {
			t.Errorf("%q：%v，应包含 %q", text, err, want)
		}
	}
}

func TestLastSeenDays(t *testing.T) {
	now := time.Unix(100*86400, 0)
	for name, c := range map[string]struct {
		status tg.UserStatusClass
		days   int
		known  bool
	}{
		"在线":    {&tg.UserStatusOnline{Expires: 1}, 0, true},
		"最近":    {&tg.UserStatusRecently{}, 0, true},
		"离线十天":  {&tg.UserStatusOffline{WasOnline: int(now.Unix()) - 10*86400 - 5}, 10, true},
		"一周内":   {&tg.UserStatusLastWeek{}, 7, true},
		"一月内":   {&tg.UserStatusLastMonth{}, 30, true},
		"隐藏":    {&tg.UserStatusEmpty{}, 0, false},
		"没有状态":  {nil, 0, false},
		"离线没时刻": {&tg.UserStatusOffline{}, 0, false},
	} {
		days, known := lastSeenDays(c.status, now)
		if days != c.days || known != c.known {
			t.Errorf("%s：%d %v，应为 %d %v", name, days, known, c.days, c.known)
		}
	}
	if got := *lastOnline(&tg.UserStatusOffline{WasOnline: 86400}); got != "1970-01-02T00:00:00.000Z" {
		t.Errorf("离线时刻：%s", got)
	}
	if got := *lastOnline(&tg.UserStatusLastWeek{}); got != "lastweek" {
		t.Errorf("状态名：%s", got)
	}
	if lastOnline(nil) != nil {
		t.Error("没有状态应为 null")
	}
}

func TestCSVReport(t *testing.T) {
	online := "online"
	data := cacheData{ChatID: "100", ChatTitle: `群"一"`, Mode: "1", Day: 30, SearchTime: "2026-09-30T00:00:00.000Z", TotalFound: 2,
		Users: []userInfo{
			{ID: "5", Username: "cat", FirstName: "=HYPERLINK(1)", LastName: "S", LastOnline: &online},
			{ID: "6", FirstName: "-1", IsDeleted: true, ErrorMessage: "权限不足"},
		}}
	report := string(csvReport(data, false))
	want := "\ufeff" + `"群组清理报告"
"群组名称","群""一"""
"群组ID","100"
"清理条件","未上线超过 30 天的用户"
"搜索时间","2026-09-30T00:00:00.000Z"
"符合条件用户数量","2"

"用户ID","用户名","姓名","最后上线时间","是否注销"
"5","cat","'=HYPERLINK(1) S","online","否"
"6","","'-1","未知","是"`
	if report != want {
		t.Errorf("报告：\n%s\n应为\n%s", report, want)
	}
	failed := string(csvReport(data, true))
	if !strings.Contains(failed, `"群组清理失败用户报告"`) || !strings.HasSuffix(failed, `"未知","是","权限不足"`) {
		t.Errorf("失败名单：\n%s", failed)
	}
}

// 缓存每份一个文件：过期的、超出 50 份里最早写的在存新结果时删掉；按群组清除只删这个群组的。
func TestCacheFiles(t *testing.T) {
	c := cache{dir: t.TempDir()}
	now := time.Now()
	write := func(name string, age time.Duration) {
		t.Helper()
		path := filepath.Join(c.dir, name)
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	write("1_1_7.json", 25*time.Hour)
	for index := 0; index < cacheLimit; index++ {
		write(fmt.Sprintf("1_4_%d.json", index), time.Duration(cacheLimit-index)*time.Minute)
	}
	write("notes.txt", 48*time.Hour)
	if err := c.save("2_5_0", cacheData{ChatID: "2", Mode: "5", TotalFound: 1, ExpiresAt: now.Add(time.Hour).UnixMilli()}, now); err != nil {
		t.Fatal(err)
	}
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(c.dir, name))
		return err == nil
	}
	if exists("1_1_7.json") || exists("1_4_0.json") || !exists("1_4_1.json") || !exists("notes.txt") {
		t.Error("应删掉过期的和超出上限里最早的，别的文件不动")
	}
	if data, ok := c.load("2_5_0", now); !ok || data.TotalFound != 1 {
		t.Errorf("读回：%+v %v", data, ok)
	}
	if _, ok := c.load("2_5_0", now.Add(2*time.Hour)); ok {
		t.Error("过期的结果不该读出来")
	}
	if _, ok := c.load("../x", now); ok {
		t.Error("不是键的样子不能当文件名")
	}
	write("10_4_0.json", time.Minute)
	if err := c.clearChat("1"); err != nil {
		t.Fatal(err)
	}
	if exists("1_4_1.json") || !exists("10_4_0.json") || !exists("2_5_0.json") {
		t.Error("只该清掉群组 1 的缓存")
	}
}

// fakeGroup 冒充一个超级群组（ID 100）或基本群组（ID 200）：members 按「最近成员」的顺序排，
// 被移出的人从列表里消失，后面的人往前挪——真实的 Telegram 就是这样。
type fakeGroup struct {
	mu        sync.Mutex
	members   []*tg.User
	admins    map[int64]bool
	selfRole  string
	messages  map[int64]int
	failBan   map[int64]string
	failFind  map[int64]bool
	calls     []string
	uploads   map[int64][]byte
	documents map[string]string
}

const selfID = 1

func newFakeGroup(members ...*tg.User) *fakeGroup {
	self := &tg.User{ID: selfID, AccessHash: 1, FirstName: "我", Self: true}
	return &fakeGroup{members: append([]*tg.User{self}, members...), admins: map[int64]bool{selfID: true}, selfRole: "creator",
		messages: map[int64]int{}, failBan: map[int64]string{}, failFind: map[int64]bool{}, uploads: map[int64][]byte{}, documents: map[string]string{}}
}

func (f *fakeGroup) participant(user *tg.User) tg.ChannelParticipantClass {
	switch {
	case user.ID == selfID && f.selfRole == "creator":
		return &tg.ChannelParticipantCreator{UserID: user.ID}
	case f.admins[user.ID] && (user.ID != selfID || f.selfRole == "admin"):
		return &tg.ChannelParticipantAdmin{UserID: user.ID, PromotedBy: 999, Date: 1, AdminRights: tg.ChatAdminRights{BanUsers: true}}
	}
	return &tg.ChannelParticipant{UserID: user.ID, Date: 1}
}

func (f *fakeGroup) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var value bin.Encoder
	switch request := input.(type) {
	case *tg.ChannelsGetParticipantRequest:
		f.calls = append(f.calls, "self")
		value = &tg.ChannelsChannelParticipant{Participant: f.participant(f.members[0])}
	case *tg.ChannelsGetParticipantsRequest:
		var picked []*tg.User
		if _, admins := request.Filter.(*tg.ChannelParticipantsAdmins); admins {
			f.calls = append(f.calls, fmt.Sprintf("admins offset=%d", request.Offset))
			for _, user := range f.members {
				if f.admins[user.ID] {
					picked = append(picked, user)
				}
			}
		} else {
			f.calls = append(f.calls, fmt.Sprintf("recent offset=%d", request.Offset))
			picked = f.members[min(request.Offset, len(f.members)):min(request.Offset+request.Limit, len(f.members))]
		}
		list := &tg.ChannelsChannelParticipants{Count: len(f.members)}
		for _, user := range picked {
			list.Participants = append(list.Participants, f.participant(user))
			list.Users = append(list.Users, user)
		}
		// 提拔管理员的人也在 users 里，他不是这一页的成员（v2 会把他当成员）。
		list.Users = append(list.Users, &tg.User{ID: 999, AccessHash: 9, FirstName: "提拔人", Deleted: true})
		value = list
	case *tg.ChannelsEditBannedRequest:
		id := request.Participant.(*tg.InputPeerUser).UserID
		if !request.BannedRights.ViewMessages {
			f.calls = append(f.calls, fmt.Sprintf("unban %d", id))
			value = &tg.Updates{}
			break
		}
		f.calls = append(f.calls, fmt.Sprintf("ban %d until=%d", id, request.BannedRights.UntilDate))
		if code := f.failBan[id]; code != "" {
			if code == "USER_NOT_PARTICIPANT" {
				// 列表读出来之后这人自己退群了。
				f.drop(id)
			}
			return tgerr.New(400, code)
		}
		f.drop(id)
		value = &tg.Updates{}
	case *tg.MessagesGetFullChatRequest:
		f.calls = append(f.calls, "full-chat")
		participants := &tg.ChatParticipants{ChatID: request.ChatID}
		var users []tg.UserClass
		for _, user := range f.members {
			users = append(users, user)
			switch {
			case user.ID == selfID:
				participants.Participants = append(participants.Participants, &tg.ChatParticipantCreator{UserID: user.ID})
			case f.admins[user.ID]:
				participants.Participants = append(participants.Participants, &tg.ChatParticipantAdmin{UserID: user.ID, InviterID: 1, Date: 1})
			default:
				participants.Participants = append(participants.Participants, &tg.ChatParticipant{UserID: user.ID, InviterID: 1, Date: 1})
			}
		}
		value = &tg.MessagesChatFull{FullChat: &tg.ChatFull{ID: request.ChatID, Participants: participants}, Users: users}
	case *tg.MessagesDeleteChatUserRequest:
		id := request.UserID.(*tg.InputUser).UserID
		f.calls = append(f.calls, fmt.Sprintf("remove %d", id))
		f.drop(id)
		value = &tg.Updates{}
	case *tg.MessagesSearchRequest:
		from, _ := request.GetFromID()
		id := from.(*tg.InputPeerUser).UserID
		f.calls = append(f.calls, fmt.Sprintf("search %d min=%d", id, request.MinDate))
		if f.failFind[id] {
			return tgerr.New(400, "SEARCH_QUERY_EMPTY")
		}
		value = &tg.MessagesChannelMessages{Count: f.messages[id]}
	case *tg.MessagesEditMessageRequest:
		text, _ := request.GetMessage()
		f.calls = append(f.calls, "edit "+text)
		value = &tg.Updates{}
	case *tg.UploadSaveFilePartRequest:
		f.uploads[request.FileID] = append(f.uploads[request.FileID], request.Bytes...)
		value = &tg.BoolTrue{}
	case *tg.MessagesSendMediaRequest:
		if _, self := request.Peer.(*tg.InputPeerSelf); !self {
			return fmt.Errorf("报告只能发到收藏夹，却发到了 %T", request.Peer)
		}
		media := request.Media.(*tg.InputMediaUploadedDocument)
		file := media.File.(*tg.InputFile)
		f.calls = append(f.calls, fmt.Sprintf("document %s %s: %s", media.MimeType, strings.SplitN(file.Name, "_", 2)[0], request.Message))
		f.documents[strings.SplitN(file.Name, "_", 2)[0]] = string(f.uploads[file.ID])
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

func (f *fakeGroup) drop(id int64) {
	f.members = slices.DeleteFunc(f.members, func(user *tg.User) bool { return user.ID == id })
}

// take 取出记下的请求；搜索、封禁这些逐人的请求太多，只数一下。
func (f *fakeGroup) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

func count(calls []string, prefix string) int {
	n := 0
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

func last(calls []string) string {
	for index := len(calls) - 1; index >= 0; index-- {
		if strings.HasPrefix(calls[index], "edit ") {
			return strings.TrimPrefix(calls[index], "edit ")
		}
	}
	return ""
}

type fixture struct {
	s      *service
	fake   *fakeGroup
	client *bot.Client
}

func newFixture(t *testing.T, fake *fakeGroup) *fixture {
	t.Helper()
	peers := bot.NewPeerCache()
	peers.SetSelf(selfID)
	peers.RememberChats([]tg.ChatClass{
		&tg.Channel{ID: 100, AccessHash: 5, Title: "测试群", Megagroup: true},
		&tg.Channel{ID: 300, AccessHash: 6, Title: "频道", Broadcast: true},
		&tg.Chat{ID: 200, Title: "小群"},
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: selfID, Self: true}, logger)
	s := &service{cache: cache{dir: filepath.Join(t.TempDir(), "clean_member")},
		now:           func() time.Time { return time.Unix(1_700_000_000, 0) },
		pause:         func(ctx context.Context, d time.Duration) error { return ctx.Err() },
		gap:           func() time.Duration { return 0 },
		progressEvery: time.Hour}
	return &fixture{s: s, fake: fake, client: client}
}

func (f *fixture) run(t *testing.T, peer tg.PeerClass, text string) error {
	t.Helper()
	inv := &command.Invocation{Prefix: ".", Command: "clean_member", Args: strings.Fields(text), Text: ".clean_member " + text,
		Message: &bot.Message{ID: 10, Peer: peer, ChatID: bot.PeerID(peer), Out: true}, Client: f.client,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return f.s.handle(context.Background(), inv)
}

var here = &tg.PeerChannel{ChannelID: 100}

// users 造 n 个普通成员，编号从 from 起；deleted 为真的是注销账号。
func users(from, n int, deleted bool) []*tg.User {
	var list []*tg.User
	for index := 0; index < n; index++ {
		list = append(list, &tg.User{ID: int64(from + index), AccessHash: 3, FirstName: fmt.Sprint("成员", from+index), Deleted: deleted,
			Status: &tg.UserStatusRecently{}})
	}
	return list
}

// 搜索：翻完三页，管理员、自己和不在这一页的提拔人都不算；报告只发到收藏夹；
// 24 小时内再搜直接用缓存，不再翻成员列表。
func TestSearchUsesPagesAndCache(t *testing.T) {
	members := append(users(10, 250, false), users(500, 3, true)...)
	fake := newFakeGroup(members...)
	fake.admins[10] = true
	f := newFixture(t, fake)
	if err := f.run(t, here, "4 search"); err != nil {
		t.Fatal(err)
	}
	calls := f.fake.take()
	if count(calls, "recent") != 2 || count(calls, "admins") != 1 || count(calls, "self") != 0 || count(calls, "ban") != 0 {
		t.Errorf("请求：%v", calls)
	}
	if got := last(calls); got != "✅ 已搜索完 测试群 里已注销的账号\n扫描 254 人，符合条件 3 人\n报告已发到收藏夹" {
		t.Errorf("结果：%q", got)
	}
	report := f.fake.documents["report"]
	if !strings.Contains(report, `"500","","成员500","recently","是"`) || strings.Contains(report, "提拔人") {
		t.Errorf("报告：\n%s", report)
	}
	if cached, ok := f.s.cached("100_4_0"); !ok || cached.TotalFound != 3 || cached.ChatTitle != "测试群" || cached.ExpiresAt != 1_700_000_000_000+cacheTTL.Milliseconds() {
		t.Errorf("缓存：%+v %v", cached, ok)
	}
	if err := f.run(t, here, "4 search"); err != nil {
		t.Fatal(err)
	}
	calls = f.fake.take()
	if count(calls, "recent") != 0 || count(calls, "document") != 1 || !strings.Contains(last(calls), "（用的是 2023-11-14 22:13 UTC 的缓存）") {
		t.Errorf("缓存命中：%v", calls)
	}
}

// 移出：封禁 60 秒再解封；移出的人从列表里消失后不会漏掉后面的人（v2 每页固定往后翻 200 会漏）；
// 失败的人进失败名单；移出之后这个群组的搜索缓存清掉。
func TestCleanDoesNotSkipShiftedMembers(t *testing.T) {
	fake := newFakeGroup(users(10, 250, false)...)
	fake.failBan[20] = "CHAT_ADMIN_REQUIRED"
	fake.failBan[21] = "USER_NOT_PARTICIPANT"
	f := newFixture(t, fake)
	later := f.s.now().Add(time.Hour).UnixMilli()
	for _, key := range []string{"100_4_0", "1000_4_0"} {
		if err := f.s.cache.save(key, cacheData{ExpiresAt: later}, f.s.now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.run(t, here, "5"); err != nil {
		t.Fatal(err)
	}
	calls := f.fake.take()
	if calls[0] != "self" || !slices.Contains(calls, "ban 10 until=1700000060") || !slices.Contains(calls, "unban 10") {
		t.Errorf("请求：%v", calls[:min(len(calls), 6)])
	}
	if len(fake.members) != 2 {
		t.Errorf("应只剩自己和移出失败的那个人，还剩 %d 人", len(fake.members))
	}
	want := "✅ 已清理完 测试群 里所有普通成员\n扫描 251 人，符合条件 250 人\n已移出 249 人（成功率 99.6%）\n报告已发到收藏夹\n" +
		"⚠️ 移出失败 1 人（权限不足×1）\n失败名单另发了一份到收藏夹"
	if got := last(calls); got != want {
		t.Errorf("结果：\n%s\n应为\n%s", got, want)
	}
	if failed := f.fake.documents["failed"]; !strings.HasSuffix(failed, `"20","","成员20","recently","否","权限不足"`) {
		t.Errorf("失败名单：\n%s", failed)
	}
	_, stale := f.s.cached("100_4_0")
	_, other := f.s.cached("1000_4_0")
	if stale || !other {
		t.Errorf("移出之后应清掉这个群组的缓存，别的群组不动：%v %v", stale, other)
	}
}

func TestCleanStopsAtLimit(t *testing.T) {
	fake := newFakeGroup(users(10, 20, true)...)
	f := newFixture(t, fake)
	if err := f.run(t, here, "4 limit:3"); err != nil {
		t.Fatal(err)
	}
	calls := f.fake.take()
	if count(calls, "ban") != 3 || !strings.Contains(last(calls), "已达到 limit:3，后面的成员没有再查") {
		t.Errorf("请求：%v", calls)
	}
}

// 模式 2 只数最近这些天的消息；模式 3 数全部；查不到的人跳过。
func TestMessageModes(t *testing.T) {
	fake := newFakeGroup(users(10, 3, false)...)
	fake.messages[10], fake.messages[11] = 0, 8
	fake.failFind[12] = true
	f := newFixture(t, fake)
	if err := f.run(t, here, "2 30 search"); err != nil {
		t.Fatal(err)
	}
	calls := f.fake.take()
	if !slices.Contains(calls, fmt.Sprintf("search 10 min=%d", 1_700_000_000-30*86400)) {
		t.Errorf("模式 2 的时间范围：%v", calls)
	}
	if got := last(calls); !strings.Contains(got, "符合条件 1 人") || !strings.Contains(got, "⚠️ 1 人查不到发言记录，已跳过") {
		t.Errorf("模式 2：%q", got)
	}
	if err := f.run(t, here, "3 10 search"); err != nil {
		t.Fatal(err)
	}
	calls = f.fake.take()
	if !slices.Contains(calls, "search 11 min=0") || !strings.Contains(last(calls), "符合条件 2 人") {
		t.Errorf("模式 3：%v", calls)
	}
}

func TestRefusals(t *testing.T) {
	fake := newFakeGroup(users(10, 3, false)...)
	fake.selfRole = "member"
	delete(fake.admins, selfID)
	f := newFixture(t, fake)
	for peer, c := range map[tg.PeerClass]struct{ text, want string }{
		here:                            {"4", "权限不足：移出成员要群主身份或封禁成员权限"},
		&tg.PeerChannel{ChannelID: 300}: {"2 30 search", "频道的订阅者不能发言"},
		&tg.PeerUser{UserID: selfID}:    {"4 search", "只能用于群组"},
	} {
		message, ok := kit.IsUserError(f.run(t, peer, c.text))
		if !ok || !strings.Contains(message, c.want) {
			t.Errorf("%s：%q，应包含 %q", c.text, message, c.want)
		}
	}
	// 没有封禁权限也能搜索。
	if err := f.run(t, here, "5 search"); err != nil {
		t.Fatal(err)
	}
	// 自己不算：没有管理员身份时也不会出现在名单里。
	if got := last(f.fake.take()); !strings.Contains(got, "符合条件 3 人") {
		t.Errorf("没有权限时搜索：%q", got)
	}
}

// 基本群组：成员从 messages.getFullChat 一次拿全，直接移出，管理员跳过。
func TestBasicGroup(t *testing.T) {
	fake := newFakeGroup(users(10, 3, true)...)
	fake.admins[11] = true
	f := newFixture(t, fake)
	if err := f.run(t, &tg.PeerChat{ChatID: 200}, "4"); err != nil {
		t.Fatal(err)
	}
	calls := f.fake.take()
	if !slices.Equal(calls[:4], []string{"full-chat", "edit ⏳ 正在清理 小群 里已注销的账号…", "full-chat", "remove 10"}) || !slices.Contains(calls, "remove 12") || slices.Contains(calls, "remove 11") {
		t.Errorf("请求：%v", calls)
	}
	if got := last(calls); !strings.Contains(got, "已移出 2 人") {
		t.Errorf("结果：%q", got)
	}
}
