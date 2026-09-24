// Package aban 是群管命令：.ban .unban .kick .mute .unmute 作用于当前群，
// .sb .unsb 作用于账号管理的所有群，.refresh 刷新管理群缓存，.aban 显示帮助。
package aban

import (
	"context"
	"log/slog"
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
}

// abanCacheTTL 是管理群列表的缓存复用时长：一天扫一次对话列表，其余时候用缓存；
// `.refresh` 会提前丢弃缓存。MiBox 的缓存不过期，新加入的群要手动 .refresh 才认。
const abanCacheTTL = 24 * time.Hour

// abanParallel 是跨群操作同时进行的请求数，和 MiBox 一样是 4。
const abanParallel = 4

// fresh 表示缓存还能用，这次不用扫描对话列表。
func (s *abanService) fresh() bool {
	cached, err := s.store.Read()
	return err == nil && len(cached.Groups) > 0 && time.Since(time.UnixMilli(cached.UpdatedAt)) < abanCacheTTL
}

// eachGroup 对每个群执行 work，最多 abanParallel 个同时进行，全部结束才返回。
func eachGroup(ctx context.Context, groups []managedGroup, work func(managedGroup)) {
	slots := make(chan struct{}, abanParallel)
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

// parseDuration 解析 60s / 5m / 1h / 1d；其他写法一律视为永久。
func parseDuration(value string) time.Duration {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return 0
	}
	digits := strings.TrimRight(value, "smhd")
	number, err := strconv.Atoi(digits)
	if err != nil || number <= 0 {
		return 0
	}
	switch {
	case strings.HasSuffix(value, "d"):
		return time.Duration(number) * 24 * time.Hour
	case strings.HasSuffix(value, "h"):
		return time.Duration(number) * time.Hour
	case strings.HasSuffix(value, "m"):
		return time.Duration(number) * time.Minute
	case strings.HasSuffix(value, "s"):
		return time.Duration(number) * time.Second
	}
	return 0
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

// resolveTarget 找出封禁命令针对的是谁：优先用明确给出的参数，
// 否则取被回复消息的发送者。
func resolveTarget(ctx context.Context, inv *command.Invocation) (tg.InputPeerClass, int64, string, error) {
	var targets []string
	for _, arg := range inv.Args {
		if !isMetaFlag(arg) {
			targets = append(targets, arg)
		}
	}
	if len(targets) > 0 {
		peer, err := inv.Client.ResolveTarget(ctx, targets[0])
		if err != nil {
			return nil, 0, "", kit.Fail("无法解析该目标，可先回复其一则消息，或确认用户名和 ID 正确")
		}
		user, ok := peer.(*tg.InputPeerUser)
		if !ok {
			return nil, 0, "", kit.Fail("目标必须是一个用户")
		}
		info, _ := inv.Client.Peers().User(user.UserID)
		return peer, user.UserID, info.DisplayName(), nil
	}
	reply, err := inv.Client.GetReply(ctx, inv.Message)
	if err != nil {
		return nil, 0, "", err
	}
	if reply == nil {
		return nil, 0, "", kit.Fail("请回复一条消息，或指定 @用户名 / 用户ID")
	}
	sender, ok := reply.Sender.(*tg.PeerUser)
	if !ok {
		return nil, 0, "", kit.Fail("无法识别该消息的发送者")
	}
	peer, err := inv.Client.InputPeer(sender)
	if err != nil {
		return nil, 0, "", kit.Fail("无法解析该用户，可先确认其在本群可见")
	}
	info, _ := inv.Client.Peers().User(sender.UserID)
	return peer, sender.UserID, info.DisplayName(), nil
}

// targetIsAdmin 判断目标在某个群组里是否有管理员权限。被限流到查不了时按「是」算：
// 宁可多要一次确认，也不能悄悄跳过确认把管理员封掉。
func targetIsAdmin(ctx context.Context, client *bot.Client, chat tg.InputPeerClass, target tg.InputPeerClass) bool {
	channel, ok := bot.InputChannel(chat)
	if !ok {
		return false
	}
	result, err := client.API().ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{Channel: channel, Participant: target})
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

// deleteHistory 删除目标在某个频道里留下的全部消息。
func deleteHistory(ctx context.Context, client *bot.Client, chat tg.InputPeerClass, target tg.InputPeerClass) bool {
	channel, ok := bot.InputChannel(chat)
	if !ok {
		return false
	}
	_, err := client.API().ChannelsDeleteParticipantHistory(ctx, &tg.ChannelsDeleteParticipantHistoryRequest{Channel: channel, Participant: target})
	return err == nil
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

func abanHelp(prefix string) string {
	p := command.Escape(prefix)
	return "<b>封禁管理</b>\n<code>" + p + "kick</code> 踢出 · <code>" + p + "ban</code> 封禁并清理消息\n<code>" + p + "unban</code> 解封 · <code>" + p + "unmute</code> 解除禁言\n<code>" + p +
		"mute [目标] [时长]</code> 禁言，时长如 60s / 5m / 1h / 1d；省略为永久\n<code>" + p + "sb [目标]</code> 在所有有管理权的群/频道封禁，并清理当前群消息\n<code>" + p + "unsb [目标]</code> 批量解封\n<code>" + p +
		"refresh</code> 刷新管理群缓存\n目标：回复消息 / @用户名 / 用户ID；管理员目标需追加 <code>true</code>。\n基本群仅支持踢出；ban/sb 在基本群执行移出，不会阻止再次加入。"
}

// Register 注册封禁管理相关的命令。
func Register(a *app.App) {
	service := &abanService{store: kit.NewStore(a, "aban.json", func() abanCache { return abanCache{} })}
	basic := func(action string) *command.Command {
		names := map[string]string{"kick": "踢出", "ban": "封禁", "unban": "解封", "mute": "禁言", "unmute": "解除禁言"}
		return &command.Command{Name: action, Description: names[action], Usage: "[目标]", Help: abanHelp, Timeout: 2 * time.Minute,
			Handle: func(ctx context.Context, inv *command.Invocation) error {
				return abanBasic(ctx, inv, service, action, names[action])
			}}
	}
	a.Registry.Register(
		&command.Command{Name: "aban", Description: "封禁管理帮助", Help: abanHelp, Handle: func(ctx context.Context, inv *command.Invocation) error {
			return inv.Edit(ctx, abanHelp(inv.Prefix))
		}},
		basic("kick"), basic("ban"), basic("unban"), basic("mute"), basic("unmute"),
		&command.Command{Name: "sb", Description: "在所有管理群中封禁", Usage: "[目标]", Help: abanHelp, Timeout: 15 * time.Minute,
			Handle: func(ctx context.Context, inv *command.Invocation) error { return abanBatch(ctx, inv, service, true) }},
		&command.Command{Name: "unsb", Description: "在所有管理群中解封", Usage: "[目标]", Help: abanHelp, Timeout: 15 * time.Minute,
			Handle: func(ctx context.Context, inv *command.Invocation) error { return abanBatch(ctx, inv, service, false) }},
		&command.Command{Name: "refresh", Description: "刷新管理群缓存", Help: abanHelp, Timeout: 5 * time.Minute,
			Handle: func(ctx context.Context, inv *command.Invocation) error {
				if err := inv.EditText(ctx, "⏳ 正在刷新管理群缓存…"); err != nil {
					return err
				}
				groups, err := service.managedGroups(ctx, inv.Client, true)
				if err != nil {
					return err
				}
				return inv.EditText(ctx, "✅ 已刷新 "+strconv.Itoa(len(groups))+" 个有管理权的群组")
			}},
	)
}

func abanBasic(ctx context.Context, inv *command.Invocation, service *abanService, action, label string) error {
	if !inv.Message.IsGroup() {
		return inv.EditText(ctx, "仅群组可用")
	}
	chat, err := inv.Client.InputPeer(inv.Message.Peer)
	if err != nil {
		return err
	}
	target, userID, display, err := resolveTarget(ctx, inv)
	if err != nil {
		if text, ok := kit.IsUserError(err); ok {
			return inv.EditText(ctx, "❌ "+text)
		}
		return err
	}
	confirmed := false
	for _, arg := range inv.Args {
		if strings.EqualFold(arg, "true") {
			confirmed = true
		}
	}
	if !confirmed && targetIsAdmin(ctx, inv.Client, chat, target) {
		return inv.Edit(ctx, "⚠️ 目标是管理员，请在命令后加上 "+command.Code("true")+" 确认执行")
	}
	if err := inv.Edit(ctx, "⏳ "+command.Escape(label)+" "+command.Escape(display)+"…"); err != nil {
		return err
	}

	_, isChannel := bot.InputChannel(chat)
	var result string
	switch action {
	case "kick":
		if err := applyRights(ctx, inv.Client, chat, target, banRights(0), true); err != nil {
			return abanFailure(ctx, inv, label, err)
		}
		if isChannel {
			// 频道里的踢出是先封禁再解封，这样目标还能重新加入；
			// 基本群的移出本身就是踢出。
			if err := applyRights(ctx, inv.Client, chat, target, clearRights(), false); err != nil {
				return abanFailure(ctx, inv, label, err)
			}
		}
		result = "✅ 已踢出 " + command.Escape(display)
	case "ban":
		cleaned := deleteHistory(ctx, inv.Client, chat, target)
		if err := applyRights(ctx, inv.Client, chat, target, banRights(0), true); err != nil {
			return abanFailure(ctx, inv, label, err)
		}
		suffix := ""
		if cleaned {
			suffix = " (已清理消息)"
		}
		if isChannel {
			result = "✅ 已封禁 " + command.Escape(display) + suffix
		} else {
			result = "✅ 已移出 " + command.Escape(display) + suffix
		}
	case "unban", "unmute":
		if err := applyRights(ctx, inv.Client, chat, target, clearRights(), false); err != nil {
			return abanFailure(ctx, inv, label, err)
		}
		result = "✅ 已" + strings.TrimPrefix(label, "解除") + "解除 " + command.Escape(display)
		if action == "unban" {
			result = "✅ 已解封 " + command.Escape(display)
		} else {
			result = "✅ 已解除禁言 " + command.Escape(display)
		}
	case "mute":
		var duration time.Duration
		for _, arg := range inv.Args {
			if isMetaFlag(arg) {
				continue
			}
			if parsed := parseDuration(arg); parsed > 0 {
				duration = parsed
			}
		}
		if err := applyRights(ctx, inv.Client, chat, target, muteRights(untilDate(duration)), false); err != nil {
			return abanFailure(ctx, inv, label, err)
		}
		result = "✅ 已禁言 " + command.Escape(display) + " " + formatDuration(duration)
	}
	inv.Log.Info("aban.applied", slog.String("action", action), slog.Int64("target", userID), slog.String("chat", inv.Message.ChatID))
	return inv.Edit(ctx, result)
}

func abanFailure(ctx context.Context, inv *command.Invocation, label string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if text, ok := kit.IsUserError(err); ok {
		return inv.EditText(ctx, "❌ "+text)
	}
	return inv.Edit(ctx, "❌ "+command.Escape(label)+"失败："+command.Code(kit.RPCCode(err)))
}

func abanBatch(ctx context.Context, inv *command.Invocation, service *abanService, ban bool) error {
	label := "解封"
	if ban {
		label = "封禁"
	}
	target, _, display, err := resolveTarget(ctx, inv)
	if err != nil {
		if text, ok := kit.IsUserError(err); ok {
			return inv.EditText(ctx, "❌ "+text)
		}
		return err
	}
	if !service.fresh() {
		if err := inv.EditText(ctx, "⏳ 正在读取管理群列表（每天一次）…"); err != nil {
			return err
		}
	}
	groups, err := service.managedGroups(ctx, inv.Client, false)
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		return inv.EditText(ctx, "❌ 无管理群组")
	}
	confirmed := false
	for _, arg := range inv.Args {
		if strings.EqualFold(arg, "true") {
			confirmed = true
		}
	}
	if !confirmed {
		var adminIn atomic.Int32
		eachGroup(ctx, groups, func(group managedGroup) {
			if group.Channel && targetIsAdmin(ctx, inv.Client, group.input(), target) {
				adminIn.Add(1)
			}
		})
		if err := ctx.Err(); err != nil {
			return err
		}
		if adminIn := int(adminIn.Load()); adminIn > 0 {
			return inv.Edit(ctx, "⚠️ 目标在 "+strconv.Itoa(adminIn)+" 个管理群中具有管理员身份，请在命令后加上 "+command.Code("true")+" 确认执行")
		}
	}
	if err := inv.Edit(ctx, "⚡ 正在 "+strconv.Itoa(len(groups))+" 个频道/群组中"+label+"该用户…"); err != nil {
		return err
	}

	started := time.Now()
	cleaned := false
	if ban {
		if chat, err := inv.Client.InputPeer(inv.Message.Peer); err == nil {
			cleaned = deleteHistory(ctx, inv.Client, chat, target)
		}
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
		// 连接层已经等过 60 秒以内的限流；这里再兜一次，照 MiBox 对单个群重试一次。
		err := kit.RetryFlood(ctx, 1, func() error {
			return applyRights(ctx, inv.Client, group.input(), target, rights, ban)
		})
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

	var detail []string
	for reason, count := range reasons {
		detail = append(detail, command.Escape(reason)+"×"+strconv.Itoa(count))
		if len(detail) == 3 {
			break
		}
	}
	text := "✅ 在" + strconv.Itoa(success) + "个频道/群组中" + label + "该用户 " + command.Escape(display)
	if failed > 0 {
		text += "\n⚠️ 失败 " + strconv.Itoa(failed) + " 个（" + strings.Join(detail, "、") + "）"
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
	return inv.Edit(ctx, text)
}
