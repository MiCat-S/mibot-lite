package cleanmember

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
	"github.com/MiCat-S/mibot-lite/internal/commands/kit"
)

const (
	// pageSize 是每页成员数，channels.getParticipants 一次最多给 200。
	pageSize = 200
	// maxOffset 是翻页的上限，同 v2。
	maxOffset = 50000
	// floodAttempts 是遇到 FLOOD_WAIT 时最多试几次，同 v2。
	floodAttempts = 3
)

// group 是要处理的群组。
type group struct {
	peer tg.InputPeerClass
	// channel 是超级群组或频道；基本群组为 nil，用 basic。
	channel *tg.InputChannel
	basic   int64
	// chatID 是不带 -100 的纯数字，同 v2 的 String(chat.id)：报告和缓存的键都用它。
	chatID    string
	title     string
	broadcast bool
}

// resolveGroup 找出要处理的群组：target 为空就是当前对话。
func resolveGroup(ctx context.Context, client *bot.Client, message *bot.Message, target string) (group, error) {
	if target == "" {
		peer, err := client.InputPeer(message.Peer)
		if err != nil {
			return group{}, err
		}
		return describe(client, peer)
	}
	peer, err := client.ResolveTarget(ctx, target)
	if errors.Is(err, bot.ErrUnaddressablePeer) {
		// 缓存里没有这个对话的 access hash：翻一遍对话列表（顺带记下所有对话）再找一次。
		// v2 的 getEntity 在本地找不到时也会这么做。
		for _, folder := range []int{0, 1} {
			if scanErr := client.EachDialog(ctx, bot.DialogPages{Folder: folder, MaxPages: 20}, func(*tg.Dialog) {}); scanErr != nil {
				return group{}, kit.FailWith("读取对话列表失败", scanErr)
			}
			if peer, err = client.ResolveTarget(ctx, target); err == nil {
				break
			}
		}
	}
	if err != nil {
		if errors.Is(err, bot.ErrUnaddressablePeer) {
			return group{}, kit.Failf("找不到对话 %s：账号不在这个群组里，或者 ID 写错了", target)
		}
		return group{}, kit.FailWith("找不到对话 "+target, err)
	}
	return describe(client, peer)
}

// describe 确认对象是群组或频道，取出后面要用的 ID 和名称。
func describe(client *bot.Client, peer tg.InputPeerClass) (group, error) {
	switch value := peer.(type) {
	case *tg.InputPeerChannel:
		info, _ := client.Peers().Channel(value.ChannelID)
		return group{peer: peer, channel: &tg.InputChannel{ChannelID: value.ChannelID, AccessHash: value.AccessHash},
			chatID: strconv.FormatInt(value.ChannelID, 10), title: client.Peers().Title(&tg.PeerChannel{ChannelID: value.ChannelID}),
			broadcast: info.Broadcast}, nil
	case *tg.InputPeerChat:
		return group{peer: peer, basic: value.ChatID, chatID: strconv.FormatInt(value.ChatID, 10),
			title: client.Peers().Title(&tg.PeerChat{ChatID: value.ChatID})}, nil
	}
	return group{}, kit.Fail("只能用于群组：在群组里用，或者用 chat:对话 ID 指定")
}

// member 是成员列表里的一个人；admin 表示列表本身就标明了他是管理员或群主。
type member struct {
	user  *tg.User
	admin bool
}

// retryFlood 执行 call，遇到 FLOOD_WAIT 等够了再试，最多 floodAttempts 次。连接层只等 60 秒以内的，
// 清理大群时限流常常更长；v2 也是等够了重试。
func (s *service) retryFlood(ctx context.Context, call func() error) error {
	for attempt := 1; ; attempt++ {
		err := call()
		wait, flooded := bot.FloodWait(err)
		if !flooded || attempt >= floodAttempts || ctx.Err() != nil {
			return err
		}
		if sleepErr := s.pause(ctx, wait+time.Second); sleepErr != nil {
			return sleepErr
		}
	}
}

// canBan 判断账号能不能在这个群组里移出成员：群主，或者有封禁成员权限的管理员。
func (s *service) canBan(ctx context.Context, client *bot.Client, g group) (bool, error) {
	if g.channel == nil {
		members, err := s.basicMembers(ctx, client, g.basic)
		if err != nil {
			return false, err
		}
		for _, m := range members {
			if m.user.ID == client.SelfID() {
				return m.admin, nil
			}
		}
		return false, nil
	}
	result, err := client.API().ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{Channel: g.channel, Participant: &tg.InputPeerSelf{}})
	if err != nil {
		return false, err
	}
	client.Peers().RememberUsers(result.Users)
	switch participant := result.Participant.(type) {
	case *tg.ChannelParticipantCreator:
		return true, nil
	case *tg.ChannelParticipantAdmin:
		return participant.AdminRights.BanUsers, nil
	}
	return false, nil
}

// admins 读管理员列表。v2 拿应答里的 users 当管理员，那里面还有提拔别人的人，不一定是管理员；
// 这里看成员条目本身。基本群组的管理员在成员列表里就标出来了，不用另读。
func (s *service) admins(ctx context.Context, client *bot.Client, g group) (map[int64]bool, error) {
	ids := map[int64]bool{}
	if g.channel == nil {
		return ids, nil
	}
	for offset := 0; ; offset += pageSize {
		var result tg.ChannelsChannelParticipantsClass
		err := s.retryFlood(ctx, func() error {
			var err error
			result, err = client.API().ChannelsGetParticipants(ctx, &tg.ChannelsGetParticipantsRequest{
				Channel: g.channel, Filter: &tg.ChannelParticipantsAdmins{}, Offset: offset, Limit: pageSize})
			return err
		})
		if err != nil {
			return nil, err
		}
		list, ok := result.(*tg.ChannelsChannelParticipants)
		if !ok {
			return ids, nil
		}
		client.Peers().RememberUsers(list.Users)
		for _, participant := range list.Participants {
			if id, admin, ok := participantUser(participant); ok && admin {
				ids[id] = true
			}
		}
		if len(list.Participants) < pageSize {
			return ids, nil
		}
		if offset >= maxOffset {
			return nil, errors.New("admin list exceeds the supported size")
		}
	}
}

// participantUser 取成员条目里的用户；已封禁、已退出的不算成员。
func participantUser(participant tg.ChannelParticipantClass) (id int64, admin bool, ok bool) {
	switch value := participant.(type) {
	case *tg.ChannelParticipant:
		return value.UserID, false, true
	case *tg.ChannelParticipantSelf:
		return value.UserID, false, true
	case *tg.ChannelParticipantAdmin:
		return value.UserID, true, true
	case *tg.ChannelParticipantCreator:
		return value.UserID, true, true
	}
	return 0, false, false
}

func usersByID(users []tg.UserClass) map[int64]*tg.User {
	byID := make(map[int64]*tg.User, len(users))
	for _, entry := range users {
		if user, ok := entry.(*tg.User); ok {
			byID[user.ID] = user
		}
	}
	return byID
}

// page 从 offset 起取一页成员，返回这一页的条目数（翻页按它算）。超级群组和频道按「最近成员」
// 每页 200 人往后翻，同 v2；基本群组一次就是全部。
//
// v2 遍历的是应答里的 users，那里还有邀请人、提拔人这些不在这一页（甚至不在群里）的用户，
// 会被当成成员去移出；这里按成员条目走。
func (s *service) page(ctx context.Context, client *bot.Client, g group, offset int) ([]member, int, error) {
	if g.channel == nil {
		if offset > 0 {
			return nil, 0, nil
		}
		members, err := s.basicMembers(ctx, client, g.basic)
		return members, len(members), err
	}
	var result tg.ChannelsChannelParticipantsClass
	err := s.retryFlood(ctx, func() error {
		var err error
		result, err = client.API().ChannelsGetParticipants(ctx, &tg.ChannelsGetParticipantsRequest{
			Channel: g.channel, Filter: &tg.ChannelParticipantsRecent{}, Offset: offset, Limit: pageSize})
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	list, ok := result.(*tg.ChannelsChannelParticipants)
	if !ok {
		return nil, 0, nil
	}
	client.Peers().RememberUsers(list.Users)
	users := usersByID(list.Users)
	var members []member
	for _, participant := range list.Participants {
		id, admin, ok := participantUser(participant)
		if !ok {
			continue
		}
		if user, found := users[id]; found {
			members = append(members, member{user: user, admin: admin})
		}
	}
	return members, len(list.Participants), nil
}

// basicMembers 读基本群组的全部成员。v2 对基本群组调 channels.getParticipants，
// 实际上一个成员也读不到；基本群组最多 200 人，messages.getFullChat 一次就给全了。
func (s *service) basicMembers(ctx context.Context, client *bot.Client, chatID int64) ([]member, error) {
	var full *tg.MessagesChatFull
	err := s.retryFlood(ctx, func() error {
		var err error
		full, err = client.API().MessagesGetFullChat(ctx, chatID)
		return err
	})
	if err != nil {
		return nil, err
	}
	client.Peers().RememberUsers(full.Users)
	client.Peers().RememberChats(full.Chats)
	info, ok := full.FullChat.(*tg.ChatFull)
	if !ok {
		return nil, kit.Fail("读不到这个群组的成员列表")
	}
	list, ok := info.Participants.(*tg.ChatParticipants)
	if !ok {
		return nil, kit.Fail("看不到这个群组的成员列表")
	}
	users := usersByID(full.Users)
	var members []member
	for _, participant := range list.Participants {
		user, found := users[participant.GetUserID()]
		if !found {
			continue
		}
		switch participant.(type) {
		case *tg.ChatParticipantCreator, *tg.ChatParticipantAdmin:
			members = append(members, member{user: user, admin: true})
		default:
			members = append(members, member{user: user})
		}
	}
	return members, nil
}

// lastSeenDays 照 v2 估算多少天没上线：在线和「最近」算 0，离线按离线时刻算，
// 「一周内」算 7，「一月内」算 30；看不到（隐藏或从未有过状态）时返回 false，这人跳过。
func lastSeenDays(status tg.UserStatusClass, now time.Time) (int, bool) {
	switch value := status.(type) {
	case *tg.UserStatusOnline, *tg.UserStatusRecently:
		return 0, true
	case *tg.UserStatusOffline:
		if value.WasOnline == 0 {
			return 0, false
		}
		return int(now.Sub(time.Unix(int64(value.WasOnline), 0)) / (24 * time.Hour)), true
	case *tg.UserStatusLastWeek:
		return 7, true
	case *tg.UserStatusLastMonth:
		return 30, true
	}
	return 0, false
}

// matches 判断一个成员是否符合条件。模式 2、3 要逐个搜索这人在群里的消息，查不到时返回错误，
// 调用方把这人算作跳过（同 v2）。
func (s *service) matches(ctx context.Context, client *bot.Client, g group, req request, user *tg.User) (bool, error) {
	switch req.mode {
	case "1":
		status, _ := user.GetStatus()
		days, known := lastSeenDays(status, s.now())
		return known && days > req.day, nil
	case "4":
		return user.Deleted, nil
	case "5":
		return true, nil
	}
	count, err := s.countMessages(ctx, client, g, req, user)
	if err != nil {
		return false, err
	}
	if req.mode == "2" {
		return count == 0, nil
	}
	return count < req.day, nil
}

// countMessages 数这个成员在群里的消息：模式 2 只数最近 day 天的，模式 3 数全部能搜到的。
// 只要一条消息，总数看应答里的 count。
func (s *service) countMessages(ctx context.Context, client *bot.Client, g group, req request, user *tg.User) (int, error) {
	request := &tg.MessagesSearchRequest{Peer: g.peer, Q: "", Filter: &tg.InputMessagesFilterEmpty{}, Limit: 1}
	if req.mode == "2" {
		request.MinDate = int(max(0, s.now().Unix()-int64(req.day)*86400))
	}
	request.SetFromID(&tg.InputPeerUser{UserID: user.ID, AccessHash: user.AccessHash})
	var result tg.MessagesMessagesClass
	err := s.retryFlood(ctx, func() error {
		var err error
		result, err = client.API().MessagesSearch(ctx, request)
		return err
	})
	if err != nil {
		return 0, err
	}
	_, count := client.Unpack(result)
	return count, nil
}

// kickRights 是移出时用的封禁，同 v2：只封 60 秒。万一后面的解封失败，对方一分钟后也能重新加入。
func kickRights(until int) tg.ChatBannedRights {
	return tg.ChatBannedRights{UntilDate: until, ViewMessages: true, SendMessages: true, SendMedia: true,
		SendStickers: true, SendGifs: true, SendGames: true, SendInline: true, SendPolls: true,
		ChangeInfo: true, InviteUsers: true, PinMessages: true}
}

// remove 移出一个成员：超级群组里先封禁、2 秒后解封（同 v2），对方还能重新加入；
// 基本群组直接移出。对方已经不在群里算成功，同 v2。
//
// 封禁成功就算移出了：解封失败时 v2 把这人记成失败，其实他已经不在群里，封禁一分钟后自己失效。
func (s *service) remove(ctx context.Context, client *bot.Client, g group, user *tg.User) error {
	if g.channel == nil {
		err := s.retryFlood(ctx, func() error {
			_, err := client.API().MessagesDeleteChatUser(ctx, &tg.MessagesDeleteChatUserRequest{
				ChatID: g.basic, UserID: &tg.InputUser{UserID: user.ID, AccessHash: user.AccessHash}})
			return err
		})
		if tgerr.Is(err, "USER_NOT_PARTICIPANT") {
			return nil
		}
		return err
	}
	target := &tg.InputPeerUser{UserID: user.ID, AccessHash: user.AccessHash}
	edit := func(rights tg.ChatBannedRights) error {
		return s.retryFlood(ctx, func() error {
			_, err := client.API().ChannelsEditBanned(ctx, &tg.ChannelsEditBannedRequest{Channel: g.channel, Participant: target, BannedRights: rights})
			return err
		})
	}
	err := edit(kickRights(int(s.now().Add(time.Minute).Unix())))
	if tgerr.Is(err, "USER_NOT_PARTICIPANT") {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.pause(ctx, 2*time.Second); err != nil {
		return err
	}
	if err := edit(tg.ChatBannedRights{}); err != nil && ctx.Err() == nil {
		client.Logger().Warn("clean_member.unban_failed", slog.Int64("user", user.ID), slog.String("error", err.Error()))
	}
	return nil
}

// failureReason 是失败名单里的原因，前两种说法同 v2，其余附上错误码。
func failureReason(err error) string {
	if tgerr.Is(err, "CHAT_ADMIN_REQUIRED") {
		return "权限不足"
	}
	if _, flooded := bot.FloodWait(err); flooded {
		return "请求频率受限"
	}
	return "移出失败（" + command.Brief(err) + "）"
}
