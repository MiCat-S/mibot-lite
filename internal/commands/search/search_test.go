package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
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

func TestParseOptions(t *testing.T) {
	cases := []struct {
		args   []string
		forced bool
		want   options
	}{
		{[]string{"海贼王", "-S", "第一集"}, false, options{spoiler: true, query: "海贼王 第一集"}},
		{[]string{"-r", "kkp", "-s"}, false, options{spoiler: true, random: true, kkp: true}},
		{[]string{"KKP"}, false, options{kkp: true}},
		{[]string{"-s"}, true, options{spoiler: true, kkp: true}},
		{nil, false, options{}},
	}
	for _, c := range cases {
		if got := parseOptions(c.args, c.forced); got != c.want {
			t.Errorf("parseOptions(%q, %v) = %+v", c.args, c.forced, got)
		}
	}
}

func video(peer int64, id int, text, name string, seconds float64) *tg.Message {
	document := &tg.Document{ID: int64(id), AccessHash: 1, FileReference: []byte{1}, MimeType: "video/mp4", Size: 3000, DCID: 2,
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeVideo{Duration: seconds, W: 640, H: 360}, &tg.DocumentAttributeFilename{FileName: name}}}
	message := &tg.Message{ID: id, PeerID: &tg.PeerChannel{ChannelID: peer}, Message: text, Date: 1, Media: &tg.MessageMediaDocument{Document: document}}
	// 真消息从网络解码出来时标志位都已设好（GetMedia 看的就是它），这里照样设上。
	message.SetFlags()
	return message
}

func TestMatching(t *testing.T) {
	if got := normalize("  One_Piece-EP.01 | 1080P#HD "); got != "one piece ep 01 1080p hd" {
		t.Errorf("normalize：%q", got)
	}
	message := video(1, 1, "【海贼王】第 1 集", "One.Piece.EP01.mp4", 60)
	for query, want := range map[string]bool{
		"海贼王":          true,
		"one piece":    true,
		"piece one":    true, // 每个词都在
		"ep01":         true,
		"EP 01":        true, // 番号式，去掉空格再比
		"海贼王 第 2 集":    false,
		"naruto":       false,
		"one piece ep": true,
	} {
		if got := matches(message, query); got != want {
			t.Errorf("matches(%q) = %v", query, got)
		}
	}
	if got := score(message, "one piece"); got != 100 {
		t.Errorf("文件名命中：%d", got)
	}
	if got := score(message, "海贼王"); got != 50 {
		t.Errorf("文字命中：%d", got)
	}
	if !isAd(video(1, 2, "加我 VPN 翻墙", "", 30), []string{"vpn"}) || !isAd(video(1, 3, "", "推广.mp4", 30), defaultFilters) {
		t.Error("广告没被过滤")
	}
	if isAd(message, []string{""}) {
		t.Error("空过滤词不该匹配所有消息")
	}
	if _, _, ok := videoDocument(&tg.Message{Media: &tg.MessageMediaWebPage{}}); ok {
		t.Error("链接预览不算视频")
	}
}

func TestParseHandle(t *testing.T) {
	cases := map[string][3]string{
		"@Channel":                         {"Channel", "", ""},
		"channel":                          {"channel", "", ""},
		"https://t.me/channel/123":         {"channel", "", ""},
		"t.me/s/channel":                   {"channel", "", ""},
		"https://www.telegram.me/@channel": {"channel", "", ""},
		"https://t.me/+AbCdEf":             {"", "AbCdEf", ""},
		"https://t.me/joinchat/AbCdEf":     {"", "AbCdEf", ""},
		"-1001234567890":                   {"", "", "-1001234567890"},
	}
	for value, want := range cases {
		username, invite, id := parseHandle(value)
		if [3]string{username, invite, id} != want {
			t.Errorf("parseHandle(%q) = %q %q %q", value, username, invite, id)
		}
	}
}

func TestCaptionAndSplit(t *testing.T) {
	long := strings.Repeat("😀", 600)
	if got := caption(long); command.UTF16Len(got) > captionLimit || !strings.HasSuffix(got, "…") {
		t.Errorf("说明没截断：%d", command.UTF16Len(got))
	}
	if got := caption("短"); got != "短" {
		t.Error(got)
	}
	if got := splitHandles(" @a \\ @b\n@c  https://t.me/d "); !slices.Equal(got, []string{"@a", "@b", "@c", "https://t.me/d"}) {
		t.Errorf("splitHandles：%q", got)
	}
}

func TestChoose(t *testing.T) {
	list := []candidate{
		{message: video(1, 1, "", "other.mp4", 100)},
		{message: video(1, 2, "one piece", "x.mp4", 50)},
		{message: video(1, 3, "one piece", "one piece.mp4", 10)},
		{message: video(1, 4, "one piece", "x.mp4", 80)},
	}
	if got := choose(list, options{query: "one piece"}, nil); got.message.ID != 3 {
		t.Errorf("应先按分数：%d", got.message.ID)
	}
	if got := choose(list[:2], options{query: "one piece"}, nil); got.message.ID != 2 {
		t.Errorf("%d", got.message.ID)
	}
	if got := choose([]candidate{list[1], list[3]}, options{query: "one piece"}, nil); got.message.ID != 4 {
		t.Errorf("同分按时长：%d", got.message.ID)
	}
	if got := choose(list, options{random: true}, func(n int) int { return n - 1 }); got.message.ID != 4 {
		t.Errorf("随机：%d", got.message.ID)
	}
}

// MiBox 的配置文件原样读得进来，写回去字段名不变，默认频道为空时是 null。
func TestConfigCompatible(t *testing.T) {
	raw := `{"schemaVersion":1,"defaultChannel":"@a","channelList":[{"title":"A","handle":"@a","linkedGroup":"@a_chat"},{"title":"B","handle":"@b"}],"adFilters":["x"]}`
	var cfg config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if *cfg.DefaultChannel != "@a" || cfg.ChannelList[0].LinkedGroup != "@a_chat" || len(cfg.AdFilters) != 1 {
		t.Errorf("%+v", cfg)
	}
	cfg.DefaultChannel = nil
	encoded, _ := json.Marshal(cfg)
	if !strings.Contains(string(encoded), `"defaultChannel":null`) || strings.Contains(string(encoded[strings.Index(string(encoded), `"B"`):]), "linkedGroup") {
		t.Errorf("写回：%s", encoded)
	}
	if d := defaults(); len(d.AdFilters) != len(defaultFilters) || d.DefaultChannel != nil {
		t.Error("默认值")
	}
}

// fakeTelegram 是几个频道和一个对话（用户 1 的收藏夹）。
type fakeTelegram struct {
	mu       sync.Mutex
	channels map[string]*tg.Channel
	messages map[int64][]*tg.Message
	comments map[int][]*tg.Message
	linked   map[int64]int64
	restrict bool
	// forwardErr 不为空时转发返回这个错误（不是禁止转发的那种）。
	forwardErr error
	// reply 是命令回复的那条消息，按编号读回时返回它。
	reply     *tg.Message
	edits     []string
	content   []byte
	calls     []string
	uploaded  bytes.Buffer
	sentMedia []*tg.MessagesSendMediaRequest
}

func respond(output bin.Decoder, value bin.Encoder) error {
	var buffer bin.Buffer
	if err := value.Encode(&buffer); err != nil {
		return err
	}
	return output.Decode(&buffer)
}

func (f *fakeTelegram) channelByID(id int64) *tg.Channel {
	for _, channel := range f.channels {
		if channel.ID == id {
			return channel
		}
	}
	return nil
}

func peerChannel(peer tg.InputPeerClass) int64 {
	if channel, ok := peer.(*tg.InputPeerChannel); ok {
		return channel.ChannelID
	}
	return 0
}

// page 按真实接口的规则取：编号倒序，小于 offset，最多 limit 条。
func page(list []*tg.Message, offset, limit int, keep func(*tg.Message) bool) []tg.MessageClass {
	sorted := slices.Clone(list)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].ID > sorted[b].ID })
	var out []tg.MessageClass
	for _, message := range sorted {
		if (offset == 0 || message.ID < offset) && (keep == nil || keep(message)) && len(out) < limit {
			out = append(out, message)
		}
	}
	return out
}

func (f *fakeTelegram) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch request := input.(type) {
	case *tg.ContactsResolveUsernameRequest:
		f.calls = append(f.calls, "resolve "+request.Username)
		channel, ok := f.channels[strings.ToLower(request.Username)]
		if !ok {
			return tgerr.New(400, "USERNAME_NOT_OCCUPIED")
		}
		return respond(output, &tg.ContactsResolvedPeer{Peer: &tg.PeerChannel{ChannelID: channel.ID}, Chats: []tg.ChatClass{channel}})
	case *tg.ChannelsGetFullChannelRequest:
		id := request.Channel.(*tg.InputChannel).ChannelID
		full := &tg.ChannelFull{ID: id, ChatPhoto: &tg.PhotoEmpty{}}
		chats := []tg.ChatClass{f.channelByID(id)}
		if linked := f.linked[id]; linked != 0 {
			full.SetLinkedChatID(linked)
			chats = append(chats, f.channelByID(linked))
		}
		return respond(output, &tg.MessagesChatFull{FullChat: full, Chats: chats})
	case *tg.MessagesSearchRequest:
		id := peerChannel(request.Peer)
		_, video := request.Filter.(*tg.InputMessagesFilterVideo)
		f.calls = append(f.calls, fmt.Sprintf("search %d q=%q video=%v limit=%d", id, request.Q, video, request.Limit))
		keep := func(message *tg.Message) bool {
			if video {
				if _, _, ok := videoDocument(message); !ok {
					return false
				}
			}
			return request.Q == "" || strings.Contains(strings.ToLower(message.Message+" "+fileName(message)), strings.ToLower(request.Q))
		}
		return respond(output, &tg.MessagesMessages{Messages: page(f.messages[id], request.OffsetID, request.Limit, keep)})
	case *tg.MessagesGetHistoryRequest:
		id := peerChannel(request.Peer)
		f.calls = append(f.calls, fmt.Sprintf("history %d offset=%d", id, request.OffsetID))
		return respond(output, &tg.MessagesMessages{Messages: page(f.messages[id], request.OffsetID, request.Limit, nil)})
	case *tg.MessagesGetRepliesRequest:
		f.calls = append(f.calls, fmt.Sprintf("replies %d", request.MsgID))
		return respond(output, &tg.MessagesMessages{Messages: page(f.comments[request.MsgID], 0, request.Limit, nil)})
	case *tg.MessagesForwardMessagesRequest:
		f.calls = append(f.calls, fmt.Sprintf("forward %d#%v", peerChannel(request.FromPeer), request.ID))
		if f.restrict {
			return tgerr.New(400, "CHAT_FORWARDS_RESTRICTED")
		}
		if f.forwardErr != nil {
			return f.forwardErr
		}
		return respond(output, &tg.Updates{})
	case *tg.MessagesDeleteMessagesRequest:
		f.calls = append(f.calls, fmt.Sprintf("delete %v", request.ID))
		return respond(output, &tg.MessagesAffectedMessages{Pts: 1, PtsCount: 1})
	case *tg.MessagesEditMessageRequest:
		f.edits = append(f.edits, request.Message)
		return respond(output, &tg.Updates{})
	case *tg.MessagesGetMessagesRequest:
		var found []tg.MessageClass
		if f.reply != nil {
			found = append(found, f.reply)
		}
		return respond(output, &tg.MessagesMessages{Messages: found})
	case *tg.UploadGetFileRequest:
		start := min(int(request.Offset), len(f.content))
		end := min(start+request.Limit, len(f.content))
		return respond(output, &tg.UploadFile{Type: &tg.StorageFileMp4{}, Bytes: f.content[start:end]})
	case *tg.UploadSaveFilePartRequest:
		f.uploaded.Write(request.Bytes)
		return respond(output, &tg.BoolTrue{})
	case *tg.MessagesSendMediaRequest:
		f.calls = append(f.calls, "send-media")
		f.sentMedia = append(f.sentMedia, request)
		return respond(output, &tg.Updates{})
	case *tg.MessagesSendMessageRequest:
		return respond(output, &tg.UpdateShortSentMessage{ID: 99})
	}
	return tgerr.New(400, "UNEXPECTED_REQUEST")
}

func newFake() *fakeTelegram {
	channel := func(id int64, name string, megagroup bool) *tg.Channel {
		return &tg.Channel{ID: id, AccessHash: id * 10, Title: strings.ToUpper(name), Username: name, Broadcast: !megagroup, Megagroup: megagroup,
			Photo: &tg.ChatPhotoEmpty{}}
	}
	return &fakeTelegram{
		channels: map[string]*tg.Channel{"first": channel(10, "first", false), "second": channel(20, "second", false),
			"talk": channel(30, "talk", true)},
		messages: map[int64][]*tg.Message{},
		comments: map[int][]*tg.Message{},
		linked:   map[int64]int64{10: 30},
	}
}

type harness struct {
	fake    *fakeTelegram
	s       *service
	uploads []string
}

func newHarness(t *testing.T, fake *fakeTelegram, cfg config) *harness {
	t.Helper()
	h := &harness{fake: fake}
	h.s = &service{store: store.New(filepath.Join(t.TempDir(), "search.json"), defaults), partial: filepath.Join(t.TempDir(), "partial"),
		pick: func(n int) int { return 0 }, linkedOf: linkedGroup}
	h.s.resolve = h.s.sources.resolve
	h.s.upload = func(ctx context.Context, inv *command.Invocation, picked candidate, text string, spoiler bool) error {
		h.uploads = append(h.uploads, fmt.Sprintf("%d text=%q spoiler=%v", picked.message.ID, text, spoiler))
		return nil
	}
	if err := h.s.store.Update(func(current *config) error { *current = cfg; return nil }); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) run(t *testing.T, args ...string) error {
	t.Helper()
	return h.runReplying(t, 0, args...)
}

// runReplying 执行一次命令；replyTo 不为 0 时命令回复了那条消息。
func (h *harness) runReplying(t *testing.T, replyTo int, args ...string) error {
	t.Helper()
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	client := bot.FromAPI(tg.NewClient(h.fake), peers, &tg.User{ID: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	text := ".so " + strings.Join(args, " ")
	inv := &command.Invocation{Prefix: ".", Command: "so", Args: args, Text: text, Client: client,
		Message: &bot.Message{ID: 500, Peer: &tg.PeerUser{UserID: 1}, ChatID: "1", Text: text, Out: true, Saved: true, ReplyToID: replyTo},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil))}
	return h.s.handle(context.Background(), inv)
}

func handle(value string) *string { return &value }

// 关键词搜索：默认频道先搜；讨论组评论里的视频优先；广告被过滤；找到就不再搜下一个频道；
// 转发成功后删掉命令消息。
func TestSearchForwardsBestMatch(t *testing.T) {
	fake := newFake()
	post := &tg.Message{ID: 7, PeerID: &tg.PeerChannel{ChannelID: 30}, Message: "海贼王 第1集 讨论", Date: 1}
	post.SetReplies(tg.MessageReplies{Replies: 2})
	fake.messages[30] = []*tg.Message{post}
	fake.comments[7] = []*tg.Message{video(30, 8, "推广 海贼王", "", 60), video(30, 9, "", "海贼王.mp4", 90)}
	fake.messages[10] = []*tg.Message{video(10, 1, "海贼王 第1集", "", 30), video(10, 2, "海贼王 VPN", "", 30)}
	fake.messages[20] = []*tg.Message{video(20, 1, "海贼王", "", 30)}
	h := newHarness(t, fake, config{SchemaVersion: 1, DefaultChannel: handle("@first"),
		ChannelList: []channel{{Title: "Second", Handle: "@second"}, {Title: "First", Handle: "@first", LinkedGroup: "@talk"}}, AdFilters: []string{"推广", "vpn"}})
	if err := h.run(t, "海贼王"); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(fake.calls, "\n")
	if strings.Contains(calls, "search 20") {
		t.Errorf("默认频道找到了就不该再搜别的频道：\n%s", calls)
	}
	// 讨论组里的 9 号文件名完全匹配（100 分），比频道里只有文字匹配的 1 号（50 分）优先。
	if !strings.Contains(calls, "forward 30#[9]") || !strings.HasSuffix(calls, "delete [500]") {
		t.Errorf("应转发讨论组的 9 号并删掉命令：\n%s", calls)
	}
}

// 相册：一条匹配，整个相册的视频都算；kkp 只要 20 秒到 3 分钟的；-r 搜完全部频道。
func TestSearchAlbumAndKKP(t *testing.T) {
	fake := newFake()
	album := []*tg.Message{video(10, 11, "合集 海贼王", "", 5), video(10, 12, "", "a.mp4", 200), video(10, 13, "", "b.mp4", 100)}
	for _, message := range album {
		message.SetGroupedID(77)
	}
	fake.messages[10] = append(album, video(10, 14, "别的", "", 60))
	fake.messages[20] = []*tg.Message{video(20, 3, "海贼王", "", 15), video(20, 4, "", "", 181), video(20, 5, "", "", 120)}
	h := newHarness(t, fake, config{SchemaVersion: 1, ChannelList: []channel{{Title: "First", Handle: "@first"}, {Title: "Second", Handle: "@second"}}})
	var picked []int
	h.s.pick = func(n int) int { picked = append(picked, n); return n - 1 }
	if err := h.run(t, "海贼王", "-r"); err != nil {
		t.Fatal(err)
	}
	// 相册 11、12、13 加上第二个频道的 3，共 4 条，随机挑到最后一条。
	if len(picked) != 1 || picked[0] != 4 || !strings.Contains(strings.Join(fake.calls, "\n"), "forward 20#[3]") {
		t.Errorf("随机：%v\n%s", picked, strings.Join(fake.calls, "\n"))
	}
	fake.calls, picked = nil, nil
	if err := h.run(t, "kkp", "-s"); err != nil {
		t.Fatal(err)
	}
	// 时长合格的：14（60 秒）、13（100 秒）、5（120 秒）；-s 不转发，直接下载上传。
	if len(picked) != 1 || picked[0] != 3 || len(h.uploads) != 1 || h.uploads[0] != `5 text="" spoiler=true` {
		t.Errorf("kkp：%v %q", picked, h.uploads)
	}
	if strings.Contains(strings.Join(fake.calls, "\n"), "forward") {
		t.Error("防剧透不该转发")
	}
}

// 禁止转发的对话：改为下载上传，说明文字是关键词；用户名已不存在的频道源被移除。
func TestSearchFallbackAndPrune(t *testing.T) {
	fake := newFake()
	fake.restrict = true
	fake.messages[20] = []*tg.Message{video(20, 3, "海贼王", "", 15)}
	h := newHarness(t, fake, config{SchemaVersion: 1, DefaultChannel: handle("@gone"),
		ChannelList: []channel{{Title: "Gone", Handle: "@gone"}, {Title: "Second", Handle: "@second"}}})
	if err := h.run(t, "海贼王"); err != nil {
		t.Fatal(err)
	}
	if len(h.uploads) != 1 || h.uploads[0] != `3 text="海贼王" spoiler=false` {
		t.Errorf("回退：%q", h.uploads)
	}
	cfg, _ := h.s.read()
	if len(cfg.ChannelList) != 1 || cfg.ChannelList[0].Handle != "@second" || cfg.DefaultChannel != nil {
		t.Errorf("没有移除不存在的频道：%+v", cfg)
	}
	err := h.run(t, "火影")
	if text, ok := kit.IsUserError(err); !ok || text != "所有频道里都没有找到匹配的视频" {
		t.Errorf("没找到：%v", err)
	}
}

// 下载上传：视频原样写回，带防剧透标记、视频属性和说明。
func TestReupload(t *testing.T) {
	fake := newFake()
	fake.content = bytes.Repeat([]byte("v"), 3000)
	peers := bot.NewPeerCache()
	peers.SetSelf(1)
	client := bot.FromAPI(tg.NewClient(fake), peers, &tg.User{ID: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	message := &bot.Message{ID: 500, Peer: &tg.PeerUser{UserID: 1}, ChatID: "1", ReplyToID: 42}
	inv := &command.Invocation{Prefix: ".", Client: client, Message: message, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	picked := candidate{message: video(10, 5, "", "a.mp4", 61), peer: &tg.InputPeerChannel{ChannelID: 10, AccessHash: 100}}
	partial := filepath.Join(t.TempDir(), "partial")
	if err := reupload(context.Background(), inv, partial, picked, "关键词", true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fake.uploaded.Bytes(), fake.content) {
		t.Errorf("上传的内容不对：%d 字节", fake.uploaded.Len())
	}
	if len(fake.sentMedia) != 1 {
		t.Fatal("没有发出视频")
	}
	sent := fake.sentMedia[0]
	media := sent.Media.(*tg.InputMediaUploadedDocument)
	attribute := media.Attributes[0].(*tg.DocumentAttributeVideo)
	reply, _ := sent.GetReplyTo()
	if !media.Spoiler || media.MimeType != "video/mp4" || attribute.Duration != 61 || !attribute.SupportsStreaming || sent.Message != "关键词" ||
		reply.(*tg.InputReplyToMessage).ReplyToMsgID != 42 {
		t.Errorf("发送请求：%+v %+v", media, sent)
	}
	if matches, _ := filepath.Glob(filepath.Join(partial, "*")); len(matches) != 0 {
		t.Errorf("暂存文件没删：%v", matches)
	}
	big := video(10, 6, "", "", 10)
	big.Media.(*tg.MessageMediaDocument).Document.(*tg.Document).Size = 3 << 30
	err := reupload(context.Background(), inv, partial, candidate{message: big}, "", false)
	if text, ok := kit.IsUserError(err); !ok || !strings.Contains(text, "超过 2 GB 上限") {
		t.Errorf("超大视频：%v", err)
	}
}

// 频道源管理：add（记下讨论组、第一个成为默认、重复和不存在的报出来）、list、default、del、ad。
func TestManageChannels(t *testing.T) {
	fake := newFake()
	h := newHarness(t, fake, defaults())
	if err := h.run(t, "add", "@first", "\\", "https://t.me/second", "@first", "@nobody"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := h.s.read()
	if len(cfg.ChannelList) != 2 || cfg.ChannelList[0] != (channel{Title: "FIRST", Handle: "@first", LinkedGroup: "@talk"}) ||
		cfg.ChannelList[1].Handle != "https://t.me/second" || *cfg.DefaultChannel != "@first" {
		t.Fatalf("添加结果：%+v", cfg)
	}
	err := h.run(t, "add", "@first")
	if text, ok := kit.IsUserError(err); !ok || text != "没有添加任何频道：@first：已存在" {
		t.Errorf("重复添加：%v", err)
	}
	if err := h.run(t, "default", "https://t.me/second"); err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "default", "@missing"); err == nil {
		t.Error("没添加的频道不能设为默认")
	}
	if got := renderList(mustRead(t, h), "."); !strings.Contains(got, "2. SECOND · <code>https://t.me/second</code>（默认）") {
		t.Errorf("列表：%s", got)
	}
	if err := h.run(t, "del", "2"); err != nil {
		t.Fatal(err)
	}
	if cfg := mustRead(t, h); len(cfg.ChannelList) != 1 || *cfg.DefaultChannel != "@first" {
		t.Errorf("删掉默认频道后应改用剩下的第一个：%+v", cfg)
	}
	if err := h.run(t, "del", "@nope"); err == nil {
		t.Error("删除不存在的应该报错")
	}
	if err := h.run(t, "ad", "add", "vpn", "新词"); err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "ad", "del", "广告", "新词"); err != nil {
		t.Fatal(err)
	}
	filters := mustRead(t, h).AdFilters
	if len(filters) != len(defaultFilters)-1 || slices.Contains(filters, "广告") || slices.Contains(filters, "新词") {
		t.Errorf("过滤词：%d %v", len(filters), filters[:3])
	}
	if err := h.run(t, "del", "all"); err != nil {
		t.Fatal(err)
	}
	if cfg := mustRead(t, h); len(cfg.ChannelList) != 0 || cfg.DefaultChannel != nil {
		t.Errorf("全部删除：%+v", cfg)
	}
	for _, args := range [][]string{{"add"}, {"del"}, {"default"}, {"ad"}, {"ad", "add"}} {
		err := h.run(t, args...)
		if text, ok := kit.IsUserError(err); !ok || !strings.HasPrefix(text, "用法：.so ") {
			t.Errorf("%v：%v", args, err)
		}
	}
	err = h.run(t, "海贼王")
	if text, ok := kit.IsUserError(err); !ok || !strings.Contains(text, "请先用 .so add 添加") {
		t.Errorf("没有频道时：%v", err)
	}
	err = h.run(t, "-s")
	if text, ok := kit.IsUserError(err); !ok || text != "请输入搜索关键词" {
		t.Errorf("没有关键词：%v", err)
	}
}

func mustRead(t *testing.T, h *harness) config {
	t.Helper()
	cfg, err := h.s.read()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// 频道源的格式在发请求之前检查。
func TestValidHandle(t *testing.T) {
	for value, want := range map[string]bool{
		"@first": true, "first_ch": true, "https://t.me/first/12": true, "https://t.me/+AbCdEf12": true,
		"-1001234567890": true, "@abc": false, "1abcde": false, "中文频道": false, "hello world": false,
		"https://t.me/": false, "https://t.me/+a": false, "a.b.c.d": false,
	} {
		if got := validHandle(value); got != want {
			t.Errorf("validHandle(%q) = %v", value, got)
		}
	}
}

func countCalls(calls []string, prefix string) int {
	count := 0
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			count++
		}
	}
	return count
}

// add：格式不对的不发请求；一次最多 50 个，多出的说明跳过了几个；没缓存的两次解析之间停一下。
func TestAddLimits(t *testing.T) {
	fake := newFake()
	h := newHarness(t, fake, defaults())
	args := []string{"add", "中文", "a.b"}
	for index := range 55 {
		args = append(args, fmt.Sprintf("@chan%04d", index))
	}
	err := h.run(t, args...)
	text, ok := kit.IsUserError(err)
	if !ok || !strings.Contains(text, "中文：不是有效的频道用户名或链接") || !strings.Contains(text, "一次最多处理 50 个，另外 7 个没有处理") || !strings.Contains(text, "…还有") {
		t.Errorf("结果：%v", err)
	}
	if got := countCalls(fake.calls, "resolve "); got != 48 {
		t.Errorf("应解析 48 个（50 个里去掉 2 个格式不对的），实际 %d", got)
	}
	fake.calls = nil
	h.s.lookupGap = 30 * time.Millisecond
	started := time.Now()
	if err := h.run(t, "add", "@first", "@second", "@talk"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 60*time.Millisecond {
		t.Errorf("三次解析之间应该停两次：%v", elapsed)
	}
}

// import 只收文件，按行拆；一行里有空格的不是频道源，不发请求。
func TestImportNeedsDocument(t *testing.T) {
	fake := newFake()
	h := newHarness(t, fake, defaults())
	photo := &tg.Message{ID: 9, PeerID: &tg.PeerUser{UserID: 1}, Media: &tg.MessageMediaPhoto{Photo: &tg.PhotoEmpty{}}}
	photo.SetFlags()
	fake.reply = photo
	err := h.runReplying(t, 9, "import")
	if text, ok := kit.IsUserError(err); !ok || !strings.Contains(text, "请回复备份文件") {
		t.Errorf("回复图片：%v", err)
	}
	fake.content = []byte("@first\r\nhello world 这是一篇文章\n\n@second\n")
	document := &tg.Document{ID: 1, AccessHash: 1, FileReference: []byte{1}, MimeType: "text/plain", Size: int64(len(fake.content)), DCID: 2,
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "search-channels.txt"}}}
	file := &tg.Message{ID: 9, PeerID: &tg.PeerUser{UserID: 1}, Media: &tg.MessageMediaDocument{Document: document}}
	file.SetFlags()
	fake.reply = file
	if err := h.runReplying(t, 9, "import"); err != nil {
		t.Fatal(err)
	}
	if cfg := mustRead(t, h); len(cfg.ChannelList) != 2 || countCalls(fake.calls, "resolve ") != 2 {
		t.Errorf("导入：%+v %q", cfg.ChannelList, fake.calls)
	}
	if last := fake.edits[len(fake.edits)-1]; !strings.Contains(last, "hello world 这是一篇文章：不是有效的频道用户名或链接") {
		t.Errorf("结果：%q", last)
	}
}

// 转发失败但不是禁止转发：报错，不去下载上传；带 noforwards 的消息不尝试转发。
func TestForwardErrorsDoNotReupload(t *testing.T) {
	fake := newFake()
	fake.forwardErr = tgerr.New(400, "CHAT_SEND_MEDIA_FORBIDDEN")
	fake.messages[10] = []*tg.Message{video(10, 1, "海贼王", "", 30)}
	h := newHarness(t, fake, config{SchemaVersion: 1, ChannelList: []channel{{Title: "First", Handle: "@first"}}})
	err := h.run(t, "海贼王")
	if text, ok := kit.IsUserError(err); !ok || text != "转发视频失败（CHAT_SEND_MEDIA_FORBIDDEN）" || len(h.uploads) != 0 {
		t.Errorf("%v %q", err, h.uploads)
	}
	protected := video(10, 2, "海贼王 保护", "", 30)
	protected.Noforwards = true
	fake.messages[10] = []*tg.Message{protected}
	fake.calls = nil
	if err := h.run(t, "海贼王"); err != nil {
		t.Fatal(err)
	}
	if countCalls(fake.calls, "forward") != 0 || len(h.uploads) != 1 {
		t.Errorf("受保护的消息：%q %q", fake.calls, h.uploads)
	}
}

// del all 删很多频道时，结果分页、名单截断，不会超出一条消息的长度。
func TestRemoveManyPaginates(t *testing.T) {
	fake := newFake()
	cfg := defaults()
	for index := range 150 {
		cfg.ChannelList = append(cfg.ChannelList, channel{Title: strings.Repeat("频", 60) + strconv.Itoa(index), Handle: fmt.Sprintf("@c%04d", index)})
	}
	h := newHarness(t, fake, cfg)
	if err := h.run(t, "del", "all"); err != nil {
		t.Fatal(err)
	}
	last := fake.edits[len(fake.edits)-1]
	if !strings.Contains(last, "已移除 150 个频道") || !strings.Contains(last, "…还有 130 个") || command.UTF16Len(last) > 4096 {
		t.Errorf("结果：%d %q", command.UTF16Len(last), last[:80])
	}
}

// 全角空格、不换行空格和普通空格一样当分隔。
func TestUnicodeSpaces(t *testing.T) {
	if got := normalize("One\u00a0Piece\u3000第1集"); got != "one piece 第1集" {
		t.Errorf("normalize：%q", got)
	}
	if !matches(video(1, 1, "海贼王\u3000第1集", "", 10), "海贼王 第1集") {
		t.Error("全角空格隔开的文字应该匹配")
	}
	if !matches(video(1, 1, "", "abc\u3000123.mp4", 10), "abc123") {
		t.Error("番号中间的全角空格应该忽略")
	}
	if got := splitHandles("@a\u3000@b"); !slices.Equal(got, []string{"@a", "@b"}) {
		t.Errorf("splitHandles：%q", got)
	}
}
