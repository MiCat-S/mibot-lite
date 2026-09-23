package bot

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
)

// UserInfo 是缓存为一个用户保存的信息：够用来寻址，也够用来显示名字。
type UserInfo struct {
	ID        int64
	Hash      int64
	HasHash   bool
	FirstName string
	LastName  string
	Username  string
	Bot       bool
	// PhotoID 和 PhotoDC 定位头像，供 .yvlu 画头像用。
	PhotoID int64
	PhotoDC int
	// EmojiStatus 是用户挂在名字旁边的自定义 emoji。
	EmojiStatus string
}

// DisplayName 按 MiBox 插件原来的格式渲染用户名。
func (u UserInfo) DisplayName() string {
	name := strings.TrimSpace(strings.TrimSpace(u.FirstName) + " " + strings.TrimSpace(u.LastName))
	if name == "" {
		name = strconv.FormatInt(u.ID, 10)
	}
	if u.Username != "" {
		name += " (@" + u.Username + ")"
	}
	return name
}

// ChannelInfo 是缓存为频道或超级群保存的信息。
type ChannelInfo struct {
	ID          int64
	Hash        int64
	HasHash     bool
	Title       string
	Username    string
	Broadcast   bool
	Megagroup   bool
	Noforwards  bool
	Creator     bool
	Left        bool
	AdminRights tg.ChatAdminRights
	PhotoID     int64
	PhotoDC     int
}

// ChatInfo 是缓存为旧式普通群保存的信息。
type ChatInfo struct {
	ID          int64
	Title       string
	Noforwards  bool
	Creator     bool
	Left        bool
	AdminRights tg.ChatAdminRights
	PhotoID     int64
	PhotoDC     int
}

// HashLookup 是持久化 access hash 存储的读取接口。
type HashLookup interface {
	GetChannelAccessHash(ctx context.Context, userID, channelID int64) (int64, bool, error)
	GetUserAccessHash(ctx context.Context, userID, targetUserID int64) (int64, bool, error)
}

// PeerCache 记下随更新和 RPC 回复一起到来的 access hash 和名字，之后
// 就能直接寻址某个 peer，省掉一次解析往返。min 实体从不提供 hash：
// 它们带的 hash 不能用来寻址。
type PeerCache struct {
	mu       sync.RWMutex
	users    map[int64]*UserInfo
	channels map[int64]*ChannelInfo
	chats    map[int64]*ChatInfo
	selfID   int64
	durable  HashLookup
}

// NewPeerCache 创建一个空缓存。
func NewPeerCache() *PeerCache {
	return &PeerCache{users: map[int64]*UserInfo{}, channels: map[int64]*ChannelInfo{}, chats: map[int64]*ChatInfo{}}
}

// SetSelf 记下已认证账号的 id。
func (c *PeerCache) SetSelf(id int64) {
	c.mu.Lock()
	c.selfID = id
	c.mu.Unlock()
}

// SetDurable 接上持久化的 hash 存储。
func (c *PeerCache) SetDurable(lookup HashLookup) {
	c.mu.Lock()
	c.durable = lookup
	c.mu.Unlock()
}

// Remember 记下一条更新携带的所有实体。
func (c *PeerCache) Remember(entities tg.Entities) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, user := range entities.Users {
		c.rememberUser(user)
	}
	for _, chat := range entities.Chats {
		c.rememberChat(chat)
	}
	for _, channel := range entities.Channels {
		c.rememberChannel(channel)
	}
}

// RememberUsers 记下 RPC 回复里的用户。
func (c *PeerCache) RememberUsers(users []tg.UserClass) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range users {
		if user, ok := entry.(*tg.User); ok {
			c.rememberUser(user)
		}
	}
}

// RememberChats 记下 RPC 回复里的普通群和频道。
func (c *PeerCache) RememberChats(chats []tg.ChatClass) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range chats {
		switch value := entry.(type) {
		case *tg.Chat:
			c.rememberChat(value)
		case *tg.Channel:
			c.rememberChannel(value)
		case *tg.ChannelForbidden:
			info := c.channels[value.ID]
			if info == nil {
				info = &ChannelInfo{ID: value.ID}
				c.channels[value.ID] = info
			}
			info.Title = value.Title
			info.Broadcast, info.Megagroup = value.Broadcast, value.Megagroup
			if value.AccessHash != 0 {
				info.Hash, info.HasHash = value.AccessHash, true
			}
		}
	}
}

func (c *PeerCache) rememberUser(user *tg.User) {
	info := c.users[user.ID]
	if info == nil {
		info = &UserInfo{ID: user.ID}
		c.users[user.ID] = info
	}
	info.FirstName, info.LastName, info.Username, info.Bot = user.FirstName, user.LastName, user.Username, user.Bot
	if photo, ok := user.GetPhoto(); ok {
		if value, ok := photo.(*tg.UserProfilePhoto); ok {
			info.PhotoID, info.PhotoDC = value.PhotoID, value.DCID
		}
	}
	if status, ok := user.GetEmojiStatus(); ok {
		if value, ok := status.(*tg.EmojiStatus); ok && value.DocumentID != 0 {
			info.EmojiStatus = strconv.FormatInt(value.DocumentID, 10)
		}
	}
	if user.AccessHash != 0 && !user.Min {
		info.Hash, info.HasHash = user.AccessHash, true
	}
}

func (c *PeerCache) rememberChannel(channel *tg.Channel) {
	info := c.channels[channel.ID]
	if info == nil {
		info = &ChannelInfo{ID: channel.ID}
		c.channels[channel.ID] = info
	}
	info.Title, info.Username = channel.Title, channel.Username
	if photo, ok := channel.GetPhoto().(*tg.ChatPhoto); ok {
		info.PhotoID, info.PhotoDC = photo.PhotoID, photo.DCID
	}
	info.Broadcast, info.Megagroup, info.Noforwards = channel.Broadcast, channel.Megagroup, channel.Noforwards
	info.Left = channel.Left
	if !channel.Min {
		info.Creator = channel.Creator
		info.AdminRights = channel.AdminRights
	}
	if channel.AccessHash != 0 && !channel.Min {
		info.Hash, info.HasHash = channel.AccessHash, true
	}
}

func (c *PeerCache) rememberChat(chat *tg.Chat) {
	info := c.chats[chat.ID]
	if info == nil {
		info = &ChatInfo{ID: chat.ID}
		c.chats[chat.ID] = info
	}
	info.Title, info.Noforwards, info.Creator, info.Left, info.AdminRights = chat.Title, chat.Noforwards, chat.Creator, chat.Left, chat.AdminRights
	if photo, ok := chat.GetPhoto().(*tg.ChatPhoto); ok {
		info.PhotoID, info.PhotoDC = photo.PhotoID, photo.DCID
	}
}

// User 返回已知的用户信息。
func (c *PeerCache) User(id int64) (UserInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	info, ok := c.users[id]
	if !ok {
		return UserInfo{ID: id}, false
	}
	return *info, true
}

// Channel 返回已知的频道信息。
func (c *PeerCache) Channel(id int64) (ChannelInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	info, ok := c.channels[id]
	if !ok {
		return ChannelInfo{ID: id}, false
	}
	return *info, true
}

// Chat 返回已知的旧式普通群信息。
func (c *PeerCache) Chat(id int64) (ChatInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	info, ok := c.chats[id]
	if !ok {
		return ChatInfo{ID: id}, false
	}
	return *info, true
}

// Title 返回 peer 的显示名：对话的标题，或者用户的名字。
func (c *PeerCache) Title(peer tg.PeerClass) string {
	switch value := peer.(type) {
	case *tg.PeerUser:
		info, _ := c.User(value.UserID)
		name := strings.TrimSpace(info.FirstName + " " + info.LastName)
		if name == "" {
			return strconv.FormatInt(value.UserID, 10)
		}
		return name
	case *tg.PeerChat:
		info, ok := c.Chat(value.ChatID)
		if !ok || info.Title == "" {
			return "-" + strconv.FormatInt(value.ChatID, 10)
		}
		return info.Title
	case *tg.PeerChannel:
		info, ok := c.Channel(value.ChannelID)
		if !ok || info.Title == "" {
			return "-100" + strconv.FormatInt(value.ChannelID, 10)
		}
		return info.Title
	}
	return ""
}

// InputPeer 把 peer 转成可寻址的形式；不知道 access hash 时返回 false。
func (c *PeerCache) InputPeer(peer tg.PeerClass) (tg.InputPeerClass, bool) {
	c.mu.RLock()
	selfID, durable := c.selfID, c.durable
	switch value := peer.(type) {
	case *tg.PeerUser:
		if selfID != 0 && value.UserID == selfID {
			c.mu.RUnlock()
			return &tg.InputPeerSelf{}, true
		}
		info, known := c.users[value.UserID]
		c.mu.RUnlock()
		if known && info.HasHash {
			return &tg.InputPeerUser{UserID: value.UserID, AccessHash: info.Hash}, true
		}
		if hash, ok := lookupHash(durable, selfID, value.UserID, false); ok {
			return &tg.InputPeerUser{UserID: value.UserID, AccessHash: hash}, true
		}
		return nil, false
	case *tg.PeerChat:
		c.mu.RUnlock()
		return &tg.InputPeerChat{ChatID: value.ChatID}, true
	case *tg.PeerChannel:
		info, known := c.channels[value.ChannelID]
		c.mu.RUnlock()
		if known && info.HasHash {
			return &tg.InputPeerChannel{ChannelID: value.ChannelID, AccessHash: info.Hash}, true
		}
		if hash, ok := lookupHash(durable, selfID, value.ChannelID, true); ok {
			return &tg.InputPeerChannel{ChannelID: value.ChannelID, AccessHash: hash}, true
		}
		return nil, false
	}
	c.mu.RUnlock()
	return nil, false
}

func lookupHash(durable HashLookup, selfID, targetID int64, channel bool) (int64, bool) {
	if durable == nil || selfID == 0 {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var hash int64
	var found bool
	var err error
	if channel {
		hash, found, err = durable.GetChannelAccessHash(ctx, selfID, targetID)
	} else {
		hash, found, err = durable.GetUserAccessHash(ctx, selfID, targetID)
	}
	if err != nil {
		return 0, false
	}
	return hash, found
}
