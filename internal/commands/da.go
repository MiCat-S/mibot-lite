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

// Da registers .da.
func Da(a *app.App) {
	service := &daService{a: a, store: newStore(a, "da.json", func() daDB { return daDB{Tasks: []daTask{}} }), active: map[string]*daSlot{}}
	// Persisted tasks that were running when the process stopped resume
	// only on an explicit `da true`.
	_ = service.store.Update(func(db *daDB) error {
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
	a.Registry.Register(&command.Command{Name: "da", Description: "批量删除群组消息", Usage: "true|stop|status", Help: daHelp, Timeout: 2 * time.Minute,
		Handle: func(ctx context.Context, inv *command.Invocation) error {
			message := inv.Message
			if !strings.HasPrefix(message.ChatID, "-") {
				return inv.EditText(ctx, "仅群组可用")
			}
			sub := strings.ToLower(inv.Arg(0))
			if sub == "" || sub == "help" || sub == "h" {
				return inv.Edit(ctx, daHelp(inv.Prefix))
			}
			if sub != "true" && sub != "stop" && sub != "status" {
				return inv.EditText(ctx, "未知命令")
			}
			id := message.ChatID
			removeCommand := func() { _ = inv.Client.DeleteMessage(ctx, message) }
			if sub != "true" {
				service.mu.Lock()
				slot := service.active[id]
				service.mu.Unlock()
				if sub == "stop" && slot != nil {
					slot.cancel()
				}
				var task *daTask
				if slot != nil {
					slot.mu.Lock()
					if slot.task != nil {
						copied := *slot.task
						task = &copied
					}
					slot.mu.Unlock()
				}
				if task == nil {
					db, _ := service.store.Read()
					for index := range db.Tasks {
						if db.Tasks[index].ChatID == id {
							task = &db.Tasks[index]
						}
					}
				}
				if task != nil {
					if sub == "stop" && slot == nil {
						task.IsRunning, task.IsPaused = false, true
						_ = service.save(task)
					}
					status := "状态查询"
					if sub == "stop" {
						status = "已手动停止"
						if slot != nil {
							status = "正在停止，等待当前请求结束"
						}
					}
					service.progress(ctx, inv.Client, task, status)
				}
				removeCommand()
				return nil
			}

			service.mu.Lock()
			if _, running := service.active[id]; running {
				service.mu.Unlock()
				removeCommand()
				return nil
			}
			// The deletion outlives the command invocation, so it runs on a
			// context detached from the handler's timeout and cancelled only
			// by `da stop` or by shutdown.
			taskCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			slot := &daSlot{cancel: cancel}
			service.active[id] = slot
			service.mu.Unlock()
			client := inv.Client
			go func() {
				defer cancel()
				defer func() {
					service.mu.Lock()
					delete(service.active, id)
					service.mu.Unlock()
				}()
				service.run(taskCtx, client, slot, id, message)
			}()
			return nil
		}})
}

// run deletes the chat's messages until it finishes, fails or is stopped.
//
// An admin deletes everything in the chat; anyone else can only delete
// their own messages, which is what the search branch does.
func (s *daService) run(ctx context.Context, client *bot.Client, slot *daSlot, id string, message *bot.Message) {
	logger := client.Logger().With(slog.String("command", "da"), slog.String("chat", id))
	db, err := s.store.Read()
	if err != nil {
		logger.Error("da.store_failed", slog.String("error", err.Error()))
		return
	}
	task := &daTask{ChatID: id, ChatName: id, StartTime: time.Now().UnixMilli(), LastUpdate: time.Now().UnixMilli(),
		LastLogTime: time.Now().UnixMilli(), Errors: []string{}}
	for index := range db.Tasks {
		if db.Tasks[index].ChatID == id {
			copied := db.Tasks[index]
			task = &copied
		}
	}
	task.IsRunning, task.IsPaused, task.SleepUntil = true, false, nil
	slot.mu.Lock()
	slot.task = task
	slot.mu.Unlock()
	_ = s.save(task)

	completed := false
	defer func() {
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
	}()

	peer, err := client.InputPeer(message.Peer)
	if err != nil {
		task.Errors = append(task.Errors, "无法定位该群组")
		return
	}
	task.ChatName = client.Peers().Title(message.Peer)
	api := client.API()

	admin := false
	if channel, ok := bot.InputChannel(peer); ok {
		result, err := api.ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{Channel: channel, Participant: &tg.InputPeerSelf{}})
		if err != nil {
			logger.Warn("da.permission", slog.String("error", err.Error()))
		} else {
			client.Peers().RememberUsers(result.Users)
			switch result.Participant.(type) {
			case *tg.ChannelParticipantAdmin, *tg.ChannelParticipantCreator:
				admin = true
			}
		}
	}
	_ = client.Delete(ctx, peer, []int{message.ID})
	s.progress(ctx, client, task, "任务已启动")

	deleteIDs := func(ids []int) {
		for {
			if ctx.Err() != nil {
				return
			}
			err := client.Delete(ctx, peer, ids)
			if err == nil {
				task.DeletedMessages += len(ids)
				_ = s.save(task)
				return
			}
			if wait, ok := tgerr.AsFloodWait(err); ok {
				until := time.Now().Add(wait).UnixMilli()
				task.SleepUntil = &until
				_ = s.save(task)
				if sleepCtx(ctx, wait) != nil {
					return
				}
				task.SleepUntil = nil
				continue
			}
			logger.Error("da.delete", slog.Int("count", len(ids)), slog.String("error", err.Error()))
			task.Errors = appendBounded(task.Errors, "部分消息删除失败", 20)
			if len(ids) > 1 {
				// One bad id must not sink the whole batch.
				for _, single := range ids {
					if ctx.Err() != nil {
						return
					}
					if err := client.Delete(ctx, peer, []int{single}); err == nil {
						task.DeletedMessages++
					}
					_ = sleepCtx(ctx, 50*time.Millisecond)
				}
			}
			_ = s.save(task)
			return
		}
	}

	if admin {
		offsetID := 0
		var batch []int
		for {
			if ctx.Err() != nil {
				return
			}
			history, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, OffsetID: offsetID, Limit: 100, MinID: 0})
			if err != nil {
				if wait, ok := tgerr.AsFloodWait(err); ok {
					if sleepCtx(ctx, wait) != nil {
						return
					}
					continue
				}
				logger.Error("da.history", slog.String("error", err.Error()))
				task.Errors = appendBounded(task.Errors, "读取历史消息失败", 20)
				return
			}
			messages, _ := client.Unpack(history)
			if len(messages) == 0 {
				break
			}
			next := 0
			for _, item := range messages {
				candidate := messageID(item)
				if candidate <= 0 {
					continue
				}
				if next == 0 || candidate < next {
					next = candidate
				}
				if _, ok := item.(*tg.Message); ok {
					batch = append(batch, candidate)
				}
			}
			if next == 0 || (offsetID > 0 && next >= offsetID) {
				break
			}
			offsetID = next
			for len(batch) >= 100 {
				deleteIDs(batch[:100])
				batch = batch[100:]
			}
			if sleepCtx(ctx, 100*time.Millisecond) != nil {
				return
			}
		}
		if len(batch) > 0 {
			deleteIDs(batch)
		}
		completed = true
		return
	}

	// Not an admin: only this account's own messages, found by search.
	offsetID := 0
	for {
		if ctx.Err() != nil {
			return
		}
		request := &tg.MessagesSearchRequest{Peer: peer, Q: "", Filter: &tg.InputMessagesFilterEmpty{}, OffsetID: offsetID, Limit: 100}
		request.SetFromID(&tg.InputPeerSelf{})
		result, err := api.MessagesSearch(ctx, request)
		if err != nil {
			if wait, ok := tgerr.AsFloodWait(err); ok {
				if sleepCtx(ctx, wait) != nil {
					return
				}
				continue
			}
			logger.Error("da.search", slog.String("error", err.Error()))
			task.Errors = appendBounded(task.Errors, "搜索自己的消息失败", 20)
			return
		}
		messages, _ := client.Unpack(result)
		if len(messages) == 0 {
			break
		}
		next, ids := 0, []int(nil)
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
		if next == 0 || (offsetID > 0 && next >= offsetID) {
			break
		}
		offsetID = next
		if len(ids) > 0 {
			deleteIDs(ids)
		}
		if sleepCtx(ctx, 200*time.Millisecond) != nil {
			return
		}
	}
	completed = true
}

// appendBounded appends and keeps at most limit entries.
func appendBounded(values []string, value string, limit int) []string {
	values = append(values, value)
	if len(values) > limit {
		values = values[len(values)-limit:]
	}
	return values
}
