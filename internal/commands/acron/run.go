package acron

import (
	"context"
	"errors"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/robfig/cron/v3"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// cronParser 和 .sum 的设置一样（robfig/cron，六段时第一段是秒）。V2 只收六段，
// 段数在 parseCron 里先挡掉，所以五段和 @daily 这类写法进不来。
var cronParser = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// parseCron 解析六段 Cron（秒 分 时 日 月 星期）。
func parseCron(expression string) (cron.Schedule, error) {
	fields := strings.Fields(expression)
	if len(fields) != 6 {
		return nil, kit.Fail("Cron 表达式要写 6 段：秒 分 时 日 月 星期")
	}
	fields[5] = sundaySeven(fields[5])
	schedule, err := cronParser.Parse(strings.Join(fields, " "))
	if err != nil {
		return nil, kit.Fail("Cron 表达式无效，6 段依次是 秒 分 时 日 月 星期")
	}
	return schedule, nil
}

// sundaySeven 处理星期里的 7。V2 用的 Node cron 包把 7 也当星期天，robfig/cron 只认 0–6，
// 不换的话 MiBox 搬来的「* * * * * 7」「… 1-7」就注册不上：单独的 7 换成 0，
// 以 7 结尾的范围改成到 6 再加上 0。
func sundaySeven(field string) string {
	parts := strings.Split(field, ",")
	for index, part := range parts {
		switch {
		case part == "7":
			parts[index] = "0"
		case strings.HasSuffix(part, "-7") && !strings.Contains(part, "/"):
			parts[index] = strings.TrimSuffix(part, "-7") + "-6,0"
		}
	}
	return strings.Join(parts, ",")
}

// nextRun 是 expression 在 now 之后的下一次执行时间（北京时间）。
func nextRun(expression string, now time.Time) (time.Time, bool) {
	schedule, err := parseCron(expression)
	if err != nil {
		return time.Time{}, false
	}
	return schedule.Next(now.In(shanghai)), true
}

// jsUnicodeEscape 是 JS 正则里的 \uXXXX 和 \u{XXXXX}，Go 的写法是 \x{…}。
var jsUnicodeEscape = regexp.MustCompile(`\\u\{([0-9A-Fa-f]{1,6})\}|\\u([0-9A-Fa-f]{4})`)

// compileRegex 编译 del_re 的正则。写法同 V2：/模式/标志，或者直接写模式。
//
// V2 用 JS 的正则在单独的线程里跑，带超时；Go 的正则保证线性时间，不需要超时，
// 代价是不支持环视和反向引用，这类写法在添加时就报出来。标志 i、m、s 照用；
// g 在 V2 里本来就被去掉，u 在 Go 里是默认行为，都忽略；别的标志 V2 也会报错。
func compileRegex(raw string) (*regexp.Regexp, error) {
	pattern, flags := strings.TrimSpace(raw), ""
	if strings.HasPrefix(pattern, "/") && strings.LastIndex(pattern, "/") > 0 {
		last := strings.LastIndex(pattern, "/")
		pattern, flags = pattern[1:last], pattern[last+1:]
	}
	if pattern == "" {
		return nil, kit.Fail("正则表达式是空的")
	}
	modifiers := ""
	for _, flag := range flags {
		switch flag {
		case 'i', 'm', 's':
			if !strings.ContainsRune(modifiers, flag) {
				modifiers += string(flag)
			}
		case 'g', 'u':
		default:
			return nil, kit.Failf("不支持正则标志 %c，能用的是 i、m、s", flag)
		}
	}
	pattern = jsUnicodeEscape.ReplaceAllString(pattern, `\x{$1$2}`)
	if modifiers != "" {
		pattern = "(?" + modifiers + ")" + pattern
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, kit.Fail("正则表达式无效（不支持环视和反向引用）")
	}
	return compiled, nil
}

// maxScan 是 del_re 最多看的消息数，同 V2 的 DEL_RE_MAX_LIMIT。
const maxScan = 1000

// runTimeout 是一次任务的时限。最慢的是 del_re：读 1000 条、分批删。
var runTimeout = 10 * time.Minute

// start 是账号连上后的后台任务：把开着的任务挂上调度器，断开时停下。
func (s *service) start(ctx context.Context, client *bot.Client) {
	s.mu.Lock()
	s.client, s.ctx = client, ctx
	s.mu.Unlock()
	st, err := s.read()
	if err != nil {
		s.log.Error("acron.load_failed", "error", err.Error())
		return
	}
	for _, t := range st.Tasks {
		if t.Disabled {
			continue
		}
		schedule, err := parseCron(t.Cron)
		if err != nil {
			s.log.Error("acron.invalid_cron", "task", t.ID, "cron", t.Cron)
			continue
		}
		s.schedule(t.ID, schedule)
	}
	s.cron.Start()
	<-ctx.Done()
	s.cron.Stop()
}

// schedule 把任务挂到调度器上，已经挂着的不重复挂。触发时按 ID 重新读任务，
// 这样关闭、删除之后即使还有一次触发在路上，也不会照旧执行。
func (s *service) schedule(id string, schedule cron.Schedule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[id]; exists {
		return
	}
	s.entries[id] = s.cron.Schedule(schedule, cron.FuncJob(func() { s.fire(id) }))
}

func (s *service) unschedule(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.entries[id]; ok {
		s.cron.Remove(entry)
		delete(s.entries, id)
	}
}

// fire 是调度器到点时调用的。同一个任务上一次还没跑完就跳过这一次；不同的任务
// 一个接一个跑，同 V2：两个任务同时往一个对话里发、删，先后就说不准了。
func (s *service) fire(id string) {
	s.mu.Lock()
	client, ctx := s.client, s.ctx
	if client == nil || ctx == nil || s.running[id] {
		s.mu.Unlock()
		return
	}
	s.running[id] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, id)
		s.mu.Unlock()
	}()
	s.serial.Lock()
	defer s.serial.Unlock()
	if ctx.Err() != nil {
		return
	}
	s.execute(ctx, client, id)
}

// execute 执行一次任务，把结果或错误记进任务，列表里能看到。
func (s *service) execute(ctx context.Context, client *bot.Client, id string) {
	st, err := s.read()
	if err != nil {
		s.log.Error("acron.load_failed", "task", id, "error", err.Error())
		return
	}
	index := st.find(id)
	if index < 0 || st.Tasks[index].Disabled {
		return
	}
	t := st.Tasks[index]
	if t.Delivery == deliveryPrepared {
		s.log.Warn("acron.skipped_unconfirmed", "task", id)
		kit.Warn(s.a, "acron.save_failed", s.updateTask(id, func(t *task) {
			t.Delivery = deliveryPending
			t.LastError = "上次执行没等到结果进程就停了，这次跳过，免得重复"
		}))
		return
	}
	// 先记下「开始了」再动手：记不下来就不执行，否则中途停掉后分辨不出做没做过。
	if err := s.updateTask(id, func(t *task) { t.Delivery = deliveryPrepared }); err != nil {
		s.log.Error("acron.save_failed", "task", id, "error", err.Error())
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, runTimeout)
	result, chatID, err := s.run(runCtx, client, t)
	cancel()
	now := millis(time.Now())
	kit.Warn(s.a, "acron.save_failed", s.updateTask(id, func(t *task) {
		if chatID != "" && !t.ResolvedPeer {
			t.ChatID, t.ResolvedPeer = chatID, true
		}
		t.LastRunAt = now
		if err != nil {
			t.LastResult, t.LastError, t.Delivery = "", failureReason(err), deliveryPending
			return
		}
		t.LastResult, t.LastError, t.Delivery = result, "", deliverySent
	}))
	if err != nil {
		s.log.Warn("acron.failed", "task", id, "type", t.Type, "error", err.Error())
		return
	}
	s.log.Info("acron.done", "task", id, "type", t.Type, "result", result)
}

// failureReason 是记进任务、在列表里显示的失败原因：不带 URL 和原始英文错误。
func failureReason(err error) string {
	if errors.Is(err, context.Canceled) {
		return "服务停了，没执行完"
	}
	return command.Brief(err)
}

// target 找到任务要发往的对话：先用解析好的 ChatID（查缓存，不联网），不行再按用户写的
// 解析一次。返回的第二项是对话的带标记 ID，第一次解析出来时要记进任务。
func target(ctx context.Context, client *bot.Client, t task) (tg.InputPeerClass, string, error) {
	if t.ChatID != "" {
		if peer, err := client.InputPeerFromChatID(t.ChatID); err == nil {
			return peer, t.ChatID, nil
		}
	}
	if strings.TrimSpace(t.Chat) == "" {
		return nil, "", kit.Fail("任务里没有对话")
	}
	peer, err := client.ResolveTarget(ctx, t.Chat)
	if err != nil {
		return nil, "", kit.FailWith("找不到对话 "+t.Chat, err)
	}
	return peer, bot.PeerID(peerOf(peer, client.SelfID())), nil
}

// run 做任务的那件事，返回记进列表的结果。
func (s *service) run(ctx context.Context, client *bot.Client, t task) (string, string, error) {
	peer, chatID, err := target(ctx, client, t)
	if err != nil {
		return "", "", err
	}
	// 用户存下的文字、命令和要复制的内容原样发出去，不给里面的 IP 打码。
	verbatim := bot.WithoutIPPrivacy(ctx)
	replyTo := messageID(t.ReplyTo)
	switch t.Type {
	case "send":
		entities := inputEntities(client.Peers(), decodeEntities(t.Entities))
		// teleproto 发消息默认带链接预览，V2 存下的文字发出去也有预览。
		if _, err := client.SendPlain(verbatim, peer, t.Message, entities, bot.SendOptions{ReplyTo: replyTo, LinkPreview: true}); err != nil {
			return "", chatID, err
		}
		return "已发送 1 条消息", chatID, nil
	case "cmd":
		result, err := s.runCommand(ctx, client, peer, t.Message, replyTo)
		return result, chatID, err
	case "copy":
		if err := copyMessage(verbatim, client, t, peer, replyTo); err != nil {
			return "", chatID, err
		}
		return "已复制发送 1 条消息", chatID, nil
	case "forward":
		from, id, err := source(client, t)
		if err != nil {
			return "", chatID, err
		}
		if err := client.ForwardToTopic(ctx, from, peer, []int{id}, replyTo); err != nil {
			return "", chatID, err
		}
		return "已转发 1 条消息", chatID, nil
	case "del":
		id := messageID(t.MsgID)
		if id == 0 {
			return "", chatID, kit.Fail("消息 ID 无效")
		}
		if err := client.Delete(ctx, peer, []int{id}); err != nil {
			return "", chatID, err
		}
		// 删不存在的消息 Telegram 也不报错，所以说「尝试」，同 V2。
		return "已尝试删除消息 " + t.MsgID, chatID, nil
	case "del_re":
		deleted, err := deleteMatching(ctx, client, peer, t)
		if err != nil {
			return "", chatID, err
		}
		return "匹配并删除 " + strconv.Itoa(deleted) + " 条", chatID, nil
	case "pin", "unpin":
		id := messageID(t.MsgID)
		if id == 0 {
			return "", chatID, kit.Fail("消息 ID 无效")
		}
		request := &tg.MessagesUpdatePinnedMessageRequest{Peer: peer, ID: id, Unpin: t.Type == "unpin"}
		if t.Type == "pin" {
			request.Silent, request.PmOneside = !t.Notify, t.PmOneSide
		}
		if _, err := client.API().MessagesUpdatePinnedMessage(ctx, request); err != nil {
			return "", chatID, err
		}
		if t.Type == "unpin" {
			return "已取消置顶消息 " + t.MsgID, chatID, nil
		}
		return "已置顶消息 " + t.MsgID, chatID, nil
	}
	return "", chatID, kit.Failf("不认识的任务类型 %s", t.Type)
}

// runCommand 把命令发到对话，再交给命令注册表执行。
//
// 账号自己通过 RPC 发的消息，Telegram 不会再作为更新推回来，光发出去命令是不会执行的
// （.checkin、--verify 也是这个情况）。所以照着发出去的样子造一条消息，按当前的前缀和
// 别名派发；V2 的 commands.dispatch 做的是同一件事。
func (s *service) runCommand(ctx context.Context, client *bot.Client, peer tg.InputPeerClass, text string, replyTo int) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", kit.Fail("任务里没有命令")
	}
	// 添加时已经挡掉了；这里挡的是从 MiBox 搬来的、或者后来起的别名让它变成了 .acron 的任务。
	if s.invokesSelf(text) {
		return "", kit.Fail("命令是 .acron 本身，执行一次就会再加一个任务，已跳过；请删掉这个任务")
	}
	id, err := client.SendPlain(bot.WithoutIPPrivacy(ctx), peer, text, nil, bot.SendOptions{ReplyTo: replyTo})
	if err != nil {
		return "", err
	}
	message := commandMessage(id, peerOf(peer, client.SelfID()), text, replyTo)
	// 命令在后台跑，比这次任务活得久，不能随任务的 ctx 一起取消；进程退出时注册表自己会掐断它。
	// 也不能沿用上面发送用的 ctx：命令自己的输出照常按设置给 IP 打码。
	if s.dispatch == nil || !s.dispatch(context.WithoutCancel(ctx), message) {
		return "已发送命令（未执行：不是可用的命令，检查前缀和命令名）", nil
	}
	return "已执行命令", nil
}

// commandMessage 造一条和刚发出的命令一样的消息，交给派发器。
func commandMessage(id int, peer tg.PeerClass, text string, replyTo int) *tg.Message {
	message := &tg.Message{ID: id, Out: true, PeerID: peer, Message: text, Date: int(time.Now().Unix())}
	if replyTo > 0 {
		header := &tg.MessageReplyHeader{}
		header.SetReplyToMsgID(replyTo)
		message.SetReplyTo(header)
	}
	return message
}

// source 是 copy、forward 的源对话和源消息。
func source(client *bot.Client, t task) (tg.InputPeerClass, int, error) {
	id := messageID(t.FromMsgID)
	if id == 0 {
		return nil, 0, kit.Fail("源消息 ID 无效")
	}
	from, err := client.InputPeerFromChatID(t.FromChatID)
	if err != nil {
		return nil, 0, kit.FailWith("找不到源消息所在的对话", err)
	}
	return from, id, nil
}

// copyMessage 把源消息的内容重新发一遍，不带「转发自」。做法同 V2（teleproto 发送 Message 对象）：
// 文字连同格式照发，媒体按引用重发，不下载。
func copyMessage(ctx context.Context, client *bot.Client, t task, to tg.InputPeerClass, replyTo int) error {
	from, id, err := source(client, t)
	if err != nil {
		return err
	}
	found, err := client.GetMessages(ctx, from, []int{id})
	if err != nil {
		return err
	}
	var original *tg.Message
	for _, message := range found {
		if message.ID == id {
			original = message
		}
	}
	if original == nil {
		return kit.Fail("源消息已经不在了")
	}
	media, err := copyMedia(original)
	if err != nil {
		return err
	}
	entities := inputEntities(client.Peers(), original.Entities)
	if media == nil {
		if strings.TrimSpace(original.Message) == "" {
			return kit.Fail("源消息里没有能复制的内容")
		}
		// 原消息带链接预览的，复制出来也带，同 teleproto。
		_, preview := original.Media.(*tg.MessageMediaWebPage)
		_, err := client.SendPlain(ctx, to, original.Message, entities, bot.SendOptions{ReplyTo: replyTo, LinkPreview: preview})
		return err
	}
	request := &tg.MessagesSendMediaRequest{Peer: to, Media: media, Message: original.Message, RandomID: rand.Int64()}
	if len(entities) > 0 {
		request.SetEntities(entities)
	}
	if replyTo > 0 {
		request.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: replyTo})
	}
	_, err = client.API().MessagesSendMedia(ctx, request)
	return err
}

// copyMedia 把消息里的媒体换成发送用的样子。图片和文件按引用重发（同 teleproto 的
// _fileToMedia），不带文件的类型照原值新建一份。纯文字、只有链接预览的返回 nil。
func copyMedia(message *tg.Message) (tg.InputMediaClass, error) {
	media, ok := message.GetMedia()
	if !ok {
		return nil, nil
	}
	switch value := media.(type) {
	case *tg.MessageMediaEmpty, *tg.MessageMediaWebPage:
		return nil, nil
	case *tg.MessageMediaPhoto:
		if photo, ok := value.Photo.(*tg.Photo); ok {
			return &tg.InputMediaPhoto{Spoiler: value.Spoiler,
				ID: &tg.InputPhoto{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference}}, nil
		}
	case *tg.MessageMediaDocument:
		if document, ok := value.Document.(*tg.Document); ok {
			return &tg.InputMediaDocument{Spoiler: value.Spoiler,
				ID: &tg.InputDocument{ID: document.ID, AccessHash: document.AccessHash, FileReference: document.FileReference}}, nil
		}
	case *tg.MessageMediaGeo:
		if point, ok := value.Geo.(*tg.GeoPoint); ok {
			return &tg.InputMediaGeoPoint{GeoPoint: &tg.InputGeoPoint{Lat: point.Lat, Long: point.Long}}, nil
		}
	case *tg.MessageMediaVenue:
		if point, ok := value.Geo.(*tg.GeoPoint); ok {
			return &tg.InputMediaVenue{GeoPoint: &tg.InputGeoPoint{Lat: point.Lat, Long: point.Long},
				Title: value.Title, Address: value.Address, Provider: value.Provider, VenueID: value.VenueID, VenueType: value.VenueType}, nil
		}
	case *tg.MessageMediaContact:
		return &tg.InputMediaContact{PhoneNumber: value.PhoneNumber, FirstName: value.FirstName, LastName: value.LastName, Vcard: value.Vcard}, nil
	case *tg.MessageMediaDice:
		return &tg.InputMediaDice{Emoticon: value.Emoticon}, nil
	case *tg.MessageMediaPoll:
		// 看不到测验的正确答案，复制出来的是问题和选项都相同的普通投票，同 .save。
		poll := tg.Poll{ID: rand.Int64(), Question: value.Poll.Question, Answers: value.Poll.Answers, MultipleChoice: value.Poll.MultipleChoice}
		return &tg.InputMediaPoll{Poll: poll}, nil
	}
	return nil, kit.Fail("这种消息没法复制，可以改用 forward 定时转发")
}

// deleteMatching 在对话最近的 limit 条消息里，删掉文字匹配正则的。
func deleteMatching(ctx context.Context, client *bot.Client, peer tg.InputPeerClass, t task) (int, error) {
	// 存下的数据也要再检查一遍：条数不对的不截断、正则为空的不当成全匹配，
	// 不然会报告一次其实没照原意做的清理，同 V2。
	limit, err := strconv.Atoi(strings.TrimSpace(t.Limit))
	if err != nil || limit < 1 || limit > maxScan {
		return 0, kit.Failf("条数要在 1–%d 之间", maxScan)
	}
	if strings.TrimSpace(t.Regex) == "" {
		return 0, kit.Fail("任务里没有正则表达式")
	}
	pattern, err := compileRegex(t.Regex)
	if err != nil {
		return 0, err
	}
	messages, err := recentMessages(ctx, client, peer, limit)
	if err != nil {
		return 0, err
	}
	var ids []int
	for _, message := range messages {
		// 只有媒体的消息文字是空的，也照样拿去匹配，同 V2。
		if pattern.MatchString(message.Message) {
			ids = append(ids, message.ID)
		}
	}
	// 一次最多删 100 条，Telegram 的上限。
	for start := 0; start < len(ids); start += 100 {
		if err := client.Delete(ctx, peer, ids[start:min(start+100, len(ids))]); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// recentMessages 从最新的往回读 limit 条（服务消息也算数，同 teleproto 的 getMessages），
// 返回其中的普通消息。
func recentMessages(ctx context.Context, client *bot.Client, peer tg.InputPeerClass, limit int) ([]*tg.Message, error) {
	var found []*tg.Message
	offset, seen := 0, 0
	for seen < limit {
		batch := min(100, limit-seen)
		result, err := client.API().MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, OffsetID: offset, Limit: batch})
		if err != nil {
			return nil, err
		}
		page, _ := client.Unpack(result)
		for _, item := range page {
			if seen >= limit {
				break
			}
			seen++
			if id := kit.MessageID(item); offset == 0 || id < offset {
				offset = id
			}
			if message, ok := item.(*tg.Message); ok {
				found = append(found, message)
			}
		}
		if len(page) < batch {
			break
		}
	}
	return found, nil
}
