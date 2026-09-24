// Package aban 是群管命令：.ban .unban .kick .mute .unmute 作用于当前群，
// .sb .unsb 作用于账号管理的所有群，.refresh 刷新管理群缓存，.aban 显示帮助。
package aban

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

// managedGroup 是本账号能在其中封禁用户的群组。
type managedGroup struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Channel bool   `json:"channel"`
	Hash    int64  `json:"accessHash,omitempty"`
}

type abanCache struct {
	Groups    []managedGroup `json:"groups"`
	UpdatedAt int64          `json:"updatedAt"`
}

type abanService struct {
	store *store.Store[abanCache]
	mu    sync.Mutex
	// later 安排延时执行，为 nil 时用 time.AfterFunc；测试里换成立即记录。
	later func(delay time.Duration, run func())
}

// abanCacheTTL 是管理群列表的缓存复用时长：一天扫一次对话列表，其余时候用缓存；
// `.refresh` 会提前丢弃缓存。MiBox 的缓存不过期，新加入的群要手动 .refresh 才认。
const abanCacheTTL = 24 * time.Hour

// abanParallel 是跨群操作同时进行的请求数，和 MiBox 一样是 4。
const abanParallel = 4

const (
	// resultLifetime 是原版 smartEdit 的 MESSAGE_AUTO_DELETE：帮助、错误、提示和结果
	// 显示这么久之后连同命令消息一起删掉。过程中的状态不删，由后面的结果覆盖。
	resultLifetime = 10 * time.Second
	// batchLifetime 是 .sb / .unsb 最终结果的显示时长。
	batchLifetime = 30 * time.Second
)

// historyRounds 是清理消息时最多调用 channels.deleteParticipantHistory 的次数。
// 每次只删一段，应答里 offset 大于 0 表示还没删完，要接着调（v2 一直调到 offset 为 0）；
// 上限防止服务端一直不归零时无休止地调下去。
const historyRounds = 100

// fresh 表示缓存还能用，这次不用扫描对话列表。
func (s *abanService) fresh() bool {
	cached, err := s.store.Read()
	return err == nil && len(cached.Groups) > 0 && time.Since(time.UnixMilli(cached.UpdatedAt)) < abanCacheTTL
}

// eachGroup 对每个群执行 work，最多 abanParallel 个同时进行，全部结束才返回。
func eachGroup(ctx context.Context, groups []managedGroup, work func(managedGroup)) {
	eachGroupLimit(ctx, groups, abanParallel, work)
}

// eachGroupLimit 同 eachGroup，同时进行的个数由 limit 指定。ctx 结束后不再开始新的群。
func eachGroupLimit(ctx context.Context, groups []managedGroup, limit int, work func(managedGroup)) {
	slots := make(chan struct{}, limit)
	var wait sync.WaitGroup
	for _, group := range groups {
		if ctx.Err() != nil {
			break
		}
		slots <- struct{}{}
		wait.Add(1)
		go func() {
			defer func() { <-slots; wait.Done() }()
			work(group)
		}()
	}
	wait.Wait()
}

// banRights 是完全封禁：目标既不能看也不能发。
func banRights(until int) tg.ChatBannedRights {
	return tg.ChatBannedRights{UntilDate: until, ViewMessages: true, SendMessages: true, SendMedia: true,
		SendStickers: true, SendGifs: true, SendGames: true, SendInline: true, EmbedLinks: true}
}

// muteRights 让目标无法发言，但不把人移出。
func muteRights(until int) tg.ChatBannedRights {
	return tg.ChatBannedRights{UntilDate: until, SendMessages: true, SendMedia: true, SendStickers: true,
		SendGifs: true, SendGames: true, SendInline: true, EmbedLinks: true}
}

// clearRights 解除所有限制。
func clearRights() tg.ChatBannedRights { return tg.ChatBannedRights{} }

// parseDuration 照原版 parseTimeString 解析禁言时长：取开头的整数（同 JS 的 parseInt），
// 再看字符串里有没有 d、h、m、s，按这个顺序认作天、小时、分钟、秒。所以 5min 是 5 分钟，
// 1day 是 1 天，1h30m 是 1 小时。没有单位、数不出整数或不是正数都算永久（返回 0）。
func parseDuration(value string) time.Duration {
	value = strings.ToLower(value)
	var unit time.Duration
	switch {
	case strings.Contains(value, "d"):
		unit = 24 * time.Hour
	case strings.Contains(value, "h"):
		unit = time.Hour
	case strings.Contains(value, "m"):
		unit = time.Minute
	case strings.Contains(value, "s"):
		unit = time.Second
	default:
		return 0
	}
	number := leadingInt(value)
	if number <= 0 || number > int64(math.MaxInt64/unit) {
		return 0
	}
	return time.Duration(number) * unit
}

// leadingInt 是 JS parseInt(value, 10) 的整数部分：跳过开头的空白，可带正负号，
// 读到第一个非数字为止；一个数字也没有时返回 0。
func leadingInt(value string) int64 {
	value = strings.TrimLeft(value, " \t\n\r\v\f")
	sign := int64(1)
	switch {
	case strings.HasPrefix(value, "-"):
		sign, value = -1, value[1:]
	case strings.HasPrefix(value, "+"):
		value = value[1:]
	}
	end := 0
	for end < len(value) && value[end] >= '0' && value[end] <= '9' {
		end++
	}
	number, err := strconv.ParseInt(value[:end], 10, 64)
	if err != nil {
		return 0
	}
	return sign * number
}

func formatDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "永久"
	case d >= 24*time.Hour:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	case d >= time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d >= time.Minute:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
	return strconv.Itoa(int(d/time.Second)) + "s"
}

func untilDate(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(time.Now().Add(d).Unix())
}

// isMetaFlag 识别那些用来确认、而不是指定目标的参数。
func isMetaFlag(arg string) bool {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "true", "false", "confirm":
		return true
	}
	return false
}

// confirmed 判断命令里有没有追加 true 确认。
func confirmed(args []string) bool {
	for _, arg := range args {
		if strings.EqualFold(arg, "true") {
			return true
		}
	}
	return false
}

// targetIsAdmin 判断目标在某个群组里是否有管理员权限：频道和超级群查 channels.getParticipant，
// 基本群查 messages.getFullChat 里的成员身份。频道身份不会是管理员，不用查。
// 被限流到查不了时按「是」算：宁可多要一次确认，也不能悄悄跳过确认把管理员封掉。
func targetIsAdmin(ctx context.Context, client *bot.Client, chat tg.InputPeerClass, participant tg.InputPeerClass) bool {
	var userID int64
	switch value := participant.(type) {
	case *tg.InputPeerChannel:
		return false
	case *tg.InputPeerUser:
		userID = value.UserID
	case *tg.InputPeerSelf:
		userID = client.SelfID()
	}
	if channel, ok := bot.InputChannel(chat); ok {
		result, err := client.API().ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{Channel: channel, Participant: participant})
		if err != nil {
			_, flooded := bot.FloodWait(err)
			return flooded
		}
		client.Peers().RememberUsers(result.Users)
		switch result.Participant.(type) {
		case *tg.ChannelParticipantAdmin, *tg.ChannelParticipantCreator:
			return true
		}
		return false
	}
	group, ok := chat.(*tg.InputPeerChat)
	if !ok || userID == 0 {
		return false
	}
	full, err := client.API().MessagesGetFullChat(ctx, group.ChatID)
	if err != nil {
		_, flooded := bot.FloodWait(err)
		return flooded
	}
	client.Peers().RememberUsers(full.Users)
	info, ok := full.FullChat.(*tg.ChatFull)
	if !ok {
		return false
	}
	members, ok := info.Participants.(*tg.ChatParticipants)
	if !ok {
		return false
	}
	for _, member := range members.Participants {
		if member.GetUserID() != userID {
			continue
		}
		switch member.(type) {
		case *tg.ChatParticipantCreator, *tg.ChatParticipantAdmin:
			return true
		}
		return false
	}
	return false
}

// applyRights 在一个群组里封禁、禁言或解除限制。基本群没有封禁权限
// 这个概念，所以在那里封禁就改成把成员移出。
func applyRights(ctx context.Context, client *bot.Client, chat tg.InputPeerClass, target tg.InputPeerClass, rights tg.ChatBannedRights, remove bool) error {
	if channel, ok := bot.InputChannel(chat); ok {
		_, err := client.API().ChannelsEditBanned(ctx, &tg.ChannelsEditBannedRequest{Channel: channel, Participant: target, BannedRights: rights})
		return err
	}
	group, ok := chat.(*tg.InputPeerChat)
	if !ok {
		return kit.Fail("该会话不支持此操作")
	}
	if !remove {
		return kit.Fail("基本群不支持禁言与解封")
	}
	user, ok := bot.InputUser(target)
	if !ok {
		return kit.Fail("目标必须是一个用户")
	}
	_, err := client.API().MessagesDeleteChatUser(ctx, &tg.MessagesDeleteChatUserRequest{ChatID: group.ChatID, UserID: user})
	return err
}

// deleteHistory 删除目标（用户或频道身份）在某个频道里留下的全部消息，
// 返回是否删完。基本群没有这个接口。
func deleteHistory(ctx context.Context, client *bot.Client, chat tg.InputPeerClass, target tg.InputPeerClass) bool {
	channel, ok := bot.InputChannel(chat)
	if !ok {
		return false
	}
	for round := 0; round < historyRounds; round++ {
		result, err := client.API().ChannelsDeleteParticipantHistory(ctx, &tg.ChannelsDeleteParticipantHistoryRequest{Channel: channel, Participant: target})
		if err != nil {
			return false
		}
		if result.Offset <= 0 {
			return true
		}
	}
	return false
}

// managedGroups 列出本账号能在其中封禁用户的所有群组，缓存没过期时
// 直接用缓存。
func (s *abanService) managedGroups(ctx context.Context, client *bot.Client, refresh bool) ([]managedGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !refresh {
		cached, err := s.store.Read()
		if err == nil && len(cached.Groups) > 0 && time.Since(time.UnixMilli(cached.UpdatedAt)) < abanCacheTTL {
			return cached.Groups, nil
		}
	}
	var groups []managedGroup
	seen := map[int64]bool{}
	for _, folder := range []int{0, 1} {
		offsetDate, offsetID := 0, 0
		var offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
		for page := 0; page < 20; page++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			request := &tg.MessagesGetDialogsRequest{OffsetDate: offsetDate, OffsetID: offsetID, OffsetPeer: offsetPeer, Limit: 100}
			if folder != 0 {
				request.SetFolderID(folder)
			}
			result, err := client.API().MessagesGetDialogs(ctx, request)
			if err != nil {
				if wait, ok := tgerr.AsFloodWait(err); ok {
					if kit.Sleep(ctx, wait) != nil {
						return nil, ctx.Err()
					}
					continue
				}
				return nil, err
			}
			var dialogs []tg.DialogClass
			var messages []tg.MessageClass
			done := true
			switch value := result.(type) {
			case *tg.MessagesDialogs:
				client.Peers().RememberUsers(value.Users)
				client.Peers().RememberChats(value.Chats)
				dialogs, messages = value.Dialogs, value.Messages
			case *tg.MessagesDialogsSlice:
				client.Peers().RememberUsers(value.Users)
				client.Peers().RememberChats(value.Chats)
				dialogs, messages, done = value.Dialogs, value.Messages, len(value.Dialogs) < 100
			default:
				done = true
			}
			for _, entry := range dialogs {
				dialog, ok := entry.(*tg.Dialog)
				if !ok {
					continue
				}
				switch peer := dialog.Peer.(type) {
				case *tg.PeerChannel:
					info, known := client.Peers().Channel(peer.ChannelID)
					if !known || seen[peer.ChannelID] || info.Left {
						continue
					}
					if !info.Creator && !info.AdminRights.BanUsers && !info.AdminRights.DeleteMessages {
						continue
					}
					seen[peer.ChannelID] = true
					groups = append(groups, managedGroup{ID: peer.ChannelID, Title: info.Title, Channel: true, Hash: info.Hash})
				case *tg.PeerChat:
					info, known := client.Peers().Chat(peer.ChatID)
					if !known || seen[peer.ChatID] || info.Left || !info.Creator && !info.AdminRights.BanUsers {
						continue
					}
					seen[peer.ChatID] = true
					groups = append(groups, managedGroup{ID: peer.ChatID, Title: info.Title})
				}
			}
			if done || len(dialogs) == 0 {
				break
			}
			last, _ := dialogs[len(dialogs)-1].(*tg.Dialog)
			if last == nil {
				break
			}
			offsetID = last.TopMessage
			for _, item := range messages {
				if message, ok := item.(*tg.Message); ok && message.ID == last.TopMessage {
					offsetDate = message.Date
				}
			}
			resolved, ok := client.Peers().InputPeer(last.Peer)
			if !ok {
				break
			}
			offsetPeer = resolved
		}
	}
	_ = s.store.Update(func(cache *abanCache) error {
		cache.Groups, cache.UpdatedAt = groups, time.Now().UnixMilli()
		return nil
	})
	return groups, nil
}

// input 把管理群转回可以直接寻址的 peer。
func (g managedGroup) input() tg.InputPeerClass {
	if g.Channel {
		return &tg.InputPeerChannel{ChannelID: g.ID, AccessHash: g.Hash}
	}
	return &tg.InputPeerChat{ChatID: g.ID}
}

// show 把命令消息改成 html；lifetime 大于 0 时到点把它删掉（原版 smartEdit 的行为）。
// 删除在后台计时，不占着命令。
func (s *abanService) show(ctx context.Context, inv *command.Invocation, html string, lifetime time.Duration) error {
	if err := inv.Edit(ctx, html); err != nil {
		return err
	}
	if lifetime > 0 {
		s.deleteLater(ctx, inv, lifetime)
	}
	return nil
}

// deleteLater 在 lifetime 之后删掉命令消息。计时脱离命令的 context：命令早已结束，
// 删除仍要照做；命令框架拿不到进程退出的信号，进程退出时尚未到点的删除随之放弃。
func (s *abanService) deleteLater(ctx context.Context, inv *command.Invocation, lifetime time.Duration) {
	background := context.WithoutCancel(ctx)
	client, message, logger := inv.Client, inv.Message, inv.Log
	run := func() {
		deleteCtx, cancel := context.WithTimeout(background, 15*time.Second)
		defer cancel()
		if err := client.DeleteMessage(deleteCtx, message); err != nil && !tgerr.Is(err, "MESSAGE_ID_INVALID") {
			logger.Warn("aban.auto_delete", slog.String("chat", message.ChatID), slog.Int("message", message.ID), slog.String("error", err.Error()))
		}
	}
	if s.later != nil {
		s.later(lifetime, run)
		return
	}
	time.AfterFunc(lifetime, run)
}

// fail 显示失败原因，到点删除。ctx 已结束时把 ctx 的错误交给命令框架，由它提示超时。
func (s *abanService) fail(ctx context.Context, inv *command.Invocation, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if text, ok := kit.IsUserError(err); ok {
		return s.show(ctx, inv, "❌ "+command.Escape(text), resultLifetime)
	}
	return s.show(ctx, inv, "❌ 操作失败："+command.Code(kit.RPCCode(err)), resultLifetime)
}

// failTarget 显示找不到目标的原因；unresolved 是用户查不到时的提示。
func (s *abanService) failTarget(ctx context.Context, inv *command.Invocation, err error, unresolved string) error {
	if errors.Is(err, errUnresolvedUser) {
		err = kit.Fail(unresolved)
	}
	return s.fail(ctx, inv, err)
}

func abanHelp(prefix string) string {
	p := command.Escape(prefix)
	return "<b>封禁管理</b>\n<code>" + p + "kick</code> 踢出 · <code>" + p + "ban</code> 封禁并清理消息\n<code>" + p + "unban</code> 解封 · <code>" + p + "unmute</code> 解除禁言\n<code>" + p +
		"mute [目标] [时长]</code> 禁言，时长如 60s / 5m / 1h / 1d；省略为永久\n<code>" + p + "sb [目标]</code> 在所有有管理权的群/频道封禁，并清理当前群消息\n<code>" + p + "unsb [目标]</code> 批量解封\n<code>" + p +
		"refresh</code> 刷新管理群缓存\n目标：回复消息 / @用户名 / 用户ID，也可以是频道；管理员目标需追加 <code>true</code>。\n用户ID 不要求对方在当前群，会从会话缓存与管理群解析。\n基本群仅支持踢出；ban/sb 在基本群执行移出，不会阻止再次加入。"
}

// Register 注册封禁管理相关的命令。
func Register(a *app.App) {
	service := &abanService{store: kit.NewStore(a, "aban.json", func() abanCache { return abanCache{} })}
	basic := func(action string) *command.Command {
		names := map[string]string{"kick": "踢出", "ban": "封禁", "unban": "解封", "mute": "禁言", "unmute": "解除禁言"}
		return &command.Command{Name: action, Description: names[action], Usage: "[目标]", Help: abanHelp, Timeout: 2 * time.Minute,
			Handle: func(ctx context.Context, inv *command.Invocation) error {
				return service.basic(ctx, inv, action, names[action])
			}}
	}
	a.Registry.Register(
		&command.Command{Name: "aban", Description: "封禁管理帮助", Help: abanHelp, Handle: func(ctx context.Context, inv *command.Invocation) error {
			return service.show(ctx, inv, abanHelp(inv.Prefix), resultLifetime)
		}},
		basic("kick"), basic("ban"), basic("unban"), basic("mute"), basic("unmute"),
		&command.Command{Name: "sb", Description: "在所有管理群中封禁", Usage: "[目标]", Help: abanHelp, Timeout: 15 * time.Minute,
			Handle: func(ctx context.Context, inv *command.Invocation) error { return service.batch(ctx, inv, true) }},
		&command.Command{Name: "unsb", Description: "在所有管理群中解封", Usage: "[目标]", Help: abanHelp, Timeout: 15 * time.Minute,
			Handle: func(ctx context.Context, inv *command.Invocation) error { return service.batch(ctx, inv, false) }},
		&command.Command{Name: "refresh", Description: "刷新管理群缓存", Help: abanHelp, Timeout: 5 * time.Minute,
			Handle: func(ctx context.Context, inv *command.Invocation) error {
				if err := inv.EditText(ctx, "⏳ 正在刷新管理群缓存…"); err != nil {
					return err
				}
				groups, err := service.managedGroups(ctx, inv.Client, true)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					return service.show(ctx, inv, "❌ 刷新失败："+command.Code(kit.RPCCode(err)), resultLifetime)
				}
				return service.show(ctx, inv, "✅ 已刷新 "+strconv.Itoa(len(groups))+" 个有管理权的群组", resultLifetime)
			}},
	)
}

// basic 执行作用于当前会话的 .kick .ban .unban .mute .unmute。和原版一样不限会话类型：
// 频道里照常执行，私聊里由 Telegram 或 applyRights 报错。
func (s *abanService) basic(ctx context.Context, inv *command.Invocation, action, label string) error {
	chat, err := inv.Client.InputPeer(inv.Message.Peer)
	if err != nil {
		return err
	}
	who, err := s.resolveTarget(ctx, inv, chat)
	if err != nil {
		return s.failTarget(ctx, inv, err, "无法解析该用户ID（会话未见过且不在管理群中）。可先回复其一则消息，或确认 ID 正确")
	}
	if !confirmed(inv.Args) && targetIsAdmin(ctx, inv.Client, chat, who.peer) {
		return s.show(ctx, inv, "⚠️ 目标是管理员，请在命令后加上 "+command.Code("true")+" 确认执行", resultLifetime)
	}
	if err := inv.Edit(ctx, "⏳ "+command.Escape(label)+" "+command.Escape(who.display)+"…"); err != nil {
		return err
	}

	_, isChannel := bot.InputChannel(chat)
	var result string
	switch action {
	case "kick":
		if err := applyRights(ctx, inv.Client, chat, who.peer, banRights(0), true); err != nil {
			return s.actionFailed(ctx, inv, label, err)
		}
		if isChannel {
			// 频道里的踢出是先封禁再解封，这样目标还能重新加入；
			// 基本群的移出本身就是踢出。
			if err := applyRights(ctx, inv.Client, chat, who.peer, clearRights(), false); err != nil {
				return s.actionFailed(ctx, inv, label, err)
			}
		}
		result = "✅ 已踢出 " + command.Escape(who.display)
	case "ban":
		cleaned := deleteHistory(ctx, inv.Client, chat, who.peer)
		if err := applyRights(ctx, inv.Client, chat, who.peer, banRights(0), true); err != nil {
			return s.actionFailed(ctx, inv, label, err)
		}
		suffix := ""
		if cleaned {
			suffix = " (已清理消息)"
		}
		if isChannel {
			result = "✅ 已封禁 " + command.Escape(who.display) + suffix
		} else {
			result = "✅ 已移出 " + command.Escape(who.display) + suffix
		}
	case "unban", "unmute":
		if err := applyRights(ctx, inv.Client, chat, who.peer, clearRights(), false); err != nil {
			return s.actionFailed(ctx, inv, label, err)
		}
		if action == "unban" {
			result = "✅ 已解封 " + command.Escape(who.display)
		} else {
			result = "✅ 已解除禁言 " + command.Escape(who.display)
		}
	case "mute":
		// 时长是目标后面的那个参数（原版取 args[1]）；回复消息时没有目标参数，就是永久。
		var duration time.Duration
		if args := targetArgs(inv.Args); len(args) > 1 {
			duration = parseDuration(args[1])
		}
		if err := applyRights(ctx, inv.Client, chat, who.peer, muteRights(untilDate(duration)), false); err != nil {
			return s.actionFailed(ctx, inv, label, err)
		}
		result = "✅ 已禁言 " + command.Escape(who.display) + " " + formatDuration(duration)
	}
	inv.Log.Info("aban.applied", slog.String("action", action), slog.Int64("target", who.id), slog.Bool("channel", who.channel), slog.String("chat", inv.Message.ChatID))
	return s.show(ctx, inv, result, resultLifetime)
}

// actionFailed 显示某个操作被 Telegram 拒绝的原因。
func (s *abanService) actionFailed(ctx context.Context, inv *command.Invocation, label string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if text, ok := kit.IsUserError(err); ok {
		return s.show(ctx, inv, "❌ "+command.Escape(text), resultLifetime)
	}
	return s.show(ctx, inv, "❌ "+command.Escape(label)+"失败："+command.Code(kit.RPCCode(err)), resultLifetime)
}

// topReasons 把失败原因按出现次数从多到少排，取前 n 个，写成「原因×次数」（原版只显示前 3 个）。
// 次数相同时按原因排序，结果不随并发顺序变化。
func topReasons(reasons map[string]int, n int) []string {
	keys := make([]string, 0, len(reasons))
	for reason := range reasons {
		keys = append(keys, reason)
	}
	sort.Slice(keys, func(i, j int) bool {
		if reasons[keys[i]] != reasons[keys[j]] {
			return reasons[keys[i]] > reasons[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	detail := make([]string, len(keys))
	for index, reason := range keys {
		detail[index] = command.Escape(reason) + "×" + strconv.Itoa(reasons[reason])
	}
	return detail
}

// batch 执行 .sb / .unsb：在所有管理群里封禁或解封同一个目标。
func (s *abanService) batch(ctx context.Context, inv *command.Invocation, ban bool) error {
	label := "解封"
	if ban {
		label = "封禁"
	}
	here, err := inv.Client.InputPeer(inv.Message.Peer)
	if err != nil {
		return err
	}
	who, err := s.resolveTarget(ctx, inv, here)
	if err != nil {
		return s.failTarget(ctx, inv, err, "无法解析该用户ID（会话未见过且不在任一管理群中）。可先 "+inv.Prefix+"refresh 后重试，或回复其一则消息")
	}
	if !s.fresh() {
		if err := inv.EditText(ctx, "⏳ 正在读取管理群列表（每天一次）…"); err != nil {
			return err
		}
	}
	groups, err := s.managedGroups(ctx, inv.Client, false)
	if err != nil {
		return s.fail(ctx, inv, err)
	}
	if len(groups) == 0 {
		return s.show(ctx, inv, "❌ 无管理群组", resultLifetime)
	}
	if !confirmed(inv.Args) && !who.channel {
		var adminIn atomic.Int32
		eachGroup(ctx, groups, func(group managedGroup) {
			if targetIsAdmin(ctx, inv.Client, group.input(), who.peer) {
				adminIn.Add(1)
			}
		})
		if err := ctx.Err(); err != nil {
			return err
		}
		if adminIn := int(adminIn.Load()); adminIn > 0 {
			return s.show(ctx, inv, "⚠️ 目标在 "+strconv.Itoa(adminIn)+" 个管理群中具有管理员身份，请在命令后加上 "+command.Code("true")+" 确认执行", resultLifetime)
		}
	}
	if err := inv.Edit(ctx, "⚡ 正在 "+strconv.Itoa(len(groups))+" 个频道/群组中"+label+"该用户…"); err != nil {
		return err
	}

	started := time.Now()
	cleaned := false
	if ban {
		cleaned = deleteHistory(ctx, inv.Client, here, who.peer)
	}
	rights := clearRights()
	if ban {
		rights = banRights(0)
	}
	success, failed, skipped := 0, 0, 0
	reasons := map[string]int{}
	var tally sync.Mutex
	eachGroup(ctx, groups, func(group managedGroup) {
		if !group.Channel && !ban {
			// 基本群没有什么可解封的：那里从来没有人被封禁过。
			tally.Lock()
			skipped++
			tally.Unlock()
			return
		}
		// 不在这里重试：连接层已经把 60 秒以内的 FLOOD_WAIT 等完重试过（最多 5 次），
		// 涵盖了原版「8 秒以内重试一次」；其余错误重试也没用，直接计入失败。
		err := applyRights(ctx, inv.Client, group.input(), who.peer, rights, ban)
		tally.Lock()
		defer tally.Unlock()
		if err == nil {
			success++
			return
		}
		failed++
		reasons[kit.RPCCode(err)]++
	})
	if err := ctx.Err(); err != nil {
		return err
	}

	text := "✅ 在" + strconv.Itoa(success) + "个频道/群组中" + label + "该用户 " + command.Escape(who.display)
	if failed > 0 {
		text += "\n⚠️ 失败 " + strconv.Itoa(failed) + " 个（" + strings.Join(topReasons(reasons, 3), "、") + "）"
	}
	if skipped > 0 {
		text += "\nℹ️ 跳过 " + strconv.Itoa(skipped) + " 个基本群（不支持跨群解封）"
	}
	if ban {
		mark := "✗"
		if cleaned {
			mark = "✓已清理"
		}
		text += "\n🗑️ 当前群组消息: " + mark
	}
	text += " | ⏱️" + strconv.FormatFloat(time.Since(started).Seconds(), 'f', 1, 64) + "s"
	inv.Log.Info("aban.batch", slog.Bool("ban", ban), slog.Int64("target", who.id), slog.Bool("channel", who.channel),
		slog.Int("success", success), slog.Int("failed", failed))
	return s.show(ctx, inv, text, batchLifetime)
}
