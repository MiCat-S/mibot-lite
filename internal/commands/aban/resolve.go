package aban

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

// target 是封禁命令的作用对象：一个用户，或者以频道身份发言的频道。
type target struct {
	peer tg.InputPeerClass
	// id 是用户 id 或频道 id（不带 -100 标记）。
	id      int64
	channel bool
	display string
}

// errUnresolvedUser 表示用户走完整条查找链仍然拿不到 access hash。
// 单群命令和跨群命令给出的提示不同，由调用方换成对应的文字。
var errUnresolvedUser = errors.New("aban: user is not resolvable")

const (
	// recentPages 和 recentPageSize 是在当前频道里按「最近成员」翻找的范围：5 页，每页 200 人。
	recentPages    = 5
	recentPageSize = 200
	// resolveParallel 是跨管理群查找用户时同时进行的请求数，和原版一样是 6。
	resolveParallel = 6
)

// targetArgs 去掉确认用的参数，剩下的第一个是目标，第二个是禁言时长。
func targetArgs(args []string) []string {
	var targets []string
	for _, arg := range args {
		if !isMetaFlag(arg) {
			targets = append(targets, arg)
		}
	}
	return targets
}

// isNumericID 对应原版的 /^-?\d+$/。
func isNumericID(value string) bool {
	digits := strings.TrimPrefix(value, "-")
	if digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// resolveTarget 找出命令针对的对象，顺序照原版 UserResolver：给了参数就用参数
// （@用户名或数字 ID），否则取被回复消息的发送者，用户和频道身份都可以。
// here 是命令所在的会话，在它里面找成员是查找链的一环。
func (s *abanService) resolveTarget(ctx context.Context, inv *command.Invocation, here tg.InputPeerClass) (target, error) {
	if args := targetArgs(inv.Args); len(args) > 0 {
		return s.resolveArg(ctx, inv.Client, here, args[0])
	}
	reply, err := inv.Client.GetReply(ctx, inv.Message)
	if err != nil {
		return target{}, err
	}
	if reply == nil {
		return target{}, kit.Fail("请回复一条消息，或指定 @用户名 / 用户ID")
	}
	switch sender := reply.Sender.(type) {
	case *tg.PeerUser:
		// 回复的发送者只在当前会话范围内找，不跨群，也不用 access hash 0 兜底（同原版）。
		peer, ok := lookupUser(ctx, inv.Client, here, sender.UserID)
		if !ok {
			return target{}, errUnresolvedUser
		}
		return userTarget(inv.Client, peer, sender.UserID)
	case *tg.PeerChannel:
		return channelTarget(ctx, inv.Client, sender.ChannelID)
	}
	return target{}, kit.Fail("无法识别该消息的发送者")
}

// resolveArg 解析参数给出的目标。纯数字按 ID 处理：正数是用户，-100 开头是频道；
// 其余按用户名解析，用户名可以属于用户也可以属于频道。
func (s *abanService) resolveArg(ctx context.Context, client *bot.Client, here tg.InputPeerClass, arg string) (target, error) {
	if !isNumericID(arg) {
		peer, err := client.ResolveUsername(ctx, strings.TrimPrefix(arg, "@"))
		if err != nil {
			return target{}, kit.Fail("无法解析该目标，可先回复其一则消息，或确认用户名和 ID 正确")
		}
		switch value := peer.(type) {
		case *tg.InputPeerUser:
			return userTarget(client, value, value.UserID)
		case *tg.InputPeerChannel:
			return channelTarget(ctx, client, value.ChannelID)
		case *tg.InputPeerSelf:
			return userTarget(client, value, client.SelfID())
		}
		return target{}, kit.Fail("目标必须是用户或频道")
	}
	parsed, ok := bot.PeerFromID(arg)
	if !ok {
		return target{}, kit.Fail("获取用户失败")
	}
	switch value := parsed.(type) {
	case *tg.PeerUser:
		peer, err := s.findUser(ctx, client, here, value.UserID)
		if err != nil {
			return target{}, err
		}
		return userTarget(client, peer, value.UserID)
	case *tg.PeerChannel:
		return channelTarget(ctx, client, value.ChannelID)
	}
	return target{}, kit.Fail("目标必须是用户或频道")
}

// findUser 按数字 ID 找用户，目标不必在当前群（原版 resolveNumericUser）：
// 先在当前会话范围内找，再到各管理群里找；都找不到而命令在频道或超级群里时，
// 用 access hash 0 直接寻址，这样还没入群的人也能预先封禁。
func (s *abanService) findUser(ctx context.Context, client *bot.Client, here tg.InputPeerClass, id int64) (tg.InputPeerClass, error) {
	if peer, ok := lookupUser(ctx, client, here, id); ok {
		return peer, nil
	}
	if peer, ok := s.acrossGroups(ctx, client, here, id); ok {
		return peer, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, inChannel := bot.InputChannel(here); inChannel {
		return &tg.InputPeerUser{UserID: id}, nil
	}
	return nil, errUnresolvedUser
}

// lookupUser 在不离开当前会话的范围内找用户的 access hash：
//
//  1. 本地缓存；
//  2. users.getUsers，access hash 填 0（teleproto 按 ID 取实体时也这样试，对联系人有效）；
//  3. 当前是频道或超级群：channels.getParticipant（access hash 填 0），再按最近成员翻 5 页；
//  4. 当前是基本群：messages.getFullChat 的成员列表。
func lookupUser(ctx context.Context, client *bot.Client, here tg.InputPeerClass, id int64) (tg.InputPeerClass, bool) {
	if peer, ok := client.Peers().InputPeer(&tg.PeerUser{UserID: id}); ok {
		return peer, true
	}
	if users, err := client.API().UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUser{UserID: id}}); err == nil {
		if peer, ok := userAmong(client, users, id); ok {
			return peer, true
		}
	}
	switch chat := here.(type) {
	case *tg.InputPeerChannel:
		return memberOfChannel(ctx, client, &tg.InputChannel{ChannelID: chat.ChannelID, AccessHash: chat.AccessHash}, id, true)
	case *tg.InputPeerChat:
		return memberOfChat(ctx, client, chat.ChatID, id)
	}
	return nil, false
}

// memberOfChannel 在一个频道或超级群里按 ID 找用户：channels.getParticipant 的应答会带上
// 这个用户（含 access hash）。scan 为真时，查不到再按最近成员翻页找。
func memberOfChannel(ctx context.Context, client *bot.Client, channel *tg.InputChannel, id int64, scan bool) (tg.InputPeerClass, bool) {
	result, err := client.API().ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{Channel: channel, Participant: &tg.InputPeerUser{UserID: id}})
	if err == nil {
		if peer, ok := userAmong(client, result.Users, id); ok {
			return peer, true
		}
	}
	if !scan {
		return nil, false
	}
	offset := 0
	for page := 0; page < recentPages; page++ {
		result, err := client.API().ChannelsGetParticipants(ctx, &tg.ChannelsGetParticipantsRequest{
			Channel: channel, Filter: &tg.ChannelParticipantsRecent{}, Offset: offset, Limit: recentPageSize})
		if err != nil {
			return nil, false
		}
		list, ok := result.(*tg.ChannelsChannelParticipants)
		if !ok {
			return nil, false
		}
		if peer, ok := userAmong(client, list.Users, id); ok {
			return peer, true
		}
		if len(list.Participants) == 0 {
			break
		}
		offset += len(list.Participants)
	}
	return nil, false
}

// memberOfChat 在基本群的成员列表里按 ID 找用户。
func memberOfChat(ctx context.Context, client *bot.Client, chatID, id int64) (tg.InputPeerClass, bool) {
	full, err := client.API().MessagesGetFullChat(ctx, chatID)
	if err != nil {
		return nil, false
	}
	return userAmong(client, full.Users, id)
}

// userAmong 记下应答里的用户，再看其中有没有这个 id、而且能寻址的那一个。
func userAmong(client *bot.Client, users []tg.UserClass, id int64) (tg.InputPeerClass, bool) {
	client.Peers().RememberUsers(users)
	for _, entry := range users {
		if user, ok := entry.(*tg.User); ok && user.ID == id {
			return client.Peers().InputPeer(&tg.PeerUser{UserID: id})
		}
	}
	return nil, false
}

// acrossGroups 到各管理群里按 ID 找用户（原版 resolveUserAcrossManagedGroups）：
// 频道和超级群排在前面，用 channels.getParticipant（access hash 填 0）；基本群读成员列表。
// 同时查 6 个，找到一个就停。命令所在的群前面已经查过，跳过。
func (s *abanService) acrossGroups(ctx context.Context, client *bot.Client, here tg.InputPeerClass, id int64) (tg.InputPeerClass, bool) {
	groups, err := s.managedGroups(ctx, client, false)
	if err != nil || len(groups) == 0 {
		return nil, false
	}
	ordered := make([]managedGroup, 0, len(groups))
	for _, channels := range []bool{true, false} {
		for _, group := range groups {
			if group.Channel == channels && !group.is(here) {
				ordered = append(ordered, group)
			}
		}
	}
	search, stop := context.WithCancel(ctx)
	defer stop()
	var once sync.Once
	var found tg.InputPeerClass
	eachGroupLimit(search, ordered, resolveParallel, func(group managedGroup) {
		var peer tg.InputPeerClass
		var ok bool
		if group.Channel {
			peer, ok = memberOfChannel(search, client, &tg.InputChannel{ChannelID: group.ID, AccessHash: group.Hash}, id, false)
		} else {
			peer, ok = memberOfChat(search, client, group.ID, id)
		}
		if ok {
			once.Do(func() {
				found = peer
				stop()
			})
		}
	})
	return found, found != nil
}

// is 判断管理群是不是 peer 指向的那个会话。
func (g managedGroup) is(peer tg.InputPeerClass) bool {
	switch value := peer.(type) {
	case *tg.InputPeerChannel:
		return g.Channel && g.ID == value.ChannelID
	case *tg.InputPeerChat:
		return !g.Channel && g.ID == value.ChatID
	}
	return false
}

// userTarget 组装用户目标，名字取缓存里的，没有就显示 ID。
// 不对自己执行：.ban 会先清掉目标在群里的全部消息，回复到自己的消息上时清的就是自己的。
func userTarget(client *bot.Client, peer tg.InputPeerClass, id int64) (target, error) {
	if id == client.SelfID() {
		return target{}, kit.Fail("不能对自己执行此操作")
	}
	info, _ := client.Peers().User(id)
	return target{peer: peer, id: id, display: info.DisplayName()}, nil
}

// channelTarget 组装频道目标，显示成「频道: 标题 (@用户名)」（同原版 formatUser）。
// 缓存里没有时用 channels.getChannels（access hash 填 0）试一次，teleproto 按 ID 取频道时也这样试。
func channelTarget(ctx context.Context, client *bot.Client, id int64) (target, error) {
	peer, ok := client.Peers().InputPeer(&tg.PeerChannel{ChannelID: id})
	if !ok {
		if result, err := client.API().ChannelsGetChannels(ctx, []tg.InputChannelClass{&tg.InputChannel{ChannelID: id}}); err == nil {
			client.Peers().RememberChats(result.GetChats())
			peer, ok = client.Peers().InputPeer(&tg.PeerChannel{ChannelID: id})
		}
	}
	if !ok {
		return target{}, kit.Fail("无法解析该频道，可先回复其一则消息")
	}
	info, _ := client.Peers().Channel(id)
	display := "频道: " + kit.OrDefault(info.Title, "-100"+strconv.FormatInt(id, 10))
	if info.Username != "" {
		display += " (@" + info.Username + ")"
	}
	return target{peer: peer, id: id, channel: true, display: display}, nil
}
