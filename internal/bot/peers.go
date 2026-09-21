package bot

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
)

// UserInfo is what the cache keeps about a user: enough to address it and
// to print its name.
type UserInfo struct {
	ID        int64
	Hash      int64
	HasHash   bool
	FirstName string
	LastName  string
	Username  string
	Bot       bool
}

// DisplayName renders a user the way the MiBox plugins did.
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

// ChannelInfo is what the cache keeps about a channel or supergroup.
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
}

// ChatInfo is what the cache keeps about a legacy group.
type ChatInfo struct {
	ID          int64
	Title       string
	Noforwards  bool
	Creator     bool
	Left        bool
	AdminRights tg.ChatAdminRights
}

// HashLookup is the read side of the persisted access-hash store.
type HashLookup interface {
	GetChannelAccessHash(ctx context.Context, userID, channelID int64) (int64, bool, error)
	GetUserAccessHash(ctx context.Context, userID, targetUserID int64) (int64, bool, error)
}

// PeerCache remembers the access hashes and names that arrive with updates
// and RPC replies, so a peer can be addressed later without a resolve
// round trip. Min entities never supply a hash: theirs is not usable for
// addressing.
type PeerCache struct {
	mu       sync.RWMutex
	users    map[int64]*UserInfo
	channels map[int64]*ChannelInfo
	chats    map[int64]*ChatInfo
	selfID   int64
	durable  HashLookup
}

// NewPeerCache builds an empty cache.
func NewPeerCache() *PeerCache {
	return &PeerCache{users: map[int64]*UserInfo{}, channels: map[int64]*ChannelInfo{}, chats: map[int64]*ChatInfo{}}
}

// SetSelf records the authenticated account id.
func (c *PeerCache) SetSelf(id int64) {
	c.mu.Lock()
	c.selfID = id
	c.mu.Unlock()
}

// SetDurable attaches the persisted hash store.
func (c *PeerCache) SetDurable(lookup HashLookup) {
	c.mu.Lock()
	c.durable = lookup
	c.mu.Unlock()
}

// Remember records every entity carried by an update.
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

// RememberUsers records users from an RPC reply.
func (c *PeerCache) RememberUsers(users []tg.UserClass) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range users {
		if user, ok := entry.(*tg.User); ok {
			c.rememberUser(user)
		}
	}
}

// RememberChats records chats and channels from an RPC reply.
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
}

// User returns what is known about a user.
func (c *PeerCache) User(id int64) (UserInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	info, ok := c.users[id]
	if !ok {
		return UserInfo{ID: id}, false
	}
	return *info, true
}

// Channel returns what is known about a channel.
func (c *PeerCache) Channel(id int64) (ChannelInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	info, ok := c.channels[id]
	if !ok {
		return ChannelInfo{ID: id}, false
	}
	return *info, true
}

// Chat returns what is known about a legacy group.
func (c *PeerCache) Chat(id int64) (ChatInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	info, ok := c.chats[id]
	if !ok {
		return ChatInfo{ID: id}, false
	}
	return *info, true
}

// Title renders a peer's display name: a chat's title or a user's name.
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

// InputPeer converts a peer into something addressable, reporting false
// when no access hash is known.
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
