package commands

import "github.com/gotd/td/tg"

const (
	dmeSelf    = int64(100)
	dmeOther   = int64(200)
	dmeAlias   = int64(300) // 自己能以它的身份发言的频道
	dmeGroup   = int64(5000)
	dmeCommand = 1000
)

func withHash[T interface{ SetAccessHash(int64) }](value T) T {
	value.SetAccessHash(1)
	return value
}

var (
	dmePrivate    = &tg.PeerUser{UserID: dmeOther}
	dmeSupergroup = &tg.PeerChannel{ChannelID: dmeGroup}
	otherUser     = []tg.UserClass{withHash(&tg.User{ID: dmeOther})}
	supergroup    = []tg.ChatClass{withHash(&tg.Channel{ID: dmeGroup, Megagroup: true, Title: "群"})}
	broadcast     = []tg.ChatClass{withHash(&tg.Channel{ID: dmeGroup, Broadcast: true, Title: "频道"})}
)
