package commands

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

type daTask struct {
	ChatID          string   `json:"chatId"`
	ChatName        string   `json:"chatName"`
	StartTime       int64    `json:"startTime"`
	DeletedMessages int      `json:"deletedMessages"`
	IsRunning       bool     `json:"isRunning"`
	IsPaused        bool     `json:"isPaused"`
	SleepUntil      *int64   `json:"sleepUntil"`
	LastUpdate      int64    `json:"lastUpdate"`
	LastLogTime     int64    `json:"lastLogTime"`
	Errors          []string `json:"errors"`
	SavedMessageID  int      `json:"savedMessageId,omitempty"`
}

type daDB struct {
	Tasks    []daTask `json:"tasks"`
	Imported bool     `json:"imported"`
}

func daHelp(prefix string) string {
	p := command.Escape(prefix)
	return "<b>批量删除</b>\n\n<code>" + p + "da true</code> 开始或恢复删除\n<code>" + p + "da stop</code> 停止任务\n<code>" + p + "da status</code> 状态发送到收藏夹\n管理员删除全部消息，普通成员仅删除自己的消息。"
}

type daService struct {
	a      *app.App
	store  *store.Store[daDB]
	mu     sync.Mutex
	active map[string]*daSlot
}

type daSlot struct {
	cancel context.CancelFunc
	task   *daTask
	mu     sync.Mutex
}

func (s *daService) save(task *daTask) error {
	task.LastUpdate = time.Now().UnixMilli()
	copied := *task
	return s.store.Update(func(db *daDB) error {
		for index := range db.Tasks {
			if db.Tasks[index].ChatID == copied.ChatID {
				db.Tasks[index] = copied
				return nil
			}
		}
		db.Tasks = append(db.Tasks, copied)
		return nil
	})
}

func (s *daService) progress(ctx context.Context, client *bot.Client, task *daTask, status string) {
	seconds := max(1, (time.Now().UnixMilli()-task.StartTime)/1000)
	state := "已停止"
	switch {
	case task.SleepUntil != nil && *task.SleepUntil > time.Now().UnixMilli():
		state = fmt.Sprintf("休眠中 (%d秒)", int(math.Ceil(float64(*task.SleepUntil-time.Now().UnixMilli())/1000)))
	case task.IsRunning:
		state = "运行中"
	case task.IsPaused:
		state = "已暂停"
	}
	tail := task.Errors
	if len(tail) > 3 {
		tail = tail[len(tail)-3:]
	}
	text := fmt.Sprintf("<b>删除任务：%s</b>\n群聊：%s\n状态：%s\n已删除：%d 条\n删除速度：%.2f 条/秒\n运行时长：%d小时 %d分钟 %d秒\n最后更新：%s\n%s",
		command.Escape(status), command.Escape(task.ChatName), state, task.DeletedMessages, float64(task.DeletedMessages)/float64(seconds),
		seconds/3600, seconds%3600/60, seconds%60, time.UnixMilli(task.LastUpdate).Format("2006/1/2 15:04:05"), command.Escape(strings.Join(tail, "\n")))
	if task.SavedMessageID > 0 {
		err := client.EditMessage(ctx, &tg.InputPeerSelf{}, task.SavedMessageID, text, false)
		if err == nil {
			_ = s.save(task)
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
	id, err := client.SendSelf(ctx, text)
	if err != nil {
		return
	}
	task.SavedMessageID = id
	_ = s.save(task)
}

// pauseInterrupted 在启动时处理上次没跑完的任务：一律改成暂停，
// 要等有人明确发 .da true 才继续，不会因为进程重启就自己又删起来。
func (s *daService) pauseInterrupted() {
	_ = s.store.Update(func(db *daDB) error {
		for index := range db.Tasks {
			task := &db.Tasks[index]
			if task.IsRunning {
				task.IsPaused = true
			}
			task.IsRunning, task.SleepUntil = false, nil
			if len(task.Errors) > 0 {
				task.Errors = []string{"历史任务存在删除失败"}
			} else {
				task.Errors = []string{}
			}
		}
		return nil
	})
}

// current 找到这个群的任务：正在跑的取运行中的副本，否则从存档里读。
func (s *daService) current(id string, slot *daSlot) *daTask {
	if slot != nil {
		slot.mu.Lock()
		running := slot.task
		var copied daTask
		if running != nil {
			copied = *running
		}
		slot.mu.Unlock()
		if running != nil {
			return &copied
		}
	}
	db, _ := s.store.Read()
	var task *daTask
	for index := range db.Tasks {
		if db.Tasks[index].ChatID == id {
			task = &db.Tasks[index]
		}
	}
	return task
}

// report 处理 .da stop 和 .da status：停掉正在跑的任务，并把状态发到收藏夹。
func (s *daService) report(ctx context.Context, inv *command.Invocation, sub string) {
	id := inv.Message.ChatID
	s.mu.Lock()
	slot := s.active[id]
	s.mu.Unlock()
	if sub == "stop" && slot != nil {
		slot.cancel()
	}
	task := s.current(id, slot)
	if task == nil {
		return
	}
	if sub == "stop" && slot == nil {
		task.IsRunning, task.IsPaused = false, true
		_ = s.save(task)
	}
	status := "状态查询"
	if sub == "stop" {
		status = "已手动停止"
		if slot != nil {
			status = "正在停止，等待当前请求结束"
		}
	}
	s.progress(ctx, inv.Client, task, status)
}

// start 启动删除任务，返回是否真的启动了；同一个群已经有任务在跑就不启动。
//
// 删除会比命令本身活得久，所以跑在一个脱离了命令超时的 context 上，
// 只有 .da stop 或者进程退出才会取消它。
func (s *daService) start(ctx context.Context, inv *command.Invocation) bool {
	id, message, client := inv.Message.ChatID, inv.Message, inv.Client
	s.mu.Lock()
	if _, running := s.active[id]; running {
		s.mu.Unlock()
		return false
	}
	taskCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	slot := &daSlot{cancel: cancel}
	s.active[id] = slot
	s.mu.Unlock()
	go func() {
		defer cancel()
		defer func() {
			s.mu.Lock()
			delete(s.active, id)
			s.mu.Unlock()
		}()
		s.run(taskCtx, client, slot, id, message)
	}()
	return true
}

// Da 注册 .da。
func Da(a *app.App) {
	service := &daService{a: a, store: newStore(a, "da.json", func() daDB { return daDB{Tasks: []daTask{}} }), active: map[string]*daSlot{}}
	service.pauseInterrupted()
	a.Registry.Register(&command.Command{Name: "da", Description: "批量删除群组消息", Usage: "true|stop|status", Help: daHelp, Timeout: 2 * time.Minute,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			if !strings.HasPrefix(inv.Message.ChatID, "-") {
				return inv.EditText(ctx, "仅群组可用")
			}
			switch sub := strings.ToLower(inv.Arg(0)); sub {
			case "", "help", "h":
				return inv.Edit(ctx, daHelp(inv.Prefix))
			case "stop", "status":
				service.report(ctx, inv, sub)
			case "true":
				// 启动了的话，命令消息由任务在查完权限之后自己删。
				if service.start(ctx, inv) {
					return nil
				}
			default:
				return inv.EditText(ctx, "未知命令")
			}
			_ = inv.Client.DeleteMessage(ctx, inv.Message)
			return nil
		}})
}

// daRun 是一次删除任务运行时要用到的东西。
type daRun struct {
	ctx    context.Context
	client *bot.Client
	api    *tg.Client
	peer   tg.InputPeerClass
	task   *daTask
	save   func(*daTask) error
	logger *slog.Logger
}

// loadTask 取出这个群上次的任务接着做，没有就新建一个，并标记为运行中。
func (s *daService) loadTask(id string) (*daTask, error) {
	db, err := s.store.Read()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	task := &daTask{ChatID: id, ChatName: id, StartTime: now, LastUpdate: now, LastLogTime: now, Errors: []string{}}
	for index := range db.Tasks {
		if db.Tasks[index].ChatID == id {
			copied := db.Tasks[index]
			task = &copied
		}
	}
	task.IsRunning, task.IsPaused, task.SleepUntil = true, false, nil
	return task, nil
}

// finish 在任务结束时收尾：存盘、报告最终状态；顺利跑完且没有失败的任务从存档里删掉。
func (s *daService) finish(ctx context.Context, client *bot.Client, task *daTask, id string, completed bool) {
	task.IsRunning = false
	task.IsPaused = ctx.Err() != nil
	task.SleepUntil = nil
	_ = s.save(task)
	status := "执行失败"
	switch {
	case ctx.Err() != nil:
		status = "已停止"
	case completed && len(task.Errors) > 0:
		status = "完成，存在删除失败"
	case completed:
		status = "任务完成"
	}
	s.progress(context.WithoutCancel(ctx), client, task, status)
	if completed && len(task.Errors) == 0 {
		_ = s.store.Update(func(db *daDB) error {
			var kept []daTask
			for _, item := range db.Tasks {
				if item.ChatID != id {
					kept = append(kept, item)
				}
			}
			db.Tasks = kept
			return nil
		})
	}
}

// run 删这个群的消息，直到删完、出错或者被停下。
//
// 管理员删全部消息；其他人只能删自己的，走搜索那一支。
func (s *daService) run(ctx context.Context, client *bot.Client, slot *daSlot, id string, message *bot.Message) {
	logger := client.Logger().With(slog.String("command", "da"), slog.String("chat", id))
	task, err := s.loadTask(id)
	if err != nil {
		logger.Error("da.store_failed", slog.String("error", err.Error()))
		return
	}
	slot.mu.Lock()
	slot.task = task
	slot.mu.Unlock()
	_ = s.save(task)

	completed := false
	defer func() { s.finish(ctx, client, task, id, completed) }()

	peer, err := client.InputPeer(message.Peer)
	if err != nil {
		task.Errors = append(task.Errors, "无法定位该群组")
		return
	}
	task.ChatName = client.Peers().Title(message.Peer)
	run := &daRun{ctx: ctx, client: client, api: client.API(), peer: peer, task: task, save: s.save, logger: logger}
	admin := run.isAdmin()
	_ = client.Delete(ctx, peer, []int{message.ID})
	s.progress(ctx, client, task, "任务已启动")
	if admin {
		completed = run.sweepAll()
	} else {
		completed = run.sweepOwn()
	}
}

// isAdmin 判断自己在这个群里能不能删别人的消息。普通群（非超级群）一律按不能处理。
func (r *daRun) isAdmin() bool {
	channel, ok := bot.InputChannel(r.peer)
	if !ok {
		return false
	}
	result, err := r.api.ChannelsGetParticipant(r.ctx, &tg.ChannelsGetParticipantRequest{Channel: channel, Participant: &tg.InputPeerSelf{}})
	if err != nil {
		r.logger.Warn("da.permission", slog.String("error", err.Error()))
		return false
	}
	r.client.Peers().RememberUsers(result.Users)
	switch result.Participant.(type) {
	case *tg.ChannelParticipantAdmin, *tg.ChannelParticipantCreator:
		return true
	}
	return false
}

// deleteIDs 删一批。遇到限流就等；别的错误就改成一条一条删，
// 免得一个删不掉的编号拖累整批。
func (r *daRun) deleteIDs(ids []int) {
	for {
		if r.ctx.Err() != nil {
			return
		}
		err := r.client.Delete(r.ctx, r.peer, ids)
		if err == nil {
			r.task.DeletedMessages += len(ids)
			_ = r.save(r.task)
			return
		}
		if wait, ok := tgerr.AsFloodWait(err); ok {
			until := time.Now().Add(wait).UnixMilli()
			r.task.SleepUntil = &until
			_ = r.save(r.task)
			if sleepCtx(r.ctx, wait) != nil {
				return
			}
			r.task.SleepUntil = nil
			continue
		}
		r.logger.Error("da.delete", slog.Int("count", len(ids)), slog.String("error", err.Error()))
		r.task.Errors = appendBounded(r.task.Errors, "部分消息删除失败", 20)
		if len(ids) > 1 {
			for _, single := range ids {
				if r.ctx.Err() != nil {
					return
				}
				if err := r.client.Delete(r.ctx, r.peer, []int{single}); err == nil {
					r.task.DeletedMessages++
				}
				_ = sleepCtx(r.ctx, 50*time.Millisecond)
			}
		}
		_ = r.save(r.task)
		return
	}
}

// collectPage 从一页消息里取出能删的编号，以及下一页的游标（这一页最小的编号）。
// 服务消息（入群、改名这类）不删，但要算进游标，否则会卡在同一页。
func collectPage(messages []tg.MessageClass) (next int, ids []int) {
	for _, item := range messages {
		candidate := messageID(item)
		if candidate <= 0 {
			continue
		}
		if next == 0 || candidate < next {
			next = candidate
		}
		if _, ok := item.(*tg.Message); ok {
			ids = append(ids, candidate)
		}
	}
	return next, ids
}

// retryAfter 处理取页出错：限流就等完再试（返回 true）；别的错误记下来，任务到此为止。
func (r *daRun) retryAfter(err error, event, failure string) bool {
	if wait, ok := tgerr.AsFloodWait(err); ok {
		return sleepCtx(r.ctx, wait) == nil
	}
	r.logger.Error(event, slog.String("error", err.Error()))
	r.task.Errors = appendBounded(r.task.Errors, failure, 20)
	return false
}

// sweepAll 是管理员的做法：从新到旧翻完整个历史，攒满 100 条删一批。
// 返回是否完整跑完。
func (r *daRun) sweepAll() bool {
	offsetID := 0
	var batch []int
	for {
		if r.ctx.Err() != nil {
			return false
		}
		history, err := r.api.MessagesGetHistory(r.ctx, &tg.MessagesGetHistoryRequest{Peer: r.peer, OffsetID: offsetID, Limit: 100, MinID: 0})
		if err != nil {
			if r.retryAfter(err, "da.history", "读取历史消息失败") {
				continue
			}
			return false
		}
		messages, _ := r.client.Unpack(history)
		if len(messages) == 0 {
			break
		}
		next, ids := collectPage(messages)
		batch = append(batch, ids...)
		if next == 0 || (offsetID > 0 && next >= offsetID) {
			break
		}
		offsetID = next
		for len(batch) >= 100 {
			r.deleteIDs(batch[:100])
			batch = batch[100:]
		}
		if sleepCtx(r.ctx, 100*time.Millisecond) != nil {
			return false
		}
	}
	if len(batch) > 0 {
		r.deleteIDs(batch)
	}
	return true
}

// sweepOwn 是普通成员的做法：只搜自己发的，搜到一页删一页。返回是否完整跑完。
func (r *daRun) sweepOwn() bool {
	offsetID := 0
	for {
		if r.ctx.Err() != nil {
			return false
		}
		request := &tg.MessagesSearchRequest{Peer: r.peer, Q: "", Filter: &tg.InputMessagesFilterEmpty{}, OffsetID: offsetID, Limit: 100}
		request.SetFromID(&tg.InputPeerSelf{})
		result, err := r.api.MessagesSearch(r.ctx, request)
		if err != nil {
			if r.retryAfter(err, "da.search", "搜索自己的消息失败") {
				continue
			}
			return false
		}
		messages, _ := r.client.Unpack(result)
		if len(messages) == 0 {
			break
		}
		next, ids := collectPage(messages)
		if next == 0 || (offsetID > 0 && next >= offsetID) {
			break
		}
		offsetID = next
		if len(ids) > 0 {
			r.deleteIDs(ids)
		}
		if sleepCtx(r.ctx, 200*time.Millisecond) != nil {
			return false
		}
	}
	return true
}

// appendBounded appends and keeps at most limit entries.
func appendBounded(values []string, value string, limit int) []string {
	values = append(values, value)
	if len(values) > limit {
		values = values[len(values)-limit:]
	}
	return values
}
