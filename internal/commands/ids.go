package commands

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/MiCat-S/mibot-lite/internal/app"
	"github.com/MiCat-S/mibot-lite/internal/bot"
	"github.com/MiCat-S/mibot-lite/internal/command"
)

// Telegram does not publish when an account was made. User ids were handed
// out roughly in order, so the date is estimated from where an id falls
// between accounts whose creation is known. The table is MiBox's.
var idTimeline = [][2]int64{
	{0, 1376438400}, {50000000, 1400000000}, {150000000, 1451606400},
	{350000000, 1483228800}, {500000000, 1514764800}, {900000000, 1559347200},
	{1100000000, 1585699200}, {1450000000, 1609459200}, {2150000000, 1640995200},
	{5100000000, 1654041600}, {5600000000, 1672531200}, {6800000000, 1704067200},
	{7800000000, 1735689600}, {8500000000, 1767225600},
}

// estimateCreation interpolates within the table. Past its end the last
// segment's pace is carried forward — MiBox fell back to the first and
// last points there, which put a new account years in the past — and the
// result never lands after now.
func estimateCreation(id int64, now time.Time) time.Time {
	lower, upper := idTimeline[len(idTimeline)-2], idTimeline[len(idTimeline)-1]
	for index := 0; index < len(idTimeline)-1; index++ {
		if id >= idTimeline[index][0] && id <= idTimeline[index+1][0] {
			lower, upper = idTimeline[index], idTimeline[index+1]
			break
		}
	}
	stamp := lower[1] + (id-lower[0])*(upper[1]-lower[1])/(upper[0]-lower[0])
	estimate := time.Unix(stamp, 0)
	if estimate.After(now) {
		return now
	}
	return estimate
}

// dcPlaces are where Telegram's data centres are, which is the question
// behind asking for one.
var dcPlaces = map[int]string{1: "迈阿密", 2: "阿姆斯特丹", 3: "迈阿密", 4: "阿姆斯特丹", 5: "新加坡"}

func dcLabel(dc int) string {
	if dc <= 0 {
		return "看不出（没有公开头像）"
	}
	if place, ok := dcPlaces[dc]; ok {
		return fmt.Sprintf("DC%d（%s）", dc, place)
	}
	return fmt.Sprintf("DC%d", dc)
}

// resolveEntity finds the subject: an argument if there is one, else the
// sender of the replied message, else the fallback.
func resolveEntity(ctx context.Context, inv *command.Invocation, fallback tg.InputPeerClass) (tg.InputPeerClass, error) {
	if argument := strings.TrimSpace(inv.Arg(0)); argument != "" {
		peer, err := inv.Client.ResolveTarget(ctx, argument)
		if err != nil {
			if errors.Is(err, bot.ErrUnaddressablePeer) {
				return nil, errors.New("没见过这个 ID，账号需要先在某个对话里遇到过对方；用 @用户名 或回复对方的消息更可靠")
			}
			return nil, fmt.Errorf("找不到 %s", argument)
		}
		return peer, nil
	}
	if inv.Message.ReplyToID != 0 {
		reply, err := inv.Client.GetReply(ctx, inv.Message)
		if err != nil || reply == nil {
			return nil, errors.New("读不到被回复的消息")
		}
		if reply.Sender != nil {
			return inv.Client.InputPeer(reply.Sender)
		}
	}
	return fallback, nil
}

// entityInfo is what .ids and .dc need about one peer.
type entityInfo struct {
	kind     string // user, channel, supergroup, group
	id       int64
	name     string
	username string
	dc       int
	about    string
	common   int
	members  int
	flags    []string
}

func fetchEntity(ctx context.Context, client *bot.Client, peer tg.InputPeerClass) (*entityInfo, error) {
	api := client.API()
	if user, ok := bot.InputUser(peer); ok {
		full, err := api.UsersGetFullUser(ctx, user)
		if err != nil {
			return nil, err
		}
		client.Peers().RememberUsers(full.Users)
		info := &entityInfo{kind: "user", about: full.FullUser.About, common: full.FullUser.CommonChatsCount}
		for _, candidate := range full.Users {
			value, ok := candidate.(*tg.User)
			if !ok || value.ID != full.FullUser.ID {
				continue
			}
			info.id = value.ID
			info.name = strings.TrimSpace(value.FirstName + " " + value.LastName)
			info.username = primaryUsername(value.Username, value.Usernames)
			if photo, ok := value.Photo.(*tg.UserProfilePhoto); ok {
				info.dc = photo.DCID
			}
			for _, flag := range []struct {
				set   bool
				label string
			}{
				{value.Bot, "🤖 机器人"}, {value.Verified, "✅ 已验证"}, {value.Premium, "⭐ Premium"},
				{value.Scam, "⚠️ 诈骗"}, {value.Fake, "❌ 虚假"}, {value.Deleted, "🗑 已注销"},
			} {
				if flag.set {
					info.flags = append(info.flags, flag.label)
				}
			}
		}
		if info.name == "" {
			info.name = "已注销的账号"
		}
		return info, nil
	}
	if channel, ok := bot.InputChannel(peer); ok {
		full, err := api.ChannelsGetFullChannel(ctx, channel)
		if err != nil {
			return nil, err
		}
		client.Peers().RememberChats(full.Chats)
		info := &entityInfo{kind: "channel"}
		if detail, ok := full.FullChat.(*tg.ChannelFull); ok {
			info.about, info.members = detail.About, detail.ParticipantsCount
		}
		for _, candidate := range full.Chats {
			value, ok := candidate.(*tg.Channel)
			if !ok || value.ID != channel.ChannelID {
				continue
			}
			info.id = value.ID
			info.name = value.Title
			info.username = primaryUsername(value.Username, value.Usernames)
			if value.Megagroup {
				info.kind = "supergroup"
			}
			if photo, ok := value.Photo.(*tg.ChatPhoto); ok {
				info.dc = photo.DCID
			}
			if value.Verified {
				info.flags = append(info.flags, "✅ 已验证")
			}
			if value.Scam {
				info.flags = append(info.flags, "⚠️ 诈骗")
			}
		}
		return info, nil
	}
	if chat, ok := peer.(*tg.InputPeerChat); ok {
		full, err := api.MessagesGetFullChat(ctx, chat.ChatID)
		if err != nil {
			return nil, err
		}
		client.Peers().RememberChats(full.Chats)
		info := &entityInfo{kind: "group", id: chat.ChatID}
		if detail, ok := full.FullChat.(*tg.ChatFull); ok {
			info.about = detail.About
		}
		for _, candidate := range full.Chats {
			if value, ok := candidate.(*tg.Chat); ok && value.ID == chat.ChatID {
				info.name, info.members = value.Title, value.ParticipantsCount
				if photo, ok := value.Photo.(*tg.ChatPhoto); ok {
					info.dc = photo.DCID
				}
			}
		}
		return info, nil
	}
	return nil, errors.New("这种对象查不了")
}

// primaryUsername prefers the editable username, then the first active
// collectible one.
func primaryUsername(username string, usernames []tg.Username) string {
	if username != "" {
		return username
	}
	for _, candidate := range usernames {
		if candidate.Active {
			return candidate.Username
		}
	}
	return ""
}

// joinedAt reads when a user joined the supergroup the command came from.
func joinedAt(ctx context.Context, inv *command.Invocation, user tg.InputPeerClass) (time.Time, bool) {
	if !inv.Message.IsGroup() {
		return time.Time{}, false
	}
	chat, err := inv.Client.InputPeer(inv.Message.Peer)
	if err != nil {
		return time.Time{}, false
	}
	channel, ok := bot.InputChannel(chat)
	if !ok {
		return time.Time{}, false
	}
	result, err := inv.Client.API().ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{Channel: channel, Participant: user})
	if err != nil {
		return time.Time{}, false
	}
	switch value := result.Participant.(type) {
	case *tg.ChannelParticipant:
		return time.Unix(int64(value.Date), 0), true
	case *tg.ChannelParticipantAdmin:
		return time.Unix(int64(value.Date), 0), true
	case *tg.ChannelParticipantSelf:
		return time.Unix(int64(value.Date), 0), true
	case *tg.ChannelParticipantBanned:
		return time.Unix(int64(value.Date), 0), true
	}
	return time.Time{}, false
}

func renderIDs(info *entityInfo, joined time.Time, now time.Time) string {
	idText := strconv.FormatInt(info.id, 10)
	username := "无"
	if info.username != "" {
		username = "@" + info.username
	}
	var lines []string
	if info.kind == "user" {
		lines = append(lines, "👤 <b>"+command.Escape(info.name)+"</b>", "",
			"• 用户名："+command.Code(username),
			"• ID："+command.Code(idText),
			"• 注册时间："+command.Code("约 "+estimateCreation(info.id, now).Format("2006年1月"))+" <i>按 ID 估算，误差约两个月</i>")
		if !joined.IsZero() {
			lines = append(lines, "• 入群时间："+command.Code(joined.Format("2006-01-02 15:04")))
		}
		lines = append(lines,
			"• DC："+command.Escape(dcLabel(info.dc)),
			"• 共同群组："+command.Code(strconv.Itoa(info.common))+" 个")
	} else {
		kind := map[string]string{"channel": "频道", "supergroup": "超级群", "group": "普通群"}[info.kind]
		fullID := idText
		if info.kind != "group" {
			fullID = "-100" + idText
		}
		lines = append(lines, "📢 <b>"+command.Escape(info.name)+"</b>", "",
			"• 类型："+kind,
			"• 用户名："+command.Code(username),
			"• ID："+command.Code(fullID))
		if info.members > 0 {
			lines = append(lines, "• 成员："+command.Code(strconv.Itoa(info.members)))
		}
		lines = append(lines, "• DC："+command.Escape(dcLabel(info.dc)))
	}
	if len(info.flags) > 0 {
		lines = append(lines, "• 状态："+strings.Join(info.flags, " "))
	}
	if about := strings.TrimSpace(info.about); about != "" {
		if runes := []rune(about); len(runes) > 300 {
			about = string(runes[:300]) + "…"
		}
		lines = append(lines, "", "<b>简介</b>", command.Escape(about))
	}
	var links []string
	if info.kind == "user" {
		links = append(links, `<a href="tg://user?id=`+idText+`">用户资料</a>`, `<a href="tg://openmessage?user_id=`+idText+`">打开对话</a>`)
	}
	if info.username != "" {
		links = append(links, `<a href="https://t.me/`+command.Escape(info.username)+`">t.me/`+command.Escape(info.username)+`</a>`)
	}
	if len(links) > 0 {
		lines = append(lines, "", "<b>链接</b>", strings.Join(links, " · "))
		if info.kind == "user" {
			lines = append(lines, command.Code("tg://user?id="+idText))
		}
	}
	return strings.Join(lines, "\n")
}

func idsHelp(prefix string) string {
	p := command.Escape(prefix)
	return "🆔 <b>用户信息</b>\n\n" +
		"• <code>" + p + "ids</code> 自己\n• <code>" + p + "ids @用户名</code> 或 <code>" + p + "ids 用户ID</code>\n" +
		"• 回复某人的消息发 <code>" + p + "ids</code>\n\n" +
		"显示 ID、用户名、估算的注册时间、所在 DC、共同群组数、简介和跳转链接；在超级群里还会显示入群时间。频道和群也能查。\n\n" +
		"注册时间是按 ID 估算的，Telegram 不公开真实时间。"
}

func dcHelp(prefix string) string {
	p := command.Escape(prefix)
	return "📍 <b>所在数据中心</b>\n\n" +
		"• <code>" + p + "dc</code> 当前对话（私聊里是对方）\n• 回复某人的消息发 <code>" + p + "dc</code>\n" +
		"• <code>" + p + "dc @用户名</code>\n\n" +
		"DC 读自头像存放的位置，没有公开头像的账号看不出来。"
}

// Entity registers .ids and .dc.
func Entity(a *app.App) {
	ids := func(ctx context.Context, inv *command.Invocation) error {
		if strings.EqualFold(inv.Arg(0), "help") || strings.EqualFold(inv.Arg(0), "h") {
			return inv.Edit(ctx, idsHelp(inv.Prefix))
		}
		peer, err := resolveEntity(ctx, inv, &tg.InputPeerSelf{})
		if err != nil {
			return inv.EditText(ctx, "❌ "+err.Error())
		}
		info, err := fetchEntity(ctx, inv.Client, peer)
		if err != nil {
			return inv.EditText(ctx, "❌ 查询失败："+command.Brief(err))
		}
		var joined time.Time
		if info.kind == "user" {
			joined, _ = joinedAt(ctx, inv, peer)
		}
		return inv.Edit(ctx, renderIDs(info, joined, time.Now()))
	}
	dc := func(ctx context.Context, inv *command.Invocation) error {
		if strings.EqualFold(inv.Arg(0), "help") || strings.EqualFold(inv.Arg(0), "h") {
			return inv.Edit(ctx, dcHelp(inv.Prefix))
		}
		here, err := inv.Client.InputPeer(inv.Message.Peer)
		if err != nil {
			here = &tg.InputPeerSelf{}
		}
		peer, err := resolveEntity(ctx, inv, here)
		if err != nil {
			return inv.EditText(ctx, "❌ "+err.Error())
		}
		info, err := fetchEntity(ctx, inv.Client, peer)
		if err != nil {
			return inv.EditText(ctx, "❌ 查询失败："+command.Brief(err))
		}
		return inv.Edit(ctx, "📍 <b>"+command.Escape(info.name)+"</b>\n所在数据中心："+command.Escape(dcLabel(info.dc)))
	}
	a.Registry.Register(
		&command.Command{Name: "ids", Description: "查用户或对话的资料", Usage: "[@用户名|ID]", Help: idsHelp, Handle: ids},
		&command.Command{Name: "dc", Description: "查用户或对话所在的数据中心", Usage: "[@用户名|ID]", Help: dcHelp, Handle: dc},
	)
}
